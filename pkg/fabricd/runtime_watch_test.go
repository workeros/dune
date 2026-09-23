package fabricd

import (
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestRuntimeWatchRemovalFencesLatePublication(t *testing.T) {
	d := newEngine(t.Context())
	defer d.Close()
	r := &runtime{id: "runtime", inc: "original"}
	d.runtimes[r.id] = r
	mailbox, unsubscribe := d.runtimeWatches.subscribe()
	defer unsubscribe()
	old := api.Runtime{Observation: api.ObservationVersion{Epoch: "connector", Revision: 1}, ID: r.id, Incarnation: r.inc, Generation: 1, SessionMetadata: &api.SessionMetadata{Revision: 12}}
	d.publishRuntime(r, old)
	d.mu.Lock()
	d.discoveryIssues = []api.RuntimeDiscoveryIssue{{Runtime: &old, Code: "REGISTRATION_TEMPORARY_FILE"}}
	d.removeRuntimeLocked(r)
	d.mu.Unlock()
	old.SessionMetadata = &api.SessionMetadata{Revision: 100}
	d.publishRuntime(r, old)
	change, err := mailbox.Next(t.Context())
	if err != nil || !change.Removed || change.Runtime.Incarnation != r.inc || len(d.discoveryIssues) != 0 {
		t.Fatal("late snapshot restored removed member", change, err)
	}
}
