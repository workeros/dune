package duneagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/host"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

const maxAnswerBytes = 1 << 20

func (b Backend) Prompt(ctx context.Context, conversation channel.ConversationSession, session channel.AgentSession, input string, emit func(channel.AgentEvent) error) (string, error) {
	if err := b.ValidateInput(input); err != nil {
		return "", err
	}
	if session.ConversationID == "" {
		return "", &api.Error{Code: "INVALID_ARGUMENT", Detail: "stored conversation_id is required"}
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
	operation, err := connection.Submit(ctx, runtime, host.AgentAction{ExpectedConversationID: session.ConversationID, Action: "prompt", Text: input, SessionID: session.ACPSessionID})
	if err != nil {
		return "", fmt.Errorf("submit ACP prompt (outcome may be unknown): %w", err)
	}
	if operation.Ref == "" {
		return "", errors.New("ACP prompt did not return an operation reference")
	}
	var answer string
	var position int64
	for {
		output, err := connection.ReadOperation(ctx, runtime, api.AgentOperationRead{Ref: operation.Ref, Position: position, Limit: 128})
		if err != nil {
			return "", fmt.Errorf("read ACP operation (outcome may be unknown): %w", err)
		}
		if output.Ref != operation.Ref || output.Incomplete || output.Position != position || output.NextPosition != position+int64(len(output.Output)) {
			return "", errors.New("ACP operation output is incomplete or does not match the submitted prompt")
		}
		for _, update := range output.Output {
			text, full, ok, err := assistantUpdate(&pb.Message{Payload: update}, session.ACPSessionID)
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
		}
		position = output.NextPosition
		// A full page may have a retained tail even when the RPC has completed.
		if len(output.Output) == 128 {
			continue
		}
		if output.Terminal() {
			if output.State != "completed" || output.StopReason == "" || answer == "" {
				return "", fmt.Errorf("ACP prompt ended without a completed answer: %s %s", output.State, output.Error)
			}
			if err := emit(channel.AgentEvent{Kind: channel.AgentFinal, Text: answer}); err != nil {
				return "", err
			}
			return answer, nil
		}
		// Waiting on this exact operation cannot be satisfied by a preceding turn.
		if _, err := connection.WaitOperation(ctx, runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 250}); err != nil {
			return "", err
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
