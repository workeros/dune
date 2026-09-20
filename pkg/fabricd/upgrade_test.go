package fabricd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

func TestUpgradePreflightIncludesLateOriginalLaunchAndLegacyBoundary(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if report := CheckUpgrade(t.Context(), state); !report.Allowed {
		t.Fatal(report)
	}
	if _, err := os.Lstat(state); !os.IsNotExist(err) {
		t.Fatal("preview created state", err)
	}
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	legacy, err := os.OpenFile(filepath.Join(state, "fabricd.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if err := syscall.Flock(int(legacy.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	report := CheckUpgrade(t.Context(), state)
	if report.Allowed || len(report.Issues) != 1 || report.Issues[0].Code != "LEGACY_CONNECTOR_REQUIRES_EXPLICIT_TRANSITION" {
		t.Fatal(report)
	}
	legacy.Close()
	registry, err := sessionregistry.Open(t.Context(), filepath.Join(state, "registry"), sessionregistry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	for _, protocol := range []int{sessionProtocol, sessionProtocol + 1} {
		key := api.SubmissionKey{SubmissionID: wire.ID(), Target: api.SubmissionTarget{OwnerID: "owner", RunnerID: "runner", MachineID: "machine", FabricID: "fabric", BindingRevision: 1}}
		claim, _, err := registry.ClaimKey(t.Context(), key, sessionregistry.Digest("profile.start", nil), "registry:launch")
		if err != nil {
			t.Fatal(err)
		}
		pending := api.Runtime{ID: wire.ID(), Incarnation: wire.ID(), Generation: 1, State: "starting", Adapter: "acp", ACPHost: &api.ACPHostInfo{Protocol: protocol}}
		if _, err := registry.AcceptLaunch(t.Context(), claim, wire.ID(), pending); err != nil {
			t.Fatal(err)
		}
		report = CheckUpgrade(t.Context(), state)
		if protocol == sessionProtocol {
			if !report.Allowed || len(report.Hosts) != 1 || report.Hosts[0].Phase != "HOST_REGISTRATION_PENDING" {
				t.Fatal(report)
			}
		} else if report.Allowed || len(report.Hosts) != 2 || len(report.Issues) != 1 || report.Issues[0].Runtime.ID != pending.ID || report.Issues[0].Code != "SESSION_PROTOCOL_UNSUPPORTED" {
			t.Fatal(report)
		}
	}
	launch, err := launchgate.Acquire(state, false)
	if err != nil {
		t.Fatal(err)
	}
	defer launch.Close()
	guard, report := PrepareUpgrade(t.Context(), state)
	if guard != nil || report.Allowed || report.Issues[0].Code != "LAUNCH_IN_PROGRESS" {
		t.Fatal("preflight crossed in-flight launch", report)
	}
}
