package tmux

import (
	"context"
	"fmt"
	"syscall"
	"testing"
	"time"
)

func TestStoppedServerQueriesReturnBeforeResume(t *testing.T) {
	s := server(t)
	_ = session(t, s, "a", "sleep 60", nil)
	before := s.Versions(t.Context())
	if before.ServerState != "running" || before.ServerPID <= 1 {
		t.Fatal(before)
	}
	if err := syscall.Kill(before.ServerPID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	// Registered after server(t)'s cleanup so even a failed query resumes this
	// private server before Close tries to communicate with it.
	t.Cleanup(func() { _ = syscall.Kill(before.ServerPID, syscall.SIGCONT) })
	for _, query := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"panes", func(ctx context.Context) error {
			if _, err := s.HostPanes(ctx); err == nil {
				return fmt.Errorf("stopped server answered pane discovery")
			}
			return nil
		}},
		{"versions", func(ctx context.Context) error {
			info := s.Versions(ctx)
			if info.ServerState != "unavailable" || info.ErrorCode != "TMUX_SERVER_UNAVAILABLE" {
				return fmt.Errorf("unexpected stopped server version: %+v", info)
			}
			return nil
		}},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		done := make(chan error, 1)
		go func() { done <- query.run(ctx) }()
		select {
		case err := <-done:
			cancel()
			if err != nil {
				t.Fatal(query.name, err)
			}
		case <-time.After(2 * time.Second):
			cancel()
			_ = syscall.Kill(before.ServerPID, syscall.SIGCONT)
			t.Fatalf("%s query waited for the server to resume after context cancellation", query.name)
		}
	}
	if err := syscall.Kill(before.ServerPID, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	after := s.Versions(t.Context())
	if after.ServerState != "running" || after.ServerPID != before.ServerPID {
		t.Fatal("query cancellation changed the original server", before, after)
	}
}
