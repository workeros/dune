package fabricd

import (
	"context"
	"fmt"
	"net"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

// ServeConn serves one reverse connection, taking ownership of conn even on
// failure. Cancellation closes the tunnel without destroying tmux sessions.
// Callers reconnect sequentially; reconnection never replays business requests.
func (d *Engine) ServeConn(ctx context.Context, conn net.Conn, target string) error {
	if conn == nil {
		return fmt.Errorf("nil connection")
	}
	defer conn.Close()
	d.mu.Lock()
	if err := d.ctx.Err(); err != nil {
		d.mu.Unlock()
		return err
	}
	d.active.Add(1)
	d.mu.Unlock()
	defer d.active.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopEngine := context.AfterFunc(d.ctx, cancel)
	defer stopEngine()
	if err := d.ctx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sess, err := yamux.Client(conn, wire.Config())
	if err != nil {
		return err
	}
	defer sess.Close()
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	d.mu.Lock()
	d.generation++
	gen := d.generation
	d.mu.Unlock()
	b := api.Binding{Capabilities: capabilities, Limits: map[string]int{"message_bytes": wire.MaxMessage, "streams": wire.MaxPendingStreams - 1, "bulk": 4, "runtimes": sessionregistry.MaxLiveRuntimes, "uploads": 64, "dedup_entries": 256, "chunk_bytes": wire.ChunkSize, "profile_bytes": api.MaxProfileBytes, "profile_attempts": api.MaxProfileAttempts, "profile_status_seconds": api.ProfileStatusRetentionSeconds, "profile_step_name_bytes": api.MaxProfileStepNameBytes, "profile_failure_detail_bytes": api.MaxProfileFailureDetailBytes, "exec_output_bytes": api.MaxExecOutputBytes, "scrollback_lines": api.MaxTerminalScrollbackLines, "scrollback_bytes": api.MaxTerminalScrollbackBytes}}
	b.Limits["submission_ordinary_keys"] = sessionregistry.DefaultMaxKeys
	b.Limits["submission_control_keys_per_class"] = sessionregistry.DefaultMaxControls
	b.Limits["submission_runtime_records"] = sessionregistry.MaxRuntimeRecords
	b.Limits["submission_reads"] = cap(d.submissionReads)
	for class, usage := range d.streams.Snapshot() {
		b.Limits["streams_"+string(class)] = usage.Limit
	}
	b.Limits["acp_conversation_bytes"] = api.MaxACPConversationBytes
	b.Capabilities = append(append([]string(nil), b.Capabilities...), "submission.acp", "submission.raw", "acp.raw.state", "acp.raw.read", "acp.persistent")
	b.Capabilities = append(b.Capabilities, "runner.upgrade.probe")
	b.Limits["raw_acp_message_bytes"] = api.RawACPMaxMessageBytes
	b.Limits["raw_acp_pending_bytes"] = api.RawACPMaxPendingBytes
	b.Limits["raw_acp_pending_messages"] = api.RawACPMaxPendingMessages
	b.Limits["raw_acp_stdout_bytes"] = api.RawACPStdoutBytes
	b.Limits["raw_acp_stderr_bytes"] = api.RawACPStderrBytes
	b.Limits["raw_acp_read_bytes"] = api.RawACPMaxReadBytes
	b.Limits["acp_conversations_bytes"] = api.MaxACPConversationsBytes
	b.Limits["acp_conversation_entries"] = api.MaxACPConversationEntries
	b.Limits["acp_entry_bytes"] = api.MaxACPEntryBytes
	b.Limits["acp_conversation_response_bytes"] = api.MaxACPConversationResponseBytes
	b.Limits["acp_conversation_read_limit"] = api.MaxACPConversationLimit
	b.Limits["acp_conversation_notification_bytes"] = api.MaxACPConversationNotificationBytes
	b.Limits["acp_conversation_notification_ids"] = api.MaxACPConversationLimit
	b.Limits["acp_conversation_reads"] = cap(d.conversationReads)

	input := wire.NewInputWindow()
	challenge := wire.ID()
	if err := input.Begin(challenge); err != nil {
		return err
	}
	ctrl, welcome, err := wire.Handshake(sess, &pb.Message{Kind: "hello", InputLeaseId: challenge, Target: target, Incarnation: d.inc, ConnectionGeneration: gen, Payload: api.Payload(api.Hello{Version: api.Version, Role: "daemon"}), Data: api.Payload(b)})
	if err != nil {
		return err
	}
	var accepted api.Binding
	if err := wire.Decode(welcome, &accepted); err != nil || accepted.Target != target || accepted.Incarnation != d.inc || accepted.Generation != gen || accepted.Version != api.Version {
		return fmt.Errorf("invalid accepted execution binding")
	}
	if err := confirmInputLease(input, ctrl, welcome, accepted); err != nil {
		return err
	}
	go input.Watch(ctx, func() { sess.Close() })
	d.recordLifecycle("gateway_attached", nil, "", "", d.sessionTerm)
	defer d.recordLifecycle("gateway_detached", nil, "", "", d.sessionTerm)
	go func() { _ = renewInputLease(ctx, input, ctrl, accepted); sess.Close() }()
	for {
		raw, err := sess.AcceptStream()
		if err != nil {
			return nil
		}
		if lease := d.streams.Acquire(wire.StreamOpening); lease != nil {
			d.mu.Lock()
			if d.ctx.Err() != nil {
				d.mu.Unlock()
				lease.Release()
				raw.Close()
				continue
			}
			d.active.Add(1)
			d.mu.Unlock()
			go func() {
				defer d.active.Done()
				defer lease.Release()
				d.handle(&executionStream{Stream: wire.Wrap(raw), ctx: ctx, engine: d, generation: gen, input: input, epoch: accepted.RouteEpoch}, target, gen, lease)
			}()
		} else {
			raw.Close()
		}
	}
}
