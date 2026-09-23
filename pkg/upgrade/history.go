package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
// Observe must ignore older revisions and preserve confirmed terminal results.
type ObservationStore interface {
	Reserve(context.Context, string, Request) (Observation, bool, error)
	Observe(context.Context, string, Observation) error
	Observation(context.Context, string, Query) (Observation, error)
	Observations(context.Context, string, ListRequest) (History, error)
}

type historyCursor struct {
	StartedAt time.Time `json:"started_at"`
	ID        string    `json:"id"`
}

func parseCursor(value string) (historyCursor, error) {
	var cursor historyCursor
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(data) > 256 || json.Unmarshal(data, &cursor) != nil || cursor.StartedAt.IsZero() || api.ValidateSubmissionID(cursor.ID) != nil {
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
		if a.StartedAt.Equal(b.StartedAt) {
			return strings.Compare(pageID(b), pageID(a))
		}
		if a.StartedAt.After(b.StartedAt) {
			return -1
		}
		return 1
	})
	bytes := 0
	for _, op := range sorted {
		if !op.Confirmed && op.Admission == api.SubmissionAccepted {
			data, _ := json.Marshal(op)
			bytes += len(data)
		}
	}
	for _, op := range sorted {
		if !op.Confirmed || op.LaunchSealed {
			// Unsent/unknown host submissions are history observations, not evidence
			// that the installation's active execution slot has been claimed.
			if op.Admission == api.SubmissionAccepted {
				copy := op
				result.Active = &copy
				continue
			}
		}
		if op.Admission == api.SubmissionExpired {
			continue
		}
		if cursor != "" && (op.StartedAt.After(before.StartedAt) || (op.StartedAt.Equal(before.StartedAt) && pageID(op) >= before.ID)) {
			continue
		}
		data, _ := json.Marshal(op)
		if len(result.Items) > 0 && (len(result.Items) == limit || bytes+len(data) > 512<<10) {
			last := result.Items[len(result.Items)-1]
			body, _ := json.Marshal(historyCursor{StartedAt: last.StartedAt, ID: pageID(last)})
			result.NextCursor = base64.RawURLEncoding.EncodeToString(body)
			break
		}
		result.Items = append(result.Items, op)
		bytes += len(data)
	}
	return result, nil
}

func pageID(operation Operation) string {
	if operation.ID != "" {
		return operation.ID
	}
	digest := sha256.Sum256([]byte(operation.Request.SubmissionID))
	return hex.EncodeToString(digest[:])
}
