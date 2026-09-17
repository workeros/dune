package metadata

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/workbench"
)

type runtimeSessionIndex struct {
	SourceID      string
	SourceAttempt string
	SessionID     string
	Sequence      int64
}

// An old Runtime can remain visible after its recovery record has moved to a
// new attempt. Its observations must neither revive the index nor look like a
// database outage to the workbench.
var ErrStaleAgentObservation = errors.New("native observation belongs to a superseded launch attempt")

// RuntimeAgentSession resolves the selected recovery record without scanning
// private launch snapshots. This index says nothing about Runtime liveness.
func (s *Store) RuntimeAgentSession(ctx context.Context, owner string, target workbench.AgentTarget) (agents.Session, error) {
	if owner == "" || target.Validate() != nil {
		return agents.Session{}, ErrInvalidArgument
	}
	return scanAgentSession(s.db.QueryRowContext(ctx, `SELECT `+agentSessionColumns+` FROM dune_agent_sessions
		WHERE owner_id=$1 AND id=(SELECT session_id FROM dune_agent_runtime_sessions WHERE owner_id=$1 AND target=$2)`, owner, target.Key()))
}

// ObserveAgentSession persists a trusted fabricd confirmation. It never sends a
// command. Different native conversations keep separate immutable snapshots;
// late observations may add history but cannot change the current selection.
func (s *Store) ObserveAgentSession(ctx context.Context, owner string, target workbench.AgentTarget, observed api.NativeSession) (agents.Session, error) {
	native := agents.NativeSession{ID: observed.ID, Cwd: observed.Cwd, Source: observed.Source, AgentVersion: observed.AgentVersion,
		ResumeSupported: observed.ResumeSupported, Reason: "Agent does not advertise native session loading"}
	if native.ResumeSupported {
		native.Reason = ""
	}
	if owner == "" || target.Validate() != nil || observed.Sequence < 1 || native.Validate() != nil {
		return agents.Session{}, ErrInvalidArgument
	}
	var captured agents.Session
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		index, err := s.lockRuntimeSession(ctx, tx, owner, target.Key())
		if err != nil {
			return err
		}
		source, err := scanAgentSession(tx.QueryRowContext(ctx, `SELECT `+agentSessionColumns+` FROM dune_agent_sessions WHERE owner_id=$1 AND id=$2`, owner, index.SourceID))
		if err != nil {
			return err
		}
		if source.Attempt.ID != index.SourceAttempt {
			return ErrStaleAgentObservation
		}
		if source.Attempt.Runtime == nil || *source.Attempt.Runtime != target.Runtime || source.Launch.Binding != target.Binding {
			return ErrConflict
		}
		if source.Launch.Recovery.ID == "" {
			native.ResumeSupported, native.Reason = false, "The original launch has no supported recovery adapter"
		}
		captured, err = s.captureRuntimeSession(ctx, tx, source, target, native)
		if err != nil {
			return err
		}
		if observed.Sequence == index.Sequence && captured.ID != index.SessionID {
			return ErrConflict // A confirmation sequence cannot name two sessions.
		}
		if observed.Sequence > index.Sequence {
			_, err = tx.ExecContext(ctx, `UPDATE dune_agent_runtime_sessions SET session_id=$1,sequence=$2 WHERE owner_id=$3 AND target=$4`, captured.ID, observed.Sequence, owner, target.Key())
			index.SessionID = captured.ID
		}
		captured.Selected = index.SessionID == captured.ID
		return err
	})
	if err != nil {
		return agents.Session{}, err
	}
	return captured, nil
}

func (s *Store) lockRuntimeSession(ctx context.Context, tx *sql.Tx, owner, target string) (runtimeSessionIndex, error) {
	query := `SELECT source_id,source_attempt,session_id,sequence FROM dune_agent_runtime_sessions WHERE owner_id=$1 AND target=$2`
	if s.postgres {
		query += ` FOR UPDATE`
	}
	var index runtimeSessionIndex
	err := tx.QueryRowContext(ctx, query, owner, target).Scan(&index.SourceID, &index.SourceAttempt, &index.SessionID, &index.Sequence)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return index, err
}

func (s *Store) captureRuntimeSession(ctx context.Context, tx *sql.Tx, source agents.Session, target workbench.AgentTarget, native agents.NativeSession) (agents.Session, error) {
	selected := source
	newRecord := false
	if source.Native != nil && (source.Native.ID != native.ID || source.Native.Cwd != native.Cwd) {
		// An in-flight resume must confirm the requested native conversation.
		// Once ready, later explicit new/load calls can select other sessions.
		if source.Attempt.State != "ready" {
			return agents.Session{}, ErrConflict
		}
		id := nativeRecordID(source.ID, target.Key(), native)
		var err error
		selected, err = scanAgentSession(tx.QueryRowContext(ctx, `SELECT `+agentSessionColumns+` FROM dune_agent_sessions WHERE owner_id=$1 AND id=$2`, source.OwnerID, id))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return agents.Session{}, err
		}
		if errors.Is(err, ErrNotFound) {
			now := time.Now().UTC().Truncate(time.Microsecond)
			selected = agents.Session{ID: id, OwnerID: source.OwnerID, Revision: 1, CreatedAt: now, UpdatedAt: now, Launch: source.Launch,
				SessionState: agents.SessionState{Status: "pending_capture", LastRuntime: &target.Runtime, Attempt: agents.LaunchAttempt{
					ID: source.Attempt.ID, Kind: "start", State: "capturing", Runtime: &target.Runtime, StartedAt: source.Attempt.StartedAt}}}
			newRecord = true
		}
	}
	changed, err := captureAgentState(&selected, target.Runtime, native)
	if err != nil {
		return agents.Session{}, err
	}
	if newRecord {
		launch, err := json.Marshal(selected.Launch)
		if err != nil {
			return agents.Session{}, err
		}
		state, err := json.Marshal(selected.SessionState)
		if err != nil {
			return agents.Session{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO dune_agent_sessions(id,owner_id,revision,launch,state,created_at,updated_at) VALUES($1,$2,1,$3,$4,$5,$5)`, selected.ID, selected.OwnerID, string(launch), string(state), selected.CreatedAt.UnixMicro())
		return selected, err
	}
	if changed {
		return writeAgentState(ctx, tx, selected)
	}
	return selected, nil
}

// Stable only within this launch/Runtime. Repeated loads deduplicate without
// assuming that equal vendor session IDs on different Runners share storage.
func nativeRecordID(source, target string, native agents.NativeSession) string {
	encoded, _ := json.Marshal([]string{source, target, native.ID, native.Cwd})
	digest := sha256.Sum256(encoded)
	return "session_" + hex.EncodeToString(digest[:])
}
