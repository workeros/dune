package duneagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/pkg/host"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

const maxAnswerBytes = 1 << 20

func (b Backend) Prompt(ctx context.Context, conversation channel.ConversationSession, session channel.AgentSession, input string, emit func(channel.AgentEvent) error) (string, error) {
	if err := b.ValidateInput(input); err != nil {
		return "", err
	}
	if emit == nil {
		return "", errors.New("IM ACP prompt and event receiver are required")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
	}
	connection, err := b.open(ctx, conversation)
	if err != nil {
		return "", err
	}
	defer connection.Close()
	runtime := toRuntime(session.Runtime)
	current, err := connection.Get(ctx, runtime)
	if err != nil || current.ID != runtime.ID || current.Incarnation != runtime.Incarnation || current.Generation != runtime.Generation || current.Adapter != "acp" || current.State != "running" {
		if err != nil {
			return "", err
		}
		return "", errors.New("IM ACP Runtime identity changed or process exited")
	}
	subscription, err := connection.Observe(ctx, runtime)
	if err != nil {
		return "", err
	}
	// Recv has no context parameter. Close the observation explicitly when
	// the turn is canceled, independent of the executor implementation.
	var closeSubscription sync.Once
	closeObserved := func() { closeSubscription.Do(func() { _ = subscription.Close() }) }
	stopOnCancel := context.AfterFunc(ctx, closeObserved)
	defer func() {
		stopOnCancel()
		closeObserved()
	}()
	state, err := connection.State(ctx, runtime)
	if err != nil {
		return "", err
	}
	if !state.Ready || state.Busy != "" || state.SessionID != session.ACPSessionID {
		return "", errors.New("IM ACP session is not ready for this prompt")
	}
	if err := connection.Action(ctx, runtime, host.AgentAction{Action: "prompt", Text: input}); err != nil {
		return "", fmt.Errorf("submit ACP prompt (outcome may be unknown): %w", err)
	}
	var answer string
	for {
		message, err := subscription.Recv()
		if err != nil {
			return "", fmt.Errorf("receive ACP prompt result (outcome may be unknown): %w", err)
		}
		switch message.Kind {
		case "acp_update":
			text, full, ok, err := assistantUpdate(message, session.ACPSessionID)
			if err != nil {
				return "", err
			}
			if !ok {
				continue
			}
			if full {
				if len(text) > maxAnswerBytes {
					return "", errors.New("ACP assistant answer exceeds one MiB")
				}
				if !strings.HasPrefix(text, answer) {
					// A complete assistant message may revise previously streamed
					// text. Keep it as the authoritative final, but do not emit
					// replacement content as an append-only delta.
					answer = text
					continue
				}
				text = strings.TrimPrefix(text, answer)
			}
			if len(answer)+len(text) > maxAnswerBytes {
				return "", errors.New("ACP assistant answer exceeds one MiB")
			}
			answer += text
			if text != "" {
				if err := emit(channel.AgentEvent{Kind: channel.AgentDelta, Text: text}); err != nil {
					return "", err
				}
			}
		case "acp_state":
			var next host.AgentState
			if err := json.Unmarshal(message.Payload, &next); err != nil {
				return "", fmt.Errorf("decode ACP state: %w", err)
			}
			if next.Revision <= state.Revision {
				continue
			}
			state = next
			if next.Error != "" {
				return "", fmt.Errorf("ACP prompt failed: %s", next.Error)
			}
			if next.SessionID != session.ACPSessionID {
				return "", errors.New("ACP session changed during IM prompt")
			}
			if next.Ready && next.Busy == "" && next.StopReason != "" {
				if next.StopReason == "cancelled" || next.StopReason == "canceled" || answer == "" {
					return "", fmt.Errorf("ACP prompt ended without an answer: %s", next.StopReason)
				}
				if err := emit(channel.AgentEvent{Kind: channel.AgentFinal, Text: answer}); err != nil {
					return "", err
				}
				return answer, nil
			}
		case "acp_notice", "exit", "error":
			return "", fmt.Errorf("ACP output is incomplete (%s); prompt outcome unknown", message.Kind)
		}
	}
}

func assistantUpdate(message *pb.Message, sessionID string) (text string, full bool, ok bool, err error) {
	var event struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			SnakeUpdate   string `json:"session_update"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	if err := json.Unmarshal(message.Payload, &event); err != nil {
		return "", false, false, fmt.Errorf("decode ACP update: %w", err)
	}
	if event.SessionID != sessionID {
		return "", false, false, nil
	}
	kind := event.Update.SessionUpdate
	if kind == "" {
		kind = event.Update.SnakeUpdate
	}
	text, ok = NormalizeAssistantUpdate(kind, event.Update.Content.Type, event.Update.Content.Text)
	return text, kind == "agent_message", ok, nil
}
