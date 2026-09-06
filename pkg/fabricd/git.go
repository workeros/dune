package fabricd

import (
	"fmt"
	"github.com/aiomni/dune/pkg/api"
	"os"
	"strings"
)

func gitArgs(a api.Git) ([]string, error) {
	if a.Patch != "" && (a.Action != "stage" && a.Action != "unstage" && a.Action != "discard" || len(a.Paths) > 0) {
		return nil, fmt.Errorf("patch requires stage/unstage/discard without paths")
	}
	for _, v := range []string{a.Ref, a.Name, a.Remote} {
		if strings.HasPrefix(v, "-") || strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("invalid Git argument")
		}
	}
	args := []string{"git", "--no-pager"}
	add := func(s ...string) { args = append(args, s...) }
	paths := func() error {
		if len(a.Paths) == 0 {
			return fmt.Errorf("paths required")
		}
		for _, p := range a.Paths {
			if p == "" {
				return fmt.Errorf("empty path")
			}
		}
		add("--")
		add(a.Paths...)
		return nil
	}
	switch a.Action {
	case "status":
		add("status", "--porcelain=v1", "-z", "--untracked-files=all")
	case "diff":
		add("diff", "--no-ext-diff", "--no-color")
		if a.Staged {
			add("--cached")
		}
		if a.Ref != "" {
			add(a.Ref)
		}
		if len(a.Paths) > 0 {
			if e := paths(); e != nil {
				return nil, e
			}
		}
	case "log":
		n := a.Limit
		if n == 0 {
			n = 50
		}
		if n < 1 || n > 1000 {
			return nil, fmt.Errorf("limit 1..1000 required")
		}
		add("log", fmt.Sprintf("-n%d", n), "--format=%H%x09%an%x09%aI%x09%s")
		if a.Ref != "" {
			add(a.Ref)
		}
	case "show":
		add("show", "--no-ext-diff", "--no-color")
		if a.Ref != "" {
			add(a.Ref)
		}
	case "stage", "unstage", "discard":
		if a.Patch != "" {
			add("apply", "--whitespace=nowarn")
			if a.Action == "stage" {
				add("--cached")
			}
			if a.Action == "unstage" {
				add("--cached", "--reverse")
			}
			if a.Action == "discard" {
				add("--reverse")
			}
			break
		}
		switch a.Action {
		case "stage":
			add("add")
		case "unstage":
			add("restore", "--staged")
		case "discard":
			add("restore", "--worktree")
		}
		if e := paths(); e != nil {
			return nil, e
		}
	case "commit", "amend":
		add("commit")
		if a.Action == "amend" {
			add("--amend")
		}
		if a.Message == "" {
			if a.Action == "commit" {
				return nil, fmt.Errorf("message required")
			}
			add("--no-edit")
		} else {
			add("-m", a.Message)
		}
	case "branch":
		add("branch")
		if a.Name != "" {
			add(a.Name)
			if a.Ref != "" {
				add(a.Ref)
			}
		}
	case "checkout":
		add("checkout")
		if a.Create {
			if a.Name == "" {
				return nil, fmt.Errorf("name required")
			}
			add("-b", a.Name)
		}
		if a.Ref != "" {
			add(a.Ref)
		} else if !a.Create {
			return nil, fmt.Errorf("ref required")
		}
	case "stash":
		switch a.Mode {
		case "", "push":
			add("stash", "push")
			if a.Message != "" {
				add("-m", a.Message)
			}
			if len(a.Paths) > 0 {
				if e := paths(); e != nil {
					return nil, e
				}
			}
		case "list":
			add("stash", "list")
		case "pop", "apply", "drop":
			add("stash", a.Mode)
			if a.Ref != "" {
				add(a.Ref)
			}
		default:
			return nil, fmt.Errorf("invalid stash mode")
		}
	case "fetch", "pull", "push":
		add(a.Action)
		if a.Action == "pull" {
			add("--no-rebase", "--no-edit")
		}
		if a.Remote != "" {
			add(a.Remote)
			if a.Ref != "" {
				add(a.Ref)
			}
		} else if a.Ref != "" {
			return nil, fmt.Errorf("remote required with ref")
		}
	case "merge", "rebase":
		add(a.Action)
		switch a.Mode {
		case "", "start":
			if a.Ref == "" {
				return nil, fmt.Errorf("ref required")
			}
			if a.Action == "merge" {
				add("--no-edit")
			}
			add(a.Ref)
		case "continue", "abort":
			add("--" + a.Mode)
		default:
			return nil, fmt.Errorf("invalid merge/rebase mode")
		}
	case "conflicts":
		add("diff", "--name-only", "--diff-filter=U", "-z")
	default:
		return nil, fmt.Errorf("unsupported Git action")
	}
	return args, nil
}
func (d *Engine) git(a api.Git) (any, error) {
	argv, e := gitArgs(a)
	if e != nil {
		return nil, e
	}
	if a.Directory == "" {
		return nil, fmt.Errorf("directory required")
	}
	d.gitMu.Lock()
	defer d.gitMu.Unlock()
	if a.Patch != "" {
		if len(a.Patch) > 256*1024 {
			return nil, fmt.Errorf("patch too large")
		}
		f, e := os.CreateTemp("", "dune-patch-*")
		if e != nil {
			return nil, e
		}
		defer os.Remove(f.Name())
		if _, e = f.WriteString(a.Patch); e != nil {
			f.Close()
			return nil, e
		}
		f.Close()
		check := append(append([]string{}, argv...), "--check", f.Name())
		r, e := d.exec(api.Exec{Command: api.Command{Argv: check}, WorkingDirectory: a.Directory, Env: gitEnv()})
		if e != nil {
			return nil, e
		}
		if r.ExitCode != 0 {
			return api.GitResult{ExecResult: r}, nil
		}
		argv = append(argv, f.Name())
	}
	r, e := d.exec(api.Exec{Command: api.Command{Argv: argv, TimeoutSeconds: 120}, WorkingDirectory: a.Directory, Env: gitEnv()})
	if e != nil {
		return nil, e
	}
	out := api.GitResult{ExecResult: r}
	if a.Action == "status" {
		entries := strings.Split(strings.TrimSuffix(r.Stdout, "\x00"), "\x00")
		for i := 0; i < len(entries); i++ {
			v := entries[i]
			if len(v) < 3 {
				continue
			}
			item := api.GitEntry{Index: v[:1], Worktree: v[1:2], Path: v[3:]}
			if strings.ContainsAny(v[:2], "RC") && i+1 < len(entries) {
				i++
				item.Original = entries[i]
			}
			out.Entries = append(out.Entries, item)
		}
	}
	if a.Action == "conflicts" && r.Stdout != "" {
		out.Conflicts = strings.Split(strings.TrimSuffix(r.Stdout, "\x00"), "\x00")
	}
	return out, nil
}
func gitEnv() map[string]string {
	return map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_ASKPASS": "/usr/bin/false", "SSH_ASKPASS": "/usr/bin/false", "GIT_EDITOR": "true", "GIT_SEQUENCE_EDITOR": "true", "GIT_MERGE_AUTOEDIT": "no", "GIT_SSH_COMMAND": "ssh -oBatchMode=yes", "LC_ALL": "C"}
}
