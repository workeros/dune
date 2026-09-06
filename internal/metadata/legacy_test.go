package metadata

import (
	"bytes"
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/storage"
)

func legacyFixture(t *testing.T) (legacyData, string, string, string) {
	t.Helper()
	id := strings.Repeat("1", 32)
	machineID := strings.Repeat("2", 32)
	cookie, credential, enrollment := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	salt := strings.Repeat("3", 32)
	password, err := pbkdf2.Key(sha256.New, "legacy-test-password", []byte(salt), 600000, 32)
	if err != nil {
		t.Fatal(err)
	}
	d := legacyData{
		Version:     1,
		Accounts:    map[string]identity.Account{id: {User: identity.User{ID: id, Email: "legacy@example.test"}, Salt: salt, PasswordHash: hex.EncodeToString(password)}},
		Sessions:    map[string]legacySession{tokenHash(cookie): {UserID: id, ExpiresAt: time.Now().Add(time.Hour).Unix()}, tokenHash("expired"): {UserID: id, ExpiresAt: 1}},
		Enrollments: map[string]legacyEnrollment{tokenHash(enrollment): {UserID: id, Name: "pending machine", ExpiresAt: time.Now().Add(time.Hour).Unix()}},
		Machines:    map[string]legacyMachine{machineID: {ID: machineID, Name: "existing machine", OS: "linux", Arch: "amd64", CreatedAt: 1700000000, OwnerID: id, CredentialHash: tokenHash(credential)}},
	}
	return d, cookie, credential, enrollment
}

func writeLegacy(t *testing.T, d legacyData) (string, []byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	contents, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), contents, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, contents
}

func TestLegacyImportTransactionsAndPreservedIdentities(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			d, cookie, credential, enrollment := legacyFixture(t)
			dir, original := writeLegacy(t, d)
			source, err := ReadLegacy(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			if again, err := ReadLegacy(ctx, dir); err == nil {
				again.Close()
				t.Fatal("source directory was not held offline")
			}
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "target")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			s, err := Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			// Inject a persistence failure after valid source loading and after
			// accounts/sessions are inserted. Every destination table must roll back.
			id := strings.Repeat("2", 32)
			machine := source.data.Machines[id]
			originalOwner := machine.OwnerID
			machine.OwnerID = "missing"
			source.data.Machines[id] = machine
			if _, err := s.ImportLegacy(ctx, source); !errors.Is(err, ErrConflict) {
				t.Fatalf("injected failure: %v", err)
			}
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM dune_principals`).Scan(&count); err != nil || count != 0 {
				t.Fatal("failed import partially committed", err)
			}
			machine.OwnerID = originalOwner
			source.data.Machines[id] = machine
			report, err := s.ImportLegacy(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			if report != (ImportCounts{Accounts: 1, Sessions: 2, Enrollments: 1, Machines: 1}) {
				t.Fatalf("import counts: %+v", report)
			}
			if _, err := s.ImportLegacy(ctx, source); err == nil {
				t.Fatal("import overwrote a populated destination")
			}
			local := identity.NewLocal(s, true)
			user, err := local.Authenticate(ctx, cookie)
			if err != nil || user.ID != originalOwner {
				t.Fatal("original session changed", err)
			}
			if _, _, err := local.Login(ctx, user.Email, "legacy-test-password"); err != nil {
				t.Fatal("original password changed", err)
			}
			if _, err := s.ReadSession(ctx, tokenHash("expired"), time.Now().Unix()); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("import renewed an expired session")
			}
			if got, err := s.MachineCredential(ctx, credential); err != nil || got != id {
				t.Fatal("original machine identity changed", err)
			}
			var runner string
			if err := s.db.QueryRow(`SELECT runner_id FROM dune_machines WHERE id=$1`, id).Scan(&runner); err != nil || runner != id {
				t.Fatal("legacy machine did not become a stable Attached Runner", err)
			}
			if row, err := s.Runner(ctx, user.ID, id); err != nil || row.ID != id || row.Binding == nil || row.Binding.MachineID != id || row.Binding.Revision != 1 {
				t.Fatal("legacy Runner lookup changed identity", err)
			}
			if _, _, err := s.Enroll(ctx, enrollment, "darwin", "arm64"); err != nil {
				t.Fatal("pending enrollment was lost", err)
			}
			if _, _, err := s.Enroll(ctx, enrollment, "darwin", "arm64"); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("pending enrollment became reusable")
			}
			actual, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
			if err != nil || !bytes.Equal(actual, original) {
				t.Fatal("source metadata was modified", err)
			}
		})
	}
}

func TestLegacyValidationBeforeImport(t *testing.T) {
	d, _, _, _ := legacyFixture(t)
	contents, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]byte{
		"duplicate key":   bytes.Replace(contents, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"case alias":      append(append([]byte{}, contents[:len(contents)-1]...), []byte(`,"Accounts":{}}`)...),
		"unknown content": append(append([]byte{}, contents[:len(contents)-1]...), []byte(`,"transcripts":[]}`)...),
		"bad reference":   bytes.ReplaceAll(contents, []byte(`"user_id":"`+strings.Repeat("1", 32)+`"`), []byte(`"user_id":"missing"`)),
		"bad identity":    bytes.Replace(contents, []byte(`"id":"`+strings.Repeat("1", 32)+`"`), []byte(`"id":"missing"`), 1),
		"trailing object": append(append([]byte{}, contents...), []byte(`{}`)...),
		"future binding":  bytes.Replace(contents, []byte(`"credential_hash":`), []byte(`"runner_id":"another-runner","credential_hash":`), 1),
		"deep nesting":    []byte(strings.Repeat("[", 70) + "0" + strings.Repeat("]", 70)),
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := writeLegacy(t, d)
			if err := os.WriteFile(filepath.Join(dir, "accounts.json"), bad, 0600); err != nil {
				t.Fatal(err)
			}
			if source, err := ReadLegacy(context.Background(), dir); err == nil {
				source.Close()
				t.Fatal("corrupt source accepted")
			}
			lock, err := lockDirectory(dir)
			if err != nil {
				t.Fatal("failed validation retained its lock", err)
			}
			lock.Close()
		})
	}
}
