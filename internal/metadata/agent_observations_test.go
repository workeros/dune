package metadata

import (
	"errors"
	"sync"
	"testing"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/aiomni/dune/pkg/workbench"
)

func observationFixture(t *testing.T, store *Store) (agents.Session, workbench.AgentTarget) {
	t.Helper()
	session, err := store.CreateAgentSession(t.Context(), "tenant", recoveryLaunch())
	if err != nil {
		t.Fatal(err)
	}
	target := workbench.AgentTarget{Binding: session.Launch.Binding, Runtime: recoveryRuntime("original")}
	session, err = store.RecordAgentRuntime(t.Context(), "tenant", session.ID, session.Attempt.ID, target.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	return session, target
}

func nativeObservation(id, cwd string, sequence int64) api.NativeSession {
	return api.NativeSession{ID: id, Cwd: cwd, Sequence: sequence, Source: "acp-response", AgentVersion: "fixture-1", ResumeSupported: true}
}

func TestAgentObservationsKeepHistoryAndSelectOnlyLatestConfirmation(t *testing.T) {
	recoveryStores(t, func(t *testing.T, store *Store, config storage.Config) {
		session, target := observationFixture(t, store)
		// Deliberately receive the second completion first, as two host Pods can.
		second := nativeObservation("native-b", "/src/b", 2)
		current, err := store.ObserveAgentSession(t.Context(), "tenant", target, second)
		if err != nil || current.ID != session.ID || !current.Selected || current.Native.ID != "native-b" {
			t.Fatal("first confirmation", current.ID, err)
		}
		first := nativeObservation("native-a", "/src/a", 1)
		older, err := store.ObserveAgentSession(t.Context(), "tenant", target, first)
		if err != nil || older.ID == current.ID || older.Selected || older.Native.ID != "native-a" {
			t.Fatal("late confirmation lost or changed the current selection", older.ID, err)
		}
		selected, err := store.RuntimeAgentSession(t.Context(), "tenant", target)
		if err != nil || selected.ID != current.ID || selected.Native.Cwd != "/src/b" {
			t.Fatal("wrong Runtime association", selected.ID, err)
		}
		if older.Launch.Profile.WorkingDirectory != session.Launch.Profile.WorkingDirectory || older.Summary().WorkingDirectory != "/src/a" || older.Launch.Profile.Env["ORIGINAL_SECRET"] != "private-test-value" {
			t.Fatal("native cwd replaced the actual process launch snapshot")
		}
		// Loading A again reuses its record, and updates only the selection.
		first.Sequence = 3
		reloaded, err := store.ObserveAgentSession(t.Context(), "tenant", target, first)
		if err != nil || reloaded.ID != older.ID || !reloaded.Selected {
			t.Fatal("reload duplicated the native record", err)
		}
		if _, err := store.ObserveAgentSession(t.Context(), "tenant", target, second); err != nil {
			t.Fatal(err)
		}
		items, err := store.AgentSessions(t.Context(), "tenant", "", 10)
		if err != nil || len(items) != 2 {
			t.Fatal("native history not deduplicated", len(items), err)
		}
		for _, item := range items {
			if item.Selected != (item.ID == older.ID) {
				t.Fatal("history record still advertised as selected", item.ID)
			}
		}
		conflicting := nativeObservation("different", "/src/c", 3)
		if _, err := store.ObserveAgentSession(t.Context(), "tenant", target, conflicting); !errors.Is(err, ErrConflict) {
			t.Fatal("same sequence accepted a different native session", err)
		}
		items, _ = store.AgentSessions(t.Context(), "tenant", "", 10)
		if len(items) != 2 {
			t.Fatal("conflicting observation left a partial history record")
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		selected, err = reopened.RuntimeAgentSession(t.Context(), "tenant", target)
		if err != nil || selected.ID != older.ID || !selected.Selected {
			t.Fatal("current association lost after host restart", err)
		}
	})
}

func TestAgentObservationsAreIsolatedAndDeduplicateAcrossConnections(t *testing.T) {
	recoveryStores(t, func(t *testing.T, store *Store, config storage.Config) {
		_, target := observationFixture(t, store)
		peer := store
		if store.postgres {
			var err error
			peer, err = Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
		}
		var group sync.WaitGroup
		results := make(chan error, 16)
		for i := range 16 {
			writer := []*Store{store, peer}[i%2]
			group.Go(func() {
				_, err := writer.ObserveAgentSession(t.Context(), "tenant", target, nativeObservation("native", "/src", 1))
				results <- err
			})
		}
		group.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatal(err)
			}
		}
		items, err := store.AgentSessions(t.Context(), "tenant", "", 10)
		if err != nil || len(items) != 1 || items[0].Revision != 3 {
			t.Fatal("duplicate confirmations created revisions or records", items, err)
		}
		if _, err := store.ObserveAgentSession(t.Context(), "other", target, nativeObservation("native", "/src", 1)); !errors.Is(err, ErrNotFound) {
			t.Fatal("cross-owner observation", err)
		}
		if _, err := store.RuntimeAgentSession(t.Context(), "other", target); !errors.Is(err, ErrNotFound) {
			t.Fatal("cross-owner selection", err)
		}
		target.Runtime.Incarnation = "replacement"
		if _, err := store.ObserveAgentSession(t.Context(), "tenant", target, nativeObservation("native", "/src", 1)); !errors.Is(err, ErrNotFound) {
			t.Fatal("cross-Runtime observation", err)
		}
	})
}

func TestAgentResumeConfirmationCannotChangeNativeSessionOrReviveOldAssociation(t *testing.T) {
	recoveryStores(t, func(t *testing.T, store *Store, _ storage.Config) {
		_, original := observationFixture(t, store)
		native := nativeObservation("original-native", "/session/cwd", 1)
		captured, err := store.ObserveAgentSession(t.Context(), "tenant", original, native)
		if err != nil {
			t.Fatal(err)
		}
		resuming, claimed, err := store.BeginAgentResume(t.Context(), "tenant", captured.ID, captured.Revision)
		if err != nil || !claimed {
			t.Fatal("resume claim", err)
		}
		next := original
		next.Runtime = recoveryRuntime("resumed")
		if _, err := store.RecordAgentRuntime(t.Context(), "tenant", captured.ID, resuming.Attempt.ID, next.Runtime); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ObserveAgentSession(t.Context(), "tenant", original, native); !errors.Is(err, ErrStaleAgentObservation) {
			t.Fatal("old attempt selected a session again", err)
		}
		wrong := native
		wrong.ID = "unexpected-new-session"
		if _, err := store.ObserveAgentSession(t.Context(), "tenant", next, wrong); !errors.Is(err, ErrConflict) {
			t.Fatal("resume changed the native ID", err)
		}
		wrong = native
		wrong.Cwd = "/wrong"
		if _, err := store.ObserveAgentSession(t.Context(), "tenant", next, wrong); !errors.Is(err, ErrConflict) {
			t.Fatal("resume changed the native cwd", err)
		}
		continued, err := store.ObserveAgentSession(t.Context(), "tenant", next, native)
		if err != nil || continued.Status != "available" || *continued.LastRuntime != next.Runtime || !continued.Selected {
			t.Fatal("resume not confirmed", err)
		}
		if _, err := store.RuntimeAgentSession(t.Context(), "tenant", original); !errors.Is(err, ErrNotFound) {
			t.Fatal("old Runtime remained selected", err)
		}
	})
}

func TestAgentRuntimeIndexAndCaptureFailuresAreAtomic(t *testing.T) {
	recoveryStores(t, func(t *testing.T, store *Store, _ storage.Config) {
		session, target := observationFixture(t, store)
		other, err := store.CreateAgentSession(t.Context(), "tenant", recoveryLaunch())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.RecordAgentRuntime(t.Context(), "tenant", other.ID, other.Attempt.ID, target.Runtime); !errors.Is(err, ErrConflict) {
			t.Fatal("one Runtime acquired a second launch snapshot", err)
		}
		unchanged, err := store.AgentSession(t.Context(), "tenant", other.ID)
		if err != nil || unchanged.Attempt.Runtime != nil || unchanged.Attempt.State != "starting" || unchanged.Selected {
			t.Fatal("index conflict partially changed the attempt", err)
		}
		selected, err := store.RuntimeAgentSession(t.Context(), "tenant", target)
		if err != nil || selected.ID != session.ID {
			t.Fatal("index conflict lost the original association", err)
		}
		bad := nativeObservation("native", "relative", 1)
		if _, err := store.ObserveAgentSession(t.Context(), "tenant", target, bad); !errors.Is(err, ErrInvalidArgument) {
			t.Fatal("relative native cwd accepted", err)
		}
		selected, err = store.RuntimeAgentSession(t.Context(), "tenant", target)
		if err != nil || selected.Native != nil || selected.Status != "pending_capture" {
			t.Fatal("invalid capture changed the native record", err)
		}
	})
}
