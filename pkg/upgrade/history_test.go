package upgrade

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
)

func TestMergeHistoryPagesPreservesBoundariesAndExecutorFacts(t *testing.T) {
	for _, limit := range []int{1, 3, 100} {
		for _, large := range []bool{false, true} {
			t.Run(fmt.Sprintf("limit-%d/large-%t", limit, large), func(t *testing.T) {
				started := time.Now().UTC()
				var live, unknown []Operation
				for index, id := range []string{"a", "b", "c", "d", "e", "f"} {
					op := Operation{Request: Request{SubmissionID: id}, StartedAt: started, Admission: api.SubmissionUnknown}
					if index%2 == 0 {
						op.ID, op.Confirmed, op.Admission = "operation-"+id, true, api.SubmissionNotAccepted
						if large {
							op.Failure = &Issue{Code: "FAILURE", Stage: strings.Repeat("x", 300<<10)}
						}
						live = append(live, op)
					} else {
						unknown = append(unknown, op)
					}
				}
				active := Operation{ID: "active-operation", Request: Request{SubmissionID: "active"}, StartedAt: started, Admission: api.SubmissionAccepted}
				live = append(live, active)
				// A concurrent reservation read may still contain this key. The
				// executor's active evidence takes precedence and appears only once.
				reserved := active
				reserved.ID, reserved.Admission = "", api.SubmissionUnknown
				unknown = append(unknown, reserved)
				cursor := ""
				var seen []string
				for pages := 0; ; pages++ {
					if pages > 6 {
						t.Fatal("cursor made no progress")
					}
					remote, err := Paginate(live, cursor, limit)
					if err != nil {
						t.Fatal(err)
					}
					local, err := Paginate(unknown, cursor, limit)
					if err != nil {
						t.Fatal(err)
					}
					page, err := MergePages(remote, local, cursor, limit)
					if err != nil || page.Active == nil || page.Active.ID != active.ID {
						t.Fatal(page, err)
					}
					for _, op := range page.Items {
						seen = append(seen, op.Request.SubmissionID)
					}
					if page.NextCursor == "" {
						break
					}
					cursor = page.NextCursor
				}
				if !reflect.DeepEqual(seen, []string{"f", "e", "d", "c", "b", "a"}) {
					t.Fatal("combined cursor skipped or repeated a submission", seen)
				}
			})
		}
	}
}

func TestHistoryCursorSurvivesUnknownConfirmation(t *testing.T) {
	started := time.Now().UTC()
	newer := Operation{Request: Request{SubmissionID: "z"}, StartedAt: started, Admission: api.SubmissionUnknown}
	older := Operation{Request: Request{SubmissionID: "a"}, StartedAt: started, Admission: api.SubmissionUnknown}
	first, err := Paginate([]Operation{older, newer}, "", 1)
	if err != nil || len(first.Items) != 1 || first.Items[0].Request.SubmissionID != "z" {
		t.Fatal(first, err)
	}
	// Both keys gain unrelated operation IDs between page reads. Their original
	// reservation time and submission key, and therefore their positions, persist.
	newer.ID, newer.Admission, newer.Confirmed = "aaa", api.SubmissionNotAccepted, true
	older.ID, older.Admission, older.Confirmed = "zzz", api.SubmissionNotAccepted, true
	second, err := Paginate([]Operation{older, newer}, first.NextCursor, 1)
	if err != nil || len(second.Items) != 1 || second.Items[0].Request.SubmissionID != "a" || second.NextCursor != "" {
		t.Fatal(second, err)
	}
}
