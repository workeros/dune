package api

// StartRequest freezes one launch before any worktree, setup or Agent side
// effect. The caller retains SubmissionKey before sending. Runtime selectors
// are empty because launch admission reserves the new identity independently.
type StartRequest struct {
	SubmissionKey
	Profile  Profile         `json:"profile"`
	Worktree *WorktreeCreate `json:"worktree,omitempty"`
}

// StartResult is meaningful even with an error. Accepted reserves an identity;
// only stage=started confirms Agent startup. Query the original key after a
// missing response; never infer a replacement launch from an empty Runtime.
type StartResult struct {
	SubmissionReceipt
	Failure *ProfileFailure `json:"failure,omitempty"`
}
