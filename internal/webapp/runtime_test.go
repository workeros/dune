package webapp

import (
	"net/http/httptest"
	"testing"
)

func TestRuntimeSelectionRejectsAmbiguousIdentity(t *testing.T) {
	for _, query := range []string{
		"", "?incarnation=current", "?generation=1",
		"?incarnation=current&generation=0", "?incarnation=current&generation=-1",
		"?incarnation=current&generation=18446744073709551616",
		"?incarnation=current&generation=1&generation=2",
		"?incarnation=current&incarnation=other&generation=1",
		"?incarnation=%0A&generation=1",
	} {
		r := httptest.NewRequest("GET", "/events"+query, nil)
		r.SetPathValue("runtime", "selected")
		out := httptest.NewRecorder()
		if _, ok := selectedRuntime(out, r); ok || out.Code != 400 {
			t.Fatal("invalid identity accepted", query, out.Code)
		}
	}
	r := httptest.NewRequest("GET", "/events?incarnation=fixed&generation=7", nil)
	r.SetPathValue("runtime", "selected")
	selected, ok := selectedRuntime(httptest.NewRecorder(), r)
	if !ok || selected.ID != "selected" || selected.Incarnation != "fixed" || selected.Generation != 7 {
		t.Fatal("selection changed", selected)
	}
}
