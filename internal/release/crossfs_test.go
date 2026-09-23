package release

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/aiomni/dune/internal/statecontract"
	"github.com/aiomni/dune/pkg/upgrade"
)

func TestLocalStagingAcrossFilesystems(t *testing.T) {
	volume := os.Getenv("DUNE_TEST_RELEASE_SOURCE_DIR")
	if volume == "" {
		t.Skip("set DUNE_TEST_RELEASE_SOURCE_DIR to a private test directory on another filesystem")
	}
	source, err := os.MkdirTemp(volume, "dune-release-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(source)
	destination := filepath.Join(t.TempDir(), "release")
	left, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	right, err := os.Stat(filepath.Dir(destination))
	if err != nil {
		t.Fatal(err)
	}
	if left.Sys().(*syscall.Stat_t).Dev == right.Sys().(*syscall.Stat_t).Dev {
		t.Fatal("cross-filesystem test directories are on the same device")
	}
	for _, name := range []string{"dune", "tmux", "rg", "licenses/NOTICE"} {
		file := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0700)
		if strings.HasPrefix(name, "licenses/") {
			mode = 0600
		}
		if err := os.WriteFile(file, []byte(name), mode); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := LocalManifest(t.Context(), source, "cross-filesystem", statecontract.ID(), upgrade.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	if err := StageLocal(t.Context(), source, destination, manifest); err != nil {
		t.Fatal(err)
	}
	if _, complete, err := Observe(t.Context(), destination, manifest.Components); err != nil || !complete {
		t.Fatal("cross-filesystem copy incomplete", err)
	}
	if _, complete, err := Observe(t.Context(), source, manifest.Components); err != nil || !complete {
		t.Fatal("staging changed source", err)
	}
	t.Logf("verified complete staging between distinct filesystem devices %d and %d", left.Sys().(*syscall.Stat_t).Dev, right.Sys().(*syscall.Stat_t).Dev)
}
