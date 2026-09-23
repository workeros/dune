package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/statecontract"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/upgrade"
)

func sum(body []byte) string { digest := sha256.Sum256(body); return hex.EncodeToString(digest[:]) }

func fixture(t *testing.T, fault string) (upgrade.Manifest, []byte) {
	t.Helper()
	manifest := upgrade.Manifest{ID: "release", Platform: upgrade.Platform{OS: "linux", Arch: "amd64"}, StateContract: statecontract.ID()}
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	for _, name := range []string{"dune", "tmux", "rg", "licenses/NOTICE"} {
		body := []byte(name)
		mode := uint32(0700)
		if name == "licenses/NOTICE" {
			mode = 0600
		}
		manifest.Components = append(manifest.Components, upgrade.Component{Path: name, SHA256: sum(body), Bytes: int64(len(body)), Mode: mode})
		if fault == "missing" && name == "rg" {
			continue
		}
		header := &tar.Header{Name: name, Mode: 0755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if name == "rg" {
			switch fault {
			case "escape":
				header.Name = "../outside"
			case "symlink":
				header.Typeflag, header.Linkname, header.Size = tar.TypeSymlink, "dune", 0
			case "hardlink":
				header.Typeflag, header.Linkname, header.Size = tar.TypeLink, "dune", 0
			case "setuid":
				header.Mode = 04755
			case "digest":
				body = []byte("RG")
			}
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write(body); err != nil {
				t.Fatal(err)
			}
		}
		if fault == "duplicate" && name == "dune" {
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	manifest.ArchiveSHA256 = sum(archive.Bytes())
	return manifest, archive.Bytes()
}

func downloadFixture(t *testing.T, fault string) (upgrade.Manifest, string, error) {
	t.Helper()
	manifest, archive := fixture(t, fault)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	manifest.ArchiveURL = server.URL + "/archive"
	if fault == "archive_hash" {
		manifest.ArchiveSHA256 = sum([]byte("other"))
	}
	destination := filepath.Join(t.TempDir(), "candidate")
	return manifest, destination, Download(t.Context(), server.Client(), manifest, destination)
}

func TestDownloadRejectsInvalidDistributionBeforePublication(t *testing.T) {
	for _, fault := range []string{"escape", "symlink", "hardlink", "setuid", "digest", "missing", "duplicate", "archive_hash"} {
		t.Run(fault, func(t *testing.T) {
			_, destination, err := downloadFixture(t, fault)
			if err == nil {
				t.Fatal("invalid candidate accepted")
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("partial candidate published", err)
			}
		})
	}
}

func TestFullReleaseObservationAndExactSourceCopy(t *testing.T) {
	for _, changed := range []string{"none", "tmux", "rg", "licenses/NOTICE", "missing", "mode"} {
		t.Run(changed, func(t *testing.T) {
			manifest, directory, err := downloadFixture(t, "")
			if err != nil {
				t.Fatal(err)
			}
			observed, complete, err := Observe(t.Context(), directory, manifest.Components)
			if err != nil || !complete {
				t.Fatal(observed, complete, err)
			}
			switch changed {
			case "none":
			case "missing":
				err = os.Remove(filepath.Join(directory, "rg"))
			case "mode":
				err = os.Chmod(filepath.Join(directory, "rg"), 0600)
			default:
				err = os.WriteFile(filepath.Join(directory, changed), []byte("changed"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			observed, complete, err = Observe(t.Context(), directory, manifest.Components)
			if err != nil || complete != (changed == "none") {
				t.Fatal(observed, complete, err)
			}
			plan := upgrade.Compare(upgrade.Inspection{Installation: &upgrade.Installation{Release: manifest, Components: observed, Complete: complete}, Running: api.RunningProgram{SHA256: manifest.ProgramSHA256()}, RunningFromSelectedRelease: true}, manifest)
			if plan.ReleaseUpdateRequired != (changed != "none") {
				t.Fatal("Dune SHA hid component change", plan)
			}
			copy := filepath.Join(t.TempDir(), "backup")
			if err := Copy(t.Context(), directory, copy, observed); err != nil {
				t.Fatal(err)
			}
			copied, _, err := Observe(t.Context(), copy, manifest.Components)
			if err != nil {
				t.Fatal(err)
			}
			for i := range observed {
				if observed[i] != copied[i] {
					t.Fatal("copy lost original state", observed[i], copied[i])
				}
			}
		})
	}
}

func TestCopyRejectsChangedSource(t *testing.T) {
	manifest, directory, err := downloadFixture(t, "")
	if err != nil {
		t.Fatal(err)
	}
	observed, _, err := Observe(t.Context(), directory, manifest.Components)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "rg"), []byte("replaced"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := Copy(t.Context(), directory, filepath.Join(t.TempDir(), "backup"), observed); err == nil {
		t.Fatal("stale source observation accepted")
	}
}
