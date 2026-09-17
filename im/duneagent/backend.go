// Package duneagent connects IM conversations to Dune's authorized, managed
// ACP Runtime path. The embedding host resolves each conversation to its
// current Tenant/Runner scope; IM senders are never treated as Dune users.
package duneagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/host"
)

type ScopeResolver interface {
	// ResolveAgentScope authorizes the persisted IM Binding target and maps
	// its TenantID to Dune's OwnerID and trusted actor. IM senders must never
	// supply this scope. The returned RunnerID must match the target.
	ResolveAgentScope(context.Context, channel.ConversationSession) (host.AgentScope, error)
}

type Backend struct {
	Executor host.AgentExecutor
	Scopes   ScopeResolver
}

func (b Backend) open(ctx context.Context, session channel.ConversationSession) (host.AgentConnection, error) {
	if b.Executor == nil || b.Scopes == nil || session.Key.TenantID == "" || session.Key.BindingID == "" || session.Target.RunnerID == "" {
		return nil, errors.New("Dune IM Agent backend scope is incomplete")
	}
	scope, err := b.Scopes.ResolveAgentScope(ctx, session)
	if err != nil {
		return nil, err
	}
	if scope.OwnerID == "" || scope.RunnerID != session.Target.RunnerID {
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
	config, err := connection.AgentConfig(ctx, session.Target.AgentConfigID)
	if err != nil {
		return channel.AgentCapabilities{}, err
	}
	if config.Adapter != "acp" {
		return channel.AgentCapabilities{Adapter: config.Adapter}, nil
	}
	profile := config.Profile(session.Target.WorkingDirectory)
	profile.ManagedACP = true
	if err := profile.Validate(); err != nil {
		return channel.AgentCapabilities{}, fmt.Errorf("invalid managed ACP Profile: %w", err)
	}
	return channel.AgentCapabilities{Adapter: "acp", AssistantDeltas: true, ReliableFinal: true}, nil
}

func (b Backend) Start(ctx context.Context, session channel.ConversationSession) (channel.AgentSession, error) {
	connection, err := b.open(ctx, session)
	if err != nil {
		return channel.AgentSession{}, err
	}
	defer connection.Close()
	runtime, err := startRuntime(ctx, connection, session)
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
	sessionID, err := newSession(readyCtx, connection, runtime, session.Target.WorkingDirectory)
	if err != nil {
		return channel.AgentSession{}, err
	}
	return channel.AgentSession{Runtime: toHandle(runtime), ACPSessionID: sessionID}, nil
}

func startRuntime(ctx context.Context, connection host.AgentConnection, session channel.ConversationSession) (api.Runtime, error) {
	config, err := connection.AgentConfig(ctx, session.Target.AgentConfigID)
	if err != nil {
		return api.Runtime{}, err
	}
	if config.Adapter != "acp" {
		return api.Runtime{}, errors.New("IM AgentConfig must use ACP")
	}
	profile := config.Profile(session.Target.WorkingDirectory)
	profile.ManagedACP = true
	runtime, err := connection.Start(ctx, profile)
	if err != nil {
		return api.Runtime{}, fmt.Errorf("start Dune ACP Runtime (outcome may be unknown): %w", err)
	}
	if err := validRuntime(runtime); err != nil {
		return api.Runtime{}, err
	}
	return runtime, nil
}

func newSession(ctx context.Context, connection host.AgentConnection, runtime api.Runtime, cwd string) (string, error) {
	if err := connection.Action(ctx, runtime, host.AgentAction{Action: "new", Cwd: cwd}); err != nil {
		return "", fmt.Errorf("create ACP session (outcome may be unknown): %w", err)
	}
	state, err := awaitState(ctx, connection, runtime, func(state host.AgentState) bool {
		return state.Ready && state.Busy == "" && state.SessionID != ""
	})
	if err != nil {
		return "", err
	}
	return state.SessionID, nil
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
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.Code == "STALE_RUNTIME" {
			return recoverSession(ctx, connection, session, existing)
		}
		return channel.AgentSession{}, fmt.Errorf("find Dune Runtime for ACP attach (outcome may be unknown): %w", err)
	}
	if current.ID != selected.ID || current.Incarnation != selected.Incarnation || current.Generation != selected.Generation || current.Adapter != "acp" {
		return channel.AgentSession{}, errors.New("IM ACP Runtime identity changed")
	}
	if current.State == "exited" {
		return recoverSession(ctx, connection, session, existing)
	}
	if current.State != "running" {
		return channel.AgentSession{}, fmt.Errorf("IM ACP Runtime has unexpected state %q", current.State)
	}
	state, err := connection.State(ctx, selected)
	if err != nil {
		return channel.AgentSession{}, err
	}
	if !state.Ready || state.SessionID != existing.ACPSessionID || state.Busy != "" {
		return channel.AgentSession{}, errors.New("IM ACP session is not ready or does not match stored session")
	}
	return existing, nil
}

func recoverSession(ctx context.Context, connection host.AgentConnection, conversation channel.ConversationSession, existing channel.AgentSession) (channel.AgentSession, error) {
	if existing.ACPSessionID == "" {
		return channel.AgentSession{}, errors.New("previous ACP session ID is missing; cannot recover context")
	}
	runtime, err := startRuntime(ctx, connection, conversation)
	if err != nil {
		return channel.AgentSession{}, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	state, err := awaitState(readyCtx, connection, runtime, func(state host.AgentState) bool {
		return state.Ready && state.Busy == "" && state.SessionID == ""
	})
	if err != nil {
		return channel.AgentSession{}, err
	}
	if !state.CanLoad {
		id, err := newSession(readyCtx, connection, runtime, conversation.Target.WorkingDirectory)
		if err != nil {
			return channel.AgentSession{}, err
		}
		return channel.AgentSession{Runtime: toHandle(runtime), ACPSessionID: id, ContextLost: true}, nil
	}
	if err := connection.Action(readyCtx, runtime, host.AgentAction{Action: "load", Cwd: conversation.Target.WorkingDirectory, SessionID: existing.ACPSessionID}); err != nil {
		return channel.AgentSession{}, fmt.Errorf("load previous ACP session (outcome may be unknown): %w", err)
	}
	loaded, err := awaitState(readyCtx, connection, runtime, func(state host.AgentState) bool {
		return state.Ready && state.Busy == "" && state.SessionID == existing.ACPSessionID
	})
	if err != nil {
		return channel.AgentSession{}, err
	}
	return channel.AgentSession{Runtime: toHandle(runtime), ACPSessionID: loaded.SessionID}, nil
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
	if err := connection.Stop(ctx, runtime); err != nil {
		return fmt.Errorf("stop ACP Runtime (outcome may be unknown): %w", err)
	}
	// runtime.stop confirms the stop request; process exit is published
	// asynchronously. Do not report destruction while Attach can still see
	// the old process as running. Only these read-only queries are repeated.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := connection.Get(ctx, runtime)
		if err != nil {
			var apiErr *api.Error
			if errors.As(err, &apiErr) && apiErr.Code == "STALE_RUNTIME" {
				return nil
			}
			return fmt.Errorf("confirm stopped ACP Runtime (outcome may be unknown): %w", err)
		}
		if current.ID != runtime.ID || current.Incarnation != runtime.Incarnation || current.Generation != runtime.Generation || current.Adapter != "acp" {
			return errors.New("ACP Runtime identity changed while confirming stop")
		}
		if current.State == "exited" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for ACP Runtime exit (outcome may be unknown): %w", ctx.Err())
		case <-ticker.C:
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
