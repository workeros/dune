package api

// SearchOptions selects a bounded search of files on disk. Include and Exclude
// are globs relative to the root; include never overrides ignore rules.
type SearchOptions struct {
	Mode           string   `json:"mode"`
	Query          string   `json:"query"`
	CaseSensitive  bool     `json:"case_sensitive,omitempty"`
	WholeWord      bool     `json:"whole_word,omitempty"`
	Regex          bool     `json:"regex,omitempty"`
	Include        []string `json:"include,omitempty"`
	Exclude        []string `json:"exclude,omitempty"`
	IncludeHidden  *bool    `json:"include_hidden,omitempty"`
	UseIgnoreFiles *bool    `json:"use_ignore_files,omitempty"`
	Limit          int      `json:"limit,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	ContextLines   int      `json:"context_lines,omitempty"`
}

// SearchRange uses zero-based UTF-8 byte offsets into SearchMatch.Text, with an
// exclusive End. Line is one-based; clients convert offsets for their editor.
type SearchRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type SearchMatch struct {
	Path   string        `json:"path"`
	Line   int           `json:"line,omitempty"`
	Text   string        `json:"text,omitempty"`
	Ranges []SearchRange `json:"ranges,omitempty"`
	Before []string      `json:"before,omitempty"`
	After  []string      `json:"after,omitempty"`
}

type SearchIssue struct {
	Code string `json:"code"`
	Path string `json:"path,omitempty"`
}

// SearchResult reports partial results explicitly; search has no continuation
// cursor. Complete is true only when no budget or processing issue limited it.
type SearchResult struct {
	Matches  []SearchMatch `json:"matches"`
	Complete bool          `json:"complete"`
	Issues   []SearchIssue `json:"issues,omitempty"`
}
