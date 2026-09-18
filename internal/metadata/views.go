package metadata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/aiomni/dune/pkg/workbench"
)

// ViewOwner includes the identity namespace because host user IDs need only be
// unique within that namespace. All three values come from authenticated scope.
type ViewOwner struct{ OwnerID, Namespace, UserID string }

func (o ViewOwner) valid() bool { return o.OwnerID != "" && o.UserID != "" }

func validViewID(id string) bool {
	return strings.TrimSpace(id) != "" && len(id) <= 128 && !strings.ContainsFunc(id, unicode.IsControl)
}

func (s *Store) View(ctx context.Context, owner ViewOwner, id string) (workbench.View, error) {
	view := workbench.View{ID: id}
	if !owner.valid() || !validViewID(id) {
		return view, ErrInvalidArgument
	}
	var payload string
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT revision,spec,updated_at FROM dune_views WHERE owner_id=$1 AND user_namespace=$2 AND user_id=$3 AND id=$4`, owner.OwnerID, owner.Namespace, owner.UserID, id).Scan(&view.Revision, &payload, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	view.UpdatedAt = time.UnixMicro(updated).UTC()
	err = json.Unmarshal([]byte(payload), &view.ViewSpec)
	return view, err
}

func (s *Store) SaveView(ctx context.Context, owner ViewOwner, view workbench.View) (workbench.View, error) {
	if !owner.valid() || !validViewID(view.ID) || view.Revision < 0 {
		return workbench.View{}, ErrInvalidArgument
	}
	panes, err := view.ViewSpec.Panes()
	if err != nil {
		return workbench.View{}, fmt.Errorf("%w: %s", ErrInvalidArgument, err)
	}
	payload, err := json.Marshal(view.ViewSpec)
	if err != nil {
		return workbench.View{}, err
	}
	view.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		for _, pane := range panes {
			if err := checkSavedTarget(ctx, tx, owner.OwnerID, pane.Target); err != nil {
				return err
			}
		}
		if view.Revision == 0 {
			result, err := tx.ExecContext(ctx, `INSERT INTO dune_views(owner_id,user_namespace,user_id,id,revision,spec,updated_at) VALUES($1,$2,$3,$4,1,$5,$6) ON CONFLICT(owner_id,user_namespace,user_id,id) DO NOTHING`, owner.OwnerID, owner.Namespace, owner.UserID, view.ID, string(payload), view.UpdatedAt.UnixMicro())
			return changedRow(result, err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE dune_views SET revision=revision+1,spec=$1,updated_at=$2 WHERE owner_id=$3 AND user_namespace=$4 AND user_id=$5 AND id=$6 AND revision=$7`, string(payload), view.UpdatedAt.UnixMicro(), owner.OwnerID, owner.Namespace, owner.UserID, view.ID, view.Revision)
		return changedRow(result, err)
	})
	if err == nil {
		view.Revision++
	}
	return view, err
}

// Saved views may retain disabled or replaced Runners so users can see stale
// panes. Ownership is checked here; execution checks the exact binding later.
func checkSavedTarget(ctx context.Context, tx *sql.Tx, owner string, target workbench.AgentTarget) error {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM dune_runners WHERE owner_id=$1 AND id=$2`, owner, target.Binding.RunnerID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
