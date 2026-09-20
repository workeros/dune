package fabricd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

type rawWrite struct {
	receipt api.SubmissionReceipt
	data    []byte
}

// rawACP owns only byte transport. No controller or protocol reinitialization
// is involved in output reads, connector reconnects, or input ownership changes.
type rawACP struct {
	mu                      sync.Mutex
	id, owner, inputState   string
	epoch, sequence         uint64
	pending                 []*rawWrite
	messageCount, byteCount int
	wake                    chan struct{}
	done                    chan struct{}
	stdout, stderr          *rawOutputWindow
	registry                *sessionregistry.Registry
	receiver                string
	write                   func([]byte) (int, error)
}

func newRawACP(ctx context.Context, registry *sessionregistry.Registry, receiver string, writer io.Writer) *rawACP {
	a := &rawACP{id: wire.ID(), inputState: "ready", wake: make(chan struct{}, 1), done: make(chan struct{}), registry: registry, receiver: receiver, write: writer.Write,
		stdout: newRawOutputWindow(api.RawACPStdoutBytes), stderr: newRawOutputWindow(api.RawACPStderrBytes)}
	go a.runWriter(ctx)
	return a
}

func (a *rawACP) notify() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *rawACP) closeInput() {
	a.mu.Lock()
	if a.inputState == "ready" {
		a.inputState = "closed"
	}
	a.mu.Unlock()
	a.notify()
}

func (a *rawACP) state() api.RawACPState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return api.RawACPState{StreamID: a.id, InputEpoch: a.epoch, InputState: a.inputState,
		PendingMessages: api.CapacityUsage{Used: a.messageCount, Limit: api.RawACPMaxPendingMessages},
		PendingBytes:    api.CapacityUsage{Used: a.byteCount, Limit: api.RawACPMaxPendingBytes}, MessageLimitBytes: api.RawACPMaxMessageBytes,
		Stdout: a.stdout.window(), Stderr: a.stderr.window()}
}

func (a *rawACP) inputError(stream string) error {
	if stream != a.id {
		return &api.Error{Code: "STALE_STREAM", Detail: "input belongs to another raw ACP stream"}
	}
	if a.inputState == "input_unrecoverable" {
		return &api.Error{Code: "STREAM_INPUT_UNRECOVERABLE", Detail: "original input stream is permanently sealed"}
	}
	if a.inputState != "ready" {
		return &api.Error{Code: "RUNTIME_STOPPED", Detail: "original input stream is closed"}
	}
	return nil
}

func validateRawWrite(request api.RawACPWrite) error {
	if api.ValidateSubmissionID(request.StreamID) != nil || api.ValidateSubmissionID(request.OwnerID) != nil || request.InputEpoch == 0 {
		return &api.Error{Code: "INVALID_ARGUMENT", Detail: "raw input requires stream, owner and input epoch"}
	}
	b := request.Data
	if len(b) == 0 || len(b) > api.RawACPMaxMessageBytes || request.Length != len(b) || b[len(b)-1] != '\n' || bytes.ContainsRune(b[:len(b)-1], '\n') || !utf8.Valid(b) {
		return &api.Error{Code: "INVALID_ARGUMENT", Detail: "one complete bounded JSON-RPC line with its original line ending required"}
	}
	digest := sha256.Sum256(b)
	if request.SHA256 != hex.EncodeToString(digest[:]) || validateRPC(b[:len(b)-1]) != nil {
		return &api.Error{Code: "INVALID_ARGUMENT", Detail: "raw input length, SHA-256 and JSON-RPC boundary must match"}
	}
	return nil
}

func (a *rawACP) submit(ctx context.Context, request api.SubmissionRequest) (api.SubmissionReceipt, error) {
	var take api.RawACPTake
	var write api.RawACPWrite
	var payload []byte
	var validation error
	switch request.Operation {
	case "acp.raw.take":
		validation = json.Unmarshal(request.Payload, &take)
		if validation == nil && (api.ValidateSubmissionID(take.StreamID) != nil || api.ValidateSubmissionID(take.OwnerID) != nil || take.ExpectedEpoch == ^uint64(0)) {
			validation = fmt.Errorf("valid stream, owner and non-exhausted epoch required")
		}
		payload = api.Payload(take)
	case "acp.raw.write":
		validation = json.Unmarshal(request.Payload, &write)
		if validation == nil {
			validation = validateRawWrite(write)
		}
		payload = api.Payload(write)
	default:
		return api.SubmissionReceipt{SubmissionKey: request.SubmissionKey, Admission: api.SubmissionUnknown}, &api.Error{Code: "UNSUPPORTED", Detail: "raw submission requires take or write"}
	}
	// The same interpreted request returns original evidence even after its
	// owner was replaced or its stream closed. Changed bytes/epoch conflict.
	claim, receipt, err := a.registry.ClaimKey(ctx, request.SubmissionKey, sessionregistry.Digest(request.Operation, payload), a.receiver)
	if err != nil {
		return receipt, err
	}
	if !claim.Acquired() {
		return receipt, submissionDecisionError(receipt)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if validation == nil {
		stream := write.StreamID
		if request.Operation == "acp.raw.take" {
			stream = take.StreamID
		}
		validation = a.inputError(stream)
	}
	if validation == nil && request.Operation == "acp.raw.take" && take.ExpectedEpoch != a.epoch {
		validation = &api.Error{Code: "STALE_CONTROL", Detail: "raw input ownership epoch changed"}
	}
	if validation == nil && request.Operation == "acp.raw.write" {
		if write.InputEpoch != a.epoch || write.OwnerID != a.owner {
			validation = &api.Error{Code: "STALE_CONTROL", Detail: "raw input owner or epoch changed"}
		} else if a.messageCount >= api.RawACPMaxPendingMessages || a.byteCount+len(write.Data) > api.RawACPMaxPendingBytes || a.sequence == ^uint64(0) {
			validation = &api.Error{Code: "SUBMISSION_CAPACITY_EXHAUSTED", Detail: "raw input pending message or byte capacity reached"}
		}
	}
	if validation != nil {
		failure, ok := validation.(*api.Error)
		if !ok {
			failure = &api.Error{Code: "INVALID_ARGUMENT", Detail: "invalid raw submission"}
		}
		receipt, err = a.registry.Reject(ctx, claim, failure.Code)
		if err != nil {
			return receipt, err
		}
		return receipt, failure
	}
	input := api.RawACPInputReceipt{StreamID: a.id, InputEpoch: a.epoch + 1}
	if request.Operation == "acp.raw.write" {
		input.InputEpoch, input.Sequence, input.Length = a.epoch, a.sequence+1, len(write.Data)
	}
	receipt, err = a.registry.AcceptRaw(ctx, claim, wire.ID(), input)
	if err != nil {
		return receipt, err
	}
	if request.Operation == "acp.raw.take" {
		a.epoch, a.owner = input.InputEpoch, take.OwnerID
		return receipt, nil
	}
	a.sequence = input.Sequence
	a.pending = append(a.pending, &rawWrite{receipt: receipt, data: write.Data})
	a.messageCount++
	a.byteCount += len(write.Data)
	a.notify()
	return receipt, nil
}

// This one writer continues short writes without interleaving. It never holds
// a.mu during pipe I/O, so ownership/state/stop remain accessible while blocked.
func (a *rawACP) runWriter(ctx context.Context) {
	defer close(a.done)
	for {
		a.mu.Lock()
		if len(a.pending) == 0 {
			closed := a.inputState != "ready"
			a.mu.Unlock()
			if closed {
				return
			}
			select {
			case <-a.wake:
			case <-ctx.Done():
				a.closeInput()
			}
			continue
		}
		message := a.pending[0]
		a.pending[0] = nil
		a.pending = a.pending[1:]
		state := a.inputState
		a.mu.Unlock()
		stage, code := "not_sent", "RUNTIME_STOPPED"
		if state == "ready" {
			if a.progress(message, "writing", "") == nil {
				stage, code = "written", ""
				remaining := message.data
				for len(remaining) != 0 {
					n, err := a.write(remaining)
					if n < 0 || n > len(remaining) || err != nil || n == 0 {
						stage, code = "input_unrecoverable", "STREAM_INPUT_UNRECOVERABLE"
						break
					}
					remaining = remaining[n:]
				}
			} else {
				code = "RESULT_UNKNOWN" // no pipe write without confirmed stage evidence
			}
		} else if state == "input_unrecoverable" {
			code = "STREAM_INPUT_UNRECOVERABLE"
		}
		a.mu.Lock()
		if stage == "input_unrecoverable" || code == "RESULT_UNKNOWN" {
			a.inputState = "input_unrecoverable"
		}
		a.mu.Unlock()
		// Failure to persist a completion never authorizes replay. The durable
		// receipt remains at its last confirmed stage; the live stream is sealed.
		if err := a.progress(message, stage, code); err != nil {
			a.mu.Lock()
			a.inputState = "input_unrecoverable"
			a.mu.Unlock()
		}
		a.mu.Lock()
		a.messageCount--
		a.byteCount -= len(message.data)
		a.mu.Unlock()
	}
}

func (a *rawACP) progress(message *rawWrite, stage, code string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.registry.Progress(ctx, message.receipt.SubmissionKey, message.receipt.OperationRef, stage, code, nil)
	return err
}

func (d *Engine) submitRaw(s *executionStream, message *pb.Message, machine string) {
	var request api.SubmissionRequest
	if wire.Decode(message, &request) != nil || request.SubmissionKey.Validate() != nil || request.Target.RuntimeID == "" || !matchesSubmissionTarget(request.Target, message, machine) {
		s.Fail("INVALID_ARGUMENT", fmt.Errorf("complete original raw submission identity required"))
		return
	}
	r, err := d.lookup(message)
	if err != nil {
		s.Fail("STALE_RUNTIME", err)
		return
	}
	if r.target != request.Target {
		s.Fail("STALE_BINDING", fmt.Errorf("raw submission differs from original Runtime binding"))
		return
	}
	if r.raw == nil || d.registry == nil {
		s.Fail("UNSUPPORTED", fmt.Errorf("Runtime is not raw ACP"))
		return
	}
	if s.Send(&pb.Message{Kind: "accepted", RequestId: message.RequestId}) != nil {
		return
	}
	receipt, err := r.raw.submit(s.ctx, request)
	if err != nil {
		failSubmission(s, receipt, err)
		return
	}
	d.recordLifecycle("admission_receipt", r, receipt.OperationRef, "", 0)
	_ = s.Send(&pb.Message{Kind: "result", Payload: api.Payload(receipt)})
}
