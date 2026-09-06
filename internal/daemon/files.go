package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func fileInfo(i os.FileInfo) api.FileInfo {
	return api.FileInfo{Name: i.Name(), Size: i.Size(), Mode: uint32(i.Mode()), IsDir: i.IsDir()}
}
func (d *Daemon) files(a api.File) (any, error) {
	if a.Path == "" {
		return nil, fmt.Errorf("path required")
	}
	switch a.Action {
	case "stat":
		i, e := os.Stat(a.Path)
		if e != nil {
			return nil, e
		}
		return fileInfo(i), nil
	case "list":
		f, e := os.Open(a.Path)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		infos, e := f.Readdir(4097)
		if e != nil && e != io.EOF {
			return nil, e
		}
		if len(infos) > 4096 {
			return nil, fmt.Errorf("directory exceeds 4096 entries")
		}
		out := []api.FileInfo{}
		for _, i := range infos {
			out = append(out, fileInfo(i))
		}
		return out, nil
	case "read":
		if a.Length <= 0 || a.Length > wire.ChunkSize || a.Offset < 0 {
			return nil, fmt.Errorf("read requires length 1..32768 and nonnegative offset")
		}
		f, e := os.Open(a.Path)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		b := make([]byte, a.Length)
		n, e := f.ReadAt(b, a.Offset)
		if e == io.EOF {
			e = nil
		}
		return map[string]any{"data": b[:n], "offset": a.Offset + int64(n)}, e
	case "write":
		if len(a.Data) > wire.ChunkSize {
			return nil, fmt.Errorf("use upload for files larger than 32768 bytes")
		}
		return nil, atomicWrite(a.Path, a.Data, a.Overwrite)
	case "mkdir":
		return nil, os.MkdirAll(a.Path, 0755)
	case "rename":
		if a.Destination == "" {
			return nil, fmt.Errorf("destination required")
		}
		if !a.Overwrite {
			return nil, fmt.Errorf("rename requires explicit overwrite=true (POSIX rename semantics)")
		}
		return nil, os.Rename(a.Path, a.Destination)
	case "remove":
		if a.Recursive {
			return nil, os.RemoveAll(a.Path)
		}
		return nil, os.Remove(a.Path)
	default:
		return nil, fmt.Errorf("unknown files action")
	}
}
func commitFile(temp, path string, overwrite bool) error {
	if overwrite {
		return os.Rename(temp, path)
	}
	if e := os.Link(temp, path); e != nil {
		return e
	}
	return os.Remove(temp)
}
func atomicWrite(path string, b []byte, overwrite bool) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".dune-write-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return commitFile(f.Name(), path, overwrite)
}

type upload struct {
	cleaner                   interface{ Release(string) }
	mu                        sync.Mutex
	id, path, temp, hash, inc string
	f                         *os.File
	size, offset              int64
	overwrite, committed      bool
	expires                   time.Time
}

func (u *upload) cleanupLocked() {
	if u.f != nil {
		u.f.Close()
		u.f = nil
	}
	if u.temp != "" {
		os.Remove(u.temp)
		if u.cleaner != nil {
			u.cleaner.Release(u.temp)
		}
		u.temp = ""
	}
}
func (u *upload) cleanup() { u.mu.Lock(); defer u.mu.Unlock(); u.cleanupLocked() }
func (u *upload) state() api.UploadState {
	return api.UploadState{ID: u.id, Incarnation: u.inc, Offset: u.offset, Size: u.size, ExpiresAt: u.expires.UTC().Format(time.RFC3339), Committed: u.committed}
}
func validHash(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}
func (d *Daemon) uploadOp(a api.Upload) (any, error) {
	select {
	case d.bulk <- struct{}{}:
		defer func() { <-d.bulk }()
	default:
		return nil, &api.Error{Code: "RESOURCE_EXHAUSTED", Detail: "bulk concurrency limit"}
	}
	if a.Action == "create" {
		if a.Path == "" || a.Size < 0 || a.Size > 10*1024*1024*1024 || !validHash(a.SHA256) {
			return nil, fmt.Errorf("path, size 0..10GiB and lowercase SHA256 required")
		}
		ttl := a.TTLSeconds
		if ttl == 0 {
			ttl = 1800
		}
		if ttl < 1 || ttl > 86400 {
			return nil, fmt.Errorf("TTL must be 1..86400 seconds")
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if len(d.uploads) >= 64 {
			return nil, fmt.Errorf("upload limit")
		}
		temp, e := d.cleaner.Create(filepath.Dir(a.Path), ".dune-upload-"+d.inc+"-*")
		if e != nil {
			return nil, e
		}
		f, e := os.OpenFile(temp, os.O_RDWR, 0600)
		if e != nil {
			os.Remove(temp)
			d.cleaner.Release(temp)
			return nil, e
		}
		u := &upload{cleaner: d.cleaner, id: wire.ID(), path: a.Path, temp: f.Name(), f: f, size: a.Size, hash: a.SHA256, overwrite: a.Overwrite, inc: d.inc, expires: time.Now().Add(time.Duration(ttl) * time.Second)}
		d.uploads[u.id] = u
		return u.state(), nil
	}
	d.mu.Lock()
	u := d.uploads[a.ID]
	d.mu.Unlock()
	if u == nil {
		return nil, &api.Error{Code: "STALE_UPLOAD", Detail: "unknown upload handle"}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if time.Now().After(u.expires) {
		u.cleanupLocked()
		return nil, &api.Error{Code: "STALE_UPLOAD", Detail: "upload expired"}
	}
	if a.Action == "query" {
		return u.state(), nil
	}
	if u.committed {
		if a.Action == "commit" {
			return u.state(), nil
		}
		return nil, fmt.Errorf("already committed")
	}
	if u.f == nil {
		return nil, &api.Error{Code: "STALE_UPLOAD", Detail: "upload cancelled"}
	}
	switch a.Action {
	case "chunk":
		if len(a.Data) == 0 || len(a.Data) > wire.ChunkSize || a.Offset < 0 || a.Offset+int64(len(a.Data)) > u.size {
			return nil, fmt.Errorf("invalid chunk bounds")
		}
		if a.ChunkSHA256 != "" {
			h := sha256.Sum256(a.Data)
			if hex.EncodeToString(h[:]) != a.ChunkSHA256 {
				return nil, &api.Error{Code: "HASH_MISMATCH", Detail: "chunk checksum mismatch"}
			}
		}
		if a.Offset != u.offset {
			return nil, &api.Error{Code: "OFFSET_CONFLICT", Detail: fmt.Sprintf("accepted offset %d", u.offset)}
		}
		n, e := u.f.WriteAt(a.Data, u.offset)
		u.offset += int64(n)
		if e != nil {
			return nil, e
		}
		return u.state(), nil
	case "commit":
		if u.offset != u.size {
			return nil, fmt.Errorf("upload incomplete")
		}
		if _, e := u.f.Seek(0, 0); e != nil {
			return nil, e
		}
		h := sha256.New()
		if _, e := io.CopyBuffer(h, u.f, make([]byte, wire.ChunkSize)); e != nil {
			return nil, e
		}
		if hex.EncodeToString(h.Sum(nil)) != u.hash {
			return nil, &api.Error{Code: "HASH_MISMATCH", Detail: "file checksum mismatch; cancel and create a new upload"}
		}
		if e := u.f.Sync(); e != nil {
			return nil, e
		}
		if e := commitFile(u.temp, u.path, u.overwrite); e != nil {
			return nil, e
		}
		u.f.Close()
		u.f = nil
		u.committed = true
		u.cleaner.Release(u.temp)
		u.temp = ""
		return u.state(), nil
	case "cancel":
		u.cleanupLocked()
		u.expires = time.Now()
		return u.state(), nil
	default:
		return nil, fmt.Errorf("unknown upload action")
	}
}
