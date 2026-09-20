// Package duneagent connects IM conversations to Dune's authorized, managed
// ACP Runtime path. The embedding host resolves each conversation to its
// current Tenant/Runner scope; IM senders are never treated as Dune users.
package duneagent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/runner"
)

type ScopeResolver interface {
	// ResolveAgentScope authorizes the persisted IM Binding target and maps
	// its TenantID to Dune's OwnerID and trusted actor. IM senders must never
	// supply this scope. The returned binding must exactly match the saved target.
	ResolveAgentScope(context.Context, channel.ConversationSession) (host.AgentScope, error)
}

// ProfileResolver loads the target's immutable Profile revision from the embedding
// application after scope authorization. It must enforce tenant ownership.
type ProfileResolver interface {
	ResolveAgentProfile(context.Context, channel.ConversationSession) (api.Profile, error)
}

type Backend struct {
	Executor host.AgentExecutor
	Scopes   ScopeResolver
	Profiles ProfileResolver
}

func (b Backend) open(ctx context.Context, session channel.ConversationSession) (host.AgentConnection, error) {
	if b.Executor == nil || b.Scopes == nil || session.Key.TenantID == "" || session.Key.BindingID == "" || session.Target.RunnerID == "" {
		return nil, errors.New("Dune IM Agent backend scope is incomplete")
	}
	scope, err := b.Scopes.ResolveAgentScope(ctx, session)
	if err != nil {
		return nil, err
	}
	target := session.Target
	expected := runner.Binding{RunnerID: target.RunnerID, FabricID: target.FabricID, MachineID: target.MachineID, Revision: target.RunnerBindingRevision}
	if target.OwnerID == "" || scope.OwnerID != target.OwnerID || expected.FabricID == "" || expected.MachineID == "" || expected.Revision < 1 || scope.Binding != expected {
		return nil, errors.New("Dune IM Agent scope lacks an owner or does not match the conversation Runner")
	}
	return b.Executor.Open(ctx, scope)
}

func (b Backend) Capabilities(ctx context.Context, session channel.ConversationSession) (channel.AgentCapabilities, error) {
	connection, err := b.open(ctx, session)
	if err != nil {
		return channel.AgentCapabilities{}, err
	}
	defer connection.Close()
	if _, err := b.profile(ctx, session); err != nil {
		return channel.AgentCapabilities{}, err
	}
	return channel.AgentCapabilities{Adapter: "acp", AssistantDeltas: true, ReliableFinal: true}, nil
}

func (b Backend) Start(ctx context.Context, session channel.ConversationSession) (out channel.AgentSession, err error) {
	key := launchKey(session, LaunchSubmissionID(session))
	defer func() {
		var submissionError *api.SubmissionError
		if err != nil && !errors.As(err, &submissionError) {
			err = &api.SubmissionError{Key: key, Cause: err}
		}
	}()
	connection, err := b.open(ctx, session)
	if err != nil {
		return channel.AgentSession{}, err
	}
	defer connection.Close()
	runtime, err := b.startRuntime(ctx, connection, session)
	if err != nil {
		return channel.AgentSession{}, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if _, err := awaitState(readyCtx, connection, runtime, func(state host.AgentState) bool {
		return state.Ready && state.Busy == "" && state.SessionID == ""
	}); err != nil {
		return channel.AgentSession{}, err
	}
	opened, err := newSession(readyCtx, connection, runtime, sessionSubmissionID(session, "new"), session.Target.WorkingDirectory)
	if err != nil {
		return channel.AgentSession{}, err
	}
	return channel.AgentSession{Runtime: toHandle(runtime), ACPSessionID: opened.NativeSession.ID, ConversationID: opened.ConversationID}, nil
}

func (b Backend) profile(ctx context.Context, session channel.ConversationSession) (api.Profile, error) {
	if b.Profiles == nil || session.Target.ProfileID == "" || session.Target.ProfileRevision < 1 {
		return api.Profile{}, errors.New("IM Agent Profile and positive revision are required")
	}
	profile, err := b.Profiles.ResolveAgentProfile(ctx, session)
	if err != nil {
		return api.Profile{}, err
	}
	if profile.Kind != "agent" || profile.Adapter != "acp" {
		return api.Profile{}, errors.New("IM Agent Profile must use ACP")
	}
	profile.WorkingDirectory = session.Target.WorkingDirectory
	profile.ManagedACP = true
	if err := profile.Validate(); err != nil {
		return api.Profile{}, fmt.Errorf("invalid managed ACP Profile: %w", err)
	}
	return profile, nil
}

func (b Backend) startRuntime(ctx context.Context, connection host.AgentConnection, session channel.ConversationSession) (api.Runtime, error) {
	profile, err := b.profile(ctx, session)
	if err != nil {
		return api.Runtime{}, err
	}
	// This stable caller-owned ID is determined before the launch is sent and
	// remains reproducible if the initial response is lost.
	submissionID := LaunchSubmissionID(session)
	runtime, err := connection.Start(ctx, submissionID, profile)
	if err != nil {
		return api.Runtime{}, fmt.Errorf("start Dune ACP Runtime (outcome may be unknown): %w", err)
	}
	if err := validRuntime(runtime); err != nil {
		return api.Runtime{}, err
	}
	return runtime, nil
}

func newSession(ctx context.Context, connection host.AgentConnection, runtime api.Runtime, submissionID, cwd string) (api.AgentOperation, error) {
	operation, err := connection.Submit(ctx, runtime, host.AgentAction{SubmissionID: submissionID, Action: "new", Cwd: cwd})
	if err != nil {
		return api.AgentOperation{}, fmt.Errorf("create ACP session (outcome may be unknown): %w", err)
	}
	result, err := awaitOperation(ctx, connection, runtime, operation)
	if err != nil {
		return api.AgentOperation{}, err
	}
	if result.NativeSession == nil || result.NativeSession.ID == "" || result.ConversationID == "" {
		return api.AgentOperation{}, errors.New("ACP new did not confirm a native session ID")
	}
	return result, nil
}

func (b Backend) Attach(ctx context.Context, session channel.ConversationSession, existing channel.AgentSession) (channel.AgentSession, error) {
	connection, err := b.open(ctx, session)
	if err != nil {
		return channel.AgentSession{}, err
	}
	defer connection.Close()
	selected := toRuntime(existing.Runtime)
	current, err := connection.Get(ctx, selected)
	if err != nil {
		return channel.AgentSession{}, fmt.Errorf("find Dune Runtime for ACP attach (outcome may be unknown): %w", err)
	}
	if current.ID != selected.ID || current.Incarnation != selected.Incarnation || current.Generation != selected.Generation || current.Adapter != "acp" {
		return channel.AgentSession{}, errors.New("IM ACP Runtime identity changed")
	}
	if current.State == "exited" || current.State == "lost" {
		code := "RUNTIME_EXITED"
		if current.State == "lost" {
			code = "RUNTIME_LOST"
		}
		return channel.AgentSession{}, &api.Error{Code: code, Detail: "original IM ACP Runtime ended; explicitly create a new session to continue"}
	}
	if current.Availability != "" {
		return channel.AgentSession{}, &api.Error{Code: "SESSION_UNAVAILABLE", Detail: "original IM ACP host is temporarily unavailable"}
	}
	if current.State != "running" {
		return channel.AgentSession{}, fmt.Errorf("IM ACP Runtime has unexpected state %q", current.State)
	}
	state, err := connection.State(ctx, selected)
	if err != nil {
		return channel.AgentSession{}, err
	}
	if !state.Ready || state.SessionID != existing.ACPSessionID || state.Busy != "" || state.Conversation == nil || state.Conversation.ID != existing.ConversationID {
		return channel.AgentSession{}, errors.New("IM ACP session is not ready or does not match stored session")
	}
	return existing, nil
}

func (b Backend) Stop(ctx context.Context, session channel.ConversationSession, existing channel.AgentSession) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	connection, err := b.open(ctx, session)
	if err != nil {
		return err
	}
	defer connection.Close()
	runtime := toRuntime(existing.Runtime)
	// This intent is derived from persisted session/Runtime identity before send.
	// Reads always use the same ID; no retry generates another stop request.
	submissionID := sessionSubmissionID(session, "stop-"+runtime.Incarnation)
	receipt, err := connection.Stop(ctx, runtime, submissionID)
	if err != nil {
		return fmt.Errorf("stop ACP Runtime (outcome may be unknown): %w", err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if receipt.Admission != api.SubmissionAccepted || receipt.SubmissionID != submissionID {
			return errors.New("original stop admission could not be confirmed")
		}
		if receipt.Stage == "stopped" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for ACP Runtime stop (outcome may be unknown): %w", ctx.Err())
		case <-ticker.C:
		}
		receipt, err = connection.QuerySubmission(ctx, runtime, submissionID)
		if err != nil {
			return fmt.Errorf("query original ACP Runtime stop (outcome may be unknown): %w", err)
		}
	}
}

func awaitState(ctx context.Context, connection host.AgentConnection, runtime api.Runtime, ready func(host.AgentState) bool) (host.AgentState, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := connection.State(ctx, runtime)
		if err != nil {
			return host.AgentState{}, err
		}
		if state.Error != "" {
			return host.AgentState{}, fmt.Errorf("ACP Agent: %s", state.Error)
		}
		if ready(state) {
			return state, nil
		}
		select {
		case <-ctx.Done():
			return host.AgentState{}, fmt.Errorf("wait for ACP state (outcome may be unknown): %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func validRuntime(runtime api.Runtime) error {
	if runtime.ID == "" || runtime.Incarnation == "" || runtime.Generation == 0 || runtime.Adapter != "acp" {
		return errors.New("Dune returned an incomplete ACP Runtime identity")
	}
	return nil
}

func toHandle(runtime api.Runtime) channel.RuntimeHandle {
	return channel.RuntimeHandle{ID: runtime.ID, Incarnation: runtime.Incarnation, Generation: runtime.Generation, Adapter: runtime.Adapter}
}

func toRuntime(handle channel.RuntimeHandle) api.Runtime {
	return api.Runtime{ID: handle.ID, Incarnation: handle.Incarnation, Generation: handle.Generation, Adapter: handle.Adapter}
}

var _ channel.AgentBackend = Backend{}
var _ channel.AgentInputValidator = Backend{}

// fabricd rejects prompts longer than 64 KiB before sending them to ACP.
// Preflight this bound so the durable inbox stays retryable, not unknown.
func (Backend) ValidateInput(input string) error {
	if strings.TrimSpace(input) == "" || len(input) > 64*1024 {
		return errors.New("IM ACP prompt must contain 1..65536 bytes of non-whitespace text")
	}
	return nil
}

// NormalizeAssistantUpdate accepts only visible answer text. Thinking, tool
// events and user echoes never become card deltas or final reply content.
func NormalizeAssistantUpdate(kind, contentType, text string) (string, bool) {
	if (kind != "agent_message_chunk" && kind != "agent_message") || contentType != "text" || text == "" {
		return "", false
	}
	return text, true
}

func awaitOperation(ctx context.Context, connection host.AgentConnection, runtime api.Runtime, operation api.AgentOperation) (api.AgentOperation, error) {
	for !operation.Terminal() {
		next, err := connection.WaitOperation(ctx, runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 1000})
		if err != nil {
			return api.AgentOperation{}, err
		}
		if next.Ref != operation.Ref {
			return api.AgentOperation{}, errors.New("ACP operation reference changed")
		}
		operation = next
	}
	if operation.State != "completed" {
		return operation, fmt.Errorf("ACP operation %s: %s", operation.State, operation.Error)
	}
	return operation, nil
}

// The persisted session revision identifies this caller turn before any send.
// A changed body under that revision conflicts instead of creating a retry.
func sessionSubmissionID(session channel.ConversationSession, action string) string {
	digest := sha256.Sum256(append(api.Payload(session.Key), []byte(fmt.Sprint("/", action, "/", session.Revision))...))
	return fmt.Sprintf("%s-%x", action, digest)
}

// LaunchSubmissionID is reproducible from the durable session snapshot used by
// Start. Save it before Start; later session revisions describe different turns.
func LaunchSubmissionID(session channel.ConversationSession) string {
	return sessionSubmissionID(session, "launch")
}

func launchKey(session channel.ConversationSession, id string) api.SubmissionKey {
	target := session.Target
	return api.SubmissionKey{SubmissionID: id, Target: api.SubmissionTarget{OwnerID: target.OwnerID, RunnerID: target.RunnerID, FabricID: target.FabricID, MachineID: target.MachineID, BindingRevision: target.RunnerBindingRevision}}
}

// QueryLaunch observes the caller's saved launch ID on the session's original
// binding. It does not resolve a Profile, open a native session, resume the IM
// processor or send a reply. Unknown turns remain fenced for explicit handling.
func (b Backend) QueryLaunch(ctx context.Context, session channel.ConversationSession, submissionID string) (api.SubmissionReceipt, error) {
	key := launchKey(session, submissionID)
	unknown := api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
	if err := key.Validate(); err != nil {
		return unknown, &api.SubmissionError{Key: key, Cause: err}
	}
	connection, err := b.open(ctx, session)
	if err != nil {
		return unknown, &api.SubmissionError{Key: key, Cause: err}
	}
	defer connection.Close()
	return connection.QuerySubmission(ctx, api.Runtime{}, submissionID)
}
