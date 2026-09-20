package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionsObserveExistingServerWithoutStartingOne(t *testing.T) {
	s := server(t)
	before, err := os.ReadFile(s.config)
	if err != nil {
		t.Fatal(err)
	}
	absent := s.Versions(t.Context())
	if absent.ClientVersion == "" || absent.ServerState != "absent" || absent.ServerPID != 0 || absent.ServerVersion != "" {
		t.Fatal(absent)
	}
	if _, err := os.Lstat(s.Socket); !os.IsNotExist(err) {
		t.Fatal("version probe started tmux", err)
	}
	_ = session(t, s, "9", "sleep 60", nil)
	first := s.Versions(t.Context())
	if first.ServerState != "running" || first.ServerPID <= 1 || first.ServerVersion == "" || first.ServerVersion != first.ClientVersion {
		t.Fatal(first)
	}
	// A newly selected client reports a different -V string but delegates the
	// actual IPC read to the original bundled client. Server diagnostics must
	// still come from the already-running process, not this disk-side string.
	wrapper := filepath.Join(t.TempDir(), "tmux-client")
	body := "#!/bin/sh\nif [ \"$1\" = -V ]; then echo 'tmux client-fixture'; exit; fi\nexec " + quote(s.Binary) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	observer := &Server{Binary: wrapper, Socket: s.Socket}
	second := observer.Versions(t.Context())
	if second.ClientVersion != "client-fixture" || second.ServerVersion != first.ServerVersion || second.ServerPID != first.ServerPID || second.ServerState != "running" {
		t.Fatal(first, second)
	}
	after, err := os.ReadFile(s.config)
	if err != nil || string(after) != string(before) {
		t.Fatal("version probe modified config", err)
	}
	if !strings.HasPrefix(second.ServerVersion, "3.") {
		t.Fatal("unexpected bundled tmux version", second)
	}
}
