package access

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

func TestScrollbackProtocolAndAuthorization(t *testing.T) {
	var denied atomic.Bool
	seen := make(chan Request, 32)
	ctx, c := policyFixture(t, checkFunc(func(ctx context.Context, r Request) (Decision, error) {
		decision, err := (Owner{}).Check(ctx, r)
		if r.Operation == "runtime.scrollback" {
			seen <- r
			decision.Allowed = decision.Allowed && !denied.Load()
		}
		return decision, err
	}), nil)
	if !slices.Contains(c.Binding.Capabilities, "runtime.scrollback") {
		t.Fatal("scrollback capability missing")
	}
	runtime, stream, err := c.Start(ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "pty", WorkingDirectory: t.TempDir(), Start: api.Command{Argv: []string{"/bin/sh", "-c", "seq 1 80; printf SCROLLBACK_END; exit 7"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		current, err := c.Get(ctx, runtime)
		if err != nil {
			t.Fatal(err)
		}
		if current.ExitCode != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, err := c.ScrollbackTerminal(ctx, runtime, api.TerminalScrollbackRequest{})
	if err != nil || !strings.Contains(got.Content, "1\n") || !strings.Contains(got.Content, "SCROLLBACK_END") || got.Truncated {
		t.Fatalf("retained snapshot: %+v %v", got, err)
	}
	request := <-seen
	if request.Scope != testScope() || request.Runtime.ID != runtime.ID || request.Runtime.Incarnation != runtime.Incarnation || request.Runtime.Generation != runtime.Generation {
		t.Fatalf("lost authenticated identity: %+v", request)
	}
	checkError := func(err error, code string) {
		t.Helper()
		var failure *api.Error
		if !errors.As(err, &failure) || failure.Code != code {
			t.Fatalf("want %s, got %v", code, err)
		}
	}
	for _, limit := range []int{-1, api.MaxTerminalScrollbackLines + 1} {
		_, err := c.ScrollbackTerminal(ctx, runtime, api.TerminalScrollbackRequest{Limit: limit})
		checkError(err, "INVALID_ARGUMENT")
	}
	for _, change := range []func(*api.Runtime){
		func(r *api.Runtime) { *r = api.Runtime{} },
		func(r *api.Runtime) { r.ID = wire.ID() },
		func(r *api.Runtime) { r.Incarnation = wire.ID() },
		func(r *api.Runtime) { r.Generation++ },
	} {
		invalid := runtime
		change(&invalid)
		_, err := c.ScrollbackTerminal(ctx, invalid, api.TerminalScrollbackRequest{})
		checkError(err, "STALE_RUNTIME")
	}
	denied.Store(true)
	got, err = c.ScrollbackTerminal(ctx, runtime, api.TerminalScrollbackRequest{})
	if err == nil || got.Content != "" {
		t.Fatalf("denied read leaked terminal content: %+v %v", got, err)
	}
	denied.Store(false)
	if err := c.Stop(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	_, err = c.ScrollbackTerminal(ctx, runtime, api.TerminalScrollbackRequest{})
	checkError(err, "STALE_RUNTIME")
}
