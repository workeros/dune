// Package agentmcp exposes the shared Agent services over authenticated MCP.
// Protocol connections carry no queue, operation, or application session state.
package agentmcp

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/profiles"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ProfileSummary struct {
	ID          string `json:"id"`
	Revision    int64  `json:"revision"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Adapter     string `json:"adapter"`
}

type ProfilePage struct {
	Items      []ProfileSummary `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type Options struct {
	Origin       string
	Authenticate func(context.Context, string) (agents.Scope, error)
	Profiles     func(context.Context, agents.Scope, runner.Query) (ProfilePage, error)
	Directory    agents.Directory
	Launcher     agents.Launcher
	Messenger    agents.Messenger
}

type scopeKey struct{}

func New(options Options) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "dune-agents", Version: "1"}, nil)
	registerTools(server, options)
	protocol := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if origin := r.Header.Get("Origin"); origin != "" && origin != options.Origin {
			http.Error(w, "cross-origin MCP request refused", http.StatusForbidden)
			return
		}
		// Credentials belong in one Authorization header, never query strings or
		// browser cookies. No protocol session ID grants authority on another Pod.
		if r.URL.RawQuery != "" || len(r.Header.Values("Authorization")) != 1 {
			unauthorized(w)
			return
		}
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
			unauthorized(w)
			return
		}
		scope, err := options.Authenticate(r.Context(), token)
		if err != nil {
			if errors.Is(err, identity.ErrUnauthorized) {
				unauthorized(w)
			} else {
				http.Error(w, "Agent authentication is temporarily unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
		protocol.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), scopeKey{}, scope)))
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="dune-agents"`)
	http.Error(w, "valid Agent credential required", http.StatusUnauthorized)
}

type startInput struct {
	SubmissionID     string                   `json:"submission_id" jsonschema:"Required caller-generated ID, saved before first send. Query this original launch on interruption; never generate another ID as recovery."`
	Binding          runner.Binding           `json:"binding"`
	Profile          *profiles.Selection      `json:"profile,omitempty"`
	Project          *agents.ProjectSelection `json:"project,omitempty"`
	DirectoryID      string                   `json:"directory_id,omitempty"`
	WorkingDirectory string                   `json:"working_directory,omitempty"`
	Worktree         *agents.WorktreeLocation `json:"worktree,omitempty"`
}

func registerTools(server *mcp.Server, options Options) {
	addTool(server, "runners_list", "List existing Runners in the caller's Tenant, including readiness. Follow next_cursor; never start a cloud environment.", true, func(ctx context.Context, scope agents.Scope, query runner.Query) (any, error) {
		page, err := options.Directory.List(ctx, scope, query)
		return struct {
			Items      []agents.RunnerAvailability `json:"items"`
			Issues     []agents.DiscoveryIssue     `json:"issues"`
			NextCursor string                      `json:"next_cursor,omitempty"`
		}{page.Runners, page.Issues, page.NextCursor}, err
	})
	addTool(server, "profiles_list", "List Agent Profile names, fixed revisions and PTY/ACP adapters in the caller's Tenant. Private commands and credentials are omitted.", true, func(ctx context.Context, scope agents.Scope, query runner.Query) (any, error) {
		return options.Profiles(ctx, scope, query)
	})
	addTool(server, "agents_list", "List Agent summaries in the caller's Tenant. Follow next_cursor and inspect issues for partially unavailable Runners.", true, func(ctx context.Context, scope agents.Scope, query runner.Query) (any, error) {
		return options.Directory.List(ctx, scope, query)
	})
	addTool(server, "agents_get", "Refresh one exact Agent reference. A stale native session must be rediscovered, never silently redirected.", true, func(ctx context.Context, scope agents.Scope, input struct {
		Ref string `json:"agent_ref"`
	}) (any, error) {
		return options.Directory.Get(ctx, scope, input.Ref)
	})
	addTool(server, "agents_start", "Start an assistant on an existing ready Runner using a fixed Profile revision or project default. Omit worktree to use the current directory. Preserve partial results on error; never replay an unknown start. ACP returns its initial new operation until ready.", false, func(ctx context.Context, scope agents.Scope, input startInput) (any, error) {
		return options.Launcher.Start(ctx, scope, agents.StartRequest{SubmissionID: input.SubmissionID, Binding: input.Binding, Profile: input.Profile, Project: input.Project, DirectoryID: input.DirectoryID, WorkingDirectory: input.WorkingDirectory, Worktree: input.Worktree})
	})
	addTool(server, "agents_prompt", "Send text to an exact Agent. Managed ACP requires expected_conversation_id observed from runtime.conversation_id; preserve it across retries. ACP returns operation_ref; pending has no output yet. PTY delivered only confirms input, not task completion. Optional wait_ms (0..30000) waits only this submission. Never replay an unknown result.", false, func(ctx context.Context, scope agents.Scope, input agents.PromptRequest) (any, error) {
		return options.Messenger.Prompt(ctx, scope, input)
	})
	addTool(server, "agents_submit", "Submit a managed ACP action with caller-owned submission_id saved before sending. Supports new/load/list/prompt/permission/cancel. Prompts require expected_conversation_id; cancel requires the exact active operation_ref; permissions require permission_id and option_id. Returns admission receipt, not task success. Never replay an unknown result.", false, func(ctx context.Context, scope agents.Scope, input agents.SubmissionRequest) (any, error) {
		return options.Messenger.Submit(ctx, scope, input)
	})
	addTool(server, "agents_submission", "Read an original submission_id and agent_ref after a lost response or reconnect. Querying has no Agent control side effects; unknown does not mean rejected. Returned operation_ref is the Runtime's SDK operation selector.", true, func(ctx context.Context, scope agents.Scope, input agents.SubmissionQuery) (any, error) {
		return options.Messenger.QuerySubmission(ctx, scope, input)
	})
	addTool(server, "agents_wait", "Choose operation_ref to wait for that submission, or agent_ref to observe activity. These selectors are exclusive. A PTY idle observation does not prove a prompt was processed. timeout_ms is 0..30000; timeout does not cancel or resend.", true, func(ctx context.Context, scope agents.Scope, input agents.WaitRequest) (any, error) {
		return options.Messenger.Wait(ctx, scope, input)
	})
	addTool(server, "agents_read", "Choose operation_ref for ACP output at position (initially 0), or agent_ref for a PTY screen snapshot. Use next_position to continue. incomplete means output was lost; false does not mean completion. Expired references never fall back to latest output.", true, func(ctx context.Context, scope agents.Scope, input agents.ReadRequest) (any, error) {
		return options.Messenger.Read(ctx, scope, input)
	})
	addTool(server, "agents_send_keys", "Send explicit terminal keys to a PTY Agent's current interaction. This may confirm a permission prompt. Use agents_prompt for ordinary text tasks; delivered is not task completion.", false, func(ctx context.Context, scope agents.Scope, input agents.KeysRequest) (any, error) {
		return options.Messenger.SendKeys(ctx, scope, input)
	})
}

func addTool[In any](server *mcp.Server, name, description string, readOnly bool, call func(context.Context, agents.Scope, In) (any, error)) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly}}, func(ctx context.Context, _ *mcp.CallToolRequest, input In) (*mcp.CallToolResult, any, error) {
		scope, ok := ctx.Value(scopeKey{}).(agents.Scope)
		if !ok || scope.OwnerID == "" {
			return nil, nil, errors.New("authenticated Agent scope required")
		}
		result, err := call(ctx, scope, input)
		if err == nil {
			return nil, result, nil
		}
		code := "OPERATION_FAILED"
		var failure *api.Error
		if errors.As(err, &failure) {
			code = failure.Code
		}
		// Raw SDK/database errors can contain private transport/configuration
		// details. Preserve known progress, without including those error strings.
		return &mcp.CallToolResult{IsError: true}, map[string]any{"code": code, "error": "Agent operation failed; inspect the returned result before another submission.", "result": result}, nil
	})
}
