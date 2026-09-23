package metadata

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/aiomni/dune/pkg/upgrade"
)

func TestUpgradeObservationsPersistUnknownSubmissionAndFenceTerminalFacts(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			cfg := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				cfg, _, _ = postgresConfig(t)
			}
			store, err := Open(t.Context(), cfg, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { store.Close() }()
			binding := runner.Binding{RunnerID: "runner", MachineID: "machine", FabricID: "fabric", Revision: 1}
			request := upgrade.Request{SubmissionID: "submission", Binding: binding, InstallationID: "installation", ExpectedInstallationRevision: "1", ExpectedRunningSHA256: strings.Repeat("a", 64), Release: upgrade.ReleaseRef{ID: "release", ManifestSHA256: strings.Repeat("b", 64)}}
			var created atomic.Int32
			var wait sync.WaitGroup
			for range 8 {
				wait.Go(func() {
					observed, first, err := store.Reserve(t.Context(), "owner", request)
					if err != nil || observed.Operation.Admission != api.SubmissionUnknown {
						t.Error(observed, err)
					}
					if first {
						created.Add(1)
					}
				})
			}
			wait.Wait()
			if created.Load() != 1 {
				t.Fatal("host authorized duplicate dispatch", created.Load())
			}
			query := upgrade.Query{Binding: binding, InstallationID: request.InstallationID, SubmissionID: request.SubmissionID}
			unknown, err := store.Observation(t.Context(), "owner", query)
			if err != nil || unknown.Operation.ID != "" || unknown.Operation.Admission != api.SubmissionUnknown {
				t.Fatal(unknown, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = Open(t.Context(), cfg, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, first, err := store.Reserve(t.Context(), "owner", request); err != nil || first {
				t.Fatal("host restart authorized replay", first, err)
			}
			list := upgrade.ListRequest{Binding: binding, InstallationID: request.InstallationID}
			pending, err := store.UnknownSubmissions(t.Context(), "owner", list)
			if err != nil || len(pending.Items) != 1 || pending.Items[0].Request != request || pending.Active != nil {
				t.Fatal("restart lost undispatched submission", pending, err)
			}
			changed := request
			changed.ExpectedInstallationRevision = "2"
			if _, _, err := store.Reserve(t.Context(), "owner", changed); err == nil {
				t.Fatal("reused original key for another request")
			}
			terminal := unknown
			terminal.Operation.ID = "operation"
			terminal.Operation.Admission = api.SubmissionNotAccepted
			terminal.Operation.Revision = "3"
			terminal.Operation.Phase = upgrade.Failed
			terminal.Operation.Confirmed = true
			terminal.Operation.Failure = &upgrade.Issue{Code: "INSTALLATION_CHANGED"}
			terminal.ObservedAt = time.Now().UTC()
			if err := store.Observe(t.Context(), "owner", terminal); err != nil {
				t.Fatal(err)
			}
			pending, err = store.UnknownSubmissions(t.Context(), "owner", list)
			if err != nil || len(pending.Items) != 0 || pending.Active != nil {
				t.Fatal("confirmed key still included as unknown", pending, err)
			}
			moved := terminal
			moved.Operation.StartedAt = terminal.Operation.StartedAt.Add(time.Second)
			if err := store.Observe(t.Context(), "owner", moved); err == nil {
				t.Fatal("observation moved immutable history position")
			}
			stale := terminal
			stale.Operation.Revision = "2"
			stale.Operation.Confirmed = false
			stale.Operation.Phase = upgrade.Queued
			if err := store.Observe(t.Context(), "owner", stale); err != nil {
				t.Fatal(err)
			}
			forged := terminal
			forged.Operation.Revision = "4"
			forged.Operation.Failure = &upgrade.Issue{Code: "REWRITTEN"}
			if err := store.Observe(t.Context(), "owner", forged); err == nil {
				t.Fatal("terminal evidence changed")
			}
			final, err := store.Observation(t.Context(), "owner", query)
			if err != nil || final.Operation.Revision != "3" || !final.Operation.Confirmed || final.Operation.Failure.Code != "INSTALLATION_CHANGED" {
				t.Fatal(final, err)
			}
			if _, err := store.Observation(t.Context(), "another-owner", query); !errors.Is(err, upgrade.ErrObservationNotFound) {
				t.Fatal("observation crossed owner boundary", err)
			}
			query.Binding.Revision++
			if _, err := store.Observation(t.Context(), "owner", query); !errors.Is(err, upgrade.ErrObservationNotFound) {
				t.Fatal("observation followed replacement binding", err)
			}
		})
	}
}

func TestUpgradeHistoryPaginatesUnknownSubmissionsWithoutInventingOperations(t *testing.T) {
	store, err := Open(t.Context(), storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	binding := runner.Binding{RunnerID: "runner", MachineID: "machine", FabricID: "fabric", Revision: 1}
	for _, id := range []string{"first", "second", "third"} {
		request := upgrade.Request{SubmissionID: id, Binding: binding, InstallationID: "installation", ExpectedInstallationRevision: "1", ExpectedRunningSHA256: strings.Repeat("a", 64), Release: upgrade.ReleaseRef{ID: "release", ManifestSHA256: strings.Repeat("b", 64)}}
		if _, _, err := store.Reserve(t.Context(), "owner", request); err != nil {
			t.Fatal(err)
		}
	}
	request := upgrade.ListRequest{Binding: binding, InstallationID: "installation", Limit: 1}
	seen := map[string]bool{}
	for {
		page, err := store.Observations(t.Context(), "owner", request)
		if err != nil || len(page.Page.Items) != 1 || page.Page.Active != nil {
			t.Fatal(page, err)
		}
		item := page.Page.Items[0]
		if item.ID != "" || seen[item.Request.SubmissionID] {
			t.Fatal("invented/repeated operation", item)
		}
		seen[item.Request.SubmissionID] = true
		request.Cursor = page.Page.NextCursor
		if request.Cursor == "" {
			break
		}
	}
	if len(seen) != 3 {
		t.Fatal(seen)
	}
}
