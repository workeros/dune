package sessionregistry

import (
	"reflect"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestCapacityCountsIndependentReservationsAndRetainedEvidenceWithoutWrites(t *testing.T) {
	dir := privateDirectory(t)
	r, err := Open(t.Context(), dir, Options{MaxKeys: 3, MaxControls: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.Close() }()
	key := testKey()
	if err := r.ReserveRuntime(t.Context(), key.Target); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"claimed", "accepted", "rejected"} {
		key.SubmissionID = state
		claim, _, err := r.ClaimKey(t.Context(), key, Digest("prompt", nil), "host")
		if err != nil {
			t.Fatal(err)
		}
		if state == "accepted" {
			_, err = r.Accept(t.Context(), claim, "original-operation")
		} else if state == "rejected" {
			_, err = r.Reject(t.Context(), claim, "INVALID_ARGUMENT")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"permission-active", "permission-consumed"} {
		if err := r.ReserveControl(t.Context(), key.Target, ControlPermission, id); err != nil {
			t.Fatal(err)
		}
	}
	key.SubmissionID = "answer"
	claim, _, err := r.ClaimControl(t.Context(), key, Digest("permission", nil), "host", ControlPermission, "permission-consumed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Accept(t.Context(), claim, "answer-operation"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Progress(t.Context(), key, "answer-operation", "written", "", nil); err != nil {
		t.Fatal(err)
	}
	want := api.SubmissionCapacity{
		Ordinary: api.CapacityUsage{Used: 3, Limit: 3}, Claimed: 1, Accepted: 1, Rejected: 1,
		Runtimes: api.CapacityUsage{Used: 1, Limit: MaxLiveRuntimes}, RuntimeRecords: api.CapacityUsage{Used: 1, Limit: MaxRuntimeRecords},
		Controls: map[string]api.ControlCapacityUsage{
			ControlPermission: {Used: 2, Limit: 2, Reserved: 1, Accepted: 1, Completed: 1},
			ControlCancel:     {Limit: 2},
			ControlStop:       {Used: 1, Limit: MaxRuntimeRecords, Reserved: 1},
			ControlForget:     {Used: 1, Limit: MaxRuntimeRecords, Reserved: 1},
		},
	}
	for i := range 3 {
		got, err := r.Capacity(t.Context())
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("capacity read changed durable evidence", got, want, err)
		}
		if i == 0 {
			r.Close()
			r, err = Open(t.Context(), dir, Options{MaxKeys: 3, MaxControls: 2})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	key.SubmissionID = "never-sent"
	if receipt, err := r.Get(t.Context(), key); err != nil || receipt.Admission != api.SubmissionUnknown {
		t.Fatal("diagnostics manufactured evidence", receipt, err)
	}
}
