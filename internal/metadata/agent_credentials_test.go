package metadata

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/aiomni/dune/pkg/workbench"
)

func credentialScope(t *testing.T, store *Store) agents.Scope {
	t.Helper()
	user := identity.User{ID: "credential-owner", Email: "credential@test.dev"}
	if err := store.RegisterAccount(t.Context(), identity.Account{User: user, Salt: "unused", PasswordHash: "unused"}, "unused", time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	return agents.Scope{Principal: user, OwnerID: "tenant"}
}

func TestAgentCredentialBindsLaunchAndRotatesAcrossDatabaseConnections(t *testing.T) {
	recoveryStores(t, func(t *testing.T, store *Store, config storage.Config) {
		scope := credentialScope(t, store)
		session, err := store.CreateAgentSession(t.Context(), scope.OwnerID, recoveryLaunch())
		if err != nil {
			t.Fatal(err)
		}
		expires := time.Now().Add(time.Hour)
		token, err := store.IssueAgentCredential(t.Context(), scope, session.ID, session.Attempt.ID, expires)
		if err != nil || token == "" {
			t.Fatal("issue credential", err)
		}
		if _, err := store.ReadAgentCredential(t.Context(), token, ""); !errors.Is(err, identity.ErrUnauthorized) {
			t.Fatal("unconfirmed launch authenticated", err)
		}
		runtime := recoveryRuntime("credential-runtime")
		if _, err := store.RecordAgentRuntime(t.Context(), scope.OwnerID, session.ID, session.Attempt.ID, runtime); err != nil {
			t.Fatal(err)
		}
		var hash string
		if err := store.db.QueryRow(`SELECT hash FROM dune_agent_credentials WHERE session_id=$1`, session.ID).Scan(&hash); err != nil || hash == token || hash != tokenHash(token) {
			t.Fatal("credential secret was not hashed", err)
		}
		peer := store
		if store.postgres {
			peer, err = Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
		}
		read, err := peer.ReadAgentCredential(t.Context(), token, "")
		if err != nil || read.Scope.OwnerID != scope.OwnerID || read.Scope.Principal.ID != scope.Principal.ID || read.Target != (workbench.AgentTarget{Binding: session.Launch.Binding, Runtime: runtime}) {
			t.Fatal("credential lost caller scope or target across connections", err)
		}
		if _, err := peer.ReadAgentCredential(t.Context(), token, "another-identity"); !errors.Is(err, identity.ErrUnauthorized) {
			t.Fatal("credential crossed identity namespace", err)
		}
		replacement, err := peer.IssueAgentCredential(t.Context(), scope, session.ID, session.Attempt.ID, expires)
		if err != nil || replacement == token {
			t.Fatal("rotation", err)
		}
		if _, err := store.ReadAgentCredential(t.Context(), token, ""); !errors.Is(err, identity.ErrUnauthorized) {
			t.Fatal("rotated token remained valid on another connection", err)
		}
		if err := store.RevokeAgentCredential(t.Context(), "other-tenant", session.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := peer.ReadAgentCredential(t.Context(), replacement, ""); err != nil {
			t.Fatal("cross Tenant revocation affected caller", err)
		}
		if err := store.RevokeAgentCredential(t.Context(), scope.OwnerID, session.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := peer.ReadAgentCredential(t.Context(), replacement, ""); !errors.Is(err, identity.ErrUnauthorized) {
			t.Fatal("revoked token accepted", err)
		}
	})
}

func TestAgentCredentialRejectsExpiredRevokedAndSupersededCalls(t *testing.T) {
	recoveryStores(t, func(t *testing.T, store *Store, _ storage.Config) {
		scope := credentialScope(t, store)
		session, target := observationFixture(t, store)
		session, err := store.ObserveAgentSession(t.Context(), scope.OwnerID, target, nativeObservation("native", "/src", 1))
		if err != nil {
			t.Fatal(err)
		}
		issue := func() string {
			t.Helper()
			token, err := store.IssueAgentCredential(t.Context(), scope, session.ID, session.Attempt.ID, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			return token
		}
		token := issue()
		if _, err := store.db.Exec(`UPDATE dune_agent_credentials SET expires_at=1`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReadAgentCredential(t.Context(), token, ""); !errors.Is(err, identity.ErrUnauthorized) {
			t.Fatal("expired credential accepted", err)
		}
		token = issue()
		if err := store.SetUserEnabled(t.Context(), scope.Principal.ID, false); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReadAgentCredential(t.Context(), token, ""); !errors.Is(err, identity.ErrUnauthorized) {
			t.Fatal("disabled caller accepted", err)
		}
		if err := store.SetUserEnabled(t.Context(), scope.Principal.ID, true); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReadAgentCredential(t.Context(), token, ""); !errors.Is(err, identity.ErrUnauthorized) {
			t.Fatal("re-enabled user revived old credential", err)
		}
		token = issue()
		if _, claimed, err := store.BeginAgentResume(t.Context(), scope.OwnerID, session.ID, session.Revision); err != nil || !claimed {
			t.Fatal("resume claim", err)
		}
		if _, err := store.ReadAgentCredential(t.Context(), token, ""); !errors.Is(err, identity.ErrUnauthorized) {
			t.Fatal("old launch credential survived resume", err)
		}
		if _, err := store.IssueAgentCredential(t.Context(), scope, session.ID, session.Attempt.ID, time.Now().Add(time.Hour)); !errors.Is(err, ErrConflict) {
			t.Fatal("superseded launch issued credential", err)
		}
		for _, invalid := range []string{"", "cookie", token + "x", strings.Repeat("x", 100000)} {
			if _, err := store.ReadAgentCredential(t.Context(), invalid, ""); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("invalid token accepted", err)
			}
		}
	})
}

func TestAgentCredentialSurvivesDatabaseReopenWithoutStoringSecret(t *testing.T) {
	recoveryStores(t, func(t *testing.T, store *Store, config storage.Config) {
		scope := credentialScope(t, store)
		session, _ := observationFixture(t, store)
		token, err := store.IssueAgentCredential(t.Context(), scope, session.ID, session.Attempt.ID, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		wrong := scope
		wrong.OwnerID = "another"
		if got, err := store.IssueAgentCredential(t.Context(), wrong, session.ID, session.Attempt.ID, time.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) || got != "" {
			t.Fatal("cross Tenant issued a credential", err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		if _, err := reopened.ReadAgentCredential(t.Context(), token, ""); err != nil {
			t.Fatal("host restart invalidated live credential", err)
		}
	})
}

func TestAgentCredentialPreservesEnterpriseIdentityWithoutLocalAccounts(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "external")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			store, err := Open(t.Context(), config, OpenOptions{ExternalIdentity: true})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			session, _ := observationFixture(t, store)
			scope := agents.Scope{OwnerID: "tenant", Principal: identity.User{ID: "email:agent@test.dev", Namespace: "sanddance", Kind: "email", Subject: "agent@test.dev"}}
			token, err := store.IssueAgentCredential(t.Context(), scope, session.ID, session.Attempt.ID, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			read, err := store.ReadAgentCredential(t.Context(), token, scope.Principal.Namespace)
			if err != nil || read.Scope != scope {
				t.Fatal("enterprise issuer identity changed", err)
			}
			if _, err := store.ReadAgentCredential(t.Context(), token, ""); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("enterprise credential accepted as personal", err)
			}
		})
	}
}
