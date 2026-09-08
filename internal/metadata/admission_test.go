package metadata

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresInstanceAdmission(t *testing.T) {
	config, _, _ := postgresConfig(t)
	ctx := context.Background()
	first, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var wg sync.WaitGroup
	winners := make(chan InstanceConfig, 8)
	for i := range 8 {
		wg.Go(func() {
			instance := InstanceConfig{BootID: wire.ID(), Fingerprint: strings.Repeat(string(rune('a'+i)), 64)}
			// Use valid hex for all candidates.
			if i > 5 {
				instance.Fingerprint = strings.Repeat(string(rune('0'+i)), 64)
			}
			duration, err := first.RegisterInstance(ctx, instance)
			if err == nil {
				if duration <= 0 || duration > instanceLeaseDuration {
					t.Error("invalid admission duration")
				}
				winners <- instance
			} else if !errors.Is(err, ErrConfigurationConflict) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatal("conflicting instances admitted concurrently", len(winners))
	}
	original := <-winners
	compatible := original
	compatible.BootID = wire.ID()
	if _, err := second.RegisterInstance(ctx, compatible); err != nil {
		t.Fatal("compatible replica rejected", err)
	}
	if _, err := second.RenewInstance(ctx, original); err != nil {
		t.Fatal("cross-pool renewal failed", err)
	}
	wrong := original
	wrong.Fingerprint = strings.Repeat("0", 64)
	if _, err := second.RenewInstance(ctx, wrong); !errors.Is(err, ErrConfigurationConflict) {
		t.Fatal("renewal replaced configuration", err)
	}
	wrong.BootID = wire.ID()
	wrong.RecoveryGeneration = wire.ID()
	if _, err := second.RegisterInstance(ctx, wrong); !errors.Is(err, ErrConfigurationConflict) {
		t.Fatal("mixed deployment modes admitted", err)
	}
	if _, err := first.db.Exec(`UPDATE dune_instances SET expires_at=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := second.RegisterInstance(ctx, wrong); err != nil {
		t.Fatal("expired reservations blocked new configuration", err)
	}
	if _, err := first.RenewInstance(ctx, original); !errors.Is(err, ErrConfigurationConflict) {
		t.Fatal("expired boot regained authority", err)
	}
	if _, err := second.ConnectionDirectory(ctx, wrong.RecoveryGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := second.RenewInstance(ctx, wrong); err != nil {
		t.Fatal("cluster renewal failed", err)
	}
	if _, err := second.RotateConnectionRecovery(ctx, wrong.RecoveryGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := first.RenewInstance(ctx, wrong); !errors.Is(err, ErrConfigurationConflict) {
		t.Fatal("old recovery generation renewed admission", err)
	}
}

func TestPostgresAdmissionRechecksAfterLockWait(t *testing.T) {
	config, _, _ := postgresConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	instance := InstanceConfig{BootID: wire.ID(), Fingerprint: strings.Repeat("a", 64)}
	if _, err := store.RegisterInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE dune_instances SET expires_at=` + store.databaseClock() + `+100`); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT id FROM dune_schema WHERE id=1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := store.RenewInstance(ctx, instance); done <- err }()
	time.Sleep(150 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrConfigurationConflict) {
			t.Fatal("lock delay revived expired admission", err)
		}
	case <-ctx.Done():
		t.Fatal("renewal remained blocked")
	}
}

func TestPostgresAdmissionCommitLoss(t *testing.T) {
	config, _, _ := postgresConfig(t)
	ctx := context.Background()
	store, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parsed, err := pgx.ParseConfig(config.Postgres.URL)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	if err := config.Postgres.BeforeConnect(ctx, parsed); err != nil {
		t.Fatal(err)
	}
	var commits atomic.Int32
	faulty := &Store{postgres: true, db: sql.OpenDB(lostAckConnector{Connector: stdlib.GetConnector(*parsed), commits: &commits})}
	defer faulty.Close()
	instance := InstanceConfig{BootID: wire.ID(), Fingerprint: strings.Repeat("a", 64)}
	if duration, err := faulty.RegisterInstance(ctx, instance); !errors.Is(err, ErrCommitUnknown) || duration != 0 || commits.Load() != 1 {
		t.Fatal("uncertain registration replayed or granted authority", duration, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM dune_instances WHERE boot_id=$1`, instance.BootID).Scan(&count); err != nil || count != 1 {
		t.Fatal("committed reservation lost", err)
	}
	if duration, err := faulty.RenewInstance(ctx, instance); !errors.Is(err, ErrCommitUnknown) || duration != 0 || commits.Load() != 2 {
		t.Fatal("uncertain renewal replayed or granted authority", duration, err)
	}
	other := InstanceConfig{BootID: wire.ID(), Fingerprint: strings.Repeat("b", 64)}
	if _, err := store.RegisterInstance(ctx, other); !errors.Is(err, ErrConfigurationConflict) {
		t.Fatal("unknown acknowledgement erased live reservation", err)
	}
}
