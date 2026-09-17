package metadata

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/workbench"
)

const agentSessionColumns = `id,owner_id,revision,launch,state,created_at,updated_at`

func scanAgentSession(row interface{ Scan(...any) error }) (agents.Session, error) {
	var session agents.Session
	var launch, state string
	var created, updated int64
	err := row.Scan(&session.ID, &session.OwnerID, &session.Revision, &launch, &state, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return session, ErrNotFound
	}
	if err != nil {
		return session, err
	}
	if err := json.Unmarshal([]byte(launch), &session.Launch); err != nil {
		return session, fmt.Errorf("cannot read Agent launch snapshot")
	}
	if err := json.Unmarshal([]byte(state), &session.SessionState); err != nil {
		return session, fmt.Errorf("cannot read Agent recovery state")
	}
	session.CreatedAt, session.UpdatedAt = time.UnixMicro(created).UTC(), time.UnixMicro(updated).UTC()
	return session, nil
}

// AgentSession is a trusted storage read. The caller authorizes the Owner before
// reading it; launch snapshots must never be sent to a model or list endpoint.
func (s *Store) AgentSession(ctx context.Context, owner, id string) (agents.Session, error) {
	return scanAgentSession(s.db.QueryRowContext(ctx, `SELECT `+agentSessionColumns+` FROM dune_agent_sessions WHERE owner_id=$1 AND id=$2`, owner, id))
}

func (s *Store) AgentSessions(ctx context.Context, owner, after string, limit int) ([]agents.Session, error) {
	if owner == "" || len(after) > 256 || limit < 1 || limit > 101 {
		return nil, ErrInvalidArgument
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentSessionColumns+` FROM dune_agent_sessions WHERE owner_id=$1 AND id>$2 ORDER BY id LIMIT $3`, owner, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []agents.Session{}
	for rows.Next() {
		item, err := scanAgentSession(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// CreateAgentSession commits before the caller sends start. A failed/unknown
// database commit does not authorize starting the process.
func (s *Store) CreateAgentSession(ctx context.Context, owner string, launch agents.LaunchSnapshot) (agents.Session, error) {
	if owner == "" || len(owner) > 256 {
		return agents.Session{}, ErrInvalidArgument
	}
	if err := launch.Validate(); err != nil {
		return agents.Session{}, fmt.Errorf("%w: %s", ErrInvalidArgument, err)
	}
	payload, err := json.Marshal(launch)
	if err != nil {
		return agents.Session{}, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	session := agents.Session{ID: "session_" + rand.Text(), OwnerID: owner, Revision: 1, CreatedAt: now, UpdatedAt: now,
		SessionState: agents.SessionState{Status: "pending_capture", Attempt: agents.LaunchAttempt{ID: "launch_" + rand.Text(), Kind: "start", State: "starting", StartedAt: now}}}
	// Detach maps and slices from the caller's mutable Profile.
	if err := json.Unmarshal(payload, &session.Launch); err != nil {
		return agents.Session{}, err
	}
	state, _ := json.Marshal(session.SessionState)
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO dune_agent_sessions(id,owner_id,revision,launch,state,created_at,updated_at) VALUES($1,$2,1,$3,$4,$5,$5)`, session.ID, owner, string(payload), string(state), now.UnixMicro())
		return err
	})
	if err != nil {
		return agents.Session{}, err
	}
	return session, nil
}

// updateAgentState changes only mutable recovery metadata. There is no method
// that updates the launch column or follows the Profile's current revision.
func (s *Store) updateAgentState(ctx context.Context, session agents.Session) (agents.Session, error) {
	state, err := json.Marshal(session.SessionState)
	if err != nil {
		return agents.Session{}, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE dune_agent_sessions SET revision=revision+1,state=$1,updated_at=$2 WHERE owner_id=$3 AND id=$4 AND revision=$5`, string(state), now.UnixMicro(), session.OwnerID, session.ID, session.Revision)
		return changedRow(result, err)
	})
	if err != nil {
		return agents.Session{}, err
	}
	session.Revision++
	session.UpdatedAt = now
	return session, nil
}

func (s *Store) agentAttempt(ctx context.Context, owner, id, attempt string) (agents.Session, error) {
	session, err := s.AgentSession(ctx, owner, id)
	if err == nil && (attempt == "" || session.Attempt.ID != attempt) {
		err = ErrConflict
	}
	return session, err
}

func (s *Store) RecordAgentRuntime(ctx context.Context, owner, id, attempt string, runtime workbench.RuntimeRef) (agents.Session, error) {
	session, err := s.agentAttempt(ctx, owner, id, attempt)
	if err != nil {
		return agents.Session{}, err
	}
	target := workbench.AgentTarget{Binding: session.Launch.Binding, Runtime: runtime}
	if err := target.Validate(); err != nil || runtime.Adapter != session.Launch.Profile.Adapter {
		return agents.Session{}, ErrInvalidArgument
	}
	if previous := session.Attempt.Runtime; previous != nil {
		if *previous != runtime {
			return agents.Session{}, ErrConflict
		}
		return session, nil
	}
	if session.Attempt.State != "starting" && session.Attempt.State != "unknown" {
		return agents.Session{}, ErrConflict
	}
	session.Attempt.Runtime = &runtime
	session.Attempt.State = "capturing"
	if session.Attempt.Kind == "start" {
		session.LastRuntime = &runtime
	}
	return s.updateAgentState(ctx, session)
}

// CaptureAgentSession records a successful new/load response or authenticated
// native hook. Repeating the same confirmed ID is safe; replacing it is not.
func (s *Store) CaptureAgentSession(ctx context.Context, owner, id, attempt string, runtime workbench.RuntimeRef, native agents.NativeSession) (agents.Session, error) {
	if err := native.Validate(); err != nil {
		return agents.Session{}, fmt.Errorf("%w: %s", ErrInvalidArgument, err)
	}
	session, err := s.agentAttempt(ctx, owner, id, attempt)
	if err != nil {
		return agents.Session{}, err
	}
	if session.Attempt.Runtime == nil || *session.Attempt.Runtime != runtime || (session.Native != nil && session.Native.ID != native.ID) {
		return agents.Session{}, ErrConflict
	}
	if native.ResumeSupported && session.Launch.Recovery.ID == "" {
		return agents.Session{}, ErrInvalidArgument
	}
	if session.Attempt.State == "ready" && session.Native != nil && *session.Native == native {
		return session, nil
	}
	if session.Attempt.State != "capturing" && session.Attempt.State != "unknown" {
		return agents.Session{}, ErrConflict
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	session.Native, session.ConfirmedAt = &native, &now
	session.LastRuntime = &runtime
	session.Status, session.Reason = "unavailable", native.Reason
	if native.ResumeSupported {
		session.Status, session.Reason = "available", ""
	}
	session.Attempt.State = "ready"
	return s.updateAgentState(ctx, session)
}

// AgentCaptureUnavailable finishes a launch that cannot expose reliable native
// recovery. A live shell/custom CLI can still be attached normally.
func (s *Store) AgentCaptureUnavailable(ctx context.Context, owner, id, attempt, reason string) (agents.Session, error) {
	if reason == "" || len(reason) > 4096 {
		return agents.Session{}, ErrInvalidArgument
	}
	session, err := s.agentAttempt(ctx, owner, id, attempt)
	if err != nil {
		return agents.Session{}, err
	}
	if session.Attempt.State != "capturing" || session.Attempt.Runtime == nil || session.Native != nil {
		return agents.Session{}, ErrConflict
	}
	session.Status, session.Reason, session.Attempt.State = "unavailable", reason, "ready"
	return s.updateAgentState(ctx, session)
}

// FailAgentAttempt never retries a start. Unknown remains a barrier even if the
// previous native session is available; only confirmation of this attempt can
// resolve it. Reasons must already be safe for user-visible summaries.
func (s *Store) FailAgentAttempt(ctx context.Context, owner, id, attempt, outcome, reason string) (agents.Session, error) {
	if (outcome != "failed" && outcome != "unknown") || reason == "" || len(reason) > 4096 {
		return agents.Session{}, ErrInvalidArgument
	}
	session, err := s.agentAttempt(ctx, owner, id, attempt)
	if err != nil {
		return agents.Session{}, err
	}
	if session.Attempt.State != "starting" && session.Attempt.State != "capturing" {
		return agents.Session{}, ErrConflict
	}
	session.Attempt.State, session.Reason = outcome, reason
	session.Status = "unavailable"
	if session.Native != nil && session.Native.ResumeSupported {
		session.Status = "available"
	}
	if outcome == "unknown" {
		session.Status = "unknown"
	}
	return s.updateAgentState(ctx, session)
}

// BeginAgentResume grants one caller the right to start. The service must first
// verify that the old Runtime is dead and the original Runner/storage is still
// usable. Duplicates of the same user action return its existing attempt, even
// after completion. There is no lease, expiry or takeover of unknown launches.
func (s *Store) BeginAgentResume(ctx context.Context, owner, id string, revision int64) (agents.Session, bool, error) {
	if revision < 1 {
		return agents.Session{}, false, ErrInvalidArgument
	}
	session, err := s.AgentSession(ctx, owner, id)
	if err != nil {
		return agents.Session{}, false, err
	}
	if session.Attempt.Kind == "resume" && session.Attempt.BaseRevision == revision {
		return session, false, nil
	}
	if session.Revision != revision {
		return agents.Session{}, false, ErrConflict
	}
	if session.Status != "available" || session.Native == nil || !session.Native.ResumeSupported || (session.Attempt.State != "ready" && session.Attempt.State != "failed") {
		return agents.Session{}, false, ErrConflict
	}
	session.Attempt = agents.LaunchAttempt{ID: "launch_" + rand.Text(), Kind: "resume", BaseRevision: revision, State: "starting", StartedAt: time.Now().UTC().Truncate(time.Microsecond)}
	session.Status, session.Reason = "pending_capture", ""
	updated, err := s.updateAgentState(ctx, session)
	if err == nil {
		return updated, true, nil
	}
	if errors.Is(err, ErrConflict) {
		// Read the winning attempt; do not retry the state transition.
		current, readErr := s.AgentSession(ctx, owner, id)
		if readErr == nil && current.Attempt.Kind == "resume" && current.Attempt.BaseRevision == revision {
			return current, false, nil
		}
	}
	return agents.Session{}, false, err
}
