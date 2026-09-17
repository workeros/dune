package fabricd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/gateway"
)

func searchEngine(t *testing.T) *Engine {
	t.Helper()
	binary := os.Getenv("DUNE_RG")
	if binary == "" {
		binary, _ = filepath.Abs("../../bin/rg")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatal("run make rg before search regressions:", err)
	}
	t.Setenv("DUNE_RG", binary)
	engine := newEngine(context.Background())
	t.Cleanup(engine.Close)
	return engine
}
func searchWrite(t *testing.T, root, name, value string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
func hasSearchIssue(result api.SearchResult, code string) bool {
	for _, issue := range result.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func TestSearchScopeAndModes(t *testing.T) {
	engine := searchEngine(t)
	root := t.TempDir()
	searchWrite(t, root, ".git/config", "needle")
	searchWrite(t, root, ".gitignore", "ignored.txt\n")
	searchWrite(t, root, "ignored.txt", "needle")
	searchWrite(t, root, ".hidden.txt", "needle")
	searchWrite(t, root, "nested/A.txt", "Needle needles needle\n")
	searchWrite(t, root, "excluded.txt", "needle")
	searchWrite(t, root, "line\nbreak.txt", "needle")
	outside := t.TempDir()
	searchWrite(t, outside, "escape.txt", "needle")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	result, err := engine.search(context.Background(), root, api.SearchOptions{Mode: "path", Query: ".TXT", Include: []string{"**/*.txt"}, Exclude: []string{"excluded.txt"}})
	if err != nil || !result.Complete || len(result.Matches) != 3 {
		t.Fatalf("scope: %+v, %v", result, err)
	}
	for _, item := range result.Matches {
		if strings.Contains(item.Path, "ignored") || strings.Contains(item.Path, "escape") {
			t.Fatal(item)
		}
	}
	hidden, ignore := false, false
	result, err = engine.search(context.Background(), root, api.SearchOptions{Mode: "path", Query: "ignored", UseIgnoreFiles: &ignore, IncludeHidden: &hidden})
	if err != nil || len(result.Matches) != 1 {
		t.Fatalf("ignore switch: %+v %v", result, err)
	}
	result, err = engine.search(context.Background(), root, api.SearchOptions{Mode: "content", Query: "needle", WholeWord: true, CaseSensitive: true, Include: []string{"nested/**"}})
	if err != nil || !result.Complete || len(result.Matches) != 1 || len(result.Matches[0].Ranges) != 1 || result.Matches[0].Ranges[0].Start != 15 {
		t.Fatalf("word/case: %+v %v", result, err)
	}
	result, err = engine.search(context.Background(), root, api.SearchOptions{Mode: "content", Query: "n[e]+dle", Regex: true, Include: []string{"nested/**"}})
	if err != nil || len(result.Matches) != 1 || len(result.Matches[0].Ranges) != 3 {
		t.Fatalf("regex: %+v %v", result, err)
	}
}

func TestSearchPreservesRootAlias(t *testing.T) {
	engine := searchEngine(t)
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	alias := filepath.Join(root, "workspace")
	searchWrite(t, actual, "note.txt", "needle\n")
	if err := os.Symlink(actual, alias); err != nil {
		t.Fatal(err)
	}
	page, err := listFilePage(context.Background(), alias, "", 10)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("list: %+v %v", page, err)
	}
	for _, mode := range []string{"path", "content"} {
		t.Run(mode, func(t *testing.T) {
			query := "needle"
			if mode == "path" {
				query = "note"
			}
			result, err := engine.search(context.Background(), alias, api.SearchOptions{Mode: mode, Query: query})
			if err != nil || !result.Complete || len(result.Matches) != 1 {
				t.Fatalf("search: %+v %v", result, err)
			}
			path := result.Matches[0].Path
			if path != page.Items[0].Path {
				t.Fatalf("search path %q differs from directory entry %q", path, page.Items[0].Path)
			}
			chunk, err := readFileChunk(api.File{Path: path, Length: 32, WithRevision: true})
			if err != nil || string(chunk.Data) != "needle\n" || chunk.Info.Path != path {
				t.Fatalf("open search result: %+v %v", chunk, err)
			}
		})
	}
}

func TestSearchUTF8AndBudgets(t *testing.T) {
	engine := searchEngine(t)
	root := t.TempDir()
	searchWrite(t, root, "unicode.txt", "\ufeff中文🙂 needle\nnext\n")
	result, err := engine.search(context.Background(), root, api.SearchOptions{Mode: "content", Query: "🙂", ContextLines: 1})
	if err != nil || !result.Complete || len(result.Matches) != 1 {
		t.Fatalf("unicode: %+v %v", result, err)
	}
	item := result.Matches[0]
	if item.Line != 1 || item.Ranges[0].Start != 9 || item.Ranges[0].End != 13 || len(item.After) != 1 || item.After[0] != "next" {
		t.Fatalf("byte ranges/BOM: %+v", item)
	}
	searchWrite(t, root, "invalid.txt", "needle\n"+strings.Repeat("x", 100000)+"\xff")
	searchWrite(t, root, "long.txt", "needle"+strings.Repeat("x", maxSearchRecord))
	searchWrite(t, root, "huge.txt", strings.Repeat("x", maxSearchFileBytes+1))
	result, err = engine.search(context.Background(), root, api.SearchOptions{Mode: "content", Query: "needle"})
	if err != nil || result.Complete || !hasSearchIssue(result, "content_encoding") || !hasSearchIssue(result, "file_size_limit") || !hasSearchIssue(result, "line_size_limit") {
		t.Fatalf("partial: %+v %v", result, err)
	}
	for _, item := range result.Matches {
		if filepath.Base(item.Path) != "unicode.txt" {
			t.Fatalf("unverified result: %+v", item)
		}
	}
	result, err = engine.search(context.Background(), root, api.SearchOptions{Mode: "path", Query: "huge"})
	if err != nil || !result.Complete || len(result.Matches) != 1 {
		t.Fatalf("large path: %+v %v", result, err)
	}
	result, err = engine.search(context.Background(), root, api.SearchOptions{Mode: "path", Query: ".txt", Limit: 1})
	if err != nil || len(result.Matches) != 1 || !hasSearchIssue(result, "result_limit") {
		t.Fatalf("result limit: %+v %v", result, err)
	}
}

func TestSearchEmptyInvalidAndCanceled(t *testing.T) {
	engine := searchEngine(t)
	root := t.TempDir()
	result, err := engine.search(context.Background(), root, api.SearchOptions{Mode: "content", Query: "nothing"})
	if err != nil || !result.Complete || len(result.Matches) != 0 {
		t.Fatalf("no match: %+v %v", result, err)
	}
	_, err = engine.search(context.Background(), root, api.SearchOptions{Mode: "content", Query: "[", Regex: true})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "INVALID_ARGUMENT" {
		t.Fatalf("invalid regex in empty root: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "rg")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DUNE_RG", binary)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = engine.search(ctx, root, api.SearchOptions{Mode: "path", Query: "a"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("cancellation: %v after %s", err, time.Since(start))
	}
	engine.searchSlots <- struct{}{}
	engine.searchSlots <- struct{}{}
	_, err = engine.search(context.Background(), root, api.SearchOptions{Mode: "path", Query: "a"})
	if !errors.As(err, &apiErr) || apiErr.Code != "RESOURCE_EXHAUSTED" {
		t.Fatalf("concurrency: %v", err)
	}
	<-engine.searchSlots
	<-engine.searchSlots
}

func TestSearchSerializedBudgetAndInvalidBoundaries(t *testing.T) {
	q := fileSearch{ctx: context.Background(), options: api.SearchOptions{Mode: "path", Limit: 1000}, result: api.SearchResult{Matches: []api.SearchMatch{}, Complete: true}}
	for i := 0; i < 1000; i++ {
		q.add(api.SearchMatch{Path: strings.Repeat("\n", 3000)})
	}
	for i := 0; i < maxSearchIssues; i++ {
		q.issue("read_error", strings.Repeat("\t", 4096))
	}
	data, _ := json.Marshal(q.result)
	if len(data) > maxSearchBytes || !hasSearchIssue(q.result, "byte_limit") {
		t.Fatalf("encoded size=%d", len(data))
	}
	root := t.TempDir()
	searchWrite(t, root, "utf8", "🙂\n")
	line := "🙂\n"
	q = fileSearch{ctx: context.Background(), options: api.SearchOptions{Mode: "content", Limit: 200}, result: api.SearchResult{Complete: true}}
	q.verifyMatches(filepath.Join(root, "utf8"), []rgMatch{{Lines: rgText{Text: &line}, LineNumber: 1, Submatches: []api.SearchRange{{Start: 1, End: 2}}}})
	if len(q.result.Matches) > 0 || !hasSearchIssue(q.result, "invalid_search_output") {
		t.Fatalf("partial codepoint: %+v", q.result)
	}
}

func TestSearchCancellationTraversesGatewayAndRetainsNoLargeCache(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	engine := searchEngine(t)
	g := gateway.New()
	var wg sync.WaitGroup
	defer func() { cancel(); g.Close(); engine.Close(); wg.Wait() }()
	left, right := net.Pipe()
	binding, handler, err := (access.Grant{Target: "search-machine", Role: gateway.RoleDaemon}).Bind()
	if err != nil {
		t.Fatal(err)
	}
	wg.Go(func() { _ = g.ServeConn(ctx, left, binding, handler) })
	wg.Go(func() { _ = engine.ServeConn(ctx, right, "search-machine") })
	for !g.Online("search-machine") {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	left, right = net.Pipe()
	binding, handler, err = (access.Grant{Target: "search-machine", Role: gateway.RoleSDK}).Bind()
	if err != nil {
		t.Fatal(err)
	}
	wg.Go(func() { _ = g.ServeConn(ctx, left, binding, handler) })
	c, err := client.Connect(ctx, right, "search-machine")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	root := t.TempDir()
	searchWrite(t, root, "found.txt", "needle")
	var result api.SearchResult
	if err = c.Files(ctx, api.File{Action: "search", Path: root, Search: &api.SearchOptions{Mode: "path", Query: "found"}}, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 {
		t.Fatal(result)
	}
	engine.mu.Lock()
	for _, cached := range engine.cache {
		if cached.result == nil || cached.result.Code != "RESULT_UNKNOWN" || len(cached.result.Payload) > 0 {
			t.Error("large search response retained", cached)
		}
	}
	engine.mu.Unlock()
	binary := filepath.Join(t.TempDir(), "rg")
	if err = os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DUNE_RG", binary)
	queryCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		var out api.SearchResult
		done <- c.Files(queryCtx, api.File{Action: "search", Path: root, Search: &api.SearchOptions{Mode: "path", Query: "found"}}, &out)
	}()
	for len(engine.searchSlots) == 0 {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled search succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("SDK did not cancel")
	}
	until := time.Now().Add(time.Second)
	for len(engine.searchSlots) != 0 {
		if time.Now().After(until) {
			t.Fatal("Gateway cancellation did not terminate/reap search")
		}
		time.Sleep(time.Millisecond)
	}
}
