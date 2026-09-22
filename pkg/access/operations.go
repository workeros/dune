package access

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func oneOf(value string, values ...string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

// Describe recognizes the actual execution vocabulary, not a configurable IAM
// action catalog. Decode semantics match fabricd, including JSON field aliases.
func Describe(scope Scope, m *pb.Message) (Request, error) {
	r := Request{Scope: scope, RequestID: m.RequestId, Operation: m.Operation, Runtime: RuntimeIdentity{ID: m.RuntimeId, Incarnation: m.RuntimeIncarnation, Generation: m.RuntimeGeneration}}
	if m.Kind != "request" || m.Target != scope.Binding.MachineID || r.RequestID == "" || len(r.RequestID) > 128 {
		return r, ErrDenied
	}
	decode := func(out any) error { return json.Unmarshal(m.Payload, out) }
	var err error
	switch m.Operation {
	case "submission.raw":
		var submission api.SubmissionRequest
		if decode(&submission) != nil || submission.SubmissionKey.Validate() != nil || !oneOf(submission.Operation, "acp.raw.take", "acp.raw.write") || submission.Target.RuntimeID == "" || !matchesSubmissionScope(scope, submission.Target, m) {
			return r, ErrDenied
		}
		r.Operation, r.Suboperation = "acp.raw", "take"
		if submission.Operation == "acp.raw.write" {
			r.Suboperation = "write"
		}
	case "acp.raw.state", "acp.raw.read":
		if r.Runtime.ID == "" || r.Runtime.Incarnation == "" || r.Runtime.Generation == 0 {
			return r, ErrDenied
		}
	case "submission.acp":
		var submission api.SubmissionRequest
		if decode(&submission) != nil || submission.SubmissionKey.Validate() != nil || submission.Operation != "acp.action" || submission.Target.RuntimeID == "" || !matchesSubmissionScope(scope, submission.Target, m) {
			return r, ErrDenied
		}
		// Authorize the actual business action, never a broad envelope grant.
		r.Operation = "acp.action"
		return describeACPAction(r, submission.Payload)
	case "submission.get":
		var key api.SubmissionKey
		if err = decode(&key); err != nil || key.Validate() != nil {
			return r, ErrDenied
		}
		if !matchesSubmissionScope(scope, key.Target, m) {
			return r, ErrDenied
		}
	case "runtime.stop", "runtime.forget":
		var submission api.SubmissionRequest
		if decode(&submission) != nil || submission.SubmissionKey.Validate() != nil || submission.Operation != m.Operation || submission.Target.RuntimeID == "" || !matchesSubmissionScope(scope, submission.Target, m) {
			return r, ErrDenied
		}
	case "machine.info", "runtime.list", "runtime.get", "runtime.capture", "runtime.scrollback", "acp.state", "acp.conversation.read", "acp.conversation.get", "agent.operation.wait", "agent.operation.read", "pty.prompt", "pty.keys":
	case "agent.mcp.configure":
		var config api.AgentMCP
		err = decode(&config)
		if err == nil {
			err = config.Validate()
		}
	case "profile.start":
		var start api.StartRequest
		if decode(&start) != nil || start.SubmissionKey.Validate() != nil || start.Target.RuntimeID != "" || !matchesSubmissionScope(scope, start.Target, m) {
			return r, ErrDenied
		}
		p := start.Profile
		err = p.Validate()
		if p.Kind != "agent" {
			return r, ErrDenied
		}
		r.Resource = Resource{Directory: p.WorkingDirectory, Adapter: p.Adapter, ManagedACP: p.ManagedACP}
		if start.Worktree != nil {
			if start.Worktree.Directory != p.WorkingDirectory {
				return r, ErrDenied
			}
			r.Resource.Destination = start.Worktree.Path
		}
	case "profile.prepare":
		var p api.Profile
		err = decode(&p)
		if err == nil {
			err = p.Validate()
		}
		if p.Kind != "environment" {
			return r, ErrDenied
		}
		r.Resource = Resource{Directory: p.WorkingDirectory, Adapter: p.Adapter, ManagedACP: p.ManagedACP}
	case "profile.status":
		var p api.ProfileStatusRequest
		err = decode(&p)
		if err == nil {
			err = api.ValidateExecutionID(p.ExecutionID)
		}
		r.Resource.ExecutionID = p.ExecutionID
	case "runtime.attach":
		var a api.Attach
		err = decode(&a)
		r.Resource.Observe = a.Observe
	case "exec":
		var a api.Exec
		err = decode(&a)
		r.Resource.Directory = a.WorkingDirectory
	case "files":
		var a api.File
		err = decode(&a)
		r.Suboperation = a.Action
		r.Resource.Path = a.Path
		if a.Action == "rename" {
			r.Resource.Destination = a.Destination
		}
		if !oneOf(a.Action, "stat", "list_page", "search", "read", "write", "mkdir", "rename", "remove") {
			return r, ErrDenied
		}
	case "upload":
		var a api.Upload
		err = decode(&a)
		r.Suboperation = a.Action
		if a.Action == "create" {
			r.Resource.Path = a.Path
		} else {
			r.Resource.UploadID = a.ID
		}
		if !oneOf(a.Action, "create", "query", "chunk", "commit", "cancel") {
			return r, ErrDenied
		}
	case "worktree.list", "worktree.create":
		var request api.WorktreeCreate
		err = decode(&request)
		r.Resource.Directory = request.Directory
		if m.Operation == "worktree.create" {
			r.Resource.Destination = request.Path
		}
	case "git":
		var a api.Git
		err = decode(&a)
		r.Suboperation = a.Action
		r.Mode = a.Mode
		r.Resource.Directory = a.Directory
		if !oneOf(a.Action, "status", "diff", "log", "show", "stage", "unstage", "discard", "commit", "amend", "branch", "head", "remotes", "operation", "checkout", "stash", "fetch", "pull", "push", "merge", "rebase", "conflicts") {
			return r, ErrDenied
		}
		switch a.Action {
		case "branch":
			if a.Name == "" {
				r.Mode = "list"
			} else {
				r.Mode = "create"
			}
			if a.Mode != "" {
				return r, ErrDenied
			}
		case "checkout":
			r.Mode = "switch"
			if a.Create {
				r.Mode = "create"
			}
			if a.Mode != "" {
				return r, ErrDenied
			}
		case "stash":
			if a.Mode == "" {
				r.Mode = "push"
			}
			if !oneOf(r.Mode, "push", "list", "pop", "apply", "drop") {
				return r, ErrDenied
			}
		case "merge", "rebase":
			if a.Mode == "" {
				r.Mode = "start"
			}
			if !oneOf(r.Mode, "start", "continue", "abort") {
				return r, ErrDenied
			}
		default:
			if a.Mode != "" {
				return r, ErrDenied
			}
		}
	case "runtime.history":
		var a struct{ Action string }
		err = decode(&a)
		r.Suboperation = a.Action
		if !oneOf(a.Action, "older", "newer", "close") {
			return r, ErrDenied
		}
	case "ports.connect":
		var a api.Port
		err = decode(&a)
		r.Resource.Port = a.Port
		if a.Port < 1 || a.Port > 65535 {
			return r, ErrDenied
		}
	default:
		return r, ErrDenied
	}
	if err != nil {
		return r, ErrDenied
	}
	for _, s := range []string{r.Resource.Path, r.Resource.Destination, r.Resource.Directory, r.Resource.UploadID, r.Resource.ExecutionID, r.Runtime.ID, r.Runtime.Incarnation} {
		if len(s) > 4096 || strings.ContainsRune(s, 0) {
			return r, ErrDenied
		}
	}
	return r, nil
}

func matchesSubmissionScope(scope Scope, target api.SubmissionTarget, m *pb.Message) bool {
	return target.OwnerID == scope.OwnerID && target.RunnerID == scope.Binding.RunnerID && target.FabricID == scope.Binding.FabricID && target.MachineID == scope.Binding.MachineID && target.BindingRevision == scope.Binding.Revision && target.RuntimeID == m.RuntimeId && target.RuntimeIncarnation == m.RuntimeIncarnation && target.RuntimeGeneration == m.RuntimeGeneration
}

func continuation(base Request, runtime RuntimeIdentity, m *pb.Message) (Request, error) {
	r := base
	r.Runtime = runtime
	if base.Operation == "ports.connect" {
		if !oneOf(m.Kind, "data", "eof") {
			return r, ErrDenied
		}
		r.Suboperation = m.Kind
		return r, nil
	}
	if !oneOf(base.Operation, "profile.start", "runtime.attach") || base.Resource.Observe || runtime.ID == "" {
		return r, ErrDenied
	}
	switch m.Kind {
	case "control", "history":
		var action api.TerminalControl
		if runtime.Adapter != "pty" || json.Unmarshal(m.Payload, &action) != nil {
			return r, ErrDenied
		}
		if (m.Kind == "control" && !oneOf(action.Action, "acquire", "take", "release")) || (m.Kind == "history" && !oneOf(action.Action, "older", "newer", "close")) {
			return r, ErrDenied
		}
		r.Suboperation = m.Kind
		r.Mode = action.Action
	case "input":
		if runtime.Adapter == "pty" {
			r.Suboperation = "input"
		} else {
			return r, ErrDenied
		}
	case "resize":
		if runtime.Adapter != "pty" {
			return r, ErrDenied
		}
		r.Suboperation = "resize"
	case "signal":
		if runtime.Adapter != "pty" {
			return r, ErrDenied
		}
		r.Suboperation = "signal"
		r.Mode = string(m.Data)
		if !oneOf(r.Mode, "INT", "TERM", "HUP", "QUIT") {
			return r, ErrDenied
		}
	default:
		return r, ErrDenied
	}
	return r, nil
}

func runtimeResult(base Request, m *pb.Message) (RuntimeIdentity, error) {
	var runtime api.Runtime
	var decodeError error
	if base.Operation == "profile.start" {
		var result api.StartResult
		decodeError = json.Unmarshal(m.Payload, &result)
		if result.Runtime == nil || result.Stage != "started" || result.Admission != api.SubmissionAccepted {
			return RuntimeIdentity{}, ErrDenied
		}
		runtime = *result.Runtime
	} else {
		decodeError = json.Unmarshal(m.Payload, &runtime)
	}
	if decodeError != nil || runtime.ID == "" || runtime.Incarnation == "" || runtime.Generation == 0 || !oneOf(runtime.Adapter, "pty", "acp") {
		return RuntimeIdentity{}, fmt.Errorf("invalid runtime response")
	}
	if base.Operation == "runtime.attach" && (runtime.ID != base.Runtime.ID || runtime.Incarnation != base.Runtime.Incarnation || runtime.Generation != base.Runtime.Generation) {
		return RuntimeIdentity{}, ErrDenied
	}
	return RuntimeIdentity{ID: runtime.ID, Incarnation: runtime.Incarnation, Generation: runtime.Generation, Adapter: runtime.Adapter}, nil
}

// A launch that creates a worktree also needs the original worktree policy.
// The payload is frozen and signed across peer forwarding with both decisions
// re-evaluated at the owning Gateway.
func launchWorktreeRequest(scope Scope, message *pb.Message) (*Request, error) {
	if message.Operation != "profile.start" {
		return nil, nil
	}
	var launch api.StartRequest
	if json.Unmarshal(message.Payload, &launch) != nil {
		return nil, ErrDenied
	}
	if launch.Worktree == nil {
		return nil, nil
	}
	request, err := Describe(scope, &pb.Message{Kind: "request", RequestId: message.RequestId, Operation: "worktree.create", Target: message.Target, Payload: api.Payload(launch.Worktree)})
	return &request, err
}

func describeACPAction(r Request, payload []byte) (Request, error) {
	var action api.ACPAction
	if json.Unmarshal(payload, &action) != nil || !oneOf(action.Action, "new", "load", "list", "prompt", "permission", "elicitation", "cancel") {
		return r, ErrDenied
	}
	r.Suboperation = action.Action
	if oneOf(action.Action, "new", "load", "list") {
		r.Resource.Directory = action.Cwd
	}
	return r, nil
}
