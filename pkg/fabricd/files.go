package fabricd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

const (
	defaultFilePageSize = 200
	maxFilePageSize     = 1000
)

func filePageLimit(limit int) (int, error) {
	if limit == 0 {
		return defaultFilePageSize, nil
	}
	if limit < 1 || limit > maxFilePageSize {
		return 0, fmt.Errorf("limit must be 1..%d", maxFilePageSize)
	}
	return limit, nil
}

func (d *Engine) files(a api.File) (any, error) {
	return d.filesContext(d.ctx, a)
}

func (d *Engine) filesContext(ctx context.Context, a api.File) (any, error) {
	if a.Path == "" {
		return nil, fmt.Errorf("path required")
	}
	switch a.Action {
	case "stat":
		return statFile(a.Path, a.WithRevision)
	case "list_page":
		return listFilePage(ctx, a.Path, a.Cursor, a.Limit)
	case "search":
		if a.Search == nil {
			return nil, &api.Error{Code: "INVALID_ARGUMENT", Detail: "search options required"}
		}
		return d.search(ctx, a.Path, *a.Search)
	case "read":
		if a.Length <= 0 || a.Length > wire.ChunkSize || a.Offset < 0 {
			return nil, fmt.Errorf("read requires length 1..32768 and nonnegative offset")
		}
		return readFileChunk(a)
	case "write":
		if len(a.Data) > wire.ChunkSize {
			return nil, fmt.Errorf("use upload for files larger than 32768 bytes")
		}
		return d.writeFile(a)
	case "mkdir":
		d.fileMu.Lock()
		defer d.fileMu.Unlock()
		return nil, os.MkdirAll(a.Path, 0755)
	case "rename":
		if a.Destination == "" {
			return nil, fmt.Errorf("destination required")
		}
		if !a.Overwrite {
			return nil, fmt.Errorf("rename requires explicit overwrite=true (POSIX rename semantics)")
		}
		d.fileMu.Lock()
		defer d.fileMu.Unlock()
		return nil, os.Rename(a.Path, a.Destination)
	case "remove":
		d.fileMu.Lock()
		defer d.fileMu.Unlock()
		if a.Recursive {
			return nil, os.RemoveAll(a.Path)
		}
		return nil, os.Remove(a.Path)
	default:
		return nil, fmt.Errorf("unknown files action")
	}
}

type upload struct {
	cleaner                   interface{ Release(string) }
	mu                        sync.Mutex
	id, path, temp, hash, inc string
	intent, expectedRevision  string
	f                         *os.File
	size, offset              int64
	committed                 bool
	file                      *api.FileInfo
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
	return api.UploadState{ID: u.id, Incarnation: u.inc, Offset: u.offset, Size: u.size, ExpiresAt: u.expires.UTC().Format(time.RFC3339), Committed: u.committed, File: u.file}
}
func validHash(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}
func (d *Engine) uploadOp(a api.Upload) (any, error) {
	select {
	case d.bulk <- struct{}{}:
		defer func() { <-d.bulk }()
	default:
		return nil, &api.Error{Code: "RESOURCE_EXHAUSTED", Detail: "bulk concurrency limit"}
	}
	if a.Action == "create" {
		if err := validateWriteIntent(a.Intent, a.ExpectedRevision); err != nil {
			return nil, err
		}
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
		u := &upload{cleaner: d.cleaner, id: wire.ID(), path: a.Path, temp: f.Name(), f: f, size: a.Size, hash: a.SHA256, intent: a.Intent, expectedRevision: a.ExpectedRevision, inc: d.inc, expires: time.Now().Add(time.Duration(ttl) * time.Second)}
		d.uploads[u.id] = u
		return u.state(), nil
	}
	if a.Intent != "" || a.ExpectedRevision != "" {
		return nil, fmt.Errorf("upload intent and expected_revision are fixed at create")
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
		before, e := u.f.Stat()
		if e != nil {
			return nil, e
		}
		if before.Size() != u.size {
			return nil, &api.Error{Code: "SIZE_MISMATCH", Detail: "temporary upload size differs from declared size"}
		}
		temporaryInfo, e := descriptorFileInfo(u.f, u.temp, before, true)
		if e != nil {
			return nil, e
		}
		if temporaryInfo.ContentHash != u.hash {
			return nil, &api.Error{Code: "HASH_MISMATCH", Detail: "file checksum mismatch; cancel and create a new upload"}
		}
		if e := u.f.Sync(); e != nil {
			return nil, e
		}
		info, e := d.commitPreparedFile(u.f, u.path, u.intent, u.expectedRevision, u.hash)
		if e != nil {
			return nil, e
		}
		u.f.Close()
		u.f = nil
		u.committed = true
		u.file = &info
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
