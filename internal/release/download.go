package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiomni/dune/pkg/upgrade"
)

// Download verifies the frozen archive before decoding any member. Both bytes
// and time are bounded independently of browser/request waiting preferences.
func Download(ctx context.Context, client *http.Client, manifest upgrade.Manifest, destination string) (err error) {
	if err := manifest.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, manifest.ArchiveURL, nil)
	if err != nil {
		return err
	}
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("release download unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > upgrade.MaxArchiveBytes {
		return fmt.Errorf("release download rejected or exceeds size limit")
	}
	archive, err := os.CreateTemp(filepath.Dir(destination), ".dune-download-")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(archive, digest), io.LimitReader(response.Body, upgrade.MaxArchiveBytes+1))
	if err != nil {
		return fmt.Errorf("release download interrupted")
	}
	if n > upgrade.MaxArchiveBytes || hex.EncodeToString(digest.Sum(nil)) != manifest.ArchiveSHA256 {
		return fmt.Errorf("release archive digest or size mismatch")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(destination)
		}
	}()
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := extract(ctx, archive, root, manifest.Components); err != nil {
		return err
	}
	observations, complete, err := Observe(ctx, destination, manifest.Components)
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("staged release is incomplete")
	}
	return syncTree(root, observations)
}

func extract(ctx context.Context, archive io.Reader, root *os.Root, components []upgrade.Component) error {
	compressed, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("invalid release compression")
	}
	defer compressed.Close()
	reader := tar.NewReader(&contextReader{ctx, io.LimitReader(compressed, upgrade.MaxReleaseBytes+(1<<20))})
	expected := make(map[string]upgrade.Component, len(components))
	directories := map[string]bool{".": true}
	for _, component := range components {
		expected[component.Path] = component
		for dir := path.Dir(component.Path); dir != "."; dir = path.Dir(dir) {
			directories[dir] = true
		}
	}
	seen := make(map[string]bool)
	entries := 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid release archive")
		}
		entries++
		if entries > upgrade.MaxComponents*4 {
			return fmt.Errorf("too many archive entries")
		}
		name := strings.TrimSuffix(header.Name, "/")
		if header.Mode & ^int64(0777) != 0 || path.Clean(name) != name || seen[name] {
			return fmt.Errorf("unsafe or duplicate archive entry")
		}
		for key := range header.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse") {
				return fmt.Errorf("sparse archive entries are unsupported")
			}
		}
		seen[name] = true
		if header.Typeflag == tar.TypeDir {
			if !directories[name] {
				return fmt.Errorf("undeclared archive directory")
			}
			if err := root.MkdirAll(name, 0700); err != nil {
				return err
			}
			continue
		}
		component, ok := expected[name]
		if !ok || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) || header.Size != component.Bytes {
			return fmt.Errorf("undeclared or unsupported archive file")
		}
		if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		if err := writeFile(ctx, root, name, reader, component); err != nil {
			return err
		}
		delete(expected, name)
	}
	if len(expected) != 0 {
		return fmt.Errorf("release archive is incomplete")
	}
	// Force gzip checksum verification; trailing decoded payload is not a second
	// distribution and is bounded just like headers and component contents.
	trailing, err := io.Copy(io.Discard, io.LimitReader(compressed, 1<<20))
	if err != nil || trailing >= 1<<20 {
		return fmt.Errorf("invalid release archive trailer")
	}
	return nil
}
