package channel

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// GroupAdmission decides whether a new group topic addresses this bot. Once
// admitted, later user messages in that same canonical topic are accepted.
type GroupAdmission interface {
	AddressedToBot(context.Context, InboundMessage) (bool, error)
}

type ActiveBinding struct {
	Binding   BotBinding
	Channel   Channel
	Subjects  GroupSubjectResolver
	Admission GroupAdmission
	Streaming bool
	ReplyMode string
}

// ActiveBindings resolves only configured, live bindings. Provider transport
// setup and credentials remain outside the provider-independent processor.
type ActiveBindings interface {
	LookupBinding(context.Context, string) (ActiveBinding, error)
}

type Processor struct {
	Work          WorkQueue
	Conversations ConversationStore
	Deliveries    DeliveryStore
	Bindings      ActiveBindings
	Agents        AgentBackend
	ClaimLease    time.Duration
	SessionLease  time.Duration
}

// ProcessOne executes at most one durably accepted event. Its bool result is
// false only when the queue has no work. It never automatically retries a
// prompt or outbound send whose result may be unknown.
func (p Processor) ProcessOne(ctx context.Context) (bool, error) {
	if p.Work == nil || p.Conversations == nil || p.Deliveries == nil || p.Bindings == nil || p.Agents == nil {
		return false, errors.New("IM processor dependencies are incomplete")
	}
	claimLease, sessionLease := p.ClaimLease, p.SessionLease
	if claimLease == 0 {
		claimLease = time.Minute
	}
	if sessionLease == 0 {
		sessionLease = time.Minute
	}
	item, found, err := p.Work.Claim(ctx, claimLease)
	if err != nil || !found {
		return found, err
	}
	active, err := p.Bindings.LookupBinding(ctx, item.Message.BindingID)
	if err != nil {
		return true, errors.Join(err, p.Work.ReleaseClaim(ctx, item))
	}
	if !active.Binding.Enabled {
		return true, p.Work.Ignore(ctx, item)
	}
	key, threadRef, err := Route(ctx, active.Binding, item.Message, active.Subjects)
	if err != nil {
		return true, errors.Join(err, p.Work.ReleaseClaim(ctx, item))
	}
	session, _, exists, err := p.Conversations.Get(ctx, key)
	if err != nil {
		return true, errors.Join(err, p.Work.ReleaseClaim(ctx, item))
	}
	if item.Message.ChatKind == ChatGroup && !exists {
		if active.Admission == nil {
			return true, errors.Join(errors.New("group admission is unavailable"), p.Work.ReleaseClaim(ctx, item))
		}
		addressed, err := active.Admission.AddressedToBot(ctx, item.Message)
		if err != nil {
			return true, errors.Join(err, p.Work.ReleaseClaim(ctx, item))
		}
		if !addressed {
			return true, p.Work.Ignore(ctx, item)
		}
	}
	// Ensure also atomically confirms an optional provider thread reference for
	// an existing conversation; the ref never changes the session key.
	session, err = p.Conversations.Ensure(ctx, key, active.Binding.Target, item.Message.Address, threadRef)
	if err != nil {
		return true, errors.Join(err, p.Work.ReleaseClaim(ctx, item))
	}
	lease, acquired, err := p.Conversations.Acquire(ctx, key, sessionLease)
	if err != nil || !acquired {
		if err == nil {
			err = errors.New("IM conversation is busy or requires reconciliation")
		}
		return true, errors.Join(err, p.Work.ReleaseClaim(ctx, item))
	}
	return true, p.processClaimed(ctx, item, active, session, lease)
}

func (p Processor) processClaimed(ctx context.Context, item WorkItem, active ActiveBinding, session ConversationSession, lease ConversationLease) error {
	caps, err := p.Agents.Capabilities(ctx, session)
	if err != nil || caps.Adapter != "acp" || !caps.ReliableFinal || (active.Streaming && !caps.AssistantDeltas) {
		if err == nil {
			err = errors.New("IM reply mode requires managed ACP with the necessary final/delta events")
		}
		return errors.Join(err, p.Conversations.ReleaseLease(ctx, lease), p.Work.ReleaseClaim(ctx, item))
	}
	if active.Channel == nil {
		return errors.Join(errors.New("IM channel is unavailable"), p.Conversations.ReleaseLease(ctx, lease), p.Work.ReleaseClaim(ctx, item))
	}
	if active.Streaming {
		if _, ok := active.Channel.(StreamingChannel); !ok {
			return errors.Join(errors.New("IM channel lacks streaming card support"), p.Conversations.ReleaseLease(ctx, lease), p.Work.ReleaseClaim(ctx, item))
		}
	}
	if validator, ok := p.Agents.(AgentInputValidator); ok {
		if err := validator.ValidateInput(item.Message.Text); err != nil {
			return errors.Join(err, p.Conversations.ReleaseLease(ctx, lease), p.Work.ReleaseClaim(ctx, item))
		}
	}
	// Persist the session barrier first. If the process dies during any later
	// remote operation, another worker sees running and cannot take over.
	if err := p.Conversations.BeginTurn(ctx, lease, item.Message.EventID); err != nil {
		return errors.Join(err, p.Conversations.ReleaseLease(ctx, lease), p.Work.ReleaseClaim(ctx, item))
	}
	if err := p.Work.BeginSubmission(ctx, item); err != nil {
		return errors.Join(err, p.unknownSession(ctx, lease, err), p.Work.ReleaseClaim(ctx, item))
	}
	if err := p.executeTurn(ctx, item, active, &session, lease); err != nil {
		return errors.Join(err, p.unknown(ctx, item, lease, err))
	}
	if err := p.Work.Complete(ctx, item); err != nil {
		return errors.Join(err, p.unknown(ctx, item, lease, err))
	}
	if err := p.Conversations.FinishTurn(ctx, lease); err != nil {
		return err // still running: do not admit another turn
	}
	return p.Conversations.ReleaseLease(ctx, lease)
}

func (p Processor) executeTurn(ctx context.Context, item WorkItem, active ActiveBinding, session *ConversationSession, lease ConversationLease) error {
	backendSession := AgentSession{Runtime: session.Runtime, ACPSessionID: session.ACPSessionID}
	var err error
	if backendSession.Runtime.ID == "" {
		backendSession, err = p.Agents.Start(ctx, *session)
	} else {
		backendSession, err = p.Agents.Attach(ctx, *session, backendSession)
	}
	if err != nil {
		return fmt.Errorf("open IM Agent session (outcome may be unknown): %w", err)
	}
	if backendSession.Runtime.ID == "" || backendSession.Runtime.Incarnation == "" || backendSession.Runtime.Generation == 0 || backendSession.Runtime.Adapter != "acp" || backendSession.ACPSessionID == "" {
		return errors.New("IM Agent returned an incomplete managed ACP session")
	}
	session.Runtime, session.ACPSessionID, session.Address = backendSession.Runtime, backendSession.ACPSessionID, item.Message.Address
	*session, err = p.Conversations.Save(ctx, lease, *session)
	if err != nil {
		return err
	}
	delivery := TurnDeliveryID(item.Message.BindingID, item.Message.EventID)
	const contextLostNotice = "⚠️ 上下文未恢复，已创建新会话。\n\n"
	visiblePrefix := ""
	if backendSession.ContextLost {
		visiblePrefix = contextLostNotice
	}
	var stream ReplyStream
	if active.Streaming {
		title := "正在处理"
		if backendSession.ContextLost {
			title = "上下文未恢复，正在重新开始"
		}
		stream, err = active.Channel.(StreamingChannel).OpenStream(ctx, item.Message.Address, OutboundMessage{Text: title, DeliveryID: delivery, Session: session.Key})
		if err != nil {
			return err
		}
		if visiblePrefix != "" {
			if err := stream.Update(ctx, OutboundMessage{Text: visiblePrefix, DeliveryID: delivery, Session: session.Key}); err != nil {
				return err
			}
		}
	}
	cumulative := visiblePrefix
	final, err := p.Agents.Prompt(ctx, *session, backendSession, item.Message.Text, func(event AgentEvent) error {
		switch event.Kind {
		case AgentDelta:
			if event.Text == "" {
				return nil
			}
			cumulative += event.Text
			if stream != nil {
				return stream.Update(ctx, OutboundMessage{Text: cumulative, DeliveryID: delivery, Session: session.Key})
			}
			return nil
		case AgentFinal:
			return nil // Prompt's final result is authoritative; do not append it
		case AgentError:
			return fmt.Errorf("Agent error: %s", event.Text)
		default:
			return fmt.Errorf("unknown Agent event kind %q", event.Kind)
		}
	})
	if err != nil {
		promptErr := fmt.Errorf("IM Agent prompt (outcome may be unknown): %w", err)
		if stream != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			failureText := cumulative
			if failureText != "" {
				failureText += "\n\n"
			}
			failureText += "⚠️ 处理未完成，Agent 结果未知。"
			closeErr := stream.Complete(cleanupCtx, OutboundMessage{Text: failureText, DeliveryID: delivery, Session: session.Key})
			return errors.Join(promptErr, closeErr)
		}
		return promptErr
	}
	if stream != nil {
		final = visiblePrefix + final
		return stream.Complete(ctx, OutboundMessage{Text: final, DeliveryID: delivery, Session: session.Key})
	}
	mode := active.ReplyMode
	if mode == "" {
		mode = "final_text"
	}
	manager := DeliveryManager{Store: p.Deliveries}
	state, created, err := manager.Reserve(ctx, Delivery{ID: delivery, Session: session.Key, Address: item.Message.Address, Mode: mode, ProviderStateVersion: 1})
	if err != nil {
		return err
	}
	if !created {
		return fmt.Errorf("IM delivery %q is %s; reconcile before retry", delivery, state.Phase)
	}
	state, err = manager.Intent(ctx, state, "send", nil)
	if err != nil {
		return err
	}
	providerState, err := active.Channel.Send(ctx, item.Message.Address, OutboundMessage{Text: visiblePrefix + final, DeliveryID: delivery, Session: session.Key})
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, markErr := manager.Unknown(cleanupCtx, state)
		return errors.Join(err, markErr)
	}
	_, err = manager.Confirm(ctx, state, true, providerState)
	return err
}

func (p Processor) unknown(ctx context.Context, item WorkItem, lease ConversationLease, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return errors.Join(p.Work.MarkUnknown(cleanupCtx, item, cause.Error()), p.Conversations.UnknownTurn(cleanupCtx, lease, cause.Error()))
}

func (p Processor) unknownSession(ctx context.Context, lease ConversationLease, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return p.Conversations.UnknownTurn(cleanupCtx, lease, cause.Error())
}
