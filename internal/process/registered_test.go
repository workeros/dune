package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRegisteredGuardianCannotSpawnBeforeDurableEvidence(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(strconv.FormatBool(allow), func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "agent-ran")
			called := false
			p, err := StartRegistered([]string{"/bin/sh", "-c", "echo ran > agent-ran"}, dir, nil, func(group int) error {
				called = true
				// The guardian is alive but has no child until this callback commits.
				children, psErr := exec.Command("ps", "-o", "pid=", "-P", strconv.Itoa(group)).Output()
				var exit *exec.ExitError
				if (psErr != nil && (!errors.As(psErr, &exit) || exit.ExitCode() != 1)) || strings.TrimSpace(string(children)) != "" {
					t.Fatalf("Agent spawned before registration: %s %v", children, psErr)
				}
				if !allow {
					return errors.New("commit acknowledgement unknown")
				}
				return nil
			})
			if !called {
				t.Fatal("guardian bypassed registration")
			}
			if !allow {
				if err == nil || p != nil {
					t.Fatal("unconfirmed registration started an Agent")
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("rejected registration executed child", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			go func() { _, _ = io.Copy(io.Discard, p.Output); p.Output.Close() }()
			go func() { _, _ = io.Copy(io.Discard, p.Stderr); p.Stderr.Close() }()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := p.WaitGroupExit(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("admitted child never ran", err)
			}
		})
	}
}

func TestRegisteredGuardianOwner(t *testing.T) {
	dir := os.Getenv("DUNE_REGISTERED_GUARD_TEST")
	if dir == "" {
		return
	}
	_, _ = StartRegistered([]string{"/bin/sh", "-c", "echo ran > agent-ran"}, dir, nil, func(group int) error {
		if err := os.WriteFile(filepath.Join(dir, "group"), []byte(strconv.Itoa(group)), 0600); err != nil {
			os.Exit(72)
		}
		os.Exit(71) // Simulate host death after registration but before launch release.
		return nil
	})
	os.Exit(73)
}

func TestRegisteredGuardianOwnerDeathNeverLaunchesChild(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRegisteredGuardianOwner$")
	cmd.Env = append(os.Environ(), "DUNE_REGISTERED_GUARD_TEST="+dir)
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 71 {
		t.Fatal("owner did not reach the registration barrier", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "group"))
	if err != nil {
		t.Fatal(err)
	}
	group, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	boot, err := BootID()
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		absent, err := Absent(boot, cmd.Process.Pid, group)
		if err != nil {
			t.Fatal(err)
		}
		if absent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(fmt.Sprintf("guardian %d survived owner death", group))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "agent-ran")); !os.IsNotExist(err) {
		t.Fatal("owner death released Agent execution", err)
	}
}
