package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/login"
)

func TestPrivateHumanCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "human", "login.json")
	unlock, err := LockUserCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if release, err := LockUserCredentials(path); err == nil {
		release()
		t.Fatal("concurrent credential mutation allowed")
	}
	value := UserCredentials{Session: login.Session{Site: "https://dune.test/tools/", Token: "private-test-token", PrincipalID: "principal", ExpiresAt: 1234}, Certificate: "/private/trust.pem"}
	if err := SaveUserCredentials(path, value); err != nil {
		t.Fatal(err)
	}
	if actual, err := LoadUserCredentials(path); err != nil || actual != value {
		t.Fatal("private credential round trip failed", err)
	}
	if err := SaveUserCredentials(path, UserCredentials{}); err == nil {
		t.Fatal("existing credential overwritten")
	}
	link := path + ".symlink"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUserCredentials(link); err == nil {
		t.Fatal("symlink credential accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUserCredentials(path); err == nil {
		t.Fatal("shared credential accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"unknown":"private-test-token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUserCredentials(path); err == nil || strings.Contains(err.Error(), "private-test-token") {
		t.Fatal("bad credential accepted or disclosed", err)
	}
}
