package webapp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAccountsBindingsAndNoPlaintextCredentials(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "accounts")
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	a, cookieA, err := s.Register("Alice@example.com", "alice-test-password")
	if err != nil {
		t.Fatal(err)
	}
	b, cookieB, err := s.Register("bob@example.com", "bob-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if a.Email != "alice@example.com" || a.ID == b.ID {
		t.Fatal("invalid account identity")
	}
	if _, _, err := s.Register("alice@example.com", "another-password"); err == nil {
		t.Fatal("duplicate normalized account accepted")
	}
	if user, ok := s.Session(cookieA); !ok || user.ID != a.ID {
		t.Fatal("valid session missing")
	}
	if _, ok := s.Session("bad-cookie"); ok {
		t.Fatal("invalid cookie accepted")
	}
	if _, _, err := s.Login(a.Email, "wrong-password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("wrong password accepted")
	}
	if _, _, err := s.Login("nobody@example.com", "wrong-password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("nonexistent account accepted")
	}
	_, newCookie, err := s.Login(a.Email, "alice-test-password")
	if err != nil {
		t.Fatal(err)
	}
	bind, expires, err := s.IssueEnrollment(a.ID, "Linux machine")
	if err != nil || expires <= time.Now().Unix() {
		t.Fatal("binding command missing", err)
	}
	machine, credential, err := s.Enroll(bind, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if machine.Name != "Linux machine" || !s.Owns(a.ID, machine.ID) || s.Owns(b.ID, machine.ID) {
		t.Fatal("machine ownership incorrect")
	}
	if len(s.Machines(a.ID)) != 1 || len(s.Machines(b.ID)) != 0 {
		t.Fatal("cross-account machine listing")
	}
	if id, ok := s.MachineCredential(credential); !ok || id != machine.ID {
		t.Fatal("machine credential not recognized")
	}
	if _, _, err := s.Enroll(bind, "linux", "amd64"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("one-time enrollment replay succeeded")
	}
	if _, ok := s.MachineCredential(cookieA); ok {
		t.Fatal("browser cookie accepted as machine credential")
	}
	if _, ok := s.Session(credential); ok {
		t.Fatal("machine credential accepted as browser login")
	}
	if err := s.Revoke(b.ID, machine.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-account revoke succeeded")
	}
	if err := s.Logout(cookieA); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Session(cookieA); ok {
		t.Fatal("logout did not revoke cookie")
	}
	if _, ok := s.Session(cookieB); !ok {
		t.Fatal("logout affected another user")
	}
	if _, ok := s.Session(newCookie); !ok {
		t.Fatal("logout affected another browser")
	}
	contents, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"alice-test-password", "bob-test-password", cookieA, cookieB, newCookie, bind, credential} {
		if strings.Contains(string(contents), secret) {
			t.Fatal("metadata contains plaintext credential")
		}
	}
	info, err := os.Stat(filepath.Join(dir, "accounts.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("account file is not private", err)
	}
	if another, err := OpenStore(dir); err == nil {
		another.Close()
		t.Fatal("concurrent writer accepted")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Owns(a.ID, machine.ID) {
		t.Fatal("binding lost on service restart")
	}
	if _, ok := s.Session(newCookie); !ok {
		t.Fatal("login lost on service restart")
	}
	if err := s.Revoke(a.ID, machine.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.MachineCredential(credential); ok {
		t.Fatal("revoked machine credential remains usable")
	}
}

func TestConcurrentEnrollmentAndExpiry(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, cookie, err := s.Register("test@example.com", "test-password-1234")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.IssueEnrollment(u.ID, "one binding")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var wins atomic.Int32
	for range 8 {
		wg.Go(func() {
			if _, _, err := s.Enroll(token, "darwin", "arm64"); err == nil {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 || len(s.Machines(u.ID)) != 1 {
		t.Fatal("concurrent enrollment consumed token more than once")
	}
	token, _, err = s.IssueEnrollment(u.ID, "expired")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.update(func(data *metadata) error {
		pending := data.Enrollments[tokenHash(token)]
		pending.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		data.Enrollments[tokenHash(token)] = pending
		session := data.Sessions[tokenHash(cookie)]
		session.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		data.Sessions[tokenHash(cookie)] = session
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Enroll(token, "linux", "amd64"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expired enrollment accepted")
	}
	if _, ok := s.Session(cookie); ok {
		t.Fatal("expired login accepted")
	}
}

func TestStoreRejectsUnknownContentAndUnsafeDirectory(t *testing.T) {
	for _, path := range []string{"relative", "/"} {
		if s, err := OpenStore(path); err == nil {
			s.Close()
			t.Fatal("unsafe data directory accepted")
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"accounts":{},"sessions":{},"enrollments":{},"machines":{},"transcripts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenStore(dir); err == nil {
		s.Close()
		t.Fatal("unknown content fields accepted into metadata store")
	}
}
