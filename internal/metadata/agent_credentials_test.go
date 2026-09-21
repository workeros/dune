package metadata

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/agents"
	publicidentity "github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/aiomni/dune/pkg/workbench"
)

func credentialStores(t *testing.T, run func(*testing.T, *Store, storage.Config)) {
	t.Helper()
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			store, err := Open(t.Context(), config, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			run(t, store, config)
		})
	}
}

func credentialFixture(t *testing.T, store *Store) (agents.Scope, workbench.AgentTarget) {
	t.Helper()
	user := publicidentity.User{ID: "credential-owner", Email: "credential@test.dev"}
	if store.localIdentity {
		if err := store.RegisterAccount(t.Context(), identity.Account{User: user, Salt: "unused", PasswordHash: "unused"}, "unused", time.Now().Add(time.Hour).Unix()); err != nil {
			t.Fatal(err)
		}
	} else {
		user = publicidentity.User{ID: "email:agent@test.dev", Namespace: "sanddance", Kind: "email", Subject: "agent@test.dev"}
	}
	scope := agents.Scope{Principal: user, OwnerID: "tenant"}
	_, enrollment, _, err := store.IssueEnrollment(t.Context(), scope.OwnerID, "runner")
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := store.Enroll(t.Context(), enrollment, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	target := workbench.AgentTarget{Binding: runner.Binding{RunnerID: machine.RunnerID, MachineID: machine.ID, FabricID: "attached", Revision: 1}, Runtime: workbench.RuntimeRef{ID: "runtime", Incarnation: "boot", Generation: 1, Adapter: "acp"}}
	return scope, target
}

func TestAgentCredentialBindsRuntimeAndRotatesAcrossDatabaseConnections(t *testing.T) {
	credentialStores(t, func(t *testing.T, store *Store, config storage.Config) {
		scope, target := credentialFixture(t, store)
		expires := time.Now().Add(time.Hour)
		token, err := store.IssueAgentCredential(t.Context(), scope, target, expires)
		if err != nil || token == "" {
			t.Fatal("issue credential", err)
		}
		var hash string
		if err := store.db.QueryRow(`SELECT hash FROM dune_agent_credentials WHERE owner_id=$1 AND target_key=$2`, scope.OwnerID, target.Key()).Scan(&hash); err != nil || hash == token || hash != tokenHash(token) {
			t.Fatal("credential secret was not hashed", err)
		}
		peer := store
		if store.postgres {
			peer, err = Open(t.Context(), config, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
		}
		read, err := peer.ReadAgentCredential(t.Context(), token, "")
		if err != nil || read.Scope.OwnerID != scope.OwnerID || read.Scope.Principal.ID != scope.Principal.ID || read.Target != target {
			t.Fatal("credential lost scope or exact target", err)
		}
		if _, err := peer.ReadAgentCredential(t.Context(), token, "another-identity"); !errors.Is(err, publicidentity.ErrUnauthorized) {
			t.Fatal("crossed identity namespace", err)
		}
		replacement, err := peer.IssueAgentCredential(t.Context(), scope, target, expires)
		if err != nil || replacement == token {
			t.Fatal("rotation", err)
		}
		if _, err := store.ReadAgentCredential(t.Context(), token, ""); !errors.Is(err, publicidentity.ErrUnauthorized) {
			t.Fatal("rotated token remained valid", err)
		}
		other := target
		other.Runtime.Generation++
		next, err := store.IssueAgentCredential(t.Context(), scope, other, expires)
		if err != nil {
			t.Fatal(err)
		}
		exact, err := peer.ReadAgentCredential(t.Context(), next, "")
		if err != nil || exact.Target != other {
			t.Fatal("new generation reused old identity", err)
		}
		if err := store.RevokeAgentCredential(t.Context(), "other-tenant", target); err != nil {
			t.Fatal(err)
		}
		if _, err := peer.ReadAgentCredential(t.Context(), replacement, ""); err != nil {
			t.Fatal("cross Tenant revocation affected caller", err)
		}
		if err := store.RevokeAgentCredential(t.Context(), scope.OwnerID, target); err != nil {
			t.Fatal(err)
		}
		if _, err := peer.ReadAgentCredential(t.Context(), replacement, ""); !errors.Is(err, publicidentity.ErrUnauthorized) {
			t.Fatal("revoked token accepted", err)
		}
		if _, err := peer.ReadAgentCredential(t.Context(), next, ""); err != nil {
			t.Fatal("revocation affected another execution", err)
		}
	})
}

func TestAgentCredentialRejectsExpiredRevokedAndMalformedBindings(t *testing.T) {
	credentialStores(t, func(t *testing.T, store *Store, _ storage.Config) {
		scope, target := credentialFixture(t, store)
		issue := func() string {
			t.Helper()
			token, err := store.IssueAgentCredential(t.Context(), scope, target, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			return token
		}
		token := issue()
		if _, err := store.db.Exec(`UPDATE dune_agent_credentials SET expires_at=1`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReadAgentCredential(t.Context(), token, ""); !errors.Is(err, publicidentity.ErrUnauthorized) {
			t.Fatal("expired credential accepted", err)
		}
		token = issue()
		if err := store.SetUserEnabled(t.Context(), scope.Principal.ID, false); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReadAgentCredential(t.Context(), token, ""); !errors.Is(err, publicidentity.ErrUnauthorized) {
			t.Fatal("disabled caller accepted", err)
		}
		if err := store.SetUserEnabled(t.Context(), scope.Principal.ID, true); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReadAgentCredential(t.Context(), token, ""); !errors.Is(err, publicidentity.ErrUnauthorized) {
			t.Fatal("re-enabled caller revived old credential", err)
		}
		token = issue()
		for _, invalid := range []string{"", "cookie", token + "x", strings.Repeat("x", 100000)} {
			if _, err := store.ReadAgentCredential(t.Context(), invalid, ""); !errors.Is(err, publicidentity.ErrUnauthorized) {
				t.Fatal("invalid token accepted", err)
			}
		}
		if _, err := store.db.Exec(`UPDATE dune_agent_credentials SET target_key='wrong'`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReadAgentCredential(t.Context(), token, ""); !errors.Is(err, publicidentity.ErrUnauthorized) {
			t.Fatal("corrupt target accepted", err)
		}
		bad := target
		bad.Runtime.Incarnation = ""
		if _, err := store.IssueAgentCredential(t.Context(), scope, bad, time.Now().Add(time.Hour)); !errors.Is(err, ErrInvalidArgument) {
			t.Fatal("partial Runtime accepted", err)
		}
		bad = target
		bad.Binding.Revision++
		if _, err := store.IssueAgentCredential(t.Context(), scope, bad, time.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) {
			t.Fatal("old Runner binding accepted", err)
		}
	})
}

func TestAgentCredentialSurvivesDatabaseReopen(t *testing.T) {
	credentialStores(t, func(t *testing.T, store *Store, config storage.Config) {
		scope, target := credentialFixture(t, store)
		token, err := store.IssueAgentCredential(t.Context(), scope, target, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		wrong := scope
		wrong.OwnerID = "another"
		if got, err := store.IssueAgentCredential(t.Context(), wrong, target, time.Now().Add(time.Hour)); !errors.Is(err, ErrNotFound) || got != "" {
			t.Fatal("cross Tenant issued a credential", err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(t.Context(), config, OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		read, err := reopened.ReadAgentCredential(t.Context(), token, "")
		if err != nil || read.Target != target {
			t.Fatal("host restart lost credential target", err)
		}
	})
}

func TestAgentCredentialConcurrentRotationLeavesOneToken(t *testing.T) {
	credentialStores(t, func(t *testing.T, store *Store, config storage.Config) {
		scope, target := credentialFixture(t, store)
		peer := store
		if store.postgres {
			var err error
			peer, err = Open(t.Context(), config, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
		}
		type issued struct {
			token string
			err   error
		}
		results := make(chan issued, 2)
		var wg sync.WaitGroup
		for _, writer := range []*Store{store, peer} {
			wg.Go(func() {
				token, err := writer.IssueAgentCredential(t.Context(), scope, target, time.Now().Add(time.Hour))
				results <- issued{token, err}
			})
		}
		wg.Wait()
		close(results)
		valid := 0
		for result := range results {
			if result.err != nil {
				t.Fatal(result.err)
			}
			if _, err := store.ReadAgentCredential(t.Context(), result.token, ""); err == nil {
				valid++
			} else if !errors.Is(err, publicidentity.ErrUnauthorized) {
				t.Fatal(err)
			}
		}
		if valid != 1 {
			t.Fatal("rotation left multiple credentials", valid)
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
			scope, target := credentialFixture(t, store)
			token, err := store.IssueAgentCredential(t.Context(), scope, target, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			read, err := store.ReadAgentCredential(t.Context(), token, scope.Principal.Namespace)
			if err != nil || read.Scope != scope || read.Target != target {
				t.Fatal("enterprise credential changed", err)
			}
			if _, err := store.ReadAgentCredential(t.Context(), token, ""); !errors.Is(err, publicidentity.ErrUnauthorized) {
				t.Fatal("enterprise credential accepted as personal", err)
			}
		})
	}
}
