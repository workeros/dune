package fabricd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/aiomni/dune/pkg/api"
)

const (
	maxSearchBytes     = 1 << 20
	maxSearchFileBytes = 16 << 20
	maxSearchRecord    = 256 << 10
	maxSearchIssues    = 16
	searchBatchSize    = 256
	searchBatchBytes   = 64 << 10
)

type fileSearch struct {
	ctx          context.Context
	root, binary string
	options      api.SearchOptions
	result       api.SearchResult
	count, bytes int
	stopped      bool
}

func validateSearch(options api.SearchOptions) (api.SearchOptions, error) {
	if options.Mode != "path" && options.Mode != "content" {
		return options, fmt.Errorf("search mode must be path or content")
	}
	if strings.TrimSpace(options.Query) == "" || len(options.Query) > 8192 || strings.IndexByte(options.Query, 0) >= 0 || !utf8.ValidString(options.Query) {
		return options, fmt.Errorf("search query requires 1..8192 UTF-8 bytes without NUL")
	}
	if strings.ContainsAny(options.Query, "\r\n") {
		return options, fmt.Errorf("multiline search is not supported")
	}
	if options.Mode == "path" && (options.Regex || options.WholeWord || options.ContextLines != 0) {
		return options, fmt.Errorf("regex, whole_word and context_lines require content mode")
	}
	var err error
	options.Limit, err = filePageLimit(options.Limit)
	if err != nil {
		return options, err
	}
	if options.TimeoutSeconds == 0 {
		options.TimeoutSeconds = 5
	}
	if options.TimeoutSeconds < 1 || options.TimeoutSeconds > 10 || options.ContextLines < 0 || options.ContextLines > 10 {
		return options, fmt.Errorf("search timeout must be 1..10 seconds and context_lines 0..10")
	}
	if len(options.Include)+len(options.Exclude) > 64 {
		return options, fmt.Errorf("at most 64 search globs are allowed")
	}
	for _, pattern := range append(slices.Clone(options.Include), options.Exclude...) {
		if pattern == "" || len(pattern) > 4096 || filepath.IsAbs(pattern) || strings.IndexByte(pattern, 0) >= 0 || !utf8.ValidString(pattern) {
			return options, fmt.Errorf("search globs must be relative UTF-8 paths")
		}
		for _, component := range strings.Split(pattern, "/") {
			if component == ".." {
				return options, fmt.Errorf("search globs cannot leave the root")
			}
		}
		if !doublestar.ValidatePattern(pattern) {
			return options, fmt.Errorf("invalid search glob: %s", pattern)
		}
	}
	return options, nil
}

func searchBinary() (string, error) {
	binary := os.Getenv("DUNE_RG")
	if binary == "" {
		executable, err := os.Executable()
		if err != nil {
			return "", err
		}
		binary = filepath.Join(filepath.Dir(executable), "rg")
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", &api.Error{Code: "DEPENDENCY_MISSING", Detail: "bundled ripgrep missing; reinstall Dune or set DUNE_RG"}
	}
	return filepath.Abs(path)
}

func (d *Engine) search(ctx context.Context, root string, options api.SearchOptions) (api.SearchResult, error) {
	options, err := validateSearch(options)
	if err != nil {
		return api.SearchResult{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: err.Error()}
	}
	if !filepath.IsAbs(root) || len(root) > 4096 || !utf8.ValidString(root) {
		return api.SearchResult{}, fmt.Errorf("search root must be an absolute UTF-8 directory")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return api.SearchResult{}, err
	}
	directory, err := os.Open(root)
	if err != nil {
		return api.SearchResult{}, err
	}
	info, statErr := directory.Stat()
	_, readErr := directory.Readdirnames(1)
	directory.Close()
	if statErr != nil {
		return api.SearchResult{}, statErr
	}
	if !info.IsDir() {
		return api.SearchResult{}, fmt.Errorf("search root must be a directory")
	}
	if readErr != nil && readErr != io.EOF {
		return api.SearchResult{}, readErr
	}
	binary, err := searchBinary()
	if err != nil {
		return api.SearchResult{}, err
	}
	select {
	case d.searchSlots <- struct{}{}:
		defer func() { <-d.searchSlots }()
	case <-ctx.Done():
		return api.SearchResult{}, ctx.Err()
	default:
		return api.SearchResult{}, &api.Error{Code: "RESOURCE_EXHAUSTED", Detail: "search concurrency limit"}
	}
	queryCtx, cancel := context.WithTimeout(ctx, time.Duration(options.TimeoutSeconds)*time.Second)
	defer cancel()
	query := &fileSearch{ctx: queryCtx, root: root, binary: binary, options: options,
		result: api.SearchResult{Matches: []api.SearchMatch{}, Complete: true}}
	err = query.run()
	if ctx.Err() != nil {
		return api.SearchResult{}, ctx.Err()
	}
	if errors.Is(queryCtx.Err(), context.DeadlineExceeded) {
		query.issue("timeout", "")
	}
	return query.result, err
}

func (q *fileSearch) issue(code, path string) {
	q.result.Complete = false
	item := api.SearchIssue{Code: code, Path: path}
	if encoded, _ := json.Marshal(item); len(encoded) > 4096 {
		item.Path = ""
	}
	if slices.Contains(q.result.Issues, item) {
		return
	}
	if len(q.result.Issues) < maxSearchIssues {
		q.result.Issues = append(q.result.Issues, item)
	} else {
		q.result.Issues[maxSearchIssues-1] = api.SearchIssue{Code: "more_issues"}
	}
}

func (q *fileSearch) add(match api.SearchMatch) {
	count := 1
	if q.options.Mode == "content" {
		count = len(match.Ranges)
		if count > q.options.Limit-q.count {
			match.Ranges = match.Ranges[:q.options.Limit-q.count]
			count = len(match.Ranges)
		}
	}
	if count == 0 || q.stopped {
		return
	}
	encoded, _ := json.Marshal(match)
	// Reserve enough space for bounded issue paths and the response envelope.
	if q.bytes+len(encoded)+1 > maxSearchBytes-(maxSearchIssues*4096+4096) {
		q.issue("byte_limit", "")
		q.stopped = true
		return
	}
	q.result.Matches = append(q.result.Matches, match)
	q.bytes += len(encoded) + 1
	q.count += count
	if q.count == q.options.Limit {
		q.issue("result_limit", "")
		q.stopped = true
	}
}

func (q *fileSearch) include(path string) bool {
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == ".git" {
			return false
		}
	}
	for _, pattern := range q.options.Exclude {
		if matches, _ := doublestar.Match(pattern, filepath.ToSlash(path)); matches {
			return false
		}
	}
	if len(q.options.Include) == 0 {
		return true
	}
	for _, pattern := range q.options.Include {
		if matches, _ := doublestar.Match(pattern, filepath.ToSlash(path)); matches {
			return true
		}
	}
	return false
}

type searchStderr struct{ data []byte }

func (b *searchStderr) Write(p []byte) (int, error) {
	if remaining := 8192 - len(b.data); remaining > 0 {
		b.data = append(b.data, p[:min(remaining, len(p))]...)
	}
	return len(p), nil
}

func scanNUL(data []byte, atEOF bool) (int, []byte, error) {
	if end := bytes.IndexByte(data, 0); end >= 0 {
		return end + 1, data[:end], nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, fmt.Errorf("unterminated ripgrep path")
	}
	return 0, nil, nil
}

func (q *fileSearch) run() error {
	if q.options.Regex {
		command := exec.CommandContext(q.ctx, q.binary, append(q.contentArgs(), "-")...)
		command.Stdin = strings.NewReader("")
		var stderr searchStderr
		command.Stderr = &stderr
		if err := command.Run(); err != nil && q.ctx.Err() == nil && !exitCode(err, 1) {
			return &api.Error{Code: "INVALID_ARGUMENT", Detail: "invalid ripgrep regular expression"}
		}
	}
	args := []string{"--no-config", "--files", "--null", "--threads", "2"}
	if q.options.IncludeHidden == nil || *q.options.IncludeHidden {
		args = append(args, "--hidden")
	}
	if q.options.UseIgnoreFiles != nil && !*q.options.UseIgnoreFiles {
		args = append(args, "--no-ignore")
	}
	args = append(args, "--", ".")
	ctx, cancel := context.WithCancel(q.ctx)
	defer cancel()
	command := exec.CommandContext(ctx, q.binary, args...)
	command.Dir = q.root
	var stderr searchStderr
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err = command.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Split(scanNUL)
	scanner.Buffer(make([]byte, 4096), 8192)
	batch := make([]string, 0, searchBatchSize)
	batchBytes := 0
	var queryErr error
	for scanner.Scan() {
		path := filepath.Clean(scanner.Text())
		if !utf8.ValidString(path) {
			q.issue("path_encoding", "")
			continue
		}
		if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) || len(filepath.Join(q.root, path)) > 4096 {
			q.issue("path_limit", "")
			continue
		}
		if !q.include(path) {
			continue
		}
		if q.options.Mode == "path" {
			haystack, needle := filepath.ToSlash(path), q.options.Query
			if !q.options.CaseSensitive {
				haystack, needle = strings.ToLower(haystack), strings.ToLower(needle)
			}
			if strings.Contains(haystack, needle) {
				q.add(api.SearchMatch{Path: filepath.Join(q.root, path)})
			}
		} else {
			batch = append(batch, path)
			batchBytes += len(path) + 1
			if len(batch) == searchBatchSize || batchBytes >= searchBatchBytes {
				queryErr = q.content(batch)
				batch, batchBytes = batch[:0], 0
			}
		}
		if q.stopped || queryErr != nil || q.ctx.Err() != nil {
			cancel()
			break
		}
	}
	if queryErr == nil && !q.stopped && q.ctx.Err() == nil && len(batch) > 0 {
		queryErr = q.content(batch)
	}
	if q.stopped || queryErr != nil || q.ctx.Err() != nil || scanner.Err() != nil {
		cancel()
	}
	waitErr := command.Wait()
	if scanner.Err() != nil && !q.stopped && q.ctx.Err() == nil {
		q.issue("path_record_limit", "")
	}
	if waitErr != nil && ctx.Err() == nil && !exitCode(waitErr, 1) {
		q.issue("read_error", "")
	}
	return queryErr
}

func exitCode(err error, code int) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == code
}

type rgText struct {
	Text  *string `json:"text"`
	Bytes string  `json:"bytes"`
}

func (v rgText) decode() ([]byte, error) {
	if v.Text != nil {
		return []byte(*v.Text), nil
	}
	return base64.StdEncoding.DecodeString(v.Bytes)
}

type rgMatch struct {
	Path           rgText            `json:"path"`
	Lines          rgText            `json:"lines"`
	LineNumber     int               `json:"line_number"`
	AbsoluteOffset int64             `json:"absolute_offset"`
	Submatches     []api.SearchRange `json:"submatches"`
}

type rgEvent struct {
	Type string  `json:"type"`
	Data rgMatch `json:"data"`
}

func (q *fileSearch) contentArgs() []string {
	args := []string{"--no-config", "--json", "--encoding", "none", "--threads", "2", "--color", "never"}
	if !q.options.Regex {
		args = append(args, "--fixed-strings")
	}
	if !q.options.CaseSensitive {
		args = append(args, "--ignore-case")
	}
	if q.options.WholeWord {
		args = append(args, "--word-regexp")
	}
	args = append(args, "-e", q.options.Query, "--")
	return args
}

func (q *fileSearch) content(paths []string) error {
	args := q.contentArgs()
	// Explicit candidates have already passed rg ignore rules and our globs.
	allowed := make(map[string]bool, len(paths))
	for _, path := range paths {
		full := filepath.Join(q.root, path)
		info, err := os.Stat(full)
		if err != nil {
			q.issue("read_error", full)
		} else if !info.Mode().IsRegular() {
			q.issue("unsupported_file", full)
		} else if info.Size() > maxSearchFileBytes {
			q.issue("file_size_limit", full)
		} else {
			args = append(args, "./"+filepath.ToSlash(path))
			allowed[filepath.Clean(path)] = true
		}
	}
	if len(allowed) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(q.ctx)
	defer cancel()
	command := exec.CommandContext(ctx, q.binary, args...)
	command.Dir = q.root
	var stderr searchStderr
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err = command.Start(); err != nil {
		return err
	}
	pending := make(map[string][]rgMatch)
	pendingBytes, pendingCount := 0, 0
	flush := func(path string) {
		matches := pending[path]
		delete(pending, path)
		for _, match := range matches {
			pendingCount -= len(match.Submatches)
			line, _ := match.Lines.decode()
			pendingBytes -= len(line)
		}
		if len(matches) > 0 && !q.stopped && q.ctx.Err() == nil {
			q.verifyMatches(filepath.Join(q.root, path), matches)
		}
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 32<<10), maxSearchRecord)
	for scanner.Scan() {
		var event rgEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			q.issue("invalid_search_output", "")
			cancel()
			break
		}
		rawPath, err := event.Data.Path.decode()
		if err != nil || !utf8.Valid(rawPath) {
			q.issue("path_encoding", "")
			continue
		}
		path := filepath.Clean(string(rawPath))
		if !allowed[path] {
			continue
		}
		switch event.Type {
		case "match":
			line, err := event.Data.Lines.decode()
			if err != nil {
				q.issue("content_encoding", filepath.Join(q.root, path))
				continue
			}
			pending[path] = append(pending[path], event.Data)
			pendingCount += len(event.Data.Submatches)
			pendingBytes += len(line)
			if pendingCount+q.count >= q.options.Limit || pendingBytes > maxSearchBytes {
				flush(path)
				if !q.stopped {
					q.issue("candidate_limit", "")
					q.stopped = true
				}
			}
		case "end":
			flush(path)
		}
		if q.stopped || q.ctx.Err() != nil {
			break
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		q.issue("line_size_limit", "")
	}
	// Cancel before Wait on early exits so the child cannot block on stdout.
	if q.stopped || q.ctx.Err() != nil || scanErr != nil {
		cancel()
	}
	waitErr := command.Wait()
	for _, path := range paths {
		flush(filepath.Clean(path))
	}
	if waitErr != nil && ctx.Err() == nil && !exitCode(waitErr, 1) {
		if bytes.Contains(stderr.data, []byte("regex parse error")) {
			return &api.Error{Code: "INVALID_ARGUMENT", Detail: "invalid ripgrep regular expression"}
		}
		q.issue("read_error", "")
	}
	return nil
}

func readSearchFile(ctx context.Context, path string) ([]byte, string) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "read_error"
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return nil, "unsupported_file"
	}
	if before.Size() > maxSearchFileBytes {
		return nil, "file_size_limit"
	}
	data := make([]byte, 0, int(before.Size()))
	chunk := make([]byte, 128<<10)
	for {
		if ctx.Err() != nil {
			return nil, "timeout"
		}
		n, readErr := file.Read(chunk)
		if len(data)+n > maxSearchFileBytes {
			return nil, "file_size_limit"
		}
		data = append(data, chunk[:n]...)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, "read_error"
		}
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, "file_changed"
	}
	if !utf8.Valid(data) {
		return nil, "content_encoding"
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, "binary_file"
	}
	return data, ""
}

func (q *fileSearch) verifyMatches(path string, matches []rgMatch) {
	data, code := readSearchFile(q.ctx, path)
	if code != "" {
		q.issue(code, path)
		return
	}
	// Validate the entire file's candidate set before publishing any of it.
	var previousOffset int64
	lineNumber := 1
	for _, match := range matches {
		if q.ctx.Err() != nil {
			return
		}
		line, err := match.Lines.decode()
		offset := match.AbsoluteOffset
		if err != nil || offset < previousOffset || offset > int64(len(data)) || int64(len(line)) > int64(len(data))-offset ||
			!bytes.Equal(data[offset:offset+int64(len(line))], line) {
			q.issue("file_changed", path)
			return
		}
		lineNumber += bytes.Count(data[previousOffset:offset], []byte{'\n'})
		previousOffset = offset
		if lineNumber != match.LineNumber || (offset > 0 && data[offset-1] != '\n') {
			q.issue("file_changed", path)
			return
		}
		text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		for _, span := range match.Submatches {
			if span.Start < 0 || span.End < span.Start || span.End > len(text) ||
				(span.Start < len(text) && !utf8.RuneStart(text[span.Start])) || (span.End < len(text) && !utf8.RuneStart(text[span.End])) {
				q.issue("invalid_search_output", path)
				return
			}
		}
	}
	// Construct only one bounded result at a time; contexts must not amplify a
	// small candidate set into unbounded retained output.
	for _, match := range matches {
		line, _ := match.Lines.decode()
		text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		item := api.SearchMatch{Path: path, Line: match.LineNumber, Text: text, Ranges: match.Submatches}
		start, end := int(match.AbsoluteOffset), int(match.AbsoluteOffset)+len(line)
		contextBytes := 0
		for i := 0; i < q.options.ContextLines && start > 0; i++ {
			previousEnd := start - 1
			previousStart := bytes.LastIndexByte(data[:previousEnd], '\n') + 1
			contextBytes += previousEnd - previousStart
			if contextBytes > maxSearchRecord {
				q.issue("context_size_limit", path)
				return
			}
			item.Before = append(item.Before, strings.TrimSuffix(string(data[previousStart:previousEnd]), "\r"))
			start = previousStart
		}
		slices.Reverse(item.Before)
		for i := 0; i < q.options.ContextLines && end < len(data); i++ {
			next := bytes.IndexByte(data[end:], '\n')
			if next < 0 {
				next = len(data) - end
			}
			contextBytes += next
			if contextBytes > maxSearchRecord {
				q.issue("context_size_limit", path)
				return
			}
			item.After = append(item.After, strings.TrimSuffix(string(data[end:end+next]), "\r"))
			end += next + 1
		}
		q.add(item)
		if q.stopped {
			return
		}
	}
}
