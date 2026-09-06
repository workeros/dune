package metadata

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/gateway"
)

const routeColumns = "machine_id,recovery_generation,epoch,owner_boot_id,owner_address,binding,published,expires_at"
const connectionLeaseDuration = 15 * time.Second

type connectionDirectory struct {
	store    *Store
	recovery string
}

var _ gateway.Directory = (*connectionDirectory)(nil)

func validBootID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == id
}

// ConnectionDirectory selects the directory in this transaction domain. The
// application supplies one stable recovery generation shared by its replicas.
// Initial creation is explicit; an existing generation is never replaced at
// startup. SQLite does not provide cluster ownership, even for a single caller.
func (s *Store) ConnectionDirectory(ctx context.Context, recovery string) (gateway.Directory, error) {
	if !s.postgres || !validBootID(recovery) {
		return nil, fmt.Errorf("connection directory requires PostgreSQL and a recovery generation")
	}
	d := &connectionDirectory{store: s, recovery: recovery}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_cluster(id,recovery_generation) VALUES(1,$1) ON CONFLICT(id) DO NOTHING`, recovery); err != nil {
			return err
		}
		return d.lockRecovery(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// RotateConnectionRecovery is a trusted offline recovery operation, not a
// service startup action. It generates a fresh identity instead of letting a
// caller reuse a historical generation after restoring a backup. On an unknown
// commit outcome the returned ID allows reconciliation without another rotation.
// The operator must stop old replicas and deploy the returned generation before
// admitting connections. Existing route records remain fenced and inspectable.
func (s *Store) RotateConnectionRecovery(ctx context.Context, expected string) (string, error) {
	if !s.postgres || !validBootID(expected) {
		return "", ErrInvalidArgument
	}
	next := wire.ID()
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE dune_cluster SET recovery_generation=$1 WHERE id=1 AND recovery_generation=$2`, next, expected)
		if err != nil {
			return err
		}
		return changedRoute(result)
	})
	return next, err
}

func (d *connectionDirectory) lockRecovery(ctx context.Context, tx *sql.Tx) error {
	var actual string
	err := tx.QueryRowContext(ctx, `SELECT recovery_generation FROM dune_cluster WHERE id=1 FOR SHARE`).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && actual != d.recovery) {
		return gateway.ErrRouteStale
	}
	return err
}

func validRoute(route gateway.RouteClaim, recovery string) bool {
	if route.RecoveryGeneration != recovery || !validBootID(route.OwnerBootID) || route.Target == "" || len(route.Target) > 128 || strings.ContainsFunc(route.Target, unicode.IsControl) {
		return false
	}
	address, err := url.Parse(route.OwnerAddress)
	if err != nil || len(route.OwnerAddress) > 2048 || address.Hostname() == "" || address.User != nil || address.RawQuery != "" || address.ForceQuery || strings.Contains(address.Hostname(), "*") || address.Fragment != "" || (address.Scheme != "https" && address.Scheme != "http") {
		return false
	}
	if ip := net.ParseIP(address.Hostname()); ip != nil && ip.IsUnspecified() {
		return false
	}
	binding := route.Binding
	if binding.Target != route.Target || binding.Version == "" || len(binding.Version) > 64 || strings.ContainsFunc(binding.Version, unicode.IsControl) || binding.Incarnation == "" || len(binding.Incarnation) > 128 || strings.ContainsFunc(binding.Incarnation, unicode.IsControl) || binding.Generation == 0 || len(binding.Capabilities) > 128 || len(binding.Limits) > 32 {
		return false
	}
	for _, capability := range binding.Capabilities {
		if capability == "" || len(capability) > 64 || strings.ContainsFunc(capability, unicode.IsControl) {
			return false
		}
	}
	for key, value := range binding.Limits {
		if key == "" || len(key) > 64 || strings.ContainsFunc(key, unicode.IsControl) || value < 0 {
			return false
		}
	}
	return true
}

func routeBinding(route gateway.RouteClaim) string {
	encoded, _ := json.Marshal(route.Binding)
	return string(encoded)
}

func scanRoute(row interface{ Scan(...any) error }) (gateway.Route, error) {
	var route gateway.Route
	var binding string
	var expiry int64
	err := row.Scan(&route.Target, &route.RecoveryGeneration, &route.Epoch, &route.OwnerBootID, &route.OwnerAddress, &binding, &route.Published, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return route, gateway.ErrRouteNotFound
	}
	if err != nil {
		return route, err
	}
	route.ExpiresAt = time.UnixMilli(expiry).UTC()
	if err := json.Unmarshal([]byte(binding), &route.Binding); err != nil {
		return gateway.Route{}, err
	}
	return route, nil
}

func (d *connectionDirectory) lockMachine(ctx context.Context, tx *sql.Tx, target string) error {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM dune_machines WHERE id=$1 FOR UPDATE`, target).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return gateway.ErrRouteNotFound
	}
	return err
}

func (d *connectionDirectory) remaining(ctx context.Context, tx *sql.Tx, route gateway.Route) (gateway.RouteLease, error) {
	now, err := d.store.databaseNow(ctx, tx)
	if err != nil {
		return gateway.RouteLease{}, err
	}
	// A backwards database-clock step must not enlarge the local grant.
	delta := route.ExpiresAt.UnixMilli() - now
	remaining := time.Duration(min(max(delta, 0), connectionLeaseDuration.Milliseconds())) * time.Millisecond
	if route.RecoveryGeneration != d.recovery {
		remaining = 0
	}
	return gateway.RouteLease{Route: route, ValidFor: remaining}, nil
}

func (d *connectionDirectory) Acquire(ctx context.Context, claim gateway.RouteClaim, expectedEpoch uint64) (gateway.RouteLease, error) {
	if !validRoute(claim, d.recovery) || expectedEpoch >= math.MaxInt64 {
		return gateway.RouteLease{}, ErrInvalidArgument
	}
	var lease gateway.RouteLease
	err := d.store.transaction(ctx, func(tx *sql.Tx) error {
		if err := d.lockRecovery(ctx, tx); err != nil {
			return err
		}
		if err := d.lockMachine(ctx, tx, claim.Target); err != nil {
			return err
		}
		old, err := scanRoute(tx.QueryRowContext(ctx, "SELECT "+routeColumns+" FROM dune_routes WHERE machine_id=$1", claim.Target))
		if err != nil && !errors.Is(err, gateway.ErrRouteNotFound) {
			return err
		}
		if old.Epoch != expectedEpoch {
			return gateway.ErrRouteStale
		}
		if err == nil && old.RecoveryGeneration == d.recovery {
			current, err := d.remaining(ctx, tx, old)
			if err != nil {
				return err
			}
			if current.ValidFor > 0 {
				return gateway.ErrRouteBusy
			}
		}
		// The machine lock serializes even the first insertion. Preserve epoch
		// through release and recovery; only a new machine identity starts at 1.
		query := `INSERT INTO dune_routes (` + routeColumns + `) VALUES($1,$2,$3,$4,$5,$6,FALSE,` + d.store.databaseClock() + `+$7) ON CONFLICT(machine_id) DO UPDATE SET recovery_generation=EXCLUDED.recovery_generation,epoch=EXCLUDED.epoch,owner_boot_id=EXCLUDED.owner_boot_id,owner_address=EXCLUDED.owner_address,binding=EXCLUDED.binding,published=FALSE,expires_at=EXCLUDED.expires_at RETURNING ` + routeColumns
		owned, err := scanRoute(tx.QueryRowContext(ctx, query, claim.Target, d.recovery, expectedEpoch+1, claim.OwnerBootID, claim.OwnerAddress, routeBinding(claim), connectionLeaseDuration.Milliseconds()))
		if err != nil {
			return err
		}
		lease, err = d.remaining(ctx, tx, owned)
		return err
	})
	if err != nil {
		return gateway.RouteLease{}, err
	}
	return lease, nil
}

func (d *connectionDirectory) Resolve(ctx context.Context, target string) (gateway.RouteLease, error) {
	var result gateway.RouteLease
	err := d.store.transaction(ctx, func(tx *sql.Tx) error {
		if err := d.lockRecovery(ctx, tx); err != nil {
			return err
		}
		route, err := scanRoute(tx.QueryRowContext(ctx, "SELECT "+routeColumns+" FROM dune_routes WHERE machine_id=$1", target))
		if err != nil {
			return err
		}
		result, err = d.remaining(ctx, tx, route)
		if result.ValidFor == 0 {
			result.Published = false
		}
		return err
	})
	return result, err
}

func changedRoute(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err == nil && rows != 1 {
		return gateway.ErrRouteStale
	}
	return err
}

// mutate compares the complete immutable term. Conditions are evaluated with a
// fresh database clock in the UPDATE, after locking and possible process pauses.
func (d *connectionDirectory) mutate(ctx context.Context, route gateway.Route, action string) (gateway.RouteLease, error) {
	if !validRoute(route.RouteClaim, d.recovery) || route.Epoch == 0 || route.Epoch > math.MaxInt64 {
		return gateway.RouteLease{}, ErrInvalidArgument
	}
	var result gateway.RouteLease
	err := d.store.transaction(ctx, func(tx *sql.Tx) error {
		if err := d.lockRecovery(ctx, tx); err != nil {
			return err
		}
		if err := d.lockMachine(ctx, tx, route.Target); err != nil {
			return err
		}
		clock := d.store.databaseClock()
		set, live := "published=FALSE,expires_at=0", ""
		switch action {
		case "publish":
			set, live = "published=TRUE", " AND expires_at>"+clock
		case "renew":
			set, live = fmt.Sprintf("expires_at=%s+%d", clock, connectionLeaseDuration.Milliseconds()), " AND expires_at>"+clock
		}
		query := `UPDATE dune_routes SET ` + set + ` WHERE machine_id=$1 AND recovery_generation=$2 AND epoch=$3 AND owner_boot_id=$4 AND owner_address=$5 AND binding=$6` + live + ` RETURNING ` + routeColumns
		updated, err := scanRoute(tx.QueryRowContext(ctx, query, route.Target, d.recovery, route.Epoch, route.OwnerBootID, route.OwnerAddress, routeBinding(route.RouteClaim)))
		if errors.Is(err, gateway.ErrRouteNotFound) {
			return gateway.ErrRouteStale
		}
		if err != nil {
			return err
		}
		result, err = d.remaining(ctx, tx, updated)
		return err
	})
	if err != nil {
		return gateway.RouteLease{}, err
	}
	return result, nil
}

func (d *connectionDirectory) Publish(ctx context.Context, route gateway.Route) error {
	_, err := d.mutate(ctx, route, "publish")
	return err
}
func (d *connectionDirectory) Renew(ctx context.Context, route gateway.Route) (gateway.RouteLease, error) {
	return d.mutate(ctx, route, "renew")
}
func (d *connectionDirectory) Release(ctx context.Context, route gateway.Route) error {
	_, err := d.mutate(ctx, route, "release")
	return err
}

func (s *Store) ConnectionRecovery(ctx context.Context) (string, error) {
	if !s.postgres {
		return "", ErrInvalidArgument
	}
	var generation string
	err := s.db.QueryRowContext(ctx, `SELECT recovery_generation FROM dune_cluster WHERE id=1`).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return generation, err
}
