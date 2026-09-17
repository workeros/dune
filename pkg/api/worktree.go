package api

// WorktreeCreate creates a fresh branch/checkout from a committed Git ref.
// It never copies uncommitted files, resets an existing branch, or reuses a path.
type WorktreeCreate struct {
	Directory string `json:"directory"`
	Path      string `json:"path"`
	Branch    string `json:"branch"`
	Ref       string `json:"ref,omitempty"`
}

type Worktree struct {
	Path     string `json:"path"`
	Head     string `json:"head"`
	Branch   string `json:"branch,omitempty"`
	Detached bool   `json:"detached"`
	Bare     bool   `json:"bare"`
	Locked   bool   `json:"locked"`
	Prunable bool   `json:"prunable"`
}
