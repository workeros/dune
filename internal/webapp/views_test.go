package webapp

import (
	"encoding/json"
	"testing"

	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/workbench"
)

func TestViewHTTPKeepsPersonalScopeAndValidatesTree(t *testing.T) {
	f := newWorkbenchFixture(t, true)
	_, token, _, err := f.store.IssueEnrollment(t.Context(), f.owner, "runner")
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := f.store.Enroll(t.Context(), token, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	target := workbench.AgentTarget{Binding: runner.Binding{RunnerID: machine.RunnerID, MachineID: machine.ID, FabricID: "attached", Revision: 1}, Runtime: workbench.RuntimeRef{ID: "runtime", Incarnation: "boot", Generation: 1, Adapter: "acp"}}
	view := struct {
		Revision int64 `json:"revision"`
		workbench.ViewSpec
	}{ViewSpec: workbench.ViewSpec{Root: &workbench.SplitNode{ID: "pane", Pane: &workbench.Pane{Target: target}}, FocusPane: "pane"}}
	if out := f.request(t, "PUT", "/workbench/views/main", 0, view); out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	for _, user := range []int{0, 1} {
		out := f.request(t, "GET", "/workbench/views/main", user, nil)
		var got workbench.View
		if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &got) != nil {
			t.Fatal(out.Code, out.Body.String())
		}
		if user == 0 && (got.Root == nil || got.Revision != 1) {
			t.Fatal("layout lost", got)
		}
		if user == 1 && (got.Root != nil || got.Revision != 0) {
			t.Fatal("colleague inherited layout", got)
		}
	}
	if out := f.request(t, "PUT", "/workbench/views/main", 0, view); out.Code != 409 {
		t.Fatal("stale write", out.Code)
	}
	view.Revision = 1
	view.Root = &workbench.SplitNode{ID: "split", Direction: "horizontal", Ratio: 0.5, Children: []*workbench.SplitNode{view.Root, {ID: "duplicate", Pane: &workbench.Pane{Target: target}}}}
	if out := f.request(t, "PUT", "/workbench/views/main", 0, view); out.Code != 400 {
		t.Fatal("two controlling panes for one Runtime", out.Code, out.Body.String())
	}
	view.Root = view.Root.Children[0]
	view.FocusPane = "missing"
	if out := f.request(t, "PUT", "/workbench/views/main", 0, view); out.Code != 400 {
		t.Fatal("invalid focus", out.Code)
	}
	view.FocusPane = "pane"
	view.Root.Pane.Target.Binding.RunnerID = "outside-runner"
	if out := f.request(t, "PUT", "/workbench/views/main", 0, view); out.Code != 404 {
		t.Fatal("foreign Runner in layout", out.Code, out.Body.String())
	}
	marker := workbench.ReadMarker{Target: target, Epoch: "events", Sequence: 9}
	if out := f.request(t, "PUT", "/workbench/read-markers", 0, marker); out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	marker.Sequence = 0
	for _, user := range []int{0, 1} {
		out := f.request(t, "POST", "/workbench/read-markers/query", user, map[string]any{"items": []workbench.ReadMarker{marker}})
		var got struct {
			Items []workbench.ReadMarker `json:"items"`
		}
		if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &got) != nil {
			t.Fatal(out.Code, out.Body.String())
		}
		want := int64(9)
		if user == 1 {
			want = 0
		}
		if len(got.Items) != 1 || got.Items[0].Sequence != want {
			t.Fatal("read state scope", got)
		}
	}
	if out := f.request(t, "GET", "/workbench/views/main", 2, nil); out.Code != 403 {
		t.Fatal("cross-tenant read", out.Code)
	}
}
