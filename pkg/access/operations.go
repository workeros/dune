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

// describe recognizes the actual execution vocabulary, not a configurable IAM
// action catalog. Decode semantics match fabricd, including JSON field aliases.
func describe(scope Scope, m *pb.Message) (Request, error) {
	r := Request{Scope: scope, RequestID: m.RequestId, Operation: m.Operation, Runtime: RuntimeIdentity{ID: m.RuntimeId, Incarnation: m.RuntimeIncarnation, Generation: m.RuntimeGeneration}}
	if m.Kind != "request" || m.Target != scope.Binding.MachineID || r.RequestID == "" || len(r.RequestID) > 128 {
		return r, ErrDenied
	}
	decode := func(out any) error { return json.Unmarshal(m.Payload, out) }
	var err error
	switch m.Operation {
	case "machine.info", "runtime.list", "runtime.get", "runtime.stop", "runtime.forget", "runtime.capture", "acp.state":
	case "profile.start":
		var p api.Profile
		err = decode(&p)
		if err == nil {
			err = p.Validate()
		}
		r.Resource = Resource{Directory: p.WorkingDirectory, Adapter: p.Adapter, ManagedACP: p.ManagedACP}
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
		if !oneOf(a.Action, "stat", "list", "read", "write", "mkdir", "rename", "remove") {
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
	case "git":
		var a api.Git
		err = decode(&a)
		r.Suboperation = a.Action
		r.Mode = a.Mode
		r.Resource.Directory = a.Directory
		if !oneOf(a.Action, "status", "diff", "log", "show", "stage", "unstage", "discard", "commit", "amend", "branch", "checkout", "stash", "fetch", "pull", "push", "merge", "rebase", "conflicts") {
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
	case "agent.config":
		var a api.AgentConfigRequest
		err = decode(&a)
		r.Suboperation = a.Action
		if a.Action == "delete" {
			r.Resource.ConfigID = a.ID
		}
		if a.Action == "save" && a.Config != nil {
			r.Resource.ConfigID = a.Config.ID
		}
		if !oneOf(a.Action, "list", "save", "delete") {
			return r, ErrDenied
		}
	case "runtime.history":
		var a struct{ Action string }
		err = decode(&a)
		r.Suboperation = a.Action
		if !oneOf(a.Action, "older", "newer", "close") {
			return r, ErrDenied
		}
	case "acp.action":
		var a struct {
			Action string
			Cwd    string
		}
		err = decode(&a)
		r.Suboperation = a.Action
		if oneOf(a.Action, "new", "load", "list") {
			r.Resource.Directory = a.Cwd
		}
		if !oneOf(a.Action, "new", "load", "list", "prompt", "permission", "cancel") {
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
	for _, s := range []string{r.Resource.Path, r.Resource.Destination, r.Resource.Directory, r.Resource.UploadID, r.Resource.ConfigID, r.Runtime.ID, r.Runtime.Incarnation} {
		if len(s) > 4096 || strings.ContainsRune(s, 0) {
			return r, ErrDenied
		}
	}
	return r, nil
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
	case "input":
		if runtime.Adapter == "acp" {
			r.Operation = "acp.raw"
			r.Suboperation = "exchange"
		} else if runtime.Adapter == "pty" {
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
	if json.Unmarshal(m.Payload, &runtime) != nil || runtime.ID == "" || runtime.Incarnation == "" || runtime.Generation == 0 || !oneOf(runtime.Adapter, "pty", "acp") {
		return RuntimeIdentity{}, fmt.Errorf("invalid runtime response")
	}
	if base.Operation == "runtime.attach" && (runtime.ID != base.Runtime.ID || runtime.Incarnation != base.Runtime.Incarnation || runtime.Generation != base.Runtime.Generation) {
		return RuntimeIdentity{}, ErrDenied
	}
	return RuntimeIdentity{ID: runtime.ID, Incarnation: runtime.Incarnation, Generation: runtime.Generation, Adapter: runtime.Adapter}, nil
}
