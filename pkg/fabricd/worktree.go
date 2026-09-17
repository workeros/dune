package fabricd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/aiomni/dune/pkg/api"
)

func (d *Engine) worktreeGit(directory string, args ...string) (string, error) {
	result, err := d.exec(api.Exec{Command: api.Command{Argv: append([]string{"git", "--no-pager"}, args...), TimeoutSeconds: 120}, WorkingDirectory: directory, Env: gitEnv()})
	if err != nil {
		return "", err
	}
	if result.TimedOut {
		return "", &api.Error{Code: "RESULT_UNKNOWN", Detail: "Git worktree command timed out; inspect existing worktrees before another creation"}
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("Git worktree command failed: %s", strings.TrimSpace(result.Stderr))
	}
	if result.StdoutTruncated {
		return "", &api.Error{Code: "RESOURCE_EXHAUSTED", Detail: "Git worktree response exceeds the output limit"}
	}
	return result.Stdout, nil
}

func cleanWorktreePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && len(path) <= 4096 && !strings.ContainsFunc(path, unicode.IsControl)
}

func (d *Engine) worktrees(directory string) ([]api.Worktree, error) {
	if !cleanWorktreePath(directory) {
		return nil, &api.Error{Code: "INVALID_ARGUMENT", Detail: "directory must be a clean absolute path"}
	}
	common, err := d.gitCommonDirectory(directory)
	if err != nil {
		return nil, err
	}
	release := d.gitLocks.acquire(common)
	defer release()
	return d.readWorktrees(directory)
}

func (d *Engine) readWorktrees(directory string) ([]api.Worktree, error) {
	output, err := d.worktreeGit(directory, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	result := []api.Worktree{}
	for _, record := range strings.Split(output, "\x00\x00") {
		if record == "" {
			continue
		}
		var worktree api.Worktree
		for _, field := range strings.Split(record, "\x00") {
			name, value, _ := strings.Cut(field, " ")
			switch name {
			case "worktree":
				worktree.Path = value
			case "HEAD":
				worktree.Head = value
			case "branch":
				worktree.Branch = strings.TrimPrefix(value, "refs/heads/")
			case "detached":
				worktree.Detached = true
			case "bare":
				worktree.Bare = true
			case "locked":
				worktree.Locked = true
			case "prunable":
				worktree.Prunable = true
			}
		}
		if worktree.Path == "" {
			return nil, fmt.Errorf("invalid Git worktree listing")
		}
		result = append(result, worktree)
		if len(result) > 1024 {
			return nil, &api.Error{Code: "RESOURCE_EXHAUSTED", Detail: "at most 1024 worktrees can be listed"}
		}
	}
	return result, nil
}

func (d *Engine) createWorktree(request api.WorktreeCreate) (api.Worktree, error) {
	if !cleanWorktreePath(request.Directory) || !cleanWorktreePath(request.Path) || request.Path == "/" || request.Branch == "" || len(request.Branch) > 256 || len(request.Ref) > 4096 || strings.HasPrefix(request.Branch, "-") || strings.HasPrefix(request.Ref, "-") || strings.ContainsFunc(request.Branch+request.Ref, unicode.IsControl) {
		return api.Worktree{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: "worktree requires clean absolute source/destination paths and a valid new branch/ref"}
	}
	common, err := d.gitCommonDirectory(request.Directory)
	if err != nil {
		return api.Worktree{}, err
	}
	release := d.gitLocks.acquire(common)
	defer release()
	if _, err := os.Lstat(request.Path); !os.IsNotExist(err) {
		return api.Worktree{}, &api.Error{Code: "PATH_EXISTS", Detail: "worktree destination must not exist"}
	}
	if _, err := d.worktreeGit(request.Directory, "check-ref-format", "--branch", request.Branch); err != nil {
		return api.Worktree{}, err
	}
	if request.Ref == "" {
		request.Ref = "HEAD"
	}
	head, err := d.worktreeGit(request.Directory, "rev-parse", "--verify", "--end-of-options", request.Ref+"^{commit}")
	if err != nil {
		return api.Worktree{}, err
	}
	head = strings.TrimSpace(head)
	if _, err = d.worktreeGit(request.Directory, "worktree", "add", "-b", request.Branch, "--", request.Path, head); err != nil {
		return api.Worktree{}, err
	}
	items, err := d.readWorktrees(request.Directory)
	if err != nil {
		return api.Worktree{}, &api.Error{Code: "RESULT_UNKNOWN", Detail: "worktree creation completed but final inspection failed: " + err.Error()}
	}
	canonical, err := filepath.EvalSymlinks(request.Path)
	if err != nil {
		return api.Worktree{}, &api.Error{Code: "RESULT_UNKNOWN", Detail: "cannot inspect created worktree path"}
	}
	for _, item := range items {
		if item.Path == canonical && item.Branch == request.Branch && item.Head == head {
			return item, nil
		}
	}
	return api.Worktree{}, &api.Error{Code: "RESULT_UNKNOWN", Detail: "created worktree identity changed before confirmation; request was not replayed"}
}
