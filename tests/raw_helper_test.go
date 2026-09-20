package tests

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

// Test callers retain their protocol state explicitly; the SDK never invents
// an input owner, submission key, newline, or replacement session on reconnect.
type rawACPTestClient struct {
	h       *harness
	runtime api.Runtime
	state   api.RawACPState
	owner   string
	offsets map[string]uint64
	buffer  map[string][]byte
}

func newRawTestClient(t *testing.T, h *harness, runtime api.Runtime) *rawACPTestClient {
	t.Helper()
	a := &rawACPTestClient{h: h, runtime: runtime, owner: wire.ID(), offsets: map[string]uint64{}, buffer: map[string][]byte{}}
	var err error
	a.state, err = h.client.RawACPState(h.ctx, runtime)
	must(t, err)
	receipt, err := h.client.TakeRawACP(h.ctx, a.key(), api.RawACPTake{StreamID: a.state.StreamID, ExpectedEpoch: a.state.InputEpoch, OwnerID: a.owner})
	must(t, err)
	a.state.InputEpoch = receipt.RawInput.InputEpoch
	return a
}

func (a *rawACPTestClient) key() api.SubmissionKey {
	return api.SubmissionKey{SubmissionID: wire.ID(), Target: api.SubmissionTarget{OwnerID: "standalone-owner", RunnerID: "standalone-runner", FabricID: "standalone-fabric", MachineID: a.h.c.Target, BindingRevision: 1, RuntimeID: a.runtime.ID, RuntimeIncarnation: a.runtime.Incarnation, RuntimeGeneration: a.runtime.Generation}}
}

func (a *rawACPTestClient) writeLine(line string) error {
	data := []byte(line + "\n")
	key := a.key()
	_, err := a.h.client.WriteRawACP(a.h.ctx, key, api.RawACPWrite{StreamID: a.state.StreamID, InputEpoch: a.state.InputEpoch, OwnerID: a.owner, Length: len(data), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Data: data})
	return err
}

func (a *rawACPTestClient) nextLine(t *testing.T, channel string) []byte {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if end := bytes.IndexByte(a.buffer[channel], '\n'); end >= 0 {
			line := append([]byte(nil), a.buffer[channel][:end+1]...)
			a.buffer[channel] = a.buffer[channel][end+1:]
			return line
		}
		out, err := a.h.client.ReadRawACP(a.h.ctx, a.runtime, api.RawACPRead{StreamID: a.state.StreamID, Channel: channel, Offset: a.offsets[channel]})
		must(t, err)
		a.offsets[channel] = out.Next
		a.buffer[channel] = append(a.buffer[channel], out.Data...)
		if len(out.Data) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatal("raw ACP line did not arrive", channel)
	return nil
}

func (a *rawACPTestClient) receive(t *testing.T, channel, contains string) {
	t.Helper()
	for range 100 {
		line := a.nextLine(t, channel)
		if bytes.Contains(line, []byte(contains)) {
			return
		}
	}
	t.Fatal("raw ACP output did not contain expected value", contains)
}
