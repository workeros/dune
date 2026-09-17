package fabricd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

func testFileEngine(t *testing.T, uploads bool) *Engine {
	t.Helper()
	d := newEngine(context.Background())
	if uploads {
		var err error
		d.cleaner, err = process.NewCleaner()
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(d.Close)
	return d
}

func requireFileCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

func fileDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func strongFileInfo(t *testing.T, path string) api.FileInfo {
	t.Helper()
	info, err := statFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestFileRevisionTracksContentAndPermissions(t *testing.T) {
	d := testFileEngine(t, false)
	path := filepath.Join(t.TempDir(), "executable")
	if err := os.WriteFile(path, []byte("before"), 0751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0751); err != nil {
		t.Fatal(err)
	}
	first := strongFileInfo(t, path)
	if first.ContentHash != fileDigest([]byte("before")) || first.Revision == "" {
		t.Fatal(first)
	}
	touched := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, touched, touched); err != nil {
		t.Fatal(err)
	}
	if current := strongFileInfo(t, path); current.Revision != first.Revision {
		t.Fatal("mtime-only change altered revision", current)
	}
	value, err := d.files(api.File{Action: "write", Path: path, Intent: "conditional", ExpectedRevision: first.Revision, Data: []byte("second")})
	if err != nil {
		t.Fatal(err)
	}
	written := value.(api.FileInfo)
	if os.FileMode(written.Mode).Perm() != 0751 || written.ContentHash != fileDigest([]byte("second")) || written.Revision == first.Revision {
		t.Fatalf("incorrect committed metadata: %+v", written)
	}
	actual, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Mode().Perm() != 0751 {
		t.Fatal("replacement lost executable permissions", actual.Mode())
	}
	if err := os.WriteFile(path, []byte("extern"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, actual.ModTime(), actual.ModTime()); err != nil {
		t.Fatal(err)
	}
	_, err = d.files(api.File{Action: "write", Path: path, Intent: "conditional", ExpectedRevision: written.Revision, Data: []byte("stale!")})
	requireFileCode(t, err, "FILE_CHANGED")
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "extern" {
		t.Fatal("stale conditional write replaced external edit", string(contents), err)
	}
	current := strongFileInfo(t, path)
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	_, err = d.files(api.File{Action: "write", Path: path, Intent: "conditional", ExpectedRevision: current.Revision, Data: []byte("stale!")})
	requireFileCode(t, err, "FILE_CHANGED")
}

func TestFileReadMetadataAndDescriptorIdentity(t *testing.T) {
	d := testFileEngine(t, false)
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, request := range []api.File{{Action: "stat", Path: path}, {Action: "read", Path: path, Length: 3}} {
		value, err := d.files(request)
		if err != nil {
			t.Fatal(err)
		}
		var info api.FileInfo
		if request.Action == "stat" {
			info = value.(api.FileInfo)
		} else {
			info = value.(api.FileChunk).Info
		}
		if info.Revision != "" || info.ContentHash != "" {
			t.Fatal("metadata-only request computed content identity", info)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replaced"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := descriptorFileInfo(f, path, before, true)
	if err != nil || info.ContentHash != fileDigest([]byte("original")) {
		t.Fatal("descriptor metadata reopened the replacement path", info, err)
	}
	if err := os.WriteFile(path+".old", []byte("in-place edit"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = descriptorFileInfo(f, path, before, true)
	requireFileCode(t, err, "FILE_CHANGED")
}

func TestFileReadsDoNotWaitForCommitLock(t *testing.T) {
	d := testFileEngine(t, false)
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("read while another file commits"), 0600); err != nil {
		t.Fatal(err)
	}
	d.fileMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := d.files(api.File{Action: "stat", Path: path, WithRevision: true})
		if err == nil {
			_, err = d.files(api.File{Action: "read", Path: path, Length: 4, WithRevision: true})
		}
		done <- err
	}()
	select {
	case err := <-done:
		d.fileMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		d.fileMu.Unlock()
		<-done
		t.Fatal("read/stat waited for the global commit lock")
	}
}

func TestFileWriteIntentValidation(t *testing.T) {
	d := testFileEngine(t, false)
	path := filepath.Join(t.TempDir(), "file")
	for _, request := range []api.File{
		{Intent: ""}, {Intent: "unknown"}, {Intent: "conditional"},
		{Intent: "conditional", ExpectedRevision: "metadata-only"},
		{Intent: "create", ExpectedRevision: strings.Repeat("a", 64)},
		{Intent: "unconditional", ExpectedRevision: strings.Repeat("a", 64)},
		{Intent: "create", Overwrite: true},
	} {
		request.Action, request.Path, request.Data = "write", path, []byte("invalid")
		if _, err := d.files(request); err == nil {
			t.Fatal("accepted invalid intent", request)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("invalid intent created target", err)
		}
	}
	_, err := d.files(api.File{Action: "write", Path: path, Intent: "conditional", ExpectedRevision: strings.Repeat("a", 64)})
	requireFileCode(t, err, "FILE_CHANGED")
	value, err := d.files(api.File{Action: "write", Path: path, Intent: "unconditional", Data: []byte("new")})
	if err != nil || os.FileMode(value.(api.FileInfo).Mode).Perm() != 0600 {
		t.Fatal("unconditional create", value, err)
	}
	_, err = d.files(api.File{Action: "write", Path: path, Intent: "create", Data: []byte("clobber")})
	requireFileCode(t, err, "FILE_EXISTS")
	contents, _ := os.ReadFile(path)
	if string(contents) != "new" {
		t.Fatal("create clobbered target", string(contents))
	}
}

func TestFileReplacementRejectsLinksAndSpecialFiles(t *testing.T) {
	d := testFileEngine(t, false)
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "absent"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"link", "dangling", "directory", "fifo"} {
		for _, intent := range []string{"conditional", "unconditional"} {
			request := api.File{Action: "write", Path: filepath.Join(root, name), Intent: intent, Data: []byte("replacement")}
			if intent == "conditional" {
				request.ExpectedRevision = strings.Repeat("a", 64)
			}
			_, err := d.files(request)
			requireFileCode(t, err, "UNSUPPORTED")
		}
	}
	contents, _ := os.ReadFile(target)
	if string(contents) != "untouched" {
		t.Fatal("replacement followed symlink", string(contents))
	}
}

func TestFileReadDigestDetectsMixedChunks(t *testing.T) {
	d := testFileEngine(t, false)
	path := filepath.Join(t.TempDir(), "large")
	original := bytes.Repeat([]byte("a"), wire.ChunkSize+7)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	value, err := d.files(api.File{Action: "read", Path: path, Length: wire.ChunkSize, WithRevision: true})
	if err != nil {
		t.Fatal(err)
	}
	first := value.(api.FileChunk)
	if first.EOF || first.Info.Size != int64(len(original)) || first.Info.ContentHash != fileDigest(original) {
		t.Fatal(first)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("b"), len(original)), 0600); err != nil {
		t.Fatal(err)
	}
	value, err = d.files(api.File{Action: "read", Path: path, Offset: first.Offset, Length: wire.ChunkSize})
	if err != nil {
		t.Fatal(err)
	}
	last := value.(api.FileChunk)
	if !last.EOF || fileDigest(append(first.Data, last.Data...)) == first.Info.ContentHash {
		t.Fatal("mixed chunks escaped the full-file digest")
	}
}

func createTestUpload(t *testing.T, d *Engine, path, intent, expected string, data []byte) api.UploadState {
	t.Helper()
	value, err := d.uploadOp(api.Upload{Action: "create", Path: path, Intent: intent, ExpectedRevision: expected, Size: int64(len(data)), SHA256: fileDigest(data)})
	if err != nil {
		t.Fatal(err)
	}
	state := value.(api.UploadState)
	if len(data) > 0 {
		if _, err := d.uploadOp(api.Upload{Action: "chunk", ID: state.ID, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func TestFileAndUploadConditionalCommitsSerialize(t *testing.T) {
	d := testFileEngine(t, true)
	path := filepath.Join(t.TempDir(), "shared")
	if err := os.WriteFile(path, []byte("original"), 0700); err != nil {
		t.Fatal(err)
	}
	before := strongFileInfo(t, path)
	upload := createTestUpload(t, d, path, "conditional", before.Revision, []byte("upload wins"))
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := d.files(api.File{Action: "write", Path: path, Intent: "conditional", ExpectedRevision: before.Revision, Data: []byte("write wins")})
		results <- err
	}()
	go func() {
		<-start
		_, err := d.uploadOp(api.Upload{Action: "commit", ID: upload.ID})
		results <- err
	}()
	close(start)
	winners := 0
	for range 2 {
		if err := <-results; err == nil {
			winners++
		} else {
			requireFileCode(t, err, "FILE_CHANGED")
		}
	}
	if winners != 1 {
		t.Fatal("conditional commits did not have exactly one winner", winners)
	}
	if current := strongFileInfo(t, path); os.FileMode(current.Mode).Perm() != 0700 {
		t.Fatal("commit lost permissions", current)
	}
}

func TestUploadFixedIntentAndCommittedMetadata(t *testing.T) {
	d := testFileEngine(t, true)
	path := filepath.Join(t.TempDir(), "upload")
	if err := os.WriteFile(path, []byte("before"), 0750); err != nil {
		t.Fatal(err)
	}
	before := strongFileInfo(t, path)
	data := []byte("replacement")
	upload := createTestUpload(t, d, path, "conditional", before.Revision, data)
	for _, request := range []api.Upload{
		{Action: "commit", ID: upload.ID, Intent: "unconditional"},
		{Action: "commit", ID: upload.ID, ExpectedRevision: before.Revision},
	} {
		if _, err := d.uploadOp(request); err == nil {
			t.Fatal("commit accepted mutable creation fields", request)
		}
	}
	value, err := d.uploadOp(api.Upload{Action: "commit", ID: upload.ID})
	if err != nil {
		t.Fatal(err)
	}
	committed := value.(api.UploadState)
	if !committed.Committed || committed.File == nil || committed.File.Size != int64(len(data)) ||
		committed.File.ContentHash != fileDigest(data) || os.FileMode(committed.File.Mode).Perm() != 0750 {
		t.Fatal("wrong commit response", committed)
	}
	if err := os.WriteFile(path, []byte("external subsequent edit"), 0600); err != nil {
		t.Fatal(err)
	}
	repeated, err := d.uploadOp(api.Upload{Action: "commit", ID: upload.ID})
	if err != nil || *repeated.(api.UploadState).File != *committed.File {
		t.Fatal("repeated commit did not preserve its known result", repeated, err)
	}
}

func TestUploadRejectsIncorrectTemporarySize(t *testing.T) {
	d := testFileEngine(t, true)
	path := filepath.Join(t.TempDir(), "upload")
	upload := createTestUpload(t, d, path, "create", "", []byte("complete"))
	if err := d.uploads[upload.ID].f.Truncate(1); err != nil {
		t.Fatal(err)
	}
	_, err := d.uploadOp(api.Upload{Action: "commit", ID: upload.ID})
	requireFileCode(t, err, "SIZE_MISMATCH")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("size-mismatched upload committed", err)
	}
}
