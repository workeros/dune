package host

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/profiles"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/workbench"
)

func (f executorFixture) agentScope() agents.Scope {
	return agents.Scope{Principal: f.principal, OwnerID: f.owner}
}

func launchShell(directory, script string) api.Profile {
	return api.Profile{Version: 1, Kind: "agent", WorkingDirectory: directory, Adapter: "pty", Start: api.Command{Argv: []string{"/bin/sh", "-c", script + "; sleep 60"}}}
}

func waitLaunchFile(t *testing.T, filename, expected string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(filename)
		if err == nil && string(content) == expected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Agent did not produce expected startup file", filename)
}

func TestAgentLauncherFreezesProjectProfileAndEffectiveEnvironment(t *testing.T) {
	f := openExecutorFixture(t)
	profile := launchShell("/unused/profile/path", `printf '%s:%s' "$DEFAULT_VALUE" "$CHOICE" > launched`)
	profile.Env = map[string]string{"CHOICE": "old-profile"}
	record, err := f.app.store.Profiles().Create(t.Context(), profiles.Record{OwnerID: f.owner, Name: "Original", Profile: profile, CreatedBy: profiles.Actor{Type: "user", Subject: f.principal.ID}})
	if err != nil {
		t.Fatal(err)
	}
	project, err := f.app.store.SaveProject(t.Context(), f.owner, "", 0, workbench.ProjectSpec{Name: "project", DefaultProfile: &profiles.Selection{ID: record.ID, Revision: 1}, Directories: []workbench.Directory{{ID: "original", Binding: f.binding, Path: f.workspace}}})
	if err != nil {
		t.Fatal(err)
	}
	record.Profile.Env = map[string]string{"CHOICE": "new-profile"}
	if _, err := f.app.store.Profiles().Update(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	f.app.agentEnvironment = func(_ context.Context, scope agents.Scope, binding runner.Binding, overrides map[string]string) (map[string]string, error) {
		if scope.OwnerID != f.owner || binding != f.binding {
			t.Fatal("wrong environment scope")
		}
		overrides["DEFAULT_VALUE"] = "saved-environment"
		return overrides, nil
	}
	result, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), agents.StartRequest{Binding: f.binding, Project: &agents.ProjectSelection{ID: project.ID, Revision: project.Revision}, DirectoryID: "original"})
	if err != nil || result.Runtime == nil || result.Session == nil {
		t.Fatal(result, err)
	}
	waitLaunchFile(t, filepath.Join(f.workspace, "launched"), "saved-environment:old-profile")
	if result.Session.SourceProfile.Revision != 1 || result.Session.WorkingDirectory != f.workspace || result.Session.Status != "unavailable" || result.Session.ProjectID != project.ID || result.Session.DirectoryID != "original" {
		t.Fatal(result.Session)
	}
	saved, err := f.app.store.AgentSession(t.Context(), f.owner, result.Session.ID)
	if err != nil || saved.Launch.Profile.Env["CHOICE"] != "old-profile" || saved.Launch.Profile.Env["DEFAULT_VALUE"] != "saved-environment" || saved.Launch.Storage == "" || saved.LastRuntime.ID != result.Runtime.ID {
		t.Fatal("snapshot does not match actual startup", err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "saved-environment") || strings.Contains(string(encoded), "DEFAULT_VALUE") || strings.Contains(string(encoded), "sleep 60") {
		t.Fatal("public launch result exposes private configuration")
	}
}

func runLaunchGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git failed: %s: %v", output, err)
	}
}

func TestAgentLauncherCreatesWorktreeWithoutCopyingDirtySource(t *testing.T) {
	f := openExecutorFixture(t)
	runLaunchGit(t, f.workspace, "init", "-q")
	if err := os.WriteFile(filepath.Join(f.workspace, "tracked"), []byte("committed"), 0600); err != nil {
		t.Fatal(err)
	}
	runLaunchGit(t, f.workspace, "add", "tracked")
	runLaunchGit(t, f.workspace, "commit", "-qm", "initial")
	if err := os.WriteFile(filepath.Join(f.workspace, "tracked"), []byte("dirty"), 0600); err != nil {
		t.Fatal(err)
	}
	profile := launchShell(f.workspace, "cat tracked > observed")
	destination := filepath.Join(t.TempDir(), "isolated worktree")
	request := agents.StartRequest{Binding: f.binding, Custom: &profile, Worktree: &agents.WorktreeLocation{Path: destination, Branch: "feat/helper"}}
	result, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), request)
	if err != nil || result.Runtime == nil || result.Worktree == nil {
		t.Fatal(result, err)
	}
	waitLaunchFile(t, filepath.Join(destination, "observed"), "committed")
	if content, err := os.ReadFile(filepath.Join(f.workspace, "tracked")); err != nil || string(content) != "dirty" {
		t.Fatal("source modifications changed", err)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "observed")); !os.IsNotExist(err) {
		t.Fatal("Agent ran in source directory", err)
	}
	canonical, err := filepath.EvalSymlinks(destination)
	if err != nil {
		t.Fatal(err)
	}
	if result.Runtime.WorkingDirectory != canonical || result.Session.WorkingDirectory != canonical {
		t.Fatal("worktree and launch location differ", result)
	}
	// Retrying the same start cannot reuse an existing checkout/branch.
	if duplicate, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), request); err == nil || duplicate.Runtime != nil {
		t.Fatal("duplicate worktree created another Agent", duplicate, err)
	}
	items, err := f.app.store.AgentSessions(t.Context(), f.owner, "", 10)
	if err != nil || len(items) != 1 {
		t.Fatal("duplicate persisted a launch", len(items), err)
	}
}

func TestAgentLauncherRejectsScopeAndStaleInputsBeforeMutations(t *testing.T) {
	f := openExecutorFixture(t)
	profile := launchShell(f.workspace, "touch should-not-run")
	project, err := f.app.store.SaveProject(t.Context(), f.owner, "", 0, workbench.ProjectSpec{Name: "project"})
	if err != nil {
		t.Fatal(err)
	}
	request := agents.StartRequest{Binding: f.binding, Custom: &profile, Worktree: &agents.WorktreeLocation{Path: filepath.Join(f.workspace, "should-not-exist"), Branch: "feat/no-start"}}
	wrongScope := f.agentScope()
	wrongScope.OwnerID = "another-tenant"
	if _, err := f.app.AgentLauncher().Start(t.Context(), wrongScope, request); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("cross-owner launch", err)
	}
	stale := request
	stale.Binding.Revision++
	if _, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), stale); !errors.Is(err, runner.ErrBindingChanged) {
		t.Fatal("stale binding launch", err)
	}
	stale = request
	stale.Project = &agents.ProjectSelection{ID: project.ID, Revision: project.Revision + 1}
	if _, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), stale); !errors.Is(err, metadata.ErrConflict) {
		t.Fatal("stale project launch", err)
	}
	stale = request
	stale.Custom = nil
	stale.Profile = &profiles.Selection{ID: "any", Revision: 0}
	if _, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), stale); err == nil {
		t.Fatal("unfixed Profile revision accepted")
	}
	if _, err := os.Stat(request.Worktree.Path); !os.IsNotExist(err) {
		t.Fatal("rejected start created worktree", err)
	}
	items, err := f.app.store.AgentSessions(t.Context(), f.owner, "", 10)
	if err != nil || len(items) != 0 {
		t.Fatal("rejected start persisted a launch", len(items), err)
	}
}

func TestAgentLauncherKeepsPreparedWorktreeOnStartFailure(t *testing.T) {
	f := openExecutorFixture(t)
	runLaunchGit(t, f.workspace, "init", "-q")
	runLaunchGit(t, f.workspace, "commit", "--allow-empty", "-qm", "initial")
	profile := api.Profile{Version: 1, Kind: "agent", Adapter: "acp", WorkingDirectory: f.workspace, Start: api.Command{Argv: []string{"/does-not-exist/agent", "--acp"}}}
	result, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), agents.StartRequest{Binding: f.binding, Custom: &profile, Worktree: &agents.WorktreeLocation{Path: filepath.Join(t.TempDir(), "prepared"), Branch: "feat/prepared"}})
	if err == nil || result.Runtime != nil || result.Worktree == nil || result.Session == nil || result.Session.Attempt.State != "failed" {
		t.Fatal(result, err)
	}
	if _, err := os.Stat(result.Worktree.Path); err != nil {
		t.Fatal("prepared worktree lost", err)
	}
	saved, err := f.app.store.AgentSession(t.Context(), f.owner, result.Session.ID)
	if err != nil || saved.Status != "unavailable" || saved.Launch.Profile.WorkingDirectory != result.Worktree.Path || saved.Launch.Recovery.ID != "acp-load" {
		t.Fatal("failed startup lost its actual configuration", err)
	}
}
