package metadata

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/jackc/pgx/v5"
)

func TestPostgresBackupRestore(t *testing.T) {
	config, admin, schema := postgresConfig(t)
	binaries := make(map[string]string)
	for _, name := range []string{"pg_dump", "pg_restore"} {
		path := name
		if dir := os.Getenv("DUNE_TEST_PG_BIN"); dir != "" {
			path = filepath.Join(dir, name)
		}
		binary, err := exec.LookPath(path)
		if err != nil {
			t.Skip("PostgreSQL backup tools not configured; set DUNE_TEST_PG_BIN")
		}
		binaries[name] = binary
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	local := identity.NewLocal(s, true)
	user, cookie, err := local.Register(ctx, "restore@example.test", "restore-test-password")
	if err != nil {
		t.Fatal(err)
	}
	enrollment, _, err := s.IssueEnrollment(ctx, user.ID, "restored machine")
	if err != nil {
		t.Fatal(err)
	}
	machine, credential, err := s.Enroll(ctx, enrollment, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotRecords(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Keep the password out of process arguments and failure output. Only this
	// test's randomly named schema is ever dumped, dropped or restored.
	address, err := url.Parse(config.Postgres.URL)
	if err != nil {
		t.Fatal("invalid test PostgreSQL URL")
	}
	connection, err := pgx.ParseConfig(config.Postgres.URL)
	if err != nil {
		t.Fatal("invalid test PostgreSQL configuration")
	}
	if address.User != nil {
		address.User = url.User(address.User.Username())
	}
	query := address.Query()
	query.Del("password")
	address.RawQuery = query.Encode()
	run := func(name string, args ...string) []byte {
		t.Helper()
		command := exec.CommandContext(ctx, binaries[name], args...)
		command.Env = append(os.Environ(), "PGPASSWORD="+connection.Password)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v (%s)", name, err, out)
		}
		return out
	}
	archive := filepath.Join(t.TempDir(), "metadata.dump")
	file, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	run("pg_dump", "--dbname="+address.String(), "--no-password", "--schema="+schema, "--format=custom", "--file="+archive)
	if manifest := run("pg_restore", "--list", archive); !strings.Contains(string(manifest), "dune_machines") {
		t.Fatal("backup omits machine identity")
	}
	if _, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
		t.Fatal(err)
	}
	run("pg_restore", "--dbname="+address.String(), "--no-password", "--single-transaction", "--exit-on-error", "--no-owner", "--no-privileges", archive)
	s, err = Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, snapshotRecords(t, s)) {
		t.Fatal("restored metadata differs from backup")
	}
	local = identity.NewLocal(s, true)
	if got, err := local.Authenticate(ctx, cookie); err != nil || got.ID != user.ID {
		t.Fatal("restored session invalid", err)
	}
	if id, err := s.MachineCredential(ctx, credential); err != nil || id != machine.ID {
		t.Fatal("restored machine identity invalid", err)
	}
}
