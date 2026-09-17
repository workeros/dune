package metadata

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/profiles"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/aiomni/dune/pkg/workbench"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func recoveryLaunch() agents.LaunchSnapshot {
	return agents.LaunchSnapshot{
		Profile:   api.Profile{Version: 1, Kind: "agent", Adapter: "acp", ManagedACP: true, WorkingDirectory: "/src/original", Start: api.Command{Argv: []string{"/opt/original/agent", "--acp"}}, Env: map[string]string{"ORIGINAL_SECRET": "private-test-value"}},
		Binding:   runner.Binding{RunnerID: "runner-a", FabricID: "attached", MachineID: "machine-a", Revision: 1},
		AgentType: "fixture", Storage: "user:1000:/home/agent", Recovery: agents.RecoveryAdapter{ID: "acp-load", Version: 1},
	}
}

func recoveryRuntime(id string) workbench.RuntimeRef {
	return workbench.RuntimeRef{ID: id, Incarnation: "fabricd-original", Generation: 1, Adapter: "acp"}
}

func recoveryStores(t *testing.T, run func(*testing.T, *Store, storage.Config)) {
	t.Helper()
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			store, err := Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			run(t, store, config)
		})
	}
}

func captureFixture(t *testing.T, store *Store, session agents.Session, runtime workbench.RuntimeRef) agents.Session {
	t.Helper()
	session, err := store.RecordAgentRuntime(t.Context(), session.OwnerID, session.ID, session.Attempt.ID, runtime)
	if err != nil {
		t.Fatal(err)
	}
	session, err = store.CaptureAgentSession(t.Context(), session.OwnerID, session.ID, session.Attempt.ID, runtime, agents.NativeSession{ID: "native-original", Cwd: session.Launch.Profile.WorkingDirectory, Source: "acp.new", AgentVersion: "1.2", ResumeSupported: true})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestAgentSnapshotSurvivesProfileChangesAndReopen(t *testing.T) {
	recoveryStores(t, func(t *testing.T, store *Store, config storage.Config) {
		launch := recoveryLaunch()
		profile, err := store.Profiles().Create(t.Context(), profiles.Record{OwnerID: "tenant", Name: "Original", Profile: launch.Profile, CreatedBy: profiles.Actor{Type: "user", Subject: "creator"}})
		if err != nil {
			t.Fatal(err)
		}
		launch.SourceProfile = &profiles.Selection{ID: profile.ID, Revision: profile.Revision}
		session, err := store.CreateAgentSession(t.Context(), "tenant", launch)
		if err != nil {
			t.Fatal(err)
		}
		if session.Status != "pending_capture" || session.Attempt.State != "starting" || session.Native != nil {
			t.Fatal("premature recovery availability", session)
		}
		launch.Profile.Env["ORIGINAL_SECRET"] = "changed-in-caller"
		launch.Profile.Start.Argv[0] = "/new/agent"
		if session.Launch.Profile.Env["ORIGINAL_SECRET"] != "private-test-value" || session.Launch.Profile.Start.Argv[0] != "/opt/original/agent" {
			t.Fatal("snapshot aliases caller data")
		}
		profile.Profile = launch.Profile
		profile, err = store.Profiles().Update(t.Context(), profile)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Profiles().Delete(t.Context(), "tenant", profiles.Selection{ID: profile.ID, Revision: profile.Revision}); err != nil {
			t.Fatal(err)
		}
		session = captureFixture(t, store, session, recoveryRuntime("first"))
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		loaded, err := reopened.AgentSession(t.Context(), "tenant", session.ID)
		if err != nil || loaded.Launch.SourceProfile.Revision != 1 || loaded.Launch.Profile.Env["ORIGINAL_SECRET"] != "private-test-value" || loaded.Launch.Profile.Start.Argv[0] != "/opt/original/agent" || loaded.Launch.Profile.WorkingDirectory != "/src/original" || loaded.Native.ID != "native-original" {
			t.Fatal("recovery lost the immutable effective configuration", err)
		}
		encoded, _ := json.Marshal(loaded)
		for _, private := range []string{"private-test-value", "ORIGINAL_SECRET", "/opt/original/agent", "profile", "launch"} {
			// Attempt IDs have a launch_ prefix, but no launch configuration key.
			if private == "launch" {
				private = `"launch":`
			}
			if strings.Contains(string(encoded), private) {
				t.Fatalf("session JSON exposes private snapshot field %q", private)
			}
		}
		if _, err := reopened.AgentSession(t.Context(), "other", session.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("cross-owner read", err)
		}
		if _, err := reopened.RecordAgentRuntime(t.Context(), "other", session.ID, session.Attempt.ID, recoveryRuntime("first")); !errors.Is(err, ErrNotFound) {
			t.Fatal("cross-owner capture", err)
		}
		if _, _, err := reopened.BeginAgentResume(t.Context(), "other", session.ID, session.Revision); !errors.Is(err, ErrNotFound) {
			t.Fatal("cross-owner resume", err)
		}
		if items, err := reopened.AgentSessions(t.Context(), "other", "", 10); err != nil || len(items) != 0 {
			t.Fatal("cross-owner list", err)
		}
	})
}

func TestAgentResumeHasOneOwnerAndRejectsLateCapture(t *testing.T) {
	recoveryStores(t, func(t *testing.T, store *Store, config storage.Config) {
		session, err := store.CreateAgentSession(t.Context(), "tenant", recoveryLaunch())
		if err != nil {
			t.Fatal(err)
		}
		session = captureFixture(t, store, session, recoveryRuntime("original"))
		peer := store
		if store.postgres {
			peer, err = Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
		}
		type result struct {
			session agents.Session
			claimed bool
			err     error
		}
		results := make(chan result, 2)
		var group sync.WaitGroup
		for _, writer := range []*Store{store, peer} {
			group.Go(func() {
				next, claimed, err := writer.BeginAgentResume(t.Context(), "tenant", session.ID, session.Revision)
				results <- result{next, claimed, err}
			})
		}
		group.Wait()
		close(results)
		var resumed agents.Session
		claims := 0
		for result := range results {
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result.claimed {
				claims++
			}
			if resumed.ID != "" && resumed.Attempt.ID != result.session.Attempt.ID {
				t.Fatal("two attempts won")
			}
			resumed = result.session
		}
		if claims != 1 || resumed.LastRuntime.ID != "original" {
			t.Fatal("invalid resume claim", claims)
		}
		if _, err := store.CaptureAgentSession(t.Context(), "tenant", session.ID, session.Attempt.ID, recoveryRuntime("original"), *session.Native); !errors.Is(err, ErrConflict) {
			t.Fatal("late original capture overwrote resume", err)
		}
		resumed, err = store.RecordAgentRuntime(t.Context(), "tenant", resumed.ID, resumed.Attempt.ID, recoveryRuntime("resumed"))
		if err != nil {
			t.Fatal(err)
		}
		if resumed.LastRuntime.ID != "original" {
			t.Fatal("resume became current before load confirmation")
		}
		wrong := *session.Native
		wrong.ID = "different-native-session"
		if _, err := store.CaptureAgentSession(t.Context(), "tenant", resumed.ID, resumed.Attempt.ID, recoveryRuntime("resumed"), wrong); !errors.Is(err, ErrConflict) {
			t.Fatal("native ID replaced", err)
		}
		resumed, err = store.CaptureAgentSession(t.Context(), "tenant", resumed.ID, resumed.Attempt.ID, recoveryRuntime("resumed"), *session.Native)
		if err != nil || resumed.LastRuntime.ID != "resumed" || resumed.Status != "available" {
			t.Fatal(resumed, err)
		}
		duplicate, claimed, err := peer.BeginAgentResume(t.Context(), "tenant", session.ID, session.Revision)
		if err != nil || claimed || duplicate.Attempt.ID != resumed.Attempt.ID || duplicate.Attempt.State != "ready" {
			t.Fatal("completed duplicate started again", duplicate, claimed, err)
		}
		unchanged, err := store.CaptureAgentSession(t.Context(), "tenant", resumed.ID, resumed.Attempt.ID, recoveryRuntime("resumed"), *session.Native)
		if err != nil || unchanged.Revision != resumed.Revision {
			t.Fatal("identical capture was not idempotent", err)
		}
	})
}

func TestAgentUnknownResumeCannotBeTakenOver(t *testing.T) {
	recoveryStores(t, func(t *testing.T, store *Store, _ storage.Config) {
		session, err := store.CreateAgentSession(t.Context(), "tenant", recoveryLaunch())
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.BeginAgentResume(t.Context(), "tenant", session.ID, session.Revision); !errors.Is(err, ErrConflict) {
			t.Fatal("resume before capture", err)
		}
		session = captureFixture(t, store, session, recoveryRuntime("original"))
		baseRevision := session.Revision
		session, claimed, err := store.BeginAgentResume(t.Context(), "tenant", session.ID, baseRevision)
		if err != nil || !claimed {
			t.Fatal(err)
		}
		session, err = store.FailAgentAttempt(t.Context(), "tenant", session.ID, session.Attempt.ID, "unknown", "start acknowledgement lost")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.BeginAgentResume(t.Context(), "tenant", session.ID, session.Revision); !errors.Is(err, ErrConflict) {
			t.Fatal("unknown attempt was replayed", err)
		}
		duplicate, claimed, err := store.BeginAgentResume(t.Context(), "tenant", session.ID, baseRevision)
		if err != nil || claimed || duplicate.Status != "unknown" || duplicate.Attempt.ID != session.Attempt.ID {
			t.Fatal("duplicate unknown resume", duplicate, claimed, err)
		}
		// A late, confirmed result can resolve this exact attempt without starting again.
		session, err = store.RecordAgentRuntime(t.Context(), "tenant", session.ID, session.Attempt.ID, recoveryRuntime("late-confirmed"))
		if err != nil {
			t.Fatal(err)
		}
		session, err = store.CaptureAgentSession(t.Context(), "tenant", session.ID, session.Attempt.ID, recoveryRuntime("late-confirmed"), *session.Native)
		if err != nil || session.Status != "available" {
			t.Fatal(session, err)
		}
		// A known failure permits a new explicit attempt, but never changes the native ID.
		session, claimed, err = store.BeginAgentResume(t.Context(), "tenant", session.ID, session.Revision)
		if err != nil || !claimed {
			t.Fatal(err)
		}
		session, err = store.FailAgentAttempt(t.Context(), "tenant", session.ID, session.Attempt.ID, "failed", "process did not start")
		if err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := store.BeginAgentResume(t.Context(), "tenant", session.ID, session.Revision); err != nil || !claimed {
			t.Fatal("explicit retry after confirmed failure", err)
		}
	})
}

func TestAgentWithoutNativeCaptureIsNotRecoverable(t *testing.T) {
	recoveryStores(t, func(t *testing.T, store *Store, _ storage.Config) {
		launch := recoveryLaunch()
		launch.Recovery = agents.RecoveryAdapter{}
		session, err := store.CreateAgentSession(t.Context(), "tenant", launch)
		if err != nil {
			t.Fatal(err)
		}
		session, err = store.RecordAgentRuntime(t.Context(), "tenant", session.ID, session.Attempt.ID, recoveryRuntime("custom"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CaptureAgentSession(t.Context(), "tenant", session.ID, session.Attempt.ID, recoveryRuntime("custom"), agents.NativeSession{ID: "native", Cwd: "/src", Source: "fixture", ResumeSupported: true}); !errors.Is(err, ErrInvalidArgument) {
			t.Fatal("recovery without adapter", err)
		}
		session, err = store.AgentCaptureUnavailable(t.Context(), "tenant", session.ID, session.Attempt.ID, "custom launch has no recovery adapter")
		if err != nil || session.Status != "unavailable" || session.Attempt.State != "ready" {
			t.Fatal(session, err)
		}
		if _, _, err := store.BeginAgentResume(t.Context(), "tenant", session.ID, session.Revision); !errors.Is(err, ErrConflict) {
			t.Fatal("unrecoverable session resumed", err)
		}
	})
}

func TestPostgresAgentResumeCommitLossDoesNotGrantLaunch(t *testing.T) {
	config, _, _ := postgresConfig(t)
	store, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session, err := store.CreateAgentSession(t.Context(), "tenant", recoveryLaunch())
	if err != nil {
		t.Fatal(err)
	}
	session = captureFixture(t, store, session, recoveryRuntime("original"))
	parsed, err := pgx.ParseConfig(config.Postgres.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Postgres.BeforeConnect(t.Context(), parsed); err != nil {
		t.Fatal(err)
	}
	var commits atomic.Int32
	faulty := &Store{postgres: true, db: sql.OpenDB(lostAckConnector{Connector: stdlib.GetConnector(*parsed), commits: &commits})}
	defer faulty.Close()
	if result, claimed, err := faulty.BeginAgentResume(t.Context(), "tenant", session.ID, session.Revision); !errors.Is(err, ErrCommitUnknown) || claimed || result.ID != "" || commits.Load() != 1 {
		t.Fatal("unknown commit granted a launch or retried", claimed, err)
	}
	// Even though the transaction committed, a second Pod can only inspect it.
	current, claimed, err := store.BeginAgentResume(t.Context(), "tenant", session.ID, session.Revision)
	if err != nil || claimed || current.Attempt.Kind != "resume" || current.Attempt.State != "starting" {
		t.Fatal("commit acknowledgement loss led to takeover", current, claimed, err)
	}
}
