package release

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/upgrade"
)

func TestLocalInstallationNormalizesModesWithoutInventingDownloadOrigin(t *testing.T) {
	source := t.TempDir()
	for _, path := range []string{"dune", "tmux", "rg", "licenses/NOTICE"} {
		full := filepath.Join(source, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(path), 0644); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := LocalManifest(context.Background(), source, "local", strings.Repeat("b", 64), upgrade.Platform{OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ArchiveURL != "" || manifest.ArchiveSHA256 != "" || manifest.ValidateDownload() == nil {
		t.Fatal("local receipt became downloadable release")
	}
	destination := filepath.Join(t.TempDir(), "release")
	if err := StageLocal(t.Context(), source, destination, manifest); err != nil {
		t.Fatal(err)
	}
	_, complete, err := Observe(t.Context(), destination, manifest.Components)
	if err != nil || !complete {
		t.Fatal("source modes leaked into installed distribution", err)
	}
}
