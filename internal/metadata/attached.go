package metadata

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/runner"
)

type Machine struct {
	ID        string `json:"id"`
	RunnerID  string `json:"runner_id"`
	Name      string `json:"name"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CreatedAt int64  `json:"created_at"`
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) IssueEnrollment(ctx context.Context, userID, name string) (string, int64, error) {
	return s.issueEnrollment(ctx, userID, name, "", "", "")
}
func (s *Store) IssueEnrollmentForSession(ctx context.Context, user identity.User, name, hash string) (string, int64, error) {
	if hash == "" {
		return "", 0, identity.ErrUnauthorized
	}
	return s.issueEnrollment(ctx, user.ID, name, hash, user.Namespace, user.Subject)
}
func (s *Store) issueEnrollment(ctx context.Context, userID, name, hash, namespace, subject string) (string, int64, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 || strings.ContainsFunc(name, unicode.IsControl) {
		return "", 0, fmt.Errorf("%w: machine name must be 1..120 bytes without control characters", ErrInvalidArgument)
	}
	token := wire.ID() + wire.ID()
	expires := time.Now().Add(10 * time.Minute).Unix()
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockPrincipal(ctx, tx, userID); err != nil {
			return err
		}
		if hash != "" {
			if err := s.checkBrowserSession(ctx, tx, userID, hash, namespace, subject); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_enrollments WHERE principal_id=$1 AND expires_at<=$2`, userID, time.Now().Unix()); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_enrollments WHERE principal_id=$1`, userID).Scan(&count); err != nil {
			return err
		}
		if count >= 5 {
			return fmt.Errorf("%w: at most five pending binding commands; existing commands expire after ten minutes", ErrInvalidArgument)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO dune_enrollments(hash,principal_id,name,expires_at,identity_namespace,identity_subject) VALUES($1,$2,$3,$4,$5,$6)`, tokenHash(token), userID, name, expires, namespace, subject)
		return err
	})
	if err != nil {
		return "", 0, err
	}
	return token, expires, nil
}

func (s *Store) Enroll(ctx context.Context, token, osName, arch string) (Machine, string, error) {
	if len(token) != 64 {
		return Machine{}, "", identity.ErrUnauthorized
	}
	if (osName != "linux" && osName != "darwin") || (arch != "amd64" && arch != "arm64") {
		return Machine{}, "", fmt.Errorf("%w: supported platforms: Linux/macOS on amd64/arm64", ErrInvalidArgument)
	}
	credential := wire.ID() + wire.ID()
	machine := Machine{ID: wire.ID(), RunnerID: wire.ID(), OS: osName, Arch: arch, CreatedAt: time.Now().Unix()}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var owner string
		hash := tokenHash(token)
		managed := false
		err := tx.QueryRowContext(ctx, `SELECT principal_id FROM dune_enrollments WHERE hash=$1`, hash).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `SELECT o.principal_id FROM dune_managed_enrollments e JOIN dune_operations o ON o.id=e.operation_id WHERE e.hash=$1`, hash).Scan(&owner)
			managed = err == nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return identity.ErrUnauthorized
		}
		if err != nil {
			return err
		}
		// Lock the principal before consuming enrollment, consistently with issue
		// and account limits. Re-read after taking the lock: another consumer may
		// have completed while this transaction waited.
		if err := s.lockPrincipal(ctx, tx, owner); err != nil {
			return err
		}
		if managed {
			return s.consumeManagedEnrollment(ctx, tx, hash, owner, &machine, credential)
		}
		var expires int64
		if err := tx.QueryRowContext(ctx, `SELECT name,expires_at FROM dune_enrollments WHERE hash=$1`, hash).Scan(&machine.Name, &expires); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return identity.ErrUnauthorized
			}
			return err
		}
		if expires <= time.Now().Unix() {
			return identity.ErrUnauthorized
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_runners WHERE owner_id=$1`, owner).Scan(&count); err != nil {
			return err
		}
		if count >= 32 {
			return fmt.Errorf("%w: runner limit reached (32 per account)", ErrInvalidArgument)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_runners(id,owner_id,name,kind,fabric_id,binding_revision,created_at) VALUES($1,$2,$3,'attached','attached',1,$4)`, machine.RunnerID, owner, machine.Name, machine.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_machines(id,runner_id,credential_hash,os,arch) VALUES($1,$2,$3,$4,$5)`, machine.ID, machine.RunnerID, tokenHash(credential), osName, arch); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM dune_enrollments WHERE hash=$1`, hash)
		return err
	})
	if err != nil {
		return Machine{}, "", err
	}
	return machine, credential, nil
}

func (s *Store) consumeManagedEnrollment(ctx context.Context, tx *sql.Tx, hash, owner string, machine *Machine, credential string) error {
	var actionID, operationID, runnerID, expectedFabric, expectedRef string
	var expectedRevision, expires int64
	query := `SELECT action_id,operation_id,runner_id,fabric_id,binding_revision,resource_ref,expires_at FROM dune_managed_enrollments WHERE hash=$1`
	if s.postgres {
		query += ` FOR UPDATE`
	}
	if err := tx.QueryRowContext(ctx, query, hash).Scan(&actionID, &operationID, &runnerID, &expectedFabric, &expectedRevision, &expectedRef, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return identity.ErrUnauthorized
		}
		return err
	}
	fabricID, revision, err := s.lockOperationRunner(ctx, tx, runnerID)
	if err != nil {
		return err
	}
	var kind, runnerOwner string
	if err := tx.QueryRowContext(ctx, `SELECT name,kind,owner_id,created_at FROM dune_runners WHERE id=$1`, runnerID).Scan(&machine.Name, &kind, &runnerOwner, &machine.CreatedAt); err != nil {
		return err
	}
	opQuery := "SELECT " + operationColumns + " FROM dune_operations WHERE id=$1"
	if s.postgres {
		opQuery += ` FOR UPDATE`
	}
	op, err := scanOperation(tx.QueryRowContext(ctx, opQuery, operationID))
	if err != nil {
		return err
	}
	now, err := s.databaseNow(ctx, tx)
	if err != nil {
		return err
	}
	resource, err := scanManagedResource(tx.QueryRowContext(ctx, "SELECT "+resourceColumns+" FROM dune_managed_resources WHERE runner_id=$1", runnerID))
	if err != nil {
		return err
	}
	bootstrap, err := scanAction(tx.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM dune_provider_actions WHERE id=$1 AND operation_id=$2 AND kind='bootstrap'", actionID, operationID))
	if err != nil {
		return err
	}
	if expires <= now || kind != "managed" || runnerOwner != owner || fabricID != expectedFabric || revision != expectedRevision ||
		op.PrincipalID != owner || op.RunnerID != runnerID || op.FabricID != expectedFabric || op.BindingRevision != expectedRevision || op.Action != "create" || op.Finished ||
		resource.FabricID != expectedFabric || resource.Ref != expectedRef || resource.Gone || resource.AccessClosed || (!resource.ExpiresAt.IsZero() && resource.ExpiresAt.UnixMilli() <= now) ||
		bootstrap.ResourceRef != expectedRef || bootstrap.Outcome == "failed" {
		return identity.ErrUnauthorized
	}
	machine.RunnerID = runnerID
	if _, err := tx.ExecContext(ctx, `INSERT INTO dune_machines(id,runner_id,credential_hash,os,arch) VALUES($1,$2,$3,$4,$5)`, machine.ID, runnerID, tokenHash(credential), machine.OS, machine.Arch); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM dune_managed_enrollments WHERE hash=$1 AND action_id=$2 AND operation_id=$3 AND runner_id=$4 AND expires_at>`+s.databaseClock(), hash, actionID, operationID, runnerID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return identity.ErrUnauthorized
	}
	return nil
}

func (s *Store) Machines(ctx context.Context, userID string) ([]Machine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id,r.id,r.name,m.os,m.arch,r.created_at FROM dune_machines m JOIN dune_runners r ON r.id=m.runner_id WHERE r.owner_id=$1 ORDER BY r.created_at,m.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Machine{}
	for rows.Next() {
		var m Machine
		if err := rows.Scan(&m.ID, &m.RunnerID, &m.Name, &m.OS, &m.Arch, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) Owns(ctx context.Context, userID, machineID string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM dune_machines m JOIN dune_runners r ON r.id=m.runner_id WHERE m.id=$1 AND r.owner_id=$2`, machineID, userID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) MachineCredential(ctx context.Context, token string) (string, error) {
	if len(token) != 64 {
		return "", identity.ErrUnauthorized
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM dune_machines WHERE credential_hash=$1`, tokenHash(token)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = identity.ErrUnauthorized
	}
	return id, err
}

func (s *Store) Revoke(ctx context.Context, userID, machineID string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM dune_runners WHERE owner_id=$1 AND id=(SELECT runner_id FROM dune_machines WHERE id=$2)`, userID, machineID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *Store) EnrollmentIdentity(ctx context.Context, token string) (identity.User, string, error) {
	var user identity.User
	if len(token) != 64 {
		return user, "", identity.ErrUnauthorized
	}
	var kind string
	clock := s.databaseClock()
	hash := tokenHash(token)
	err := s.db.QueryRowContext(ctx, `SELECT p.id,p.email,e.identity_namespace,e.identity_subject FROM dune_enrollments e JOIN dune_principals p ON p.id=e.principal_id WHERE e.hash=$1 AND e.expires_at>(`+clock+`/1000) AND p.enabled=TRUE`, hash).Scan(&user.ID, &user.Email, &user.Namespace, &user.Subject)
	if err == nil {
		return user, "attached", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return user, "", err
	}
	err = s.db.QueryRowContext(ctx, `SELECT p.id,p.email,o.identity_namespace,o.identity_subject FROM dune_managed_enrollments e JOIN dune_operations o ON o.id=e.operation_id JOIN dune_principals p ON p.id=o.principal_id WHERE e.hash=$1 AND e.expires_at>`+clock+` AND p.enabled=TRUE`, hash).Scan(&user.ID, &user.Email, &user.Namespace, &user.Subject)
	kind = "managed"
	if errors.Is(err, sql.ErrNoRows) {
		err = identity.ErrUnauthorized
	}
	return user, kind, err
}

func (s *Store) EnrollmentUser(ctx context.Context, token string) (identity.User, error) {
	user, _, err := s.EnrollmentIdentity(ctx, token)
	return user, err
}
func (s *Store) RevokeAuthorized(ctx context.Context, user identity.User, hash string, expected authorization.Resource) error {
	if expected.Runner.Binding == nil || expected.Runner.Kind != "attached" {
		return ErrInvalidArgument
	}
	b := *expected.Runner.Binding
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockPrincipal(ctx, tx, user.ID); err != nil {
			return err
		}
		if err := s.checkBrowserSession(ctx, tx, user.ID, hash, user.Namespace, user.Subject); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM dune_runners WHERE id=$1 AND owner_id=$2 AND kind='attached' AND fabric_id=$3 AND binding_revision=$4 AND EXISTS(SELECT 1 FROM dune_machines WHERE runner_id=$1 AND id=$5)`, b.RunnerID, expected.OwnerID, b.FabricID, b.Revision, b.MachineID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return runner.ErrBindingChanged
		}
		return nil
	})
}
