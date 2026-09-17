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
	"github.com/aiomni/dune/pkg/api"
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

// AttachedRunnerFacts contains only the ownership and binding facts a trusted
// host needs to apply its own product policy. Credentials and enrollment
// material never leave Dune through this view.
type AttachedRunnerFacts struct {
	OwnerID   string
	Runner    runner.Runner
	CreatedBy identity.User
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func validRunnerName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 || strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("%w: machine name must be 1..120 bytes without control characters", ErrInvalidArgument)
	}
	return name, nil
}

// IssueEnrollment is for trusted local callers. Browser code uses
// IssueEnrollmentForSession to recheck local session revocation in the transaction.
func (s *Store) IssueEnrollment(ctx context.Context, userID, name string) (runner.Runner, string, int64, error) {
	return s.issueAttachedEnrollment(ctx, identity.User{ID: userID}, userID, name, "")
}

func (s *Store) IssueEnrollmentForSession(ctx context.Context, user identity.User, name, sessionHash string) (runner.Runner, string, int64, error) {
	if user.ID == "" || sessionHash == "" {
		return runner.Runner{}, "", 0, identity.ErrUnauthorized
	}
	return s.issueAttachedEnrollment(ctx, user, user.ID, name, sessionHash)
}

func (s *Store) IssueTenantEnrollment(ctx context.Context, user identity.User, ownerID, name, sessionHash string) (runner.Runner, string, int64, error) {
	if user.ID == "" || ownerID == "" || ownerID == user.ID || sessionHash == "" {
		return runner.Runner{}, "", 0, identity.ErrUnauthorized
	}
	return s.issueAttachedEnrollment(ctx, user, ownerID, name, sessionHash)
}

func (s *Store) issueAttachedEnrollment(ctx context.Context, user identity.User, ownerID, name, sessionHash string) (runner.Runner, string, int64, error) {
	name, err := validRunnerName(name)
	if err != nil {
		return runner.Runner{}, "", 0, err
	}
	logical := runner.Runner{ID: wire.ID(), Name: name, Kind: "attached"}
	token, expires, err := s.issueEnrollment(ctx, user, ownerID, name, sessionHash, logical.ID, "attached")
	if err != nil {
		return runner.Runner{}, "", 0, err
	}
	return logical, token, expires, nil
}

// IssueManagedEnrollment is the small Dune-side attachment primitive available
// to an external Managed implementation. Provider operations and their state do
// not enter Dune; only the resulting logical Runner and one-shot binding token do.
func (s *Store) IssueManagedEnrollment(ctx context.Context, user identity.User, ownerID string, logical runner.Runner, fabricID string) (string, int64, error) {
	if user.ID == "" || ownerID == "" || logical.ID == "" || fabricID == "" || logical.Kind != "managed" || logical.Binding != nil {
		return "", 0, ErrInvalidArgument
	}
	return s.issueEnrollment(ctx, user, ownerID, logical.Name, "", logical.ID, "managed:"+fabricID)
}

func (s *Store) issueEnrollment(ctx context.Context, user identity.User, ownerID, name, sessionHash, runnerID, mode string) (string, int64, error) {
	if ownerID == "" || runnerID == "" {
		return "", 0, ErrInvalidArgument
	}
	name, err := validRunnerName(name)
	if err != nil {
		return "", 0, err
	}
	kind, fabricID := "attached", "attached"
	if strings.HasPrefix(mode, "managed:") {
		kind, fabricID = "managed", strings.TrimPrefix(mode, "managed:")
	}
	token := wire.ID() + wire.ID()
	expires := time.Now().Add(10 * time.Minute).Unix()
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if sessionHash != "" {
			if err := s.checkLocalSession(ctx, tx, user.ID, sessionHash); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_enrollments WHERE owner_id=$1 AND expires_at<=$2`, ownerID, time.Now().Unix()); err != nil {
			return err
		}
		if err := disableUnenrollableAttached(ctx, tx, ownerID); err != nil {
			return err
		}
		tenantOwned := ownerID != user.ID
		if !tenantOwned {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_enrollments WHERE owner_id=$1`, ownerID).Scan(&count); err != nil {
				return err
			}
			if count >= 5 {
				return fmt.Errorf("%w: at most five pending binding commands", ErrInvalidArgument)
			}
		}
		if !tenantOwned {
			var runners int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_runners WHERE owner_id=$1 AND enabled=TRUE`, ownerID).Scan(&runners); err != nil {
				return err
			}
			if runners >= 32 {
				return fmt.Errorf("%w: runner limit reached", ErrInvalidArgument)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_runners(id,owner_id,created_by_id,created_by_namespace,created_by_subject,name,kind,fabric_id,binding_revision,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,1,$9)`, runnerID, ownerID, user.ID, user.Namespace, user.Subject, name, kind, fabricID, time.Now().Unix()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO dune_enrollments(hash,owner_id,issued_to_id,issued_to_kind,namespace,subject,name,runner_id,kind,fabric_id,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, tokenHash(token), ownerID, user.ID, user.Kind, user.Namespace, user.Subject, name, runnerID, kind, fabricID, expires)
		return err
	})
	if err != nil {
		return "", 0, err
	}
	return token, expires, nil
}

// Tokens are removed before locking their pending Runners, matching Enroll's
// lock order. Expired or revoked commands must not leave usable-looking entries
// that permanently consume a personal Runner quota.
func disableUnenrollableAttached(ctx context.Context, tx *sql.Tx, ownerID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE dune_runners SET enabled=FALSE WHERE owner_id=$1 AND kind='attached' AND machine_id IS NULL AND enabled=TRUE AND NOT EXISTS (SELECT 1 FROM dune_enrollments e WHERE e.runner_id=dune_runners.id)`, ownerID)
	return err
}

func (s *Store) Enroll(ctx context.Context, token, osName, arch string) (Machine, string, error) {
	if len(token) != 64 {
		return Machine{}, "", identity.ErrUnauthorized
	}
	if (osName != "linux" && osName != "darwin") || (arch != "amd64" && arch != "arm64") {
		return Machine{}, "", fmt.Errorf("%w: supported platforms: Linux/macOS on amd64/arm64", ErrInvalidArgument)
	}
	credential := wire.ID() + wire.ID()
	machine := Machine{ID: wire.ID(), OS: osName, Arch: arch, CreatedAt: time.Now().Unix()}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		query := `SELECT owner_id,name,runner_id,kind,fabric_id,expires_at FROM dune_enrollments WHERE hash=$1`
		if s.postgres {
			query += ` FOR UPDATE`
		}
		var owner, kind, fabricID string
		var expires int64
		if err := tx.QueryRowContext(ctx, query, tokenHash(token)).Scan(&owner, &machine.Name, &machine.RunnerID, &kind, &fabricID, &expires); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return identity.ErrUnauthorized
			}
			return err
		}
		if expires <= time.Now().Unix() {
			return identity.ErrUnauthorized
		}

		result, err := tx.ExecContext(ctx, `UPDATE dune_runners SET machine_id=$2,credential_hash=$3,os=$4,arch=$5 WHERE id=$1 AND owner_id=$6 AND kind=$7 AND fabric_id=$8 AND machine_id IS NULL AND enabled=TRUE`, machine.RunnerID, machine.ID, tokenHash(credential), osName, arch, owner, kind, fabricID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return identity.ErrUnauthorized
		}
		result, err = tx.ExecContext(ctx, `DELETE FROM dune_enrollments WHERE hash=$1 AND expires_at>$2`, tokenHash(token), time.Now().Unix())
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return identity.ErrUnauthorized
		}
		return nil
	})
	if err != nil {
		return Machine{}, "", err
	}
	return machine, credential, nil
}

func (s *Store) Machines(ctx context.Context, userID string) ([]Machine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT machine_id,id,name,os,arch,created_at FROM dune_runners WHERE owner_id=$1 AND enabled=TRUE AND machine_id IS NOT NULL ORDER BY created_at,machine_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Machine{}
	for rows.Next() {
		var machine Machine
		if err := rows.Scan(&machine.ID, &machine.RunnerID, &machine.Name, &machine.OS, &machine.Arch, &machine.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, machine)
	}
	return out, rows.Err()
}

func (s *Store) Owns(ctx context.Context, userID, machineID string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM dune_runners WHERE machine_id=$1 AND owner_id=$2 AND enabled=TRUE`, machineID, userID).Scan(&found)
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
	err := s.db.QueryRowContext(ctx, `SELECT machine_id FROM dune_runners WHERE credential_hash=$1 AND enabled=TRUE`, tokenHash(token)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = identity.ErrUnauthorized
	}
	return id, err
}

// Revoke persists the disabled state and drops the machine credential. Route
// expiry and the Gateway validity callback provide the cross-instance backstop.
func (s *Store) Revoke(ctx context.Context, userID, machineID string) error {
	return s.disableRunner(ctx, userID, machineID, runner.Binding{})
}

func (s *Store) disableRunner(ctx context.Context, owner, machineID string, expected runner.Binding) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		query := `UPDATE dune_runners SET enabled=FALSE,credential_hash=NULL WHERE owner_id=$1 AND machine_id=$2 AND enabled=TRUE`
		args := []any{owner, machineID}
		if expected.Valid() {
			query += ` AND id=$3 AND fabric_id=$4 AND binding_revision=$5`
			args = append(args, expected.RunnerID, expected.FabricID, expected.Revision)
		}
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			if expected.Valid() {
				return runner.ErrBindingChanged
			}
			return ErrNotFound
		}
		return nil
	})
}

// SetManagedSuspended gates discovery and user execution without discarding or
// disabling the machine credential. A resumed provider can therefore reconnect
// fabricd while the user gate remains closed; SandDance opens that gate only
// after it observes the connector online. The machine ID comes from Dune's
// binding record, never from lifecycle-service output.
func (s *Store) SetManagedSuspended(ctx context.Context, owner, runnerID string, suspended bool) (string, error) {
	var machine sql.NullString
	err := s.db.QueryRowContext(ctx, `UPDATE dune_runners SET suspended=$3 WHERE id=$1 AND owner_id=$2 AND kind='managed' AND enabled=TRUE RETURNING machine_id`, runnerID, owner, suspended).Scan(&machine)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return machine.String, err
}

// RevokeManaged persistently disables a logical Managed Runner and invalidates
// both an issued machine credential and a still-pending enrollment token. It
// is idempotent so a lifecycle service can retry provider cleanup after the
// access gate was closed by an earlier failed destroy operation.
func (s *Store) RevokeManaged(ctx context.Context, owner, runnerID string) (string, error) {
	var machine sql.NullString
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		query := `UPDATE dune_runners SET enabled=FALSE,suspended=FALSE,credential_hash=NULL WHERE id=$1 AND owner_id=$2 AND kind='managed' AND enabled=TRUE RETURNING machine_id`
		if err := tx.QueryRowContext(ctx, query, runnerID, owner).Scan(&machine); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				var exists bool
				err = tx.QueryRowContext(ctx, `SELECT TRUE FROM dune_runners WHERE id=$1 AND owner_id=$2 AND kind='managed' AND enabled=FALSE`, runnerID, owner).Scan(&exists)
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM dune_enrollments WHERE owner_id=$1 AND runner_id=$2`, owner, runnerID)
		return err
	})
	return machine.String, err
}

// AttachedRunner returns one active Attached Runner in an exact owner scope.
// Pending is represented by a nil Runner.Binding.
func (s *Store) AttachedRunner(ctx context.Context, ownerID, runnerID string) (AttachedRunnerFacts, error) {
	var facts AttachedRunnerFacts
	var binding runner.Binding
	var machine sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT owner_id,id,name,kind,created_at,fabric_id,binding_revision,machine_id,created_by_id,created_by_namespace,created_by_subject FROM dune_runners WHERE owner_id=$1 AND id=$2 AND kind='attached' AND enabled=TRUE`, ownerID, runnerID).Scan(
		&facts.OwnerID, &facts.Runner.ID, &facts.Runner.Name, &facts.Runner.Kind, &facts.Runner.CreatedAt,
		&binding.FabricID, &binding.Revision, &machine,
		&facts.CreatedBy.ID, &facts.CreatedBy.Namespace, &facts.CreatedBy.Subject,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AttachedRunnerFacts{}, ErrNotFound
	}
	if err != nil {
		return AttachedRunnerFacts{}, err
	}
	if machine.Valid {
		binding.RunnerID, binding.MachineID = facts.Runner.ID, machine.String
		facts.Runner.Binding = &binding
	}
	return facts, nil
}

// HasActiveAttached reports whether an owner still has any active Attached
// Runner. It is a bounded existence query for host-owned Tenant deletion rules.
func (s *Store) HasActiveAttached(ctx context.Context, ownerID string) (bool, error) {
	if ownerID == "" {
		return false, ErrInvalidArgument
	}
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM dune_runners WHERE owner_id=$1 AND kind='attached' AND enabled=TRUE)`, ownerID).Scan(&found)
	return found, err
}

// CancelAttachedEnrollment atomically invalidates a pending Attached command
// and disables its preallocated logical Runner. If enrollment won the race, the
// exact binding is preserved and ErrBindingChanged is returned.
func (s *Store) CancelAttachedEnrollment(ctx context.Context, ownerID, runnerID string) error {
	return s.cancelAttachedEnrollment(ctx, ownerID, runnerID, "")
}

func (s *Store) CancelEnrollmentForSession(ctx context.Context, user identity.User, runnerID, sessionHash string) error {
	if user.ID == "" || sessionHash == "" {
		return identity.ErrUnauthorized
	}
	return s.cancelAttachedEnrollment(ctx, user.ID, runnerID, sessionHash)
}

func (s *Store) cancelAttachedEnrollment(ctx context.Context, ownerID, runnerID, sessionHash string) error {
	if ownerID == "" || runnerID == "" {
		return ErrInvalidArgument
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if sessionHash != "" {
			if err := s.checkLocalSession(ctx, tx, ownerID, sessionHash); err != nil {
				return err
			}
		}
		// Enroll locks the token before binding the Runner. Keep that lock order so
		// either enrollment or cancellation wins atomically on every SQL backend.
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_enrollments WHERE owner_id=$1 AND runner_id=$2 AND kind='attached'`, ownerID, runnerID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE dune_runners SET enabled=FALSE,credential_hash=NULL WHERE id=$1 AND owner_id=$2 AND kind='attached' AND machine_id IS NULL AND enabled=TRUE`, runnerID, ownerID)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed == 1 {
			return nil
		}
		var kind string
		var machine sql.NullString
		var enabled bool
		if err := tx.QueryRowContext(ctx, `SELECT kind,machine_id,enabled FROM dune_runners WHERE id=$1 AND owner_id=$2`, runnerID, ownerID).Scan(&kind, &machine, &enabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if kind == "attached" && enabled && machine.Valid {
			return runner.ErrBindingChanged
		}
		return ErrNotFound
	})
}

func (s *Store) EnrollmentIdentity(ctx context.Context, token string) (identity.User, string, string, error) {
	var user identity.User
	if len(token) != 64 {
		return user, "", "", identity.ErrUnauthorized
	}
	var ownerID, kind string
	err := s.db.QueryRowContext(ctx, `SELECT issued_to_id,issued_to_kind,owner_id,namespace,subject,kind FROM dune_enrollments WHERE hash=$1 AND expires_at>`+s.databaseClock()+`/1000`, tokenHash(token)).Scan(&user.ID, &user.Kind, &ownerID, &user.Namespace, &user.Subject, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		err = identity.ErrUnauthorized
	}
	return user, ownerID, kind, err
}

func (s *Store) EnrollmentUser(ctx context.Context, token string) (identity.User, error) {
	user, _, _, err := s.EnrollmentIdentity(ctx, token)
	return user, err
}

func (s *Store) RevokeAuthorized(ctx context.Context, user identity.User, sessionHash string, expected authorization.Resource) error {
	if expected.Runner.Binding == nil || expected.Runner.Kind != "attached" {
		return ErrInvalidArgument
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.checkLocalSession(ctx, tx, user.ID, sessionHash); err != nil {
			return err
		}
		b := *expected.Runner.Binding
		result, err := tx.ExecContext(ctx, `UPDATE dune_runners SET enabled=FALSE,credential_hash=NULL WHERE id=$1 AND owner_id=$2 AND kind='attached' AND fabric_id=$3 AND binding_revision=$4 AND machine_id=$5 AND enabled=TRUE`, b.RunnerID, expected.OwnerID, b.FabricID, b.Revision, b.MachineID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return runner.ErrBindingChanged
		}
		return nil
	})
}

func (s *Store) ConfirmMachineOnline(ctx context.Context, binding api.Binding) error {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM dune_runners WHERE machine_id=$1 AND enabled=TRUE`, binding.Target).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.ErrUnauthorized
	}
	return err
}
