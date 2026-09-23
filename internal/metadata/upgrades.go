package metadata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/upgrade"
)

// Reserve implements the public host store contract. Metadata's transaction is
// never a Runner submission: an interrupted host dispatch remains unknown.
func (s *Store) Reserve(ctx context.Context, owner string, request upgrade.Request) (upgrade.Observation, bool, error) {
	now := time.Now().UTC()
	result := upgrade.Observation{Operation: upgrade.Operation{Request: request, Admission: api.SubmissionUnknown, StartedAt: now, UpdatedAt: now}, Freshness: "last_known", ObservedAt: now}
	if owner == "" || request.Validate() != nil {
		return result, false, ErrInvalidArgument
	}
	created := false
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockUpgradeScope(ctx, tx, owner, request.InstallationID); err != nil {
			return err
		}
		existing, err := readUpgradeObservation(ctx, tx, owner, request.InstallationID, "submission_id", request.SubmissionID)
		if err == nil {
			if existing.Operation.Request != request {
				return &api.Error{Code: "SUBMISSION_CONFLICT", Detail: "original host submission differs"}
			}
			result = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_upgrade_observations WHERE owner_id=$1 AND installation_id=$2`, owner, request.InstallationID).Scan(&count); err != nil {
			return err
		}
		if count >= 4096 {
			return &api.Error{Code: "UPGRADE_CAPACITY_EXHAUSTED", Detail: "host submission evidence capacity exhausted"}
		}
		body, err := encodeUpgradeObservation(result)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO dune_upgrade_observations(owner_id,installation_id,submission_id,operation_id,revision,data) VALUES($1,$2,$3,'',0,$4)`, owner, request.InstallationID, request.SubmissionID, body)
		created = err == nil
		return err
	})
	return result, created, err
}
func (s *Store) lockUpgradeScope(ctx context.Context, tx *sql.Tx, owner, installation string) error {
	if !s.postgres {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, owner+"/"+installation)
	return err
}
func readUpgradeObservation(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, owner, installation, column, value string) (upgrade.Observation, error) {
	var result upgrade.Observation
	var body string
	err := tx.QueryRowContext(ctx, `SELECT data FROM dune_upgrade_observations WHERE owner_id=$1 AND installation_id=$2 AND `+column+`=$3`, owner, installation, value).Scan(&body)
	if err != nil {
		return result, err
	}
	if len(body) > 512<<10 || json.Unmarshal([]byte(body), &result) != nil {
		return result, fmt.Errorf("invalid stored upgrade observation")
	}
	return result, nil
}
func encodeUpgradeObservation(observation upgrade.Observation) (string, error) {
	data, err := json.Marshal(observation)
	if err != nil {
		return "", err
	}
	if len(data) > 512<<10 {
		return "", fmt.Errorf("upgrade observation exceeds evidence limit")
	}
	return string(data), nil
}
func (s *Store) Observe(ctx context.Context, owner string, observation upgrade.Observation) error {
	op := observation.Operation
	revision, err := strconv.ParseInt(op.Revision, 10, 64)
	if owner == "" || op.Request.Validate() != nil || api.ValidateSubmissionID(op.ID) != nil || err != nil || revision < 1 || observation.ObservedAt.IsZero() || op.StartedAt.IsZero() {
		return ErrInvalidArgument
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockUpgradeScope(ctx, tx, owner, op.Request.InstallationID); err != nil {
			return err
		}
		before, err := readUpgradeObservation(ctx, tx, owner, op.Request.InstallationID, "submission_id", op.Request.SubmissionID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			old := before.Operation
			if old.Request != op.Request || (old.ID != "" && old.ID != op.ID) {
				return &api.Error{Code: "SUBMISSION_CONFLICT", Detail: "observation changed original upgrade identity"}
			}
			if !old.StartedAt.Equal(op.StartedAt) {
				return fmt.Errorf("observation changed original reservation time")
			}
			oldRevision, _ := strconv.ParseInt(old.Revision, 10, 64)
			if revision < oldRevision || (revision == oldRevision && observation.ObservedAt.Before(before.ObservedAt)) {
				return nil
			}
			if revision == oldRevision && !reflect.DeepEqual(old, op) {
				return fmt.Errorf("same observation revision changed its facts")
			}
			if old.Confirmed && op.Admission != api.SubmissionExpired {
				previous, current := old, op
				previous.Revision, current.Revision = "", ""
				previous.UpdatedAt, current.UpdatedAt = time.Time{}, time.Time{}
				previous.LaunchSealed, current.LaunchSealed = false, false
				if !reflect.DeepEqual(previous, current) {
					return fmt.Errorf("confirmed terminal facts cannot change")
				}
			}
			if old.Confirmed && (!op.Confirmed || old.Phase != op.Phase) {
				return fmt.Errorf("confirmed upgrade observation cannot regress")
			}
		} else {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_upgrade_observations WHERE owner_id=$1 AND installation_id=$2`, owner, op.Request.InstallationID).Scan(&count); err != nil {
				return err
			}
			if count >= 4096 {
				return &api.Error{Code: "UPGRADE_CAPACITY_EXHAUSTED", Detail: "host observation evidence capacity exhausted"}
			}
		}
		body, err := encodeUpgradeObservation(observation)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO dune_upgrade_observations(owner_id,installation_id,submission_id,operation_id,revision,data) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(owner_id,installation_id,submission_id) DO UPDATE SET operation_id=excluded.operation_id,revision=excluded.revision,data=excluded.data`, owner, op.Request.InstallationID, op.Request.SubmissionID, op.ID, revision, body)
		return err
	})
}
func (s *Store) Observation(ctx context.Context, owner string, query upgrade.Query) (upgrade.Observation, error) {
	if owner == "" || query.Validate() != nil {
		return upgrade.Observation{}, ErrInvalidArgument
	}
	column, value := "submission_id", query.SubmissionID
	if value == "" {
		column, value = "operation_id", query.OperationID
	}
	result, err := readUpgradeObservation(ctx, s.db, owner, query.InstallationID, column, value)
	if errors.Is(err, sql.ErrNoRows) {
		return result, upgrade.ErrObservationNotFound
	}
	if err != nil {
		return result, err
	}
	if result.Operation.Request.Binding != query.Binding {
		return upgrade.Observation{}, upgrade.ErrObservationNotFound
	}
	result.Freshness = "last_known"
	return result, nil
}
func (s *Store) Observations(ctx context.Context, owner string, request upgrade.ListRequest) (upgrade.History, error) {
	return s.upgradeHistory(ctx, owner, request, false)
}

func (s *Store) UnknownSubmissions(ctx context.Context, owner string, request upgrade.ListRequest) (upgrade.Page, error) {
	history, err := s.upgradeHistory(ctx, owner, request, true)
	return history.Page, err
}

func (s *Store) upgradeHistory(ctx context.Context, owner string, request upgrade.ListRequest, unknownOnly bool) (upgrade.History, error) {
	result := upgrade.History{Freshness: "last_known"}
	if owner == "" || request.Validate() != nil {
		return result, ErrInvalidArgument
	}
	query := `SELECT data FROM dune_upgrade_observations WHERE owner_id=$1 AND installation_id=$2`
	if unknownOnly {
		query += ` AND operation_id=''`
	}
	rows, err := s.db.QueryContext(ctx, query+` LIMIT 4096`, owner, request.InstallationID)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	var operations []upgrade.Operation
	for rows.Next() {
		var body string
		var item upgrade.Observation
		if err := rows.Scan(&body); err != nil {
			return result, err
		}
		if json.Unmarshal([]byte(body), &item) != nil {
			return result, fmt.Errorf("invalid upgrade observation")
		}
		if item.Operation.Request.Binding != request.Binding {
			continue
		}
		// Unknown host submissions lack an executor operation ID. A stable local
		// ordering uses the original submission ID without inventing an operation.
		operations = append(operations, item.Operation)
		if item.ObservedAt.After(result.ObservedAt) {
			result.ObservedAt = item.ObservedAt
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	result.Page, err = upgrade.Paginate(operations, request.Cursor, request.Limit)
	return result, err
}
