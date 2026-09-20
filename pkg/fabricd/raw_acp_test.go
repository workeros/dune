package fabricd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/pkg/api"
)

type rawWriterFunc func([]byte) (int, error)

func (f rawWriterFunc) Write(p []byte) (int, error) { return f(p) }

func rawTestKey(id string) api.SubmissionKey {
	return api.SubmissionKey{SubmissionID: id, Target: api.SubmissionTarget{OwnerID: "owner", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1, RuntimeID: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1}}
}

func rawWriteRequest(state api.RawACPState, owner string, data []byte) api.RawACPWrite {
	return api.RawACPWrite{StreamID: state.StreamID, InputEpoch: state.InputEpoch, OwnerID: owner, Length: len(data), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Data: data}
}

func rawFixture(t *testing.T, writer io.Writer) (*rawACP, *sessionregistry.Registry) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := sessionregistry.Open(t.Context(), dir, sessionregistry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a := newRawACP(t.Context(), r, "acp:host", writer)
	t.Cleanup(func() {
		a.closeInput()
		select {
		case <-a.done:
		case <-time.After(5 * time.Second):
			t.Error("writer did not end")
		}
		r.Close()
	})
	return a, r
}

func takeRawTest(t *testing.T, a *rawACP, id, owner string) api.RawACPState {
	t.Helper()
	s := a.state()
	r, err := a.submit(t.Context(), api.SubmissionRequest{SubmissionKey: rawTestKey(id), Operation: "acp.raw.take", Payload: api.Payload(api.RawACPTake{StreamID: s.StreamID, ExpectedEpoch: s.InputEpoch, OwnerID: owner})})
	if err != nil || r.RawInput == nil || r.RawInput.InputEpoch != s.InputEpoch+1 {
		t.Fatal(r, err)
	}
	return a.state()
}

func TestRawWriterCompletesOriginalBytesAfterCallerCancellationAndTakeover(t *testing.T) {
	var mu sync.Mutex
	var received bytes.Buffer
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	a, registry := rawFixture(t, rawWriterFunc(func(p []byte) (int, error) {
		blocked := false
		once.Do(func() { blocked = true })
		if blocked {
			close(entered)
			<-resume
		}
		n := min(len(p), 7) // deterministic short writes, including split UTF-8
		mu.Lock()
		received.Write(p[:n])
		mu.Unlock()
		return n, nil
	}))
	t.Cleanup(func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	})
	state := takeRawTest(t, a, "take-first", "owner-a")
	first := []byte(" {\"jsonrpc\": \"2.0\", \"method\": \"first\", \"params\":{\"text\":\"汉🙂\"}}\r\n")
	ctx, cancel := context.WithCancel(t.Context())
	request := api.SubmissionRequest{SubmissionKey: rawTestKey("first"), Operation: "acp.raw.write", Payload: api.Payload(rawWriteRequest(state, "owner-a", first))}
	r, err := a.submit(ctx, request)
	if err != nil || r.Admission != api.SubmissionAccepted || r.RawInput.Sequence != 1 {
		t.Fatal(r, err)
	}
	cancel()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("writer not entered")
	}
	state = takeRawTest(t, a, "take-second", "owner-b")
	second := []byte("{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n")
	secondRequest := api.SubmissionRequest{SubmissionKey: rawTestKey("second"), Operation: "acp.raw.write", Payload: api.Payload(rawWriteRequest(state, "owner-b", second))}
	r, err = a.submit(t.Context(), secondRequest)
	if err != nil || r.RawInput.Sequence != 2 {
		t.Fatal(r, err)
	}
	duplicate, err := a.submit(t.Context(), request)
	if err != nil || duplicate.RawInput.Sequence != 1 {
		t.Fatal(duplicate, err)
	}
	stale := request
	stale.SubmissionID = "late-old-owner"
	_, err = a.submit(t.Context(), stale)
	requireSubmissionCode(t, err, "STALE_CONTROL")
	close(resume)
	waitTimeoutTest(t, func() bool { return a.state().PendingMessages.Used == 0 })
	mu.Lock()
	actual := append([]byte(nil), received.Bytes()...)
	mu.Unlock()
	if !bytes.Equal(actual, append(append([]byte(nil), first...), second...)) {
		t.Fatalf("input reordered/reencoded: %q", actual)
	}
	for _, key := range []api.SubmissionKey{request.SubmissionKey, secondRequest.SubmissionKey} {
		r, err := registry.Get(t.Context(), key)
		if err != nil || r.Stage != "written" {
			t.Fatal(r, err)
		}
	}
}

func TestRawPartialPipeFailureSealsInputAndDiscardsQueuedMessages(t *testing.T) {
	entered, resume := make(chan struct{}), make(chan struct{})
	var received bytes.Buffer
	a, registry := rawFixture(t, rawWriterFunc(func(p []byte) (int, error) {
		received.Write(p[:9])
		close(entered)
		<-resume
		return 9, io.ErrClosedPipe
	}))
	t.Cleanup(func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	})
	state := takeRawTest(t, a, "take", "owner")
	data := []byte("{\"jsonrpc\":\"2.0\",\"method\":\"prefix-only\"}\n")
	request := api.SubmissionRequest{SubmissionKey: rawTestKey("partial"), Operation: "acp.raw.write", Payload: api.Payload(rawWriteRequest(state, "owner", data))}
	if _, err := a.submit(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("writer not entered")
	}
	queued := request
	queued.SubmissionID = "queued"
	if _, err := a.submit(t.Context(), queued); err != nil {
		t.Fatal(err)
	}
	takeRawTest(t, a, "take-during-write", "replacement")
	close(resume)
	waitTimeoutTest(t, func() bool { return a.state().PendingMessages.Used == 0 })
	if a.state().InputState != "input_unrecoverable" || !bytes.Equal(received.Bytes(), data[:9]) {
		t.Fatal(a.state(), received.String())
	}
	for key, stage := range map[string]string{"partial": "input_unrecoverable", "queued": "not_sent"} {
		r, err := registry.Get(t.Context(), rawTestKey(key))
		if err != nil || r.Stage != stage || r.ErrorCode != "STREAM_INPUT_UNRECOVERABLE" {
			t.Fatal(r, err)
		}
	}
	request.SubmissionID = "after-failure"
	_, err := a.submit(t.Context(), request)
	requireSubmissionCode(t, err, "STREAM_INPUT_UNRECOVERABLE")
	state = a.state()
	_, err = a.submit(t.Context(), api.SubmissionRequest{SubmissionKey: rawTestKey("take-after-failure"), Operation: "acp.raw.take", Payload: api.Payload(api.RawACPTake{StreamID: state.StreamID, ExpectedEpoch: state.InputEpoch, OwnerID: "third"})})
	requireSubmissionCode(t, err, "STREAM_INPUT_UNRECOVERABLE")
	a.stdout.append([]byte("tail"))
	output, err := a.read(api.RawACPRead{StreamID: state.StreamID, Channel: "stdout"})
	if err != nil || string(output.Data) != "tail" {
		t.Fatal(output, err)
	}
}

func TestRawMessageBoundaryAndDefaultPendingByteLimit(t *testing.T) {
	resume := make(chan struct{})
	a, _ := rawFixture(t, rawWriterFunc(func(p []byte) (int, error) { <-resume; return len(p), nil }))
	t.Cleanup(func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	})
	state := takeRawTest(t, a, "take", "owner")
	for i, data := range [][]byte{[]byte(`{"jsonrpc":"2.0","method":"incomplete"}`), []byte("{}\n{}\n"), []byte("not-json\n"), []byte("{\"jsonrpc\":\"2.0\",\"method\":\"x\",\"params\":\"\xff\"}\n")} {
		_, err := a.submit(t.Context(), api.SubmissionRequest{SubmissionKey: rawTestKey(fmt.Sprint("invalid-", i)), Operation: "acp.raw.write", Payload: api.Payload(rawWriteRequest(state, "owner", data))})
		requireSubmissionCode(t, err, "INVALID_ARGUMENT")
	}
	if a.state().PendingMessages.Used != 0 {
		t.Fatal("incomplete input admitted")
	}
	prefix, suffix := "{\"jsonrpc\":\"2.0\",\"method\":\"large\",\"params\":{\"text\":\"", "\"}}\n"
	data := []byte(prefix + strings.Repeat("x", api.RawACPMaxMessageBytes-len(prefix)-len(suffix)) + suffix)
	for i := 0; i < 4; i++ {
		_, err := a.submit(t.Context(), api.SubmissionRequest{SubmissionKey: rawTestKey(fmt.Sprint("full-", i)), Operation: "acp.raw.write", Payload: api.Payload(rawWriteRequest(state, "owner", data))})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := a.submit(t.Context(), api.SubmissionRequest{SubmissionKey: rawTestKey("over-full"), Operation: "acp.raw.write", Payload: api.Payload(rawWriteRequest(state, "owner", data))})
	requireSubmissionCode(t, err, "SUBMISSION_CAPACITY_EXHAUSTED")
	if usage := a.state(); usage.PendingBytes.Used != api.RawACPMaxPendingBytes || usage.PendingMessages.Used != 4 {
		t.Fatal(usage)
	}
	close(resume)
}

func TestRawOutputByteWindowsPreserveSplitsAndReportGaps(t *testing.T) {
	a := &rawACP{id: "stream", stdout: newRawOutputWindow(13), stderr: newRawOutputWindow(7)}
	data := []byte("{\"汉🙂\": 1}\r\n")
	for _, b := range data {
		a.stdout.append([]byte{b})
	}
	a.stderr.append([]byte("err\x1b\n"))
	out, err := a.read(api.RawACPRead{StreamID: "stream", Channel: "stdout"})
	requireSubmissionCode(t, err, "STREAM_GAP")
	if len(out.Data) != 0 || out.Window.Oldest != uint64(len(data)-13) {
		t.Fatal(out)
	}
	var actual []byte
	for offset := out.Window.Oldest; offset < out.Window.Next; {
		part, err := a.read(api.RawACPRead{StreamID: "stream", Channel: "stdout", Offset: offset, MaxBytes: 3})
		if err != nil {
			t.Fatal(err)
		}
		actual = append(actual, part.Data...)
		offset = part.Next
	}
	if !bytes.Equal(actual, data[len(data)-13:]) {
		t.Fatal(actual)
	}
	diagnostic, err := a.read(api.RawACPRead{StreamID: "stream", Channel: "stderr"})
	if err != nil || string(diagnostic.Data) != "err\x1b\n" {
		t.Fatal(diagnostic, err)
	}
}

func TestRawPendingCountLimitIncludesBlockedWriter(t *testing.T) {
	resume := make(chan struct{})
	a, _ := rawFixture(t, rawWriterFunc(func(p []byte) (int, error) { <-resume; return len(p), nil }))
	t.Cleanup(func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	})
	state := takeRawTest(t, a, "take", "owner")
	data := []byte("{\"jsonrpc\":\"2.0\",\"method\":\"small\"}\n")
	for i := range api.RawACPMaxPendingMessages {
		_, err := a.submit(t.Context(), api.SubmissionRequest{SubmissionKey: rawTestKey(fmt.Sprint("small-", i)), Operation: "acp.raw.write", Payload: api.Payload(rawWriteRequest(state, "owner", data))})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := a.submit(t.Context(), api.SubmissionRequest{SubmissionKey: rawTestKey("count-full"), Operation: "acp.raw.write", Payload: api.Payload(rawWriteRequest(state, "owner", data))})
	requireSubmissionCode(t, err, "SUBMISSION_CAPACITY_EXHAUSTED")
	if usage := a.state(); usage.PendingMessages.Used != api.RawACPMaxPendingMessages || usage.PendingBytes.Used >= api.RawACPMaxPendingBytes {
		t.Fatal(usage)
	}
	close(resume)
}
