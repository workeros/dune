package fabricd

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

func contentHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 128*1024)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileInfo(path string, info os.FileInfo, hashContents bool) (api.FileInfo, error) {
	result := api.FileInfo{
		Name:       info.Name(),
		Path:       path,
		Size:       info.Size(),
		Mode:       uint32(info.Mode()),
		IsDir:      info.IsDir(),
		ModifiedAt: info.ModTime().UTC().Format(time.RFC3339Nano),
	}
	if hashContents && info.Mode().IsRegular() {
		hash, err := contentHash(path)
		if err != nil {
			return api.FileInfo{}, err
		}
		result.ContentHash = hash
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d\x00%d\x00%d", result.Mode, result.Size, info.ModTime().UnixNano())
	result.Revision = hex.EncodeToString(h.Sum(nil))
	return result, nil
}

func detailedFileInfo(path string) (api.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return api.FileInfo{}, err
	}
	return fileInfo(path, info, true)
}

func filePageLimit(limit int) (int, error) {
	if limit == 0 {
		return defaultFilePageSize, nil
	}
	if limit < 1 || limit > maxFilePageSize {
		return 0, fmt.Errorf("limit must be 1..%d", maxFilePageSize)
	}
	return limit, nil
}

func encodeFileCursor(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeFileCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", fmt.Errorf("invalid cursor")
	}
	return string(decoded), nil
}

func listFilePage(path, cursor string, limit int) (api.FilePage, error) {
	pageSize, err := filePageLimit(limit)
	if err != nil {
		return api.FilePage{}, err
	}
	after, err := decodeFileCursor(cursor)
	if err != nil {
		return api.FilePage{}, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return api.FilePage{}, err
	}
	page := api.FilePage{Items: []api.FileInfo{}}
	for _, entry := range entries {
		if entry.Name() <= after {
			continue
		}
		if len(page.Items) == pageSize {
			page.NextCursor = encodeFileCursor(page.Items[len(page.Items)-1].Name)
			break
		}
		info, err := entry.Info()
		if err != nil {
			return api.FilePage{}, err
		}
		item, err := fileInfo(filepath.Join(path, entry.Name()), info, false)
		if err != nil {
			return api.FilePage{}, err
		}
		page.Items = append(page.Items, item)
	}
	return page, nil
}

func searchFiles(root, query, cursor string, limit int) (api.FilePage, error) {
	if strings.TrimSpace(query) == "" {
		return api.FilePage{}, fmt.Errorf("query required")
	}
	pageSize, err := filePageLimit(limit)
	if err != nil {
		return api.FilePage{}, err
	}
	after, err := decodeFileCursor(cursor)
	if err != nil {
		return api.FilePage{}, err
	}
	query = strings.ToLower(query)
	type match struct {
		relative string
		item     api.FileInfo
	}
	matches := []match{}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !strings.Contains(strings.ToLower(relative), query) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		item, err := fileInfo(path, info, false)
		if err != nil {
			return err
		}
		matches = append(matches, match{relative: relative, item: item})
		return nil
	})
	if err != nil {
		return api.FilePage{}, err
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].relative < matches[j].relative })
	page := api.FilePage{Items: []api.FileInfo{}}
	for _, match := range matches {
		if match.relative <= after {
			continue
		}
		if len(page.Items) == pageSize {
			last, err := filepath.Rel(root, page.Items[len(page.Items)-1].Path)
			if err != nil {
				return api.FilePage{}, err
			}
			page.NextCursor = encodeFileCursor(last)
			break
		}
		page.Items = append(page.Items, match.item)
	}
	return page, nil
}

func fileChanged(path, expected string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &api.Error{Code: "FILE_CHANGED", Detail: "file no longer exists"}
		}
		return err
	}
	current, err := fileInfo(path, info, false)
	if err != nil {
		return err
	}
	if current.Revision != expected {
		return &api.Error{Code: "FILE_CHANGED", Detail: "file changed since it was opened"}
	}
	return nil
}

func (d *Engine) files(a api.File) (any, error) {
	if a.Path == "" {
		return nil, fmt.Errorf("path required")
	}
	switch a.Action {
	case "stat":
		d.fileMu.Lock()
		defer d.fileMu.Unlock()
		return detailedFileInfo(a.Path)
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
			item, e := fileInfo(filepath.Join(a.Path, i.Name()), i, false)
			if e != nil {
				return nil, e
			}
			out = append(out, item)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil
	case "list_page":
		return listFilePage(a.Path, a.Cursor, a.Limit)
	case "search":
		return searchFiles(a.Path, a.Query, a.Cursor, a.Limit)
	case "read":
		if a.Length <= 0 || a.Length > wire.ChunkSize || a.Offset < 0 {
			return nil, fmt.Errorf("read requires length 1..32768 and nonnegative offset")
		}
		d.fileMu.Lock()
		defer d.fileMu.Unlock()
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
		if e != nil {
			return nil, e
		}
		rawInfo, e := f.Stat()
		if e != nil {
			return nil, e
		}
		info, e := fileInfo(a.Path, rawInfo, a.Offset == 0)
		if e != nil {
			return nil, e
		}
		next := a.Offset + int64(n)
		return api.FileChunk{Data: b[:n], Offset: next, EOF: next >= info.Size, Info: info}, nil
	case "write":
		if len(a.Data) > wire.ChunkSize {
			return nil, fmt.Errorf("use upload for files larger than 32768 bytes")
		}
		d.fileMu.Lock()
		defer d.fileMu.Unlock()
		if !a.Force && a.ExpectedRevision != "" {
			if err := fileChanged(a.Path, a.ExpectedRevision); err != nil {
				return nil, err
			}
		}
		if err := atomicWrite(a.Path, a.Data, a.Overwrite); err != nil {
			return nil, err
		}
		return detailedFileInfo(a.Path)
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
	expectedRevision          string
	f                         *os.File
	size, offset              int64
	overwrite, force          bool
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
		u := &upload{cleaner: d.cleaner, id: wire.ID(), path: a.Path, temp: f.Name(), f: f, size: a.Size, hash: a.SHA256, expectedRevision: a.ExpectedRevision, overwrite: a.Overwrite, force: a.Force, inc: d.inc, expires: time.Now().Add(time.Duration(ttl) * time.Second)}
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
		d.fileMu.Lock()
		defer d.fileMu.Unlock()
		if !u.force && u.expectedRevision != "" {
			if e := fileChanged(u.path, u.expectedRevision); e != nil {
				return nil, e
			}
		}
		rawInfo, e := u.f.Stat()
		if e != nil {
			return nil, e
		}
		info, e := fileInfo(u.temp, rawInfo, false)
		if e != nil {
			return nil, e
		}
		info.Name = filepath.Base(u.path)
		info.Path = u.path
		info.ContentHash = u.hash
		if e := commitFile(u.temp, u.path, u.overwrite); e != nil {
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
