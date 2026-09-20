package tests

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
)

func TestLaunchAdmissionSurvivesMissingFirstResponse(t *testing.T) {
	for _, interrupt := range []string{"caller-cancel", "connector-kill-during-setup"} {
		t.Run(interrupt, func(t *testing.T) {
			h := start(t)
			mock := filepath.Join(h.dir, "mock-acp")
			if output, err := exec.Command("go", "build", "-o", mock, "../samples/mock-acp").CombinedOutput(); err != nil {
				t.Fatalf("mock build: %s %v", output, err)
			}
			key := api.SubmissionKey{SubmissionID: "launch-saved-before-send", Target: api.SubmissionTarget{OwnerID: "standalone-owner", RunnerID: "standalone-runner", FabricID: "standalone-fabric", MachineID: h.c.Target, BindingRevision: 1}}
			before, err := h.client.QuerySubmission(h.ctx, key)
			must(t, err)
			if before.Admission != api.SubmissionUnknown {
				t.Fatal("read before launch closed its admission path", before)
			}
			p := profile(h.dir, "acp", mock)
			p.ManagedACP = true
			processLog := filepath.Join(h.dir, "process.log")
			p.Env = map[string]string{"DUNE_MOCK_PROCESS_LOG": processLog}
			p.Setup.Steps = []api.Command{{Name: "barrier", Argv: []string{"/bin/sh", "-c", "echo once >> launch-count; while [ ! -f launch-release ]; do sleep 0.02; done"}}}
			request := api.StartRequest{SubmissionKey: key, Profile: p}
			ctx, cancel := context.WithCancel(h.ctx)
			defer cancel()
			returned := make(chan error, 1)
			go func() {
				_, stream, err := h.client.Start(ctx, request)
				if stream != nil {
					stream.Close()
				}
				returned <- err
			}()
			var receipt api.SubmissionReceipt
			for deadline := time.Now().Add(8 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				receipt, err = h.client.QuerySubmission(h.ctx, key)
				must(t, err)
				_, markerErr := os.Stat(filepath.Join(h.dir, "launch-count"))
				if receipt.Stage == "setup" && markerErr == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("setup never reached durable admission", receipt)
				}
			}
			reserved := *receipt.Runtime
			if receipt.Admission != api.SubmissionAccepted || reserved.State != "starting" {
				t.Fatal("reserved identity was reported as a started Agent", receipt)
			}
			if interrupt == "caller-cancel" {
				cancel()
			} else {
				h.stopProcess("fabricd", syscall.SIGKILL)
			}
			var submissionError *api.SubmissionError
			select {
			case err := <-returned:
				if !errors.As(err, &submissionError) || submissionError.Key != key {
					t.Fatal("missing startup response lost original caller key", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("interrupted Start remained blocked")
			}
			must(t, os.WriteFile(filepath.Join(h.dir, "launch-release"), nil, 0600))
			if interrupt == "caller-cancel" {
				for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
					receipt, err = h.client.QuerySubmission(h.ctx, key)
					must(t, err)
					if receipt.Stage == "started" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("caller disconnect cancelled admitted launch", receipt)
					}
				}
				h.stopProcess("fabricd", syscall.SIGKILL)
			}
			h.client.Close()
			h.startProcess("fabricd")
			h.reconnect()
			observed, err := h.client.QuerySubmission(h.ctx, key)
			must(t, err)
			if observed.Admission != api.SubmissionAccepted || observed.Runtime == nil || observed.Runtime.ID != reserved.ID || observed.Runtime.Incarnation != reserved.Incarnation || observed.OperationRef != receipt.OperationRef {
				t.Fatal("restart lost original launch association", observed)
			}
			duplicate, stream, err := h.client.Start(h.ctx, request)
			if stream != nil {
				stream.Close()
			}
			var failure *api.Error
			if !errors.As(err, &failure) || failure.Code != "ALREADY_SUBMITTED" || duplicate.OperationRef != observed.OperationRef {
				t.Fatal("duplicate launch did not return original evidence", duplicate, err)
			}
			count, err := os.ReadFile(filepath.Join(h.dir, "launch-count"))
			must(t, err)
			if strings.Count(string(count), "once") != 1 {
				t.Fatal("setup was replayed", string(count))
			}
			processes, err := os.ReadFile(processLog)
			if interrupt == "caller-cancel" {
				waitManagedACPReady(t, h, *observed.Runtime)
				processes, err = os.ReadFile(processLog)
				must(t, err)
				if strings.Count(string(processes), "\n") != 1 || observed.Stage != "started" {
					t.Fatal("lost response created another Agent", observed, string(processes))
				}
				must(t, testStopRuntime(h.client, h.ctx, *observed.Runtime))
			} else if !os.IsNotExist(err) || observed.Stage != "setup" {
				t.Fatal("interrupted setup was replayed or invented Agent success", observed, err)
			}
		})
	}
}
