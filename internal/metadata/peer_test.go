package metadata

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/storage"
	"modernc.org/sqlite"
)

func peerRecord(t *testing.T, s *Store) authorization.PeerAccess {
	t.Helper()
	ctx := context.Background()
	local := identity.NewLocal(s, true)
	u, cookie, err := local.Register(ctx, wire.ID()+"@peer.test", "peer-test-password")
	if err != nil {
		t.Fatal(err)
	}
	enrollment, _, err := s.IssueEnrollment(ctx, u.ID, "peer machine")
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := s.Enroll(ctx, enrollment, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := s.CreateAccess(ctx, wire.ID(), tokenHash(cookie), u.ID, "", machine.ID, time.Now().Add(time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	return authorization.PeerAccess{PeerReference: authorization.PeerReference{SourceBootID: wire.ID(), OwnerBootID: wire.ID(), Target: machine.ID, RequestDigest: tokenHash("original request")}, Connection: connection,
		Request: access.Request{Scope: connection.Scope(), RequestID: wire.ID(), Operation: "runtime.list"}, DecisionID: wire.ID()}
}

func TestPeerAccessTransactions(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			s, err := Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close() }()
			r := peerRecord(t, s)
			hash := tokenHash(wire.ID())
			if err := s.CreatePeerAccess(ctx, hash, r, 30*time.Second); err != nil {
				t.Fatal(err)
			}
			if backend == "sqlite" {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
			}
			other := s
			if backend == "postgres" {
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			for _, field := range []string{"source", "owner", "target", "namespace", "request"} {
				ref := r.PeerReference
				switch field {
				case "source":
					ref.SourceBootID = wire.ID()
				case "owner":
					ref.OwnerBootID = wire.ID()
				case "target":
					ref.Target = wire.ID()
				case "namespace":
					ref.Namespace = "another issuer"
				case "request":
					ref.RequestDigest = tokenHash("another request")
				}
				if _, err := other.ConsumePeerAccess(ctx, hash, ref); !errors.Is(err, identity.ErrUnauthorized) {
					t.Fatal(field, err)
				}
			}
			var wins atomic.Int32
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					got, err := other.ConsumePeerAccess(ctx, hash, r.PeerReference)
					if err == nil {
						if got != r {
							t.Error("stored authority changed")
						}
						wins.Add(1)
					} else if !errors.Is(err, identity.ErrUnauthorized) {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			if wins.Load() != 1 {
				t.Fatal("capability was not consumed once", wins.Load())
			}
			// Another entry cannot rewrite the original principal's binding or subject.
			for _, field := range []string{"version", "subject", "binding", "owner"} {
				wrong := r
				switch field {
				case "version":
					wrong.Connection.AuthVersion++
				case "subject":
					wrong.Connection.Subject = "forged"
				case "binding":
					wrong.Connection.BindingRevision++
				case "owner":
					wrong.Connection.OwnerID = wire.ID()
				}
				wrong.Request.Scope = wrong.Connection.Scope()
				if err := other.CreatePeerAccess(ctx, tokenHash(wire.ID()), wrong, 30*time.Second); !errors.Is(err, identity.ErrUnauthorized) {
					t.Fatal(field, err)
				}
			}
			for range 64 {
				if err := s.CreatePeerAccess(ctx, tokenHash(wire.ID()), r, 30*time.Second); err != nil {
					t.Fatal(err)
				}
			}
			if err := other.CreatePeerAccess(ctx, tokenHash(wire.ID()), r, 30*time.Second); !errors.Is(err, identity.ErrLoginLimit) {
				t.Fatal("per-session bound exceeded", err)
			}
			if _, err := s.db.Exec(`UPDATE dune_peer_access SET expires_at=0 WHERE session_hash=$1`, r.Connection.SessionHash); err != nil {
				t.Fatal(err)
			}
			if err := other.CreatePeerAccess(ctx, hash, r, 30*time.Second); err != nil {
				t.Fatal("expired capacity not reclaimed", err)
			}
			if _, err := s.db.Exec(`UPDATE dune_peer_access SET expires_at=0 WHERE hash=$1`, hash); err != nil {
				t.Fatal(err)
			}
			if _, err := other.ConsumePeerAccess(ctx, hash, r.PeerReference); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("expired capability accepted", err)
			}
			if _, err := s.db.Exec(`DELETE FROM dune_sessions WHERE hash=$1`, r.Connection.SessionHash); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := other.db.QueryRow(`SELECT COUNT(*) FROM dune_peer_access`).Scan(&count); err != nil || count != 0 {
				t.Fatal("logout left capabilities", count, err)
			}
			if err := other.CreatePeerAccess(ctx, hash, r, 30*time.Second); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("revoked session delegated", err)
			}
		})
	}
}

func TestPeerAccessLostAcknowledgement(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "metadata")
	s, err := Open(ctx, storage.Config{SQLiteDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := peerRecord(t, s)
	connector, err := sqlite.NewConnector(filepath.Join(dir, "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	var commits atomic.Int32
	interrupted := &Store{db: sql.OpenDB(lostAckConnector{Connector: connector, commits: &commits})}
	defer interrupted.Close()
	hash := tokenHash(wire.ID())
	if err := interrupted.CreatePeerAccess(ctx, hash, r, 30*time.Second); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 1 {
		t.Fatal("unknown issuance hidden or retried", err)
	}
	if got, err := interrupted.ConsumePeerAccess(ctx, hash, r.PeerReference); !errors.Is(err, ErrCommitUnknown) || got != (authorization.PeerAccess{}) || commits.Load() != 2 {
		t.Fatal("unknown consumption granted access or retried", got, err)
	}
	if _, err := s.ConsumePeerAccess(ctx, hash, r.PeerReference); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("uncertain consumption replayed", err)
	}
}

func TestPeerAccessSchemaUpgrade(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			s, err := Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r := peerRecord(t, s)
			for _, statement := range []string{`DROP TABLE dune_instances`, `DROP TABLE dune_peer_access`, `UPDATE dune_schema SET version=10`, `CREATE INDEX dune_peer_access_session ON dune_sessions(hash)`} {
				if _, err := s.db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.migrate(ctx); err == nil {
				t.Fatal("partial migration accepted")
			}
			var version int
			if err := s.db.QueryRow(`SELECT version FROM dune_schema`).Scan(&version); err != nil || version != 10 {
				t.Fatal(version, err)
			}
			if _, err := s.db.Exec(`DROP INDEX dune_peer_access_session`); err != nil {
				t.Fatal(err)
			}
			if err := s.migrate(ctx); err != nil {
				t.Fatal("failed migration left its table", err)
			}
			if ok, err := s.CheckAccess(ctx, r.Connection, time.Now().Unix()); err != nil || !ok {
				t.Fatal("upgrade changed existing access", err)
			}
			if err := s.CreatePeerAccess(ctx, tokenHash(wire.ID()), r, 30*time.Second); err != nil {
				t.Fatal(err)
			}
			for _, ttl := range []time.Duration{0, -time.Second, 31 * time.Second} {
				if err := s.CreatePeerAccess(ctx, tokenHash(wire.ID()), r, ttl); !errors.Is(err, ErrInvalidArgument) {
					t.Fatal("unbounded lifetime accepted", ttl, err)
				}
			}
		})
	}
}
