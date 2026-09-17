package fabricd

import (
	"container/heap"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aiomni/dune/pkg/api"
)

const (
	directoryReadBatchSize = 256
	directoryScanTimeout   = 5 * time.Second
)

type directoryCursor struct {
	Directory string `json:"directory"`
	After     string `json:"after"`
}

func decodeDirectoryCursor(encoded, directory string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	if len(encoded) > 16*1024 {
		return "", fmt.Errorf("invalid directory cursor")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	var cursor directoryCursor
	if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Directory != directory || cursor.After == "" ||
		cursor.After == "." || cursor.After == ".." || strings.ContainsAny(cursor.After, "/\x00") || !utf8.ValidString(cursor.After) {
		return "", fmt.Errorf("cursor does not belong to this directory")
	}
	return cursor.After, nil
}

// A max-heap keeps only the smallest pageSize+1 names after the cursor. Pages
// rescan the directory, trading scan work for bounded memory and no saved state.
type directoryNames []string

func (h directoryNames) Len() int           { return len(h) }
func (h directoryNames) Less(i, j int) bool { return h[i] > h[j] }
func (h directoryNames) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *directoryNames) Push(value any)    { *h = append(*h, value.(string)) }
func (h *directoryNames) Pop() any {
	last := len(*h) - 1
	value := (*h)[last]
	*h = (*h)[:last]
	return value
}

func listFilePage(parent context.Context, path, cursor string, limit int) (api.FilePage, error) {
	pageSize, err := filePageLimit(limit)
	if err != nil {
		return api.FilePage{}, err
	}
	directory, err := filepath.Abs(path)
	if err != nil {
		return api.FilePage{}, err
	}
	after, err := decodeDirectoryCursor(cursor, directory)
	if err != nil {
		return api.FilePage{}, err
	}
	ctx, cancel := context.WithTimeout(parent, directoryScanTimeout)
	defer cancel()
	f, err := os.Open(directory)
	if err != nil {
		return api.FilePage{}, err
	}
	defer f.Close()
	names := make(directoryNames, 0, pageSize+1)
	for {
		if err := ctx.Err(); err != nil {
			return api.FilePage{}, &api.Error{Code: "DIRECTORY_SCAN_INCOMPLETE", Detail: "directory scan did not finish within its budget"}
		}
		batch, err := f.Readdirnames(directoryReadBatchSize)
		if err != nil && err != io.EOF {
			return api.FilePage{}, err
		}
		for _, name := range batch {
			if !utf8.ValidString(name) {
				return api.FilePage{}, &api.Error{Code: "UNSUPPORTED", Detail: "directory contains a filename that cannot be represented as UTF-8"}
			}
			if name <= after {
				continue
			}
			if len(names) < pageSize+1 {
				heap.Push(&names, name)
			} else if name < names[0] {
				names[0] = name
				heap.Fix(&names, 0)
			}
		}
		if err == io.EOF {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return api.FilePage{}, &api.Error{Code: "DIRECTORY_SCAN_INCOMPLETE", Detail: "directory scan did not finish within its budget"}
	}
	sort.Strings(names)
	page := api.FilePage{Items: make([]api.FileInfo, 0, pageSize)}
	if len(names) > pageSize {
		names = names[:pageSize]
		encoded, _ := json.Marshal(directoryCursor{Directory: directory, After: names[len(names)-1]})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	for _, name := range names {
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if err != nil {
			return api.FilePage{}, err
		}
		page.Items = append(page.Items, fileInfo(path, info))
	}
	return page, nil
}
