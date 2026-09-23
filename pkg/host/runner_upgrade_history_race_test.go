package host

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/upgrade"
)

// The original dispatcher can finish at any of these read boundaries. Running
// its effects synchronously makes the interleaving deterministic without sleeps.
type upgradeHistoryBarrier struct {
	upgrade.ObservationStore
	point    string
	complete func(context.Context) error
}

func (s *upgradeHistoryBarrier) trigger(ctx context.Context, point string) error {
	if s.point != point || s.complete == nil {
		return nil
	}
	complete := s.complete
	s.complete = nil // The receipt's nested Observe must not dispatch again.
	return complete(ctx)
}

func (s *upgradeHistoryBarrier) UnknownSubmissions(ctx context.Context, owner string, request upgrade.ListRequest) (upgrade.Page, error) {
	if err := s.trigger(ctx, "before-unknown"); err != nil {
		return upgrade.Page{}, err
	}
	page, err := s.ObservationStore.UnknownSubmissions(ctx, owner, request)
	if err != nil {
		return page, err
	}
	return page, s.trigger(ctx, "after-unknown")
}

func (s *upgradeHistoryBarrier) Observe(ctx context.Context, owner string, observation upgrade.Observation) error {
	if err := s.ObservationStore.Observe(ctx, owner, observation); err != nil {
		return err
	}
	return s.trigger(ctx, "after-runner-list")
}

func TestRunnerUpgradeHistoryRetainsSubmissionsAcrossSourceReads(t *testing.T) {
	for _, point := range []string{"before-unknown", "after-unknown", "after-runner-list"} {
		t.Run(point, func(t *testing.T) {
			f := openInstalledRunnerFixture(t)
			server := httptest.NewServer(f.app)
			defer server.Close()
			config := fmt.Sprintf("gateway: ws://127.0.0.1:7443/api/v1/ws/tunnel\nupgrade_control_url: %s/api/v1/runner-upgrade-control\ntoken: %s\ntarget: %s\nsession_dir: %s\n", server.URL, f.credential, f.binding.MachineID, f.stateDir)
			if err := os.WriteFile(f.configPath, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			f.app.upgradeSource = releaseSourceFunc(func(context.Context, upgrade.ReleaseRef, upgrade.Platform) (upgrade.Manifest, error) {
				return f.manifest, nil
			})
			scope := upgrade.Scope{Principal: f.principal, OwnerID: f.owner, Binding: f.binding}
			digest, _ := f.manifest.Digest()
			request := upgrade.Request{Binding: f.binding, InstallationID: f.observed.ID, ExpectedInstallationRevision: "999", ExpectedRunningSHA256: f.program.SHA256, Release: upgrade.ReleaseRef{ID: f.manifest.ID, ManifestSHA256: digest}}
			reserved := make(map[string]upgrade.Observation)
			for _, id := range []string{"a", "b", "c", "d"} {
				request.SubmissionID = id
				observation, created, err := f.app.upgradeObservations.Reserve(t.Context(), f.owner, request)
				if err != nil || !created {
					t.Fatal(observation, created, err)
				}
				reserved[id] = observation
			}
			dispatch := func(ctx context.Context, id string) error {
				original := reserved[id].Operation
				submission := upgrade.Submission{Request: original.Request, ReservedAt: original.StartedAt}
				var op upgrade.Operation
				if err := (&runnerUpgrades{app: f.app}).call(ctx, scope, "runner.upgrade.start", submission, &op); err != nil {
					return err
				}
				if op.Admission != api.SubmissionNotAccepted {
					return fmt.Errorf("expected durable rejection, got %s", op.Admission)
				}
				_, err := f.app.RunnerUpgrader().GetUpgrade(ctx, scope, upgrade.Query{Binding: f.binding, InstallationID: f.observed.ID, SubmissionID: id})
				return err
			}
			// A known d gives the host a live observation to persist after its
			// Runner read, providing a second barrier independent of read order.
			if err := dispatch(t.Context(), "d"); err != nil {
				t.Fatal(err)
			}
			barrier := &upgradeHistoryBarrier{ObservationStore: f.app.upgradeObservations, point: point, complete: func(ctx context.Context) error {
				return dispatch(ctx, "c")
			}}
			f.app.upgradeObservations = barrier
			list := upgrade.ListRequest{Binding: f.binding, InstallationID: f.observed.ID, Limit: 2}
			var seen []string
			for pageNumber := 0; ; pageNumber++ {
				if pageNumber >= 4 {
					t.Fatal("history cursor did not finish")
				}
				history, err := f.app.RunnerUpgrader().ListUpgrades(t.Context(), scope, list)
				if err != nil || history.Freshness != "live" || history.Page.Active != nil {
					t.Fatal(history, err)
				}
				for _, op := range history.Page.Items {
					seen = append(seen, op.Request.SubmissionID)
					if op.Request.SubmissionID == "c" {
						want := api.SubmissionNotAccepted
						if point == "after-runner-list" {
							want = api.SubmissionUnknown
						}
						if op.Admission != want || (op.ID == "") != (want == api.SubmissionUnknown) {
							t.Fatal("history must use executor facts when read, otherwise retain unknown", op)
						}
					}
				}
				if history.Page.NextCursor == "" {
					break
				}
				list.Cursor = history.Page.NextCursor
			}
			if barrier.complete != nil || !reflect.DeepEqual(seen, []string{"d", "c", "b", "a"}) {
				t.Fatalf("submission disappeared between source reads: %v", seen)
			}
		})
	}
}
