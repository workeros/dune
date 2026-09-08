package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/deployment"
	"github.com/aiomni/dune/pkg/storage"
)

func TestDisabledManagedPreservesAdmissionFingerprint(t *testing.T) {
	store, err := metadata.Open(context.Background(), storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewLocal(store, true)
	urls, err := deployment.NewURLs("https://dune.example.test/tools/", "")
	if err != nil {
		t.Fatal(err)
	}
	options := Options{PublicURL: urls.PublicURL}
	actual, err := configurationFingerprint(options, urls, service, "")
	if err != nil {
		t.Fatal(err)
	}
	legacy := struct {
		Protocol, Path, Namespace, Policy, Version string
		Lifetime                                   time.Duration
		Registration, Cluster                      bool
	}{Protocol: api.Version, Path: urls.Path, Namespace: service.Namespace(), Lifetime: identity.SessionLifetime, Registration: true, Policy: "owner-v1"}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	if expected := hex.EncodeToString(sum[:]); actual != expected {
		t.Fatalf("disabled Managed changed existing admission identity: got %s want %s", actual, expected)
	}
	managed := options
	managed.Managed = &ManagedOptions{}
	if _, err := configurationFingerprint(managed, urls, service, "catalog-a"); err == nil {
		t.Fatal("Managed PostgreSQL identity accepted no declared configuration version")
	}
	managed.ConfigurationVersion = "provider-v1"
	a, err := configurationFingerprint(managed, urls, service, "catalog-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := configurationFingerprint(managed, urls, service, "catalog-b")
	if err != nil {
		t.Fatal(err)
	}
	if a == b || a == actual {
		t.Fatal("Managed catalog identity was not isolated from the legacy configuration")
	}
}
