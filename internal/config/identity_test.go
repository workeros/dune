package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateIdentityConfigurationRejectsInvalidInput(t *testing.T) {
	file := filepath.Join(t.TempDir(), "identity.yaml")
	for _, source := range []string{
		"oidc:\n  client_secret: [private-secret]\n",
		"oidc:\n  unknown: private-secret\n",
		"oidc: {}\n---\nsecret: private-secret\n",
		"oidc: {}\nsession_lifetime: private-secret\n",
		"oidc: {}\nsession_lifetime: 25h\n",
		"oidc: {}\nsession_lifetime: 1s\n",
		"oidc:\n  issuer: http://example.test\n  client_id: review\n  client_secret: private-secret\n",
		"{}",
	} {
		if err := os.WriteFile(file, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Identity(context.Background(), file); err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatal("invalid identity configuration accepted or disclosed its secret", err)
		}
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Identity(context.Background(), file); err == nil {
		t.Fatal("shared identity configuration accepted")
	}
}
