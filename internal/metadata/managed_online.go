package metadata

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/gateway"
)

func validOnlineBinding(binding api.Binding) bool {
	if binding.Version != api.Version || binding.Target == "" || len(binding.Target) > 128 || strings.ContainsFunc(binding.Target, unicode.IsControl) ||
		binding.Incarnation == "" || len(binding.Incarnation) > 128 || strings.ContainsFunc(binding.Incarnation, unicode.IsControl) || binding.Generation == 0 ||
		len(binding.Capabilities) > 128 || len(binding.Limits) > 32 || (binding.RouteRecovery == "") != (binding.RouteEpoch == 0) || binding.RouteEpoch > math.MaxInt64 {
		return false
	}
	if binding.RouteRecovery != "" && !validBootID(binding.RouteRecovery) {
		return false
	}
	for _, capability := range binding.Capabilities {
		if capability == "" || len(capability) > 64 || !utf8.ValidString(capability) || strings.ContainsFunc(capability, unicode.IsControl) {
			return false
		}
	}
	for key, value := range binding.Limits {
		if key == "" || len(key) > 64 || !utf8.ValidString(key) || strings.ContainsFunc(key, unicode.IsControl) || value < 0 {
			return false
		}
	}
	return utf8.ValidString(binding.Target) && utf8.ValidString(binding.Incarnation)
}

// ConfirmMachineOnline consumes a trusted Gateway observation after fabricd
// has confirmed its input grant. Attached machines need no lifecycle update.
// A Managed create succeeds only after both provider actions, enrollment and
// the first usable connection agree on the same resource and Runner binding.
// Unknown commit results are returned to the caller and are never replayed in
// the same handshake; a later connection reconciles the finished operation.
func (s *Store) ConfirmMachineOnline(ctx context.Context, binding api.Binding) error {
	if !validOnlineBinding(binding) {
		return ErrInvalidArgument
	}
	var runnerID, kind string
	err := s.db.QueryRowContext(ctx, `SELECT r.id,r.kind FROM dune_machines m JOIN dune_runners r ON r.id=m.runner_id WHERE m.id=$1`, binding.Target).Scan(&runnerID, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.ErrUnauthorized
	}
	if err != nil {
		return err
	}
	if kind == "attached" {
		return nil
	}
	if kind != "managed" {
		return identity.ErrUnauthorized
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		fabricID, revision, err := s.lockOperationRunner(ctx, tx, runnerID)
		if err != nil {
			return err
		}
		var currentKind string
		if err := tx.QueryRowContext(ctx, `SELECT r.kind FROM dune_runners r JOIN dune_machines m ON m.runner_id=r.id WHERE r.id=$1 AND m.id=$2`, runnerID, binding.Target).Scan(&currentKind); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return identity.ErrUnauthorized
			}
			return err
		}
		if currentKind != "managed" {
			return identity.ErrUnauthorized
		}

		opColumns := "o." + strings.ReplaceAll(operationColumns, ",", ",o.")
		opQuery := `SELECT ` + opColumns + ` FROM dune_operations o JOIN dune_managed_creations c ON c.operation_id=o.id AND c.runner_id=o.runner_id WHERE o.runner_id=$1 AND o.action='create'`
		if s.postgres {
			opQuery += ` FOR UPDATE OF o`
		}
		op, err := scanOperation(tx.QueryRowContext(ctx, opQuery, runnerID))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return identity.ErrUnauthorized
			}
			return err
		}
		if op.RunnerID != runnerID || op.FabricID != fabricID || op.BindingRevision != revision || op.Action != "create" {
			return identity.ErrUnauthorized
		}
		if op.Finished {
			if op.Outcome != "succeeded" || op.Exclusive || op.Worker != "" || !op.Until.IsZero() {
				return identity.ErrUnauthorized
			}
		} else if op.Outcome != "" || op.Exclusive {
			return identity.ErrUnauthorized
		}
		resource, err := scanManagedResource(tx.QueryRowContext(ctx, "SELECT "+resourceColumns+" FROM dune_managed_resources WHERE runner_id=$1", runnerID))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return identity.ErrUnauthorized
			}
			return err
		}
		created, err := scanAction(tx.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM dune_provider_actions WHERE operation_id=$1 AND kind='create'", op.ID))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return identity.ErrUnauthorized
			}
			return err
		}
		bootstrap, err := scanAction(tx.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM dune_provider_actions WHERE operation_id=$1 AND kind='bootstrap'", op.ID))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return identity.ErrUnauthorized
			}
			return err
		}
		now, err := s.databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		if resource.RunnerID != runnerID || resource.FabricID != fabricID || resource.Ref == "" || resource.ConfirmedAt.IsZero() || resource.Gone || resource.AccessClosed || (!resource.ExpiresAt.IsZero() && resource.ExpiresAt.UnixMilli() <= now) ||
			created.Outcome != "succeeded" || created.CompletedAt.IsZero() || bootstrap.Outcome != "succeeded" || bootstrap.CompletedAt.IsZero() || bootstrap.ResourceRef != resource.Ref {
			return identity.ErrUnauthorized
		}
		if err := s.confirmManagedOnlineRoute(ctx, tx, binding, now); err != nil {
			return err
		}
		if op.Finished {
			return nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE dune_operations SET finished=TRUE,finished_at=`+s.databaseClock()+`,outcome='succeeded',exclusive=FALSE,worker='',lease_until=0 WHERE id=$1 AND finished=FALSE AND outcome='' AND exclusive=FALSE`, op.ID)
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
		// A stopped first-connection schedule becomes eligible after this durable
		// fact changes. Preserve a renewal decision that was already frozen.
		if _, err := tx.ExecContext(ctx, `UPDATE dune_managed_maintenance SET reason=CASE WHEN renew_until=0 THEN '' ELSE reason END,next_check_at=CASE WHEN renew_until=0 THEN `+s.databaseClock()+` ELSE next_check_at END WHERE runner_id=$1`, runnerID); err != nil {
			return err
		}
		return nil
	})
}

func (s *Store) confirmManagedOnlineRoute(ctx context.Context, tx *sql.Tx, binding api.Binding, now int64) error {
	clusterQuery := `SELECT recovery_generation FROM dune_cluster WHERE id=1`
	if s.postgres {
		clusterQuery += ` FOR SHARE`
	}
	var recovery string
	err := tx.QueryRowContext(ctx, clusterQuery).Scan(&recovery)
	if errors.Is(err, sql.ErrNoRows) {
		if binding.RouteRecovery != "" || binding.RouteEpoch != 0 {
			return identity.ErrUnauthorized
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !s.postgres || binding.RouteRecovery != recovery || binding.RouteEpoch == 0 {
		return identity.ErrUnauthorized
	}
	routeQuery := "SELECT " + routeColumns + " FROM dune_routes WHERE machine_id=$1"
	if s.postgres {
		routeQuery += ` FOR SHARE`
	}
	route, err := scanRoute(tx.QueryRowContext(ctx, routeQuery, binding.Target))
	if err != nil {
		if errors.Is(err, gateway.ErrRouteNotFound) {
			return identity.ErrUnauthorized
		}
		return err
	}
	expected := binding
	expected.RouteRecovery, expected.RouteEpoch = "", 0
	if route.RecoveryGeneration != recovery || route.Epoch != binding.RouteEpoch || !route.Published || route.ExpiresAt.UnixMilli() <= now || !reflect.DeepEqual(route.Binding, expected) {
		return identity.ErrUnauthorized
	}
	return nil
}
