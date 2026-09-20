package fabricd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/pkg/api"
)

// Artifacts are observations, never authority for adoption or deletion. Only
// independently registered inode/instance evidence can enter a forget plan.
func (d *Engine) sessionArtifactIssues(ctx context.Context, discovery sessionregistry.HostDiscovery) []api.RuntimeDiscoveryIssue {
	const maxIssues = 128
	issues := []api.RuntimeDiscoveryIssue{}
	add := func(runtime *api.Runtime, code, reference string) {
		if len(issues) < maxIssues {
			issues = append(issues, api.RuntimeDiscoveryIssue{Runtime: runtime, Code: code, ArtifactRef: reference})
		}
	}
	known := make(map[string]sessionregistry.HostRecord)
	names := make(map[string]bool)
	for _, issue := range discovery.Issues {
		if issue.Runtime != nil {
			names[issue.Runtime.ID] = true
		}
	}
	for _, host := range discovery.Hosts {
		known[host.Runtime.ID], names[host.Runtime.ID] = host, true
		if host.Retiring {
			names[".forget-"+host.Instance] = true
			continue
		}
		if ctx.Err() != nil {
			add(nil, "ARTIFACT_SCAN_INCOMPLETE", "")
			break
		}
		observed := host.Runtime
		if d.verifyHostResources(host) != nil {
			// Registration validation reports the identity problem. Never inspect
			// paths supplied by an unverified record, even for diagnostics.
			continue
		}
		info, err := os.Lstat(host.Resources.Socket.Path)
		if os.IsNotExist(err) {
			add(&observed, "IPC_SOCKET_MISSING", "")
		} else if err != nil || !matchesFile(info, host.Resources.Socket, os.ModeSocket) {
			add(&observed, "IPC_SOCKET_REPLACED", "")
		}
		info, err = os.Lstat(host.Resources.Directory.Path)
		if os.IsNotExist(err) {
			add(&observed, "RUNTIME_DIRECTORY_MISSING", "")
			continue
		}
		if err != nil || !matchesFile(info, host.Resources.Directory, os.ModeDir) {
			add(&observed, "RUNTIME_DIRECTORY_INVALID", "")
			continue
		}
		root, err := os.OpenRoot(host.Resources.Directory.Path)
		if err != nil {
			add(&observed, "RUNTIME_DIRECTORY_INVALID", "")
			continue
		}
		info, err = root.Stat(".")
		if err != nil || !matchesFile(info, host.Resources.Directory, os.ModeDir) {
			root.Close()
			add(&observed, "RUNTIME_DIRECTORY_INVALID", "")
			continue
		}
		var instance string
		if err := privateRootFile(root, "instance.json", 256, &instance); err != nil || instance != host.Instance {
			add(&observed, "INSTANCE_MARKER_INVALID", "")
		}
		var disk sessionRegistration
		var registered sessionRegistration
		_ = json.Unmarshal(host.Registration, &registered) // verifyHostResources already validated it.
		err = privateRootFile(root, "registration.json", 64*1024, &disk)
		diskSocket, identityErr := sessionSocketPath(disk)
		if os.IsNotExist(err) {
			add(&observed, "REGISTRATION_FILE_MISSING", "")
		} else if err != nil || identityErr != nil || diskSocket != host.Resources.Socket.Path || disk.Instance != host.Instance || disk.Target != host.Target || disk.Installation != installationID(d.stateDir) || disk.Program != registered.Program {
			add(&observed, "REGISTRATION_FILE_INVALID", "")
		}
		directory, err := root.Open(".")
		if err == nil {
			entries, readErr := directory.ReadDir(33)
			if len(entries) > 32 || (readErr != nil && readErr != io.EOF) {
				add(&observed, "ARTIFACT_SCAN_INCOMPLETE", "")
			}
			for _, entry := range entries[:min(len(entries), 32)] {
				if strings.HasPrefix(entry.Name(), ".pending-") {
					add(&observed, "REGISTRATION_TEMPORARY_FILE", "")
					break
				}
			}
			directory.Close()
		} else {
			add(&observed, "ARTIFACT_SCAN_INCOMPLETE", "")
		}
		root.Close()
	}
	rootPath := filepath.Join(d.stateDir, "acp", "runtimes")
	directory, err := os.OpenFile(rootPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		add(nil, "RUNTIME_INDEX_DIRECTORY_INVALID", "")
	} else {
		entries, readErr := directory.ReadDir(257)
		if len(entries) > 256 || (readErr != nil && readErr != io.EOF) {
			add(nil, "ARTIFACT_SCAN_INCOMPLETE", "")
		}
		for _, entry := range entries[:min(len(entries), 256)] {
			if names[entry.Name()] {
				continue
			}
			digest := sha256.Sum256([]byte(installationID(d.stateDir) + "/" + entry.Name()))
			add(nil, "UNREGISTERED_RUNTIME_ARTIFACT", fmt.Sprintf("directory:%x", digest[:16]))
		}
		directory.Close()
	}
	panes, err := d.acpTmux.HostPanes(ctx)
	if err != nil {
		add(nil, "TMUX_DISCOVERY_UNAVAILABLE", "")
	}
	for _, pane := range panes {
		if host, ok := known[pane.ID]; ok {
			if pane.Instance != host.Instance {
				observed := host.Runtime
				add(&observed, "HOST_PANE_IDENTITY_MISMATCH", pane.Reference)
			}
			continue
		}
		if _, registered := known[pane.ID]; !registered && names[pane.ID] {
			continue
		}
		add(nil, "UNREGISTERED_HOST_PANE", pane.Reference)
	}
	if len(issues) == maxIssues {
		issues = append(issues, api.RuntimeDiscoveryIssue{Code: "ARTIFACT_SCAN_INCOMPLETE"})
	}
	return issues
}
