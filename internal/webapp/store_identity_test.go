package webapp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
)

func TestIdentityAtomicPersistenceAndCancellation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "accounts")
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	local := identity.NewLocal(store, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := local.Register(ctx, "owner@example.test", "a-strong-test-password"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled registration: %v", err)
	}
	// Fail persistence after the in-memory transaction created both objects.
	// Neither object may become visible and the email must remain available.
	store.dir = filepath.Join(dir, "missing")
	if _, _, err := local.Register(context.Background(), "owner@example.test", "a-strong-test-password"); err == nil {
		t.Fatal("registration succeeded without durable storage")
	}
	if len(store.data.Accounts) != 0 || len(store.data.Sessions) != 0 {
		t.Fatal("failed registration partially created an account/session")
	}
	store.dir = dir
	user, token, err := local.Register(context.Background(), "owner@example.test", "a-strong-test-password")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 32; i++ {
		if err := store.CreateSession(context.Background(), user.ID, tokenHash(fmt.Sprint(i)), time.Now().Add(time.Hour).Unix(), 32); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := local.Login(context.Background(), user.Email, "a-strong-test-password"); !errors.Is(err, identity.ErrSessionLimit) {
		t.Fatalf("session limit: %v", err)
	}
	if len(store.data.Sessions) != 32 {
		t.Fatal("failed login added a session")
	}
	if err := local.Logout(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := local.Login(context.Background(), user.Email, "a-strong-test-password"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Authenticate(context.Background(), token); err == nil || errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("storage failure was confused with invalid credentials: %v", err)
	}
}
