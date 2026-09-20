package fabricd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/gateway"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

// Only the test executable can install this deterministic pipe boundary.
// The normal host has no environment-controlled fault or alternate input path.
func rawHostTestHelper(args []string) (int, bool) {
	if len(args) != 2 || args[0] != "_acp_host" {
		return 0, false
	}
	var boot sessionBootstrap
	if privateFile(filepath.Join(args[1], "bootstrap.json"), 8*1024*1024, &boot) != nil || boot.Profile.Env["DUNE_RAW_TEST_GATE"] == "" {
		return 0, false
	}
	gate, fail := boot.Profile.Env["DUNE_RAW_TEST_GATE"], boot.Profile.Env["DUNE_RAW_TEST_FAIL"] == "1"
	first := true
	err := runSessionHostWithRawWriter(args[1], func(writer io.Writer) io.Writer {
		return rawWriterFunc(func(p []byte) (int, error) {
			n, err := writer.Write(p[:min(len(p), 17)])
			if err != nil || !first {
				return n, err
			}
			first = false
			conn, err := net.DialTimeout("tcp", gate, 3*time.Second)
			if err != nil {
				return n, err
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
			if _, err := conn.Write([]byte{1}); err != nil {
				return n, err
			}
			var release [1]byte
			if _, err := io.ReadFull(conn, release[:]); err != nil {
				return n, err
			}
			if fail {
				return n, io.ErrClosedPipe
			}
			return n, nil
		})
	})
	if err != nil {
		return 1, true
	}
	return 0, true
}

// The actual Agent records every byte it reads, separately from host receipts.
// It echoes bytes in deliberately unrelated fragments and keeps stderr separate.
func runRawRecorder() int {
	path := os.Getenv("DUNE_RAW_TEST_RECORD")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 1
	}
	defer file.Close()
	input, e1 := os.Stdin.Stat()
	output, e2 := os.Stdout.Stat()
	if e1 != nil || e2 != nil {
		return 1
	}
	identity := api.Payload(map[string]any{"pid": os.Getpid(), "stdin_pipe": input.Mode()&os.ModeNamedPipe != 0, "stdout_pipe": output.Mode()&os.ModeNamedPipe != 0})
	processes, err := os.OpenFile(path+".process", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 1
	}
	_, _ = processes.Write(append(identity, '\n'))
	processes.Close()
	_, _ = os.Stderr.Write([]byte("separate diagnostic 汉\r\n"))
	if gate := os.Getenv("DUNE_RAW_TEST_OUTPUT_GATE"); gate != "" {
		conn, err := net.DialTimeout("tcp", gate, 3*time.Second)
		if err != nil {
			return 1
		}
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		if _, err := conn.Write([]byte{1}); err != nil {
			return 1
		}
		var release [1]byte
		if _, err := io.ReadFull(conn, release[:]); err != nil {
			return 1
		}
		conn.Close()
		if _, err := os.Stdout.Write(rawOutputBurst()); err != nil {
			return 1
		}
		if err := os.WriteFile(path+".output-complete", []byte("done"), 0600); err != nil {
			return 1
		}
	}
	buf := make([]byte, 8191)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			if _, e := file.Write(buf[:n]); e != nil {
				return 1
			}
			if _, e := os.Stdout.Write(buf[:n]); e != nil {
				return 1
			}
		}
		if err != nil {
			if err == io.EOF {
				return 0
			}
			return 1
		}
	}
}

func rawOutputBurst() []byte {
	// Deliberately ends at arbitrary UTF-8 and JSON boundaries. Raw transport
	// must preserve all bytes, regardless of its retained window boundary.
	return bytes.Repeat([]byte("{汉🙂}\r\n"), (api.RawACPStdoutBytes+1024*1024)/len([]byte("{汉🙂}\r\n"))+1)
}

func TestRawIncompleteNetworkInputAndOfflineOutputGap(t *testing.T) {
	h := newCleanupProcessHarness(t)
	h.start("")
	gate, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	work := t.TempDir()
	record := filepath.Join(work, "received")
	r, launch, err := testStartProfile(h.client, h.ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "acp", WorkingDirectory: work, Start: api.Command{Argv: []string{os.Args[0]}}, Env: map[string]string{"DUNE_RAW_TEST_RECORD": record, "DUNE_RAW_TEST_OUTPUT_GATE": gate.Addr().String()}})
	if err != nil {
		t.Fatal(err)
	}
	launch.Close()
	_ = gate.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	blocked, err := gate.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	var ready [1]byte
	if _, err := io.ReadFull(blocked, ready[:]); err != nil {
		t.Fatal(err)
	}
	state, err := h.client.RawACPState(h.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	take, err := h.client.TakeRawACP(h.ctx, cleanupTestKey(r, "take"), api.RawACPTake{StreamID: state.StreamID, OwnerID: "caller"})
	if err != nil {
		t.Fatal(err)
	}
	state.InputEpoch = take.RawInput.InputEpoch
	key := cleanupTestKey(r, "incomplete-network")
	request := api.SubmissionRequest{SubmissionKey: key, Operation: "acp.raw.write", Payload: api.Payload(rawWriteRequest(state, "caller", []byte("{\"jsonrpc\":\"2.0\",\"method\":\"never-received\"}\n")))}
	left, right := net.Pipe()
	grant, handler, err := (access.Grant{Target: h.client.Binding.Target, Role: gateway.RoleSDK}).Bind()
	if err != nil {
		t.Fatal(err)
	}
	go h.g.ServeConn(h.ctx, left, grant, handler)
	sess, err := yamux.Client(right, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	ctrl, welcome, err := wire.Handshake(sess, &pb.Message{Kind: "hello", Target: h.client.Binding.Target, Payload: api.Payload(api.Hello{Version: api.Version, Role: "sdk"})})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	var binding api.Binding
	if err := wire.Decode(welcome, &binding); err != nil {
		t.Fatal(err)
	}
	stream, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := wire.Write(&encoded, &pb.Message{Kind: "request", RequestId: wire.ID(), Operation: "submission.raw", Target: binding.Target, Incarnation: binding.Incarnation, ConnectionGeneration: binding.Generation, RuntimeId: r.ID, RuntimeIncarnation: r.Incarnation, RuntimeGeneration: r.Generation, Payload: api.Payload(request)}); err != nil {
		t.Fatal(err)
	}
	// End the frame before its final bytes, despite having a valid declared
	// length. Neither Gateway nor host can interpret this as full admission.
	if _, err := stream.Write(encoded.Bytes()[:encoded.Len()-11]); err != nil {
		t.Fatal(err)
	}
	stream.Close()
	sess.Close()
	receipt, err := h.client.QuerySubmission(h.ctx, key)
	if err != nil || receipt.Admission != api.SubmissionUnknown {
		t.Fatal(receipt, err)
	}
	if data, err := os.ReadFile(record); err != nil || len(data) != 0 {
		t.Fatal("network prefix reached stdin", data, err)
	}
	h.kill()
	if _, err := blocked.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	waitTimeoutTest(t, func() bool { _, err := os.Stat(record + ".output-complete"); return err == nil })
	h.start("")
	stateAfter, err := h.client.RawACPState(h.ctx, r)
	burst := rawOutputBurst()
	if err != nil || stateAfter.StreamID != state.StreamID || stateAfter.InputEpoch != state.InputEpoch || stateAfter.Stdout.Next != uint64(len(burst)) || stateAfter.Stdout.Oldest != uint64(len(burst)-api.RawACPStdoutBytes) {
		t.Fatal(stateAfter, err)
	}
	gap, err := h.client.ReadRawACP(h.ctx, r, api.RawACPRead{StreamID: state.StreamID, Channel: "stdout"})
	requireSubmissionCode(t, err, "STREAM_GAP")
	if gap.Window != stateAfter.Stdout || len(gap.Data) != 0 {
		t.Fatal("SDK lost gap position", gap)
	}
	var retained []byte
	for offset := gap.Window.Oldest; offset < gap.Window.Next; {
		page, err := h.client.ReadRawACP(h.ctx, r, api.RawACPRead{StreamID: state.StreamID, Channel: "stdout", Offset: offset})
		if err != nil {
			t.Fatal(err)
		}
		retained = append(retained, page.Data...)
		offset = page.Next
	}
	if !bytes.Equal(retained, burst[len(burst)-api.RawACPStdoutBytes:]) {
		t.Fatal("retained bytes were modified")
	}
	diagnostic, err := h.client.ReadRawACP(h.ctx, r, api.RawACPRead{StreamID: state.StreamID, Channel: "stderr"})
	if err != nil || string(diagnostic.Data) != "separate diagnostic 汉\r\n" {
		t.Fatal(diagnostic, err)
	}
	if data, err := os.ReadFile(record); err != nil || len(data) != 0 {
		t.Fatal("reconnect added input", data, err)
	}
	if stop, err := h.client.Stop(h.ctx, cleanupTestKey(r, "stop")); err != nil || stop.Stage != "stopped" {
		t.Fatal(stop, err)
	}
	t.Logf("offline bytes=%d retained=%d gap=%d; incomplete input=0 bytes", len(burst), len(retained), gap.Window.Oldest)
}

func TestRawOriginalPipeAcrossConnectorCrashAndInputTakeover(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("partial-error-", fail), func(t *testing.T) {
			h := newCleanupProcessHarness(t)
			h.start("")
			gate, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Close()
			work := t.TempDir()
			record := filepath.Join(work, "received")
			env := map[string]string{"DUNE_RAW_TEST_RECORD": record, "DUNE_RAW_TEST_GATE": gate.Addr().String()}
			if fail {
				env["DUNE_RAW_TEST_FAIL"] = "1"
			}
			r, launch, err := testStartProfile(h.client, h.ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "acp", WorkingDirectory: work, Start: api.Command{Argv: []string{os.Args[0]}}, Env: env})
			if err != nil {
				t.Fatal(err)
			}
			launch.Close()
			if !r.PersistentACP || r.ACPMode != "raw" {
				t.Fatal("raw host capability absent", r)
			}
			state, err := h.client.RawACPState(h.ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			take, err := h.client.TakeRawACP(h.ctx, cleanupTestKey(r, "first-owner"), api.RawACPTake{StreamID: state.StreamID, OwnerID: "first"})
			if err != nil {
				t.Fatal(err)
			}
			state.InputEpoch = take.RawInput.InputEpoch
			first := []byte(" {\"jsonrpc\":\"2.0\",\"method\":\"first\",\"params\":{\"text\":\"" + strings.Repeat("汉🙂", 1024) + "\"}}\r\n")
			firstKey := cleanupTestKey(r, "first-write")
			firstRequest := rawWriteRequest(state, "first", first)
			accepted, err := h.client.WriteRawACP(h.ctx, firstKey, firstRequest)
			if err != nil || accepted.Admission != api.SubmissionAccepted {
				t.Fatal(accepted, err)
			}
			_ = gate.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
			blocked, err := gate.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer blocked.Close()
			var signal [1]byte
			if _, err := io.ReadFull(blocked, signal[:]); err != nil {
				t.Fatal(err)
			}
			waitTimeoutTest(t, func() bool { data, _ := os.ReadFile(record); return bytes.Equal(data, first[:17]) })
			h.kill()
			h.start("")
			observed, err := h.client.RawACPState(h.ctx, r)
			if err != nil || observed.StreamID != state.StreamID || observed.InputEpoch != state.InputEpoch || observed.PendingMessages.Used != 1 {
				t.Fatal(observed, err)
			}
			take, err = h.client.TakeRawACP(h.ctx, cleanupTestKey(r, "second-owner"), api.RawACPTake{StreamID: state.StreamID, ExpectedEpoch: state.InputEpoch, OwnerID: "second"})
			if err != nil {
				t.Fatal(err)
			}
			state.InputEpoch = take.RawInput.InputEpoch
			second := []byte("{\"jsonrpc\":\"2.0\",\"id\":\"second\",\"result\":{}}\n")
			secondKey := cleanupTestKey(r, "second-write")
			if _, err := h.client.WriteRawACP(h.ctx, secondKey, rawWriteRequest(state, "second", second)); err != nil {
				t.Fatal(err)
			}
			if duplicate, err := h.client.WriteRawACP(h.ctx, firstKey, firstRequest); err != nil || duplicate.OperationRef != accepted.OperationRef {
				t.Fatal(duplicate, err)
			}
			_, err = h.client.WriteRawACP(h.ctx, cleanupTestKey(r, "late-old-owner"), firstRequest)
			requireSubmissionCode(t, err, "STALE_CONTROL")
			if _, err := blocked.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			want := append(append([]byte(nil), first...), second...)
			stages := map[api.SubmissionKey]string{firstKey: "written", secondKey: "written"}
			if fail {
				want = first[:17]
				stages[firstKey] = "input_unrecoverable"
				stages[secondKey] = "not_sent"
			}
			for key, stage := range stages {
				waitTimeoutTest(t, func() bool {
					receipt, err := h.client.QuerySubmission(h.ctx, key)
					return err == nil && receipt.Stage == stage
				})
			}
			waitTimeoutTest(t, func() bool { data, _ := os.ReadFile(record); return bytes.Equal(data, want) })
			if fail {
				h.kill()
				h.start("")
				state, err = h.client.RawACPState(h.ctx, r)
				if err != nil || state.InputState != "input_unrecoverable" {
					t.Fatal(state, err)
				}
				_, err = h.client.TakeRawACP(h.ctx, cleanupTestKey(r, "third-owner"), api.RawACPTake{StreamID: state.StreamID, ExpectedEpoch: state.InputEpoch, OwnerID: "third"})
				requireSubmissionCode(t, err, "STREAM_INPUT_UNRECOVERABLE")
				_, err = h.client.WriteRawACP(h.ctx, cleanupTestKey(r, "after-error"), rawWriteRequest(state, "second", second))
				requireSubmissionCode(t, err, "STREAM_INPUT_UNRECOVERABLE")
			}
			var echoed []byte
			waitTimeoutTest(t, func() bool {
				page, err := h.client.ReadRawACP(h.ctx, r, api.RawACPRead{StreamID: state.StreamID, Channel: "stdout", Offset: uint64(len(echoed)), MaxBytes: 37})
				if err != nil {
					t.Fatal(err)
				}
				echoed = append(echoed, page.Data...)
				return len(echoed) == len(want)
			})
			if !bytes.Equal(echoed, want) {
				t.Fatal("stdout changed exact bytes")
			}
			diagnostic, err := h.client.ReadRawACP(h.ctx, r, api.RawACPRead{StreamID: state.StreamID, Channel: "stderr"})
			if err != nil || string(diagnostic.Data) != "separate diagnostic 汉\r\n" {
				t.Fatal(diagnostic, err)
			}
			if stop, err := h.client.Stop(h.ctx, cleanupTestKey(r, "stop")); err != nil || stop.Stage != "stopped" {
				t.Fatal(stop, err)
			}
			data, err := os.ReadFile(record)
			if err != nil || !bytes.Equal(data, want) {
				t.Fatal("extra input after seal", len(data), err)
			}
			processes, err := os.ReadFile(record + ".process")
			var identity struct {
				PID        int  `json:"pid"`
				StdinPipe  bool `json:"stdin_pipe"`
				StdoutPipe bool `json:"stdout_pipe"`
			}
			if err != nil || bytes.Count(processes, []byte{'\n'}) != 1 || json.Unmarshal(processes, &identity) != nil || !identity.StdinPipe || !identity.StdoutPipe || identity.PID <= 0 {
				t.Fatal(string(processes), err)
			}
			t.Logf("original pid=%d, received=%d bytes, partial failure=%v", identity.PID, len(data), fail)
		})
	}
}

func TestRawFullQueueAndBlockedStdinCannotBlockStop(t *testing.T) {
	h := newCleanupProcessHarness(t)
	h.start("")
	r, launch, err := testStartProfile(h.client, h.ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "acp", WorkingDirectory: t.TempDir(), Start: api.Command{Argv: []string{"/bin/sleep", "30"}}})
	if err != nil {
		t.Fatal(err)
	}
	launch.Close()
	state, err := h.client.RawACPState(h.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	take, err := h.client.TakeRawACP(h.ctx, cleanupTestKey(r, "take"), api.RawACPTake{StreamID: state.StreamID, OwnerID: "caller"})
	if err != nil {
		t.Fatal(err)
	}
	state.InputEpoch = take.RawInput.InputEpoch
	prefix, suffix := "{\"jsonrpc\":\"2.0\",\"method\":\"blocked\",\"params\":{\"text\":\"", "\"}}\n"
	data := []byte(prefix + strings.Repeat("x", api.RawACPMaxMessageBytes-len(prefix)-len(suffix)) + suffix)
	for i := range 4 {
		if _, err := h.client.WriteRawACP(h.ctx, cleanupTestKey(r, fmt.Sprint("full-", i)), rawWriteRequest(state, "caller", data)); err != nil {
			t.Fatal(err)
		}
	}
	_, err = h.client.WriteRawACP(h.ctx, cleanupTestKey(r, "overflow"), rawWriteRequest(state, "caller", data))
	requireSubmissionCode(t, err, "SUBMISSION_CAPACITY_EXHAUSTED")
	state, err = h.client.RawACPState(h.ctx, r)
	if err != nil || state.PendingBytes.Used != api.RawACPMaxPendingBytes {
		t.Fatal(state, err)
	}
	started := time.Now()
	stop, err := h.client.Stop(h.ctx, cleanupTestKey(r, "stop"))
	if err != nil || stop.Stage != "stopped" || time.Since(started) > 5*time.Second {
		t.Fatal("blocked writer delayed stop", stop, err, time.Since(started))
	}
	for i := range 4 {
		receipt, err := h.client.QuerySubmission(h.ctx, cleanupTestKey(r, fmt.Sprint("full-", i)))
		want := "not_sent"
		if i == 0 {
			want = "input_unrecoverable"
		}
		if err != nil || receipt.Stage != want {
			t.Fatal(receipt, err)
		}
	}
}
