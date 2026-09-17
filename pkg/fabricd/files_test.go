package fabricd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/pkg/api"
)

func TestFilePagesAndCAS(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"alpha.txt":       "alpha",
		"bravo-match.txt": "bravo",
		"charlie.txt":     "charlie",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "match.md"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}

	engine := newEngine(context.Background())
	defer engine.Close()
	firstValue, err := engine.files(api.File{Action: "list_page", Path: root, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	first := firstValue.(api.FilePage)
	if len(first.Items) != 2 || first.Items[0].Name != "alpha.txt" || first.Items[1].Name != "bravo-match.txt" || first.NextCursor == "" {
		t.Fatalf("unexpected first page: %+v", first)
	}
	secondValue, err := engine.files(api.File{Action: "list_page", Path: root, Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	second := secondValue.(api.FilePage)
	if len(second.Items) != 2 || second.Items[0].Name != "charlie.txt" || second.Items[1].Name != "nested" || second.NextCursor != "" {
		t.Fatalf("unexpected second page: %+v", second)
	}

	path := filepath.Join(root, "alpha.txt")
	readValue, err := engine.files(api.File{Action: "read", Path: path, Length: 3, WithRevision: true})
	if err != nil {
		t.Fatal(err)
	}
	chunk := readValue.(api.FileChunk)
	if string(chunk.Data) != "alp" || chunk.Offset != 3 || chunk.EOF || chunk.Info.Revision == "" || chunk.Info.ContentHash == "" {
		t.Fatalf("unexpected read chunk: %+v", chunk)
	}
	writtenValue, err := engine.files(api.File{Action: "write", Path: path, Data: []byte("updated"), Intent: "conditional", ExpectedRevision: chunk.Info.Revision})
	if err != nil {
		t.Fatal(err)
	}
	written := writtenValue.(api.FileInfo)
	if written.Revision == chunk.Info.Revision {
		t.Fatal("successful write did not advance revision")
	}
	_, err = engine.files(api.File{Action: "write", Path: path, Data: []byte("stale"), Intent: "conditional", ExpectedRevision: chunk.Info.Revision})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "FILE_CHANGED" {
		t.Fatalf("stale write error = %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "updated" {
		t.Fatalf("stale write changed file: %q, %v", contents, err)
	}
	_, err = engine.files(api.File{Action: "write", Path: path, Data: []byte("forced"), Intent: "unconditional"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUploadCommitChecksRevision(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "large.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(context.Background())
	cleaner, err := process.NewCleaner()
	if err != nil {
		t.Fatal(err)
	}
	engine.cleaner = cleaner
	defer engine.Close()
	info, err := statFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("uploaded replacement")
	digest := sha256.Sum256(data)
	createdValue, err := engine.uploadOp(api.Upload{Action: "create", Path: path, Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), Intent: "conditional", ExpectedRevision: info.Revision})
	if err != nil {
		t.Fatal(err)
	}
	created := createdValue.(api.UploadState)
	if _, err = engine.uploadOp(api.Upload{Action: "chunk", ID: created.ID, Data: data}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("concurrent edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = engine.uploadOp(api.Upload{Action: "commit", ID: created.ID})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "FILE_CHANGED" {
		t.Fatalf("stale upload commit error = %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "concurrent edit" {
		t.Fatalf("stale upload replaced file: %q, %v", contents, err)
	}
}
