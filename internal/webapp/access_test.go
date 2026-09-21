package webapp

import (
	"context"
	"errors"
	"github.com/aiomni/dune/internal/metadata"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/gateway"
	publicidentity "github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
)

func TestConnectionIdentityIsolationAndTicketConsumption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := OpenStore(filepath.Join(t.TempDir(), "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	local := identity.NewLocal(store, true)
	access := authorization.New(ctx, local, store, nil, nil)
	type connection struct {
		user               publicidentity.User
		cookie, credential string
		machine            metadata.Machine
		grant              *authorization.ClientGrant
	}
	connections := make([]connection, 2)
	for i, email := range []string{"first@example.test", "second@example.test"} {
		c := &connections[i]
		c.user, c.cookie, err = local.Register(ctx, email, "a-strong-test-password")
		if err != nil {
			t.Fatal(err)
		}
		_, token, _, err := store.IssueEnrollment(context.Background(), c.user.ID, "machine")
		if err != nil {
			t.Fatal(err)
		}
		c.machine, c.credential, err = store.Enroll(context.Background(), token, "linux", "amd64")
		if err != nil {
			t.Fatal(err)
		}
		c.grant, err = access.ClientRunner(ctx, c.cookie, runner.Binding{RunnerID: c.machine.RunnerID, MachineID: c.machine.ID, FabricID: "attached", Revision: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer c.grant.Close()
		binding, handler, err := access.Authorize(c.grant.Token())
		if err != nil || binding.Target != c.machine.ID || binding.Role != gateway.RoleSDK || handler == nil {
			t.Fatalf("connection lost its fixed identity: %+v %v", binding, err)
		}
		if _, _, err := access.Authorize(c.grant.Token()); err == nil {
			t.Fatal("ticket replay accepted")
		}
		c.grant.Close()
		c.grant.Close()
		if !c.grant.Valid() {
			t.Fatal("disposing a ticket revoked its established connection")
		}
		binding, _, err = access.Authorize(c.credential)
		if err != nil || binding.Role != gateway.RoleDaemon || binding.Target != c.machine.ID {
			t.Fatal("machine identity bound to the wrong role/target")
		}
		if _, _, err := access.Authorize(c.cookie); !errors.Is(err, publicidentity.ErrUnauthorized) {
			t.Fatal("browser cookie accepted as tunnel credential")
		}
		if _, err := access.ClientRunner(ctx, c.credential, runner.Binding{RunnerID: c.machine.RunnerID, MachineID: c.machine.ID, FabricID: "attached", Revision: 1}); !errors.Is(err, publicidentity.ErrUnauthorized) {
			t.Fatal("machine credential accepted as user")
		}
	}
	a, b := connections[0], connections[1]
	if _, err := access.ClientRunner(ctx, a.cookie, runner.Binding{RunnerID: b.machine.RunnerID, MachineID: b.machine.ID, FabricID: "attached", Revision: 1}); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal("cross-account target accepted")
	}
	if err := local.Logout(ctx, a.cookie); err != nil {
		t.Fatal(err)
	}
	if a.grant.Valid() || !b.grant.Valid() {
		t.Fatal("revocation was not isolated to its original user session")
	}
	if err := store.Revoke(context.Background(), b.user.ID, b.machine.ID); err != nil {
		t.Fatal(err)
	}
	if b.grant.Valid() {
		t.Fatal("revoked ownership remained usable")
	}
	if _, _, err := access.Authorize(b.credential); err == nil {
		t.Fatal("revoked machine credential remained usable")
	}
	cancel()
	if _, _, err := access.Authorize(a.credential); !errors.Is(err, context.Canceled) {
		t.Fatal("shutdown still authorized a machine")
	}
}
