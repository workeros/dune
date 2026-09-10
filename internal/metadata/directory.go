package metadata

import (
	"context"
	"database/sql"
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

const routeColumns = "machine_id,epoch,owner_boot_id,owner_address,binding,published,expires_at"
const connectionLeaseDuration = 15 * time.Second

type connectionDirectory struct{ store *Store }

var _ gateway.Directory = (*connectionDirectory)(nil)

func (s *Store) ConnectionDirectory(ctx context.Context) (gateway.Directory, error) {
	if !s.postgres {
		return nil, fmt.Errorf("connection directory requires PostgreSQL")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &connectionDirectory{store: s}, nil
}

func validRoute(route gateway.RouteClaim) bool {
	if !wire.ValidID(route.OwnerBootID) || route.Target == "" || len(route.Target) > 128 || strings.ContainsFunc(route.Target, unicode.IsControl) {
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
	if binding.RouteEpoch != 0 || binding.Target != route.Target || binding.Version == "" || len(binding.Version) > 64 || strings.ContainsFunc(binding.Version, unicode.IsControl) || binding.Incarnation == "" || len(binding.Incarnation) > 128 || strings.ContainsFunc(binding.Incarnation, unicode.IsControl) || binding.Generation == 0 || len(binding.Capabilities) > 128 || len(binding.Limits) > 32 {
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
	err := row.Scan(&route.Target, &route.Epoch, &route.OwnerBootID, &route.OwnerAddress, &binding, &route.Published, &expiry)
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
	err := tx.QueryRowContext(ctx, `SELECT id FROM dune_runners WHERE machine_id=$1 AND enabled=TRUE FOR UPDATE`, target).Scan(&id)
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
	delta := route.ExpiresAt.UnixMilli() - now
	remaining := time.Duration(min(max(delta, 0), connectionLeaseDuration.Milliseconds())) * time.Millisecond
	return gateway.RouteLease{Route: route, ValidFor: remaining}, nil
}

func (d *connectionDirectory) Acquire(ctx context.Context, claim gateway.RouteClaim, expectedEpoch uint64) (gateway.RouteLease, error) {
	if !validRoute(claim) || expectedEpoch >= math.MaxInt64 {
		return gateway.RouteLease{}, ErrInvalidArgument
	}
	var lease gateway.RouteLease
	err := d.store.transaction(ctx, func(tx *sql.Tx) error {
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
		if err == nil {
			current, err := d.remaining(ctx, tx, old)
			if err != nil {
				return err
			}
			if current.ValidFor > 0 {
				return gateway.ErrRouteBusy
			}
		}
		query := `INSERT INTO dune_routes (` + routeColumns + `) VALUES($1,$2,$3,$4,$5,FALSE,` + d.store.databaseClock() + `+$6) ON CONFLICT(machine_id) DO UPDATE SET epoch=EXCLUDED.epoch,owner_boot_id=EXCLUDED.owner_boot_id,owner_address=EXCLUDED.owner_address,binding=EXCLUDED.binding,published=FALSE,expires_at=EXCLUDED.expires_at RETURNING ` + routeColumns
		owned, err := scanRoute(tx.QueryRowContext(ctx, query, claim.Target, expectedEpoch+1, claim.OwnerBootID, claim.OwnerAddress, routeBinding(claim), connectionLeaseDuration.Milliseconds()))
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
	if err != nil {
		return gateway.RouteLease{}, err
	}
	return result, nil
}

func (d *connectionDirectory) mutate(ctx context.Context, route gateway.Route, action string) (gateway.RouteLease, error) {
	if !validRoute(route.RouteClaim) || route.Epoch == 0 || route.Epoch > math.MaxInt64 {
		return gateway.RouteLease{}, ErrInvalidArgument
	}
	var result gateway.RouteLease
	err := d.store.transaction(ctx, func(tx *sql.Tx) error {
		if err := d.lockMachine(ctx, tx, route.Target); err != nil {
			return err
		}
		clock := d.store.databaseClock()
		set, live := "published=FALSE,expires_at=0", ""
		switch action {
		case "publish":
			set, live = "published=TRUE", " AND expires_at>"+clock
		case "renew":
			set, live = fmt.Sprintf("expires_at=GREATEST(expires_at,%s+%d)", clock, connectionLeaseDuration.Milliseconds()), " AND expires_at>"+clock
		}
		query := `UPDATE dune_routes SET ` + set + ` WHERE machine_id=$1 AND epoch=$2 AND owner_boot_id=$3 AND owner_address=$4 AND binding=$5` + live + ` RETURNING ` + routeColumns
		updated, err := scanRoute(tx.QueryRowContext(ctx, query, route.Target, route.Epoch, route.OwnerBootID, route.OwnerAddress, routeBinding(route.RouteClaim)))
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

func (s *Store) OnlineConnections(ctx context.Context, machines []string) (map[string]bool, error) {
	if !s.postgres || len(machines) > 100 {
		return nil, ErrInvalidArgument
	}
	for _, id := range machines {
		if id == "" || len(id) > 128 {
			return nil, ErrInvalidArgument
		}
	}
	online := make(map[string]bool, len(machines))
	if len(machines) == 0 {
		return online, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT machine_id FROM dune_routes WHERE published=TRUE AND expires_at>floor(extract(epoch FROM clock_timestamp())*1000)::bigint AND machine_id=ANY($1)`, machines)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var machine string
		if err := rows.Scan(&machine); err != nil {
			return nil, err
		}
		online[machine] = true
	}
	return online, rows.Err()
}
