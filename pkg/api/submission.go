package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SubmissionTarget is an exact selector, not an authorization grant. OwnerID
// and the Runner binding must be checked against the authenticated request.
// An empty Runtime identity selects launch admission; otherwise all three
// Runtime identity fields are required. Transport epochs are deliberately absent.
type SubmissionTarget struct {
	OwnerID            string `json:"owner_id"`
	RunnerID           string `json:"runner_id"`
	FabricID           string `json:"fabric_id"`
	MachineID          string `json:"machine_id"`
	BindingRevision    int64  `json:"binding_revision"`
	RuntimeID          string `json:"runtime_id,omitempty"`
	RuntimeIncarnation string `json:"runtime_incarnation,omitempty"`
	RuntimeGeneration  uint64 `json:"runtime_generation,omitempty"`
}

func (t SubmissionTarget) Validate() error {
	for _, value := range []string{t.OwnerID, t.RunnerID, t.FabricID, t.MachineID} {
		if !submissionTargetText(value) {
			return fmt.Errorf("submission requires an owner and complete Runner binding")
		}
	}
	if t.BindingRevision <= 0 {
		return fmt.Errorf("submission binding revision must be positive")
	}
	if t.RuntimeID == "" && t.RuntimeIncarnation == "" && t.RuntimeGeneration == 0 {
		return nil
	}
	if !submissionTargetText(t.RuntimeID) || !submissionTargetText(t.RuntimeIncarnation) || t.RuntimeGeneration == 0 {
		return fmt.Errorf("submission requires the complete original Runtime identity")
	}
	return nil
}

func submissionTargetText(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

// SubmissionKey is retained by the caller before sending, including when no
// server response arrives. The same ID in a different target is a different key.
type SubmissionKey struct {
	SubmissionID string           `json:"submission_id"`
	Target       SubmissionTarget `json:"target"`
}

func (k SubmissionKey) Validate() error {
	if err := ValidateSubmissionID(k.SubmissionID); err != nil {
		return err
	}
	return k.Target.Validate()
}

// ValidateSubmissionID never normalizes an ID: changing it changes the request.
func ValidateSubmissionID(id string) error {
	if len(id) < 1 || len(id) > 128 {
		return fmt.Errorf("submission_id must contain 1..128 ASCII letters, digits, '-' or '_'")
	}
	for i := range len(id) {
		c := id[i]
		if c != '-' && c != '_' && (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return fmt.Errorf("submission_id must contain 1..128 ASCII letters, digits, '-' or '_'")
		}
	}
	return nil
}

type SubmissionAdmission string

const (
	SubmissionUnknown     SubmissionAdmission = "unknown"
	SubmissionAccepted    SubmissionAdmission = "accepted"
	SubmissionNotAccepted SubmissionAdmission = "not_accepted"
	SubmissionExpired     SubmissionAdmission = "expired"
)

// SubmissionReceipt separates admission from execution. Unknown includes an
// absent record: a request may still arrive. NotAccepted requires an irrevocable
// rejection. Expired never authorizes replay. An operation reference confirms
// only the original operation identity, not completion or business success.
type SubmissionReceipt struct {
	SubmissionKey
	Admission    SubmissionAdmission `json:"admission"`
	OperationRef string              `json:"operation_ref,omitempty"`
	Worktree     *Worktree           `json:"worktree,omitempty"`
	Runtime      *Runtime            `json:"runtime,omitempty"`
	Stage        string              `json:"stage,omitempty"`
	ErrorCode    string              `json:"error_code,omitempty"`
	Cleanup      *CleanupProgress    `json:"cleanup,omitempty"`
	RawInput     *RawACPInputReceipt `json:"raw_input,omitempty"`
}

// CleanupProgress contains only durably confirmed steps of the original forget.
// Reading it never schedules work. Paths and process-control authority remain
// private to the installation's independent registry.
type CleanupProgress struct {
	Confirmed []string `json:"confirmed"`
	Remaining []string `json:"remaining"`
}

// SubmissionRequest carries one frozen business request. Operation selects the
// existing business vocabulary; the submission envelope never grants authority
// to an operation that the current authenticated caller could not perform.
type SubmissionRequest struct {
	SubmissionKey
	Operation string          `json:"operation"`
	Payload   json.RawMessage `json:"payload"`
}

// SubmissionError retains the query key even for local cancellation or an EOF
// before a response. Unwrap preserves both context errors and structured API
// errors; Key alone makes no claim about server admission.
type SubmissionError struct {
	Key   SubmissionKey
	Cause error
}

func (e *SubmissionError) Error() string { return e.Cause.Error() }
func (e *SubmissionError) Unwrap() error { return e.Cause }
