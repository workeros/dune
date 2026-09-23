package upgrade_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aiomni/dune/internal/release"
	"github.com/aiomni/dune/pkg/upgrade"
)

func TestPublishedArchiveMatchesGoManifestAndDownloader(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("Python publication tool is unavailable")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	target := runtime.GOOS + "-" + runtime.GOARCH
	// Packaging hashes bytes; executable validation belongs to the target check.
	if err := os.WriteFile(filepath.Join(dir, "dune-"+target), []byte("publication test image"), 0700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer server.Close()
	cmd := exec.Command("python3", filepath.Join(root, "scripts/package-release.py"), "--release-id", "test<&发行>", "--base-url", server.URL+"/", "--platform", target, "--output-dir", dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("publication: %v %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(dir, "dune-"+target+".manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest upgrade.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(dir, "dune-"+target+".ref.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ref upgrade.ReleaseRef
	if err := json.Unmarshal(data, &ref); err != nil {
		t.Fatal(err)
	}
	if !manifest.Matches(ref) {
		t.Fatal("Python canonical reference differs from Go digest")
	}
	catalogFile, err := os.Open(filepath.Join(dir, "upgrade-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer catalogFile.Close()
	catalog, err := upgrade.ReadCatalog(catalogFile)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := catalog.Resolve(context.Background(), ref, manifest.Platform)
	if err != nil {
		t.Fatal(err)
	}
	if err := release.Download(t.Context(), nil, approved, filepath.Join(t.TempDir(), "release")); err != nil {
		t.Fatal("published archive refused by real downloader", err)
	}
}
