package fabricd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestACPHostWithSymlinkedAncestorSurvivesConnectorRestart(t *testing.T) {
	h := newCleanupProcessHarness(t)
	alias := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}
	h.state = filepath.Join(alias, "sessions")
	h.start("")
	original, stream, err := testStartProfile(h.client, h.ctx, api.Profile{
		Version: 1, Kind: "agent", Adapter: "acp", WorkingDirectory: t.TempDir(),
		Start: api.Command{Argv: []string{"/bin/cat"}},
	})
	if err != nil {
		t.Fatal("host could not start through a symlinked ancestor", err)
	}
	stream.Close()
	before, err := h.client.Get(h.ctx, original)
	if err != nil || before.ACPHost == nil || before.ACPHost.HostPID == 0 {
		t.Fatal("host did not publish its process identity", before, err)
	}
	h.kill()
	h.start("")
	after, err := h.client.Get(h.ctx, original)
	if err != nil || after.ACPHost == nil || after.ACPHost.HostPID != before.ACPHost.HostPID || after.ID != original.ID || after.Incarnation != original.Incarnation {
		t.Fatal("reconnect replaced the original host", after, err)
	}
	if err := testStopRuntime(h.client, h.ctx, original); err != nil {
		t.Fatal(err)
	}
	if err := testForgetRuntime(h.client, h.ctx, original); err != nil {
		t.Fatal(err)
	}
}
