package upgrade

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aiomni/dune/pkg/api"
)

var ErrObservationNotFound = errors.New("upgrade observation not found")

// ObservationStore is owned by the embedding host and shared by its replicas.
// Reserve atomically remembers the exact request before its single dispatch.
// The first reservation fixes Operation.StartedAt, which Observe must preserve
// along with confirmed terminal results while ignoring older revisions.
type ObservationStore interface {
	Reserve(context.Context, string, Request) (Observation, bool, error)
	Observe(context.Context, string, Observation) error
	Observation(context.Context, string, Query) (Observation, error)
	Observations(context.Context, string, ListRequest) (History, error)
	// UnknownSubmissions excludes every key for which executor evidence exists.
	// It uses the same immutable reservation time and submission-key ordering.
	UnknownSubmissions(context.Context, string, ListRequest) (Page, error)
}

type historyCursor struct {
	StartedAt    time.Time `json:"started_at"`
	SubmissionID string    `json:"submission_id"`
}

func parseCursor(value string) (historyCursor, error) {
	var cursor historyCursor
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(data) > 256 || json.Unmarshal(data, &cursor) != nil || cursor.StartedAt.IsZero() || api.ValidateSubmissionID(cursor.SubmissionID) != nil {
		return cursor, fmt.Errorf("invalid upgrade history cursor")
	}
	return cursor, nil
}

// Paginate applies the same stable ordering to executor history and the host's
// known subset. Freshness is carried separately; an offline page is not complete
// evidence of everything that may have executed on an unreachable Runner.
func Paginate(operations []Operation, cursor string, limit int) (Page, error) {
	result := Page{Items: []Operation{}}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return result, fmt.Errorf("history limit must be 1..100")
	}
	var before historyCursor
	if cursor != "" {
		var err error
		before, err = parseCursor(cursor)
		if err != nil {
			return result, err
		}
	}
	sorted := slices.Clone(operations)
	slices.SortFunc(sorted, func(a, b Operation) int {
		return comparePosition(position(a), position(b))
	})
	bytes := 0
	for _, op := range sorted {
		if isActive(op) && result.Active == nil {
			copy := op
			result.Active = &copy
			data, _ := json.Marshal(op)
			bytes += len(data)
		}
	}
	for _, op := range sorted {
		if isActive(op) {
			continue
		}
		if op.Admission == api.SubmissionExpired {
			continue
		}
		if cursor != "" && comparePosition(position(op), before) <= 0 {
			continue
		}
		data, _ := json.Marshal(op)
		if len(result.Items) > 0 && (len(result.Items) == limit || bytes+len(data) > 512<<10) {
			last := result.Items[len(result.Items)-1]
			result.NextCursor = encodeCursor(last)
			break
		}
		result.Items = append(result.Items, op)
		bytes += len(data)
	}
	return result, nil
}

func position(operation Operation) historyCursor {
	return historyCursor{StartedAt: operation.StartedAt, SubmissionID: operation.Request.SubmissionID}
}

func comparePosition(a, b historyCursor) int {
	if a.StartedAt.Equal(b.StartedAt) {
		return strings.Compare(b.SubmissionID, a.SubmissionID)
	}
	return b.StartedAt.Compare(a.StartedAt)
}

func encodeCursor(operation Operation) string {
	return position(operation).encode()
}

func (c historyCursor) encode() string {
	body, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(body)
}

func isActive(op Operation) bool {
	// A host reservation is never evidence that the executor claimed a slot.
	return op.Admission == api.SubmissionAccepted && (!op.Confirmed || op.LaunchSealed)
}

// MergePages merges equally ordered source prefixes before pagination, with
// live executor facts taking precedence over host reservations. A source may
// stop early at its byte budget: do not emit items beyond that source's last
// known position, or the combined cursor could skip its unseen next items.
func MergePages(live, unknown Page, cursor string, limit int) (Page, error) {
	var boundary *historyCursor
	for _, page := range []Page{live, unknown} {
		if page.NextCursor == "" {
			continue
		}
		last, err := parseCursor(page.NextCursor)
		if err != nil {
			return Page{}, err
		}
		if boundary == nil || comparePosition(last, *boundary) < 0 {
			boundary = &last
		}
	}
	bySubmission := make(map[string]Operation)
	for _, page := range []Page{unknown, live} {
		for _, op := range page.Items {
			bySubmission[op.Request.SubmissionID] = op
		}
		if page.Active != nil {
			bySubmission[page.Active.Request.SubmissionID] = *page.Active
		}
	}
	operations := make([]Operation, 0, len(bySubmission))
	for _, op := range bySubmission {
		if boundary == nil || isActive(op) || comparePosition(position(op), *boundary) <= 0 {
			operations = append(operations, op)
		}
	}
	result, err := Paginate(operations, cursor, limit)
	if err == nil && boundary != nil && result.NextCursor == "" {
		result.NextCursor = boundary.encode()
	}
	return result, err
}
