package host

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
)

type attachedIdentity struct{}

func (attachedIdentity) Authenticate(context.Context, string) (identity.Authentication, error) {
	return identity.Authentication{}, identity.ErrUnauthorized
}
func (attachedIdentity) Logout(context.Context, string) error { return nil }
func (attachedIdentity) Namespace() string                    { return "sanddance" }
func (attachedIdentity) LoginMethods(string) []identity.LoginMethod {
	return nil
}

func TestAttachedRunnerManagerExposesFactsAndOwnsCancellation(t *testing.T) {
	app, err := Open(context.Background(), Options{DataDir: filepath.Join(t.TempDir(), "metadata"), PublicURL: "http://dune.example.test/", Identity: attachedIdentity{}})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	creator := identity.User{ID: "email:creator@example.test", Kind: "email", Namespace: "sanddance", Subject: "creator@example.test"}
	logical, _, _, err := app.store.IssueTenantEnrollment(context.Background(), creator, "tenant-1", "Pending Devbox", "external-session")
	if err != nil {
		t.Fatal(err)
	}
	manager := app.AttachedRunnerManager()
	facts, err := manager.Get(context.Background(), "tenant-1", logical.ID)
	if err != nil || facts.OwnerID != "tenant-1" || facts.Runner.ID != logical.ID || facts.Runner.Binding != nil || facts.CreatedBy.ID != creator.ID {
		t.Fatal("host lost Attached facts", facts, err)
	}
	if active, err := manager.HasActive(context.Background(), "tenant-1"); err != nil || !active {
		t.Fatal("host lost active Attached fact", active, err)
	}
	if err := manager.CancelEnrollment(context.Background(), "tenant-1", logical.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Get(context.Background(), "tenant-1", logical.ID); !errors.Is(err, ErrAttachedRunnerNotFound) {
		t.Fatal("cancelled Runner did not return public not-found error", err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.HasActive(context.Background(), "tenant-1"); err == nil {
		t.Fatal("closed App admitted Attached operation")
	}
}

func TestAttachedRunnerManagerRejectsIncompleteScope(t *testing.T) {
	app, err := Open(context.Background(), Options{DataDir: filepath.Join(t.TempDir(), "metadata"), PublicURL: "http://dune.example.test/", Identity: attachedIdentity{}})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	manager := app.AttachedRunnerManager()
	if _, err := manager.Get(context.Background(), "", "runner-1"); !errors.Is(err, identity.ErrInvalidArgument) {
		t.Fatal("Get accepted an incomplete owner scope", err)
	}
	if _, err := manager.HasActive(context.Background(), ""); !errors.Is(err, identity.ErrInvalidArgument) {
		t.Fatal("HasActive accepted an incomplete owner scope", err)
	}
	if err := manager.CancelEnrollment(context.Background(), "tenant-1", ""); !errors.Is(err, identity.ErrInvalidArgument) {
		t.Fatal("CancelEnrollment accepted an incomplete Runner scope", err)
	}
}

func TestAttachedRunnerManagerPreservesEnrolledBinding(t *testing.T) {
	app, err := Open(context.Background(), Options{DataDir: filepath.Join(t.TempDir(), "metadata"), PublicURL: "http://dune.example.test/", Identity: attachedIdentity{}})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	creator := identity.User{ID: "email:creator@example.test", Kind: "email", Namespace: "sanddance", Subject: "creator@example.test"}
	logical, token, _, err := app.store.IssueTenantEnrollment(context.Background(), creator, "tenant-1", "Bound Devbox", "external-session")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.store.Enroll(context.Background(), token, "linux", "amd64"); err != nil {
		t.Fatal(err)
	}
	manager := app.AttachedRunnerManager()
	if err := manager.CancelEnrollment(context.Background(), "tenant-1", logical.ID); !errors.Is(err, runner.ErrBindingChanged) {
		t.Fatal("host detached an enrolled Runner", err)
	}
	facts, err := manager.Get(context.Background(), "tenant-1", logical.ID)
	if err != nil || facts.Runner.Binding == nil {
		t.Fatal("enrolled binding was lost", facts, err)
	}
}
