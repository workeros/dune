package fabricd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aiomni/dune/pkg/api"
)

func fileInfo(path string, info os.FileInfo) api.FileInfo {
	return api.FileInfo{
		Name: filepath.Base(path), Path: path, Size: info.Size(), Mode: uint32(info.Mode()),
		IsDir: info.IsDir(), ModifiedAt: info.ModTime().UTC().Format(time.RFC3339Nano),
	}
}

func filePermissions(mode os.FileMode) os.FileMode {
	return mode.Perm() | mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
}

func contentRevision(digest string, mode os.FileMode) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%d", digest, filePermissions(mode))
	return hex.EncodeToString(h.Sum(nil))
}

func withFileRevision(info api.FileInfo, digest string) api.FileInfo {
	info.ContentHash = digest
	info.Revision = contentRevision(digest, os.FileMode(info.Mode))
	return info
}

func changedFile(detail string) error {
	return &api.Error{Code: "FILE_CHANGED", Detail: detail}
}

// A descriptor binds metadata and bytes to one object. The before/after checks
// detect observable in-place writes, but do not make external writers atomic.
func descriptorFileInfo(f *os.File, path string, before os.FileInfo, withRevision bool) (api.FileInfo, error) {
	var digest string
	if withRevision && before.Mode().IsRegular() {
		h := sha256.New()
		read, err := io.CopyBuffer(h, io.NewSectionReader(f, 0, before.Size()+1), make([]byte, 128*1024))
		if err != nil {
			return api.FileInfo{}, err
		}
		if read != before.Size() {
			return api.FileInfo{}, changedFile("file size changed while reading")
		}
		digest = hex.EncodeToString(h.Sum(nil))
	}
	after, err := f.Stat()
	if err != nil {
		return api.FileInfo{}, err
	}
	if before.Size() != after.Size() || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		return api.FileInfo{}, changedFile("file changed while reading")
	}
	info := fileInfo(path, after)
	if digest != "" {
		info = withFileRevision(info, digest)
	}
	return info, nil
}

func statFile(path string, withRevision bool) (api.FileInfo, error) {
	if !withRevision {
		info, err := os.Stat(path)
		if err != nil {
			return api.FileInfo{}, err
		}
		return fileInfo(path, info), nil
	}
	// Nonblocking open avoids hanging on a FIFO before its type can be checked.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return api.FileInfo{}, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return api.FileInfo{}, err
	}
	if !before.Mode().IsRegular() && !before.IsDir() {
		return api.FileInfo{}, &api.Error{Code: "UNSUPPORTED", Detail: "content revision requires a regular file"}
	}
	return descriptorFileInfo(f, path, before, true)
}

func readFileChunk(a api.File) (api.FileChunk, error) {
	f, err := os.OpenFile(a.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return api.FileChunk{}, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return api.FileChunk{}, err
	}
	if !before.Mode().IsRegular() {
		return api.FileChunk{}, &api.Error{Code: "UNSUPPORTED", Detail: "read requires a regular file"}
	}
	b := make([]byte, a.Length)
	n, err := f.ReadAt(b, a.Offset)
	if err != nil && err != io.EOF {
		return api.FileChunk{}, err
	}
	info, err := descriptorFileInfo(f, a.Path, before, a.WithRevision)
	if err != nil {
		return api.FileChunk{}, err
	}
	next := a.Offset + int64(n)
	return api.FileChunk{Data: b[:n], Offset: next, EOF: next >= info.Size, Info: info}, nil
}
