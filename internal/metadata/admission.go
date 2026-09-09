package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aiomni/dune/internal/wire"
)

var ErrConfigurationConflict = errors.New("another live instance uses incompatible configuration")

// InstanceConfig records only a nonsecret configuration digest and deployment
// generation. BootID is fresh for each registration, independently of routing.
type InstanceConfig struct{ BootID, Fingerprint, RecoveryGeneration string }

const instanceColumns = "boot_id,fingerprint,recovery_generation,expires_at"

const (
	instanceLeaseDuration = 15 * time.Second
	instanceAdmissionLock = int64(1146441288)
)

func (s *Store) RegisterInstance(ctx context.Context, instance InstanceConfig) (time.Duration, error) {
	return s.instanceLease(ctx, instance, false)
}
func (s *Store) RenewInstance(ctx context.Context, instance InstanceConfig) (time.Duration, error) {
	return s.instanceLease(ctx, instance, true)
}

// No early-release operation is provided: previously issued protocol input may
// remain buffered until its original grant expires, even after App.Close.
func (s *Store) instanceLease(ctx context.Context, instance InstanceConfig, renew bool) (time.Duration, error) {
	if !s.postgres || !wire.ValidID(instance.BootID) || !peerHash(instance.Fingerprint) || (instance.RecoveryGeneration != "" && !wire.ValidID(instance.RecoveryGeneration)) {
		return 0, ErrInvalidArgument
	}
	var remaining time.Duration
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		// Serialize membership changes, including simultaneous standalone/cluster
		// startup. This lock carries no process authority itself.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, instanceAdmissionLock); err != nil {
			return err
		}
		var recovery string
		err := tx.QueryRowContext(ctx, `SELECT recovery_generation FROM dune_cluster WHERE id=1 FOR SHARE`).Scan(&recovery)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if (err == nil && recovery != instance.RecoveryGeneration) || (renew && err != nil && instance.RecoveryGeneration != "") {
			return ErrConfigurationConflict
		}
		clock := s.databaseClock()
		var until int64
		if renew {
			err = tx.QueryRowContext(ctx, `UPDATE dune_instances SET expires_at=GREATEST(expires_at,`+clock+`+15000) WHERE boot_id=$1 AND fingerprint=$2 AND recovery_generation=$3 AND expires_at>`+clock+` RETURNING expires_at`, instance.BootID, instance.Fingerprint, instance.RecoveryGeneration).Scan(&until)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrConfigurationConflict
			}
			if err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `DELETE FROM dune_instances WHERE expires_at<=`+clock); err != nil {
				return err
			}
			var count, conflicts int
			if err := tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE fingerprint<>$1 OR recovery_generation<>$2) FROM dune_instances`, instance.Fingerprint, instance.RecoveryGeneration).Scan(&count, &conflicts); err != nil {
				return err
			}
			if conflicts != 0 {
				return ErrConfigurationConflict
			}
			if count >= 256 {
				return fmt.Errorf("instance admission capacity exceeded")
			}
			if err := tx.QueryRowContext(ctx, `INSERT INTO dune_instances(boot_id,fingerprint,recovery_generation,expires_at) VALUES($1,$2,$3,`+clock+`+15000) RETURNING expires_at`, instance.BootID, instance.Fingerprint, instance.RecoveryGeneration).Scan(&until); err != nil {
				return err
			}
		}
		now, err := s.databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		remaining = min(instanceLeaseDuration, time.Duration(until-now)*time.Millisecond)
		if remaining <= 0 {
			return ErrConfigurationConflict
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return remaining, nil
}
