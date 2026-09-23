package fabricd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/retainedprogram"
	"github.com/aiomni/dune/pkg/api"
)

func TestMachineInfoReportsOriginalImageAfterInstallationReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dune")
	identity, err := retainedprogram.Current(path)
	if err != nil {
		t.Fatal(err)
	}
	h := newCleanupProcessHarness(t)
	h.program = path
	h.start("")
	inspect := func() api.RunningProgram {
		t.Helper()
		var info api.MachineInfo
		if err := h.client.Call(h.ctx, "machine.info", nil, &info); err != nil {
			t.Fatal(err)
		}
		if info.RunningProgram == nil || info.RunningProgram.SHA256 != identity.SHA256 || info.RunningProgram.PID != h.cmd.Process.Pid || info.RunningProgram.StartID == "" {
			t.Fatal(info)
		}
		return *info.RunningProgram
	}
	before := inspect()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("different installed image"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".old"); err != nil {
		t.Fatal(err)
	}
	after := inspect()
	if after.StartID != before.StartID {
		t.Fatal("inspection invented another process", after)
	}
}
