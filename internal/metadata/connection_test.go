package metadata

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	public "github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/jackc/pgx/v5"
	"modernc.org/sqlite"
)

func TestPostgresCredentialRotation(t *testing.T) {
	config, admin, schema := postgresConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	roleName := "dune_rotate_" + wire.ID()
	role := pgx.Identifier{roleName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'old-test-only-password'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP OWNED BY "+role); err != nil {
			t.Error(err)
		}
		if _, err := admin.Exec(ctx, "DROP ROLE "+role); err != nil {
			t.Error(err)
		}
	})
	if _, err := admin.Exec(ctx, "GRANT USAGE, CREATE ON SCHEMA "+pgx.Identifier{schema}.Sanitize()+" TO "+role); err != nil {
		t.Fatal(err)
	}
	var password atomic.Value
	password.Store("old-test-only-password")
	var calls atomic.Int32
	config.Postgres.BeforeConnect = func(ctx context.Context, c *pgx.ConnConfig) error {
		calls.Add(1)
		c.User = roleName
		c.Password = password.Load().(string)
		c.RuntimeParams["search_path"] = schema
		return nil
	}
	s, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.db.SetMaxIdleConns(0)
	if _, err := admin.Exec(ctx, "ALTER ROLE "+role+" PASSWORD 'new-test-only-password'"); err != nil {
		t.Fatal(err)
	}
	password.Store("new-test-only-password")
	if err := s.db.PingContext(ctx); err != nil {
		t.Fatal("replacement connection did not use the new credential", err)
	}
	if calls.Load() < 2 {
		t.Fatal("credential provider ran only at startup")
	}
	password.Store("old-test-only-password")
	if err := s.db.PingContext(ctx); err == nil {
		t.Fatal("test PostgreSQL is not enforcing password authentication")
	}
	password.Store("new-test-only-password")
	if err := s.db.PingContext(ctx); err != nil {
		t.Fatal("pool could not recover from failed authentication", err)
	}
}

func TestPostgresConcurrentSchemaMigration(t *testing.T) {
	config, _, _ := postgresConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			s, err := Open(ctx, config)
			if err != nil {
				t.Error(err)
				return
			}
			s.Close()
		})
	}
	wg.Wait()
}

type lostAckConnector struct {
	driver.Connector
	commits *atomic.Int32
}

func (c lostAckConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return lostAckConn{Conn: conn, commits: c.commits}, nil
}

type lostAckConn struct {
	driver.Conn
	commits *atomic.Int32
}

func (c lostAckConn) Begin() (driver.Tx, error) {
	tx, err := c.Conn.Begin()
	if err != nil {
		return nil, err
	}
	return lostAckTx{Tx: tx, commits: c.commits}, nil
}

type lostAckTx struct {
	driver.Tx
	commits *atomic.Int32
}

func (t lostAckTx) Commit() error {
	if err := t.Tx.Commit(); err != nil {
		return err
	}
	t.commits.Add(1)
	return io.ErrUnexpectedEOF // Durable commit happened; its acknowledgement is lost.
}

func TestCommitAcknowledgementLossIsNotReplayed(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "metadata")
	s, err := Open(ctx, storage.Config{SQLiteDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	connector, err := sqlite.NewConnector(filepath.Join(dir, "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	var commits atomic.Int32
	interrupted := &Store{db: sql.OpenDB(lostAckConnector{Connector: connector, commits: &commits})}
	defer interrupted.Close()
	a := identity.Account{User: identity.User{ID: wire.ID(), Email: "committed@example.test"}, Salt: "salt", PasswordHash: "hash"}
	err = interrupted.RegisterAccount(ctx, a, "session-hash", time.Now().Add(time.Hour).Unix())
	if !errors.Is(err, ErrCommitUnknown) || commits.Load() != 1 {
		t.Fatalf("commit loss: %v, attempts %d", err, commits.Load())
	}
	if got, err := s.ReadAccount(ctx, a.Email); err != nil || got.ID != a.ID {
		t.Fatal("failure injection did not persist its commit", err)
	}
	if got, err := s.ReadSession(ctx, "session-hash", time.Now().Unix()); err != nil || got.ID != a.ID {
		t.Fatal("commit was incomplete", err)
	}
	request := public.LinkRequest{RequestID: wire.ID(), Actor: "admin:test", PrincipalID: a.ID, Namespace: "issuer", Subject: "subject", Reason: "verified migration"}
	if _, err := interrupted.LinkIdentity(ctx, request); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 2 {
		t.Fatal("link commit loss was hidden or replayed", err)
	}
	if record, err := s.IdentityLink(ctx, request.RequestID); err != nil || record.LinkRequest != request {
		t.Fatal("cannot reconcile committed link", err)
	}
	if _, err := s.ReadSession(ctx, "session-hash", time.Now().Unix()); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("committed link did not revoke original session", err)
	}
	if err := s.CreateSession(ctx, a.ID, "fresh-session", time.Now().Add(time.Hour).Unix(), 32); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkIdentity(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadSession(ctx, "fresh-session", time.Now().Unix()); err != nil {
		t.Fatal("reconciled retry revoked new session", err)
	}
	enrollment, _, err := s.IssueEnrollment(ctx, a.ID, "access machine")
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := s.Enroll(ctx, enrollment, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := interrupted.CreateAccess(ctx, "ticket-hash", "fresh-session", a.ID, "", machine.ID, time.Now().Add(time.Minute).Unix()); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 3 {
		t.Fatal("access issuance commit loss was hidden or replayed", err)
	}
	if _, err := interrupted.ConsumeAccess(ctx, "ticket-hash", "", time.Now().Unix()); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 4 {
		t.Fatal("access consumption commit loss was hidden or replayed", err)
	}
	if _, err := s.ConsumeAccess(ctx, "ticket-hash", "", time.Now().Unix()); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("uncertain consumption allowed replay", err)
	}
	cookie, proof := wire.ID()+wire.ID(), wire.ID()+wire.ID()
	if err := s.CreateSession(ctx, a.ID, tokenHash(cookie), time.Now().Add(time.Hour).Unix(), 32); err != nil {
		t.Fatal(err)
	}
	cli, err := s.BeginCLI(ctx, tokenHash(proof), "https://cli.test/", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmCLI(ctx, cli.ID, "https://cli.test/", "", cli.Code, cookie, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if session, err := interrupted.ConsumeCLI(ctx, cli.ID, "https://cli.test/", "", proof); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 5 || session.Token != "" {
		t.Fatal("CLI commit loss returned or replayed a credential", err)
	}
	if _, err := s.ConsumeCLI(ctx, cli.ID, "https://cli.test/", "", proof); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("uncertain CLI consumption allowed replay", err)
	}
	var children int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM dune_sessions WHERE parent_hash=$1`, tokenHash(cookie)).Scan(&children); err != nil || children != 1 {
		t.Fatal("CLI commit did not persist exactly one child", err)
	}
}
