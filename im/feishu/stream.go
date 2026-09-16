package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aiomni/dune/im/channel"
	larkcard "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
)

const streamElementID = "answer"
const streamUpdateInterval = 110 * time.Millisecond // below CardKit's 10 updates/s/card ceiling
const maxStreamTextBytes = 28 * 1024

// StreamState is version 1 of Feishu's opaque provider_state. The common
// delivery record owns identity, address, phase and revision.
type StreamState struct {
	CardID        string `json:"card_id,omitempty"`
	MessageID     string `json:"message_id,omitempty"`
	ConfirmedText string `json:"confirmed_text,omitempty"`
	PendingText   string `json:"pending_text,omitempty"`
	Sequence      int    `json:"sequence"`
}

func (c *Channel) OpenStream(ctx context.Context, address channel.ReplyAddress, initial channel.OutboundMessage) (channel.ReplyStream, error) {
	if c.config.ReplyMode != ReplyStreaming || c.deliveries == nil {
		return nil, errors.New("Feishu binding is not configured for durable streaming_card")
	}
	if initial.DeliveryID == "" || len(initial.DeliveryID) > 128 {
		return nil, errors.New("streaming_card requires a stable 1..128 byte DeliveryID")
	}
	if initial.Session.TenantID != c.binding.TenantID || initial.Session.BindingID != c.binding.ID || initial.Session.ChatID == "" || initial.Session.SubjectID == "" {
		return nil, errors.New("streaming_card requires the canonical session key")
	}
	to, err := decodeAddress(address)
	if err != nil {
		return nil, err
	}
	if err := c.validateDeliverySession(to, initial.Session); err != nil {
		return nil, err
	}
	delivery, created, err := (channel.DeliveryManager{Store: c.deliveries}).Reserve(ctx, channel.Delivery{
		ID: initial.DeliveryID, Session: initial.Session,
		Address: address, Mode: string(ReplyStreaming), ProviderStateVersion: 1,
	})
	if err != nil {
		return nil, err
	}
	storedAddress, addressErr := decodeAddress(delivery.Address)
	if delivery.Session.BindingID != c.binding.ID || delivery.ID != initial.DeliveryID || addressErr != nil || storedAddress != to {
		return nil, errors.New("stored CardKit delivery does not match binding or reply address")
	}
	var state StreamState
	if len(delivery.ProviderState) > 0 && json.Unmarshal(delivery.ProviderState, &state) != nil {
		return nil, errors.New("invalid Feishu delivery provider state")
	}
	if !created {
		if delivery.Phase == "active" && state.CardID != "" && state.MessageID != "" {
			return &cardStream{owner: c, delivery: delivery, state: state, to: to}, nil
		}
		return nil, fmt.Errorf("Feishu stream %q is %s; reconcile before retry", initial.DeliveryID, delivery.Phase)
	}
	stream := &cardStream{owner: c, delivery: delivery, state: state, to: to}
	if err := stream.commit(ctx, "creating"); err != nil {
		return nil, err
	}
	cardJSON, err := initialStreamCard(initial.Text)
	if err != nil {
		return nil, err
	}
	response, err := c.client.Cardkit.V1.Card.Create(ctx, larkcard.NewCreateCardReqBuilder().
		Body(larkcard.NewCreateCardReqBodyBuilder().Type("card_json").Data(cardJSON).Build()).Build())
	if err != nil {
		return nil, stream.remoteFailure(ctx, fmt.Errorf("create CardKit entity (outcome may be unknown): %w", err))
	}
	if response == nil || !response.Success() || response.Data == nil || value(response.Data.CardId) == "" {
		return nil, stream.remoteFailure(ctx, fmt.Errorf("create CardKit entity failed: %v", response))
	}
	stream.state.CardID = value(response.Data.CardId)
	if err := stream.commit(ctx, "created"); err != nil {
		return nil, err
	}
	content, err := json.Marshal(map[string]any{"type": "card", "data": map[string]string{"card_id": stream.state.CardID}})
	if err != nil {
		return nil, err
	}
	if err := stream.commit(ctx, "sending"); err != nil {
		return nil, err
	}
	messageID, err := c.sendContent(ctx, to, "interactive", string(content), outboundUUID(c.binding.ID, initial.DeliveryID, "card-message"))
	if err != nil {
		return nil, stream.remoteFailure(ctx, fmt.Errorf("send CardKit entity (outcome may be unknown): %w", err))
	}
	stream.state.MessageID = messageID
	if err := stream.commit(ctx, "active"); err != nil {
		return nil, err
	}
	return stream, nil
}

func initialStreamCard(title string) (string, error) {
	if strings.TrimSpace(title) == "" {
		title = "正在处理"
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"streaming_mode": true},
		"header": map[string]any{"title": map[string]string{"tag": "plain_text", "content": title}},
		"body": map[string]any{"elements": []any{
			map[string]string{"tag": "markdown", "element_id": streamElementID, "content": ""},
		}},
	}
	data, err := json.Marshal(card)
	return string(data), err
}

type cardStream struct {
	mu       sync.Mutex
	owner    *Channel
	delivery channel.Delivery
	state    StreamState
	to       replyAddress
	lastSent time.Time
	blocked  bool // a failed state commit requires external reconciliation
}

func (s *cardStream) Update(ctx context.Context, message channel.OutboundMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateMessage(message); err != nil {
		return err
	}
	return s.updateLocked(ctx, message.Text, false)
}

func (s *cardStream) validateMessage(message channel.OutboundMessage) error {
	if message.DeliveryID != s.delivery.ID || message.Session != s.delivery.Session {
		return errors.New("CardKit update does not match its delivery or session")
	}
	return nil
}

func (s *cardStream) updateLocked(ctx context.Context, fullText string, final bool) error {
	if s.blocked {
		return errors.New("CardKit stream state is uncertain; reconcile before continuing")
	}
	if s.delivery.Phase != "active" {
		return fmt.Errorf("cannot update Feishu stream in phase %q", s.delivery.Phase)
	}
	if fullText == s.state.ConfirmedText {
		return nil
	}
	if !final && !strings.HasPrefix(fullText, s.state.ConfirmedText) {
		return errors.New("CardKit update must extend confirmed cumulative text")
	}
	if len(fullText) > maxStreamTextBytes {
		return errors.New("CardKit stream text exceeds 28 KiB")
	}
	if err := s.throttle(ctx); err != nil {
		return err
	}
	s.state.PendingText = fullText
	s.state.Sequence++
	if err := s.commit(ctx, "updating"); err != nil {
		return err
	}
	uuid := streamUUID(s.delivery, s.state, "content")
	request := larkcard.NewContentCardElementReqBuilder().CardId(s.state.CardID).ElementId(streamElementID).
		Body(larkcard.NewContentCardElementReqBodyBuilder().Uuid(uuid).Sequence(s.state.Sequence).Content(fullText).Build()).Build()
	response, err := s.owner.client.Cardkit.V1.CardElement.Content(ctx, request)
	if err != nil {
		return s.remoteFailure(ctx, fmt.Errorf("update CardKit text (outcome may be unknown): %w", err))
	}
	if response == nil || !response.Success() {
		return s.remoteFailure(ctx, fmt.Errorf("update CardKit text failed: %v", response))
	}
	s.lastSent = time.Now()
	s.state.ConfirmedText, s.state.PendingText = fullText, ""
	return s.commit(ctx, "active")
}

func (s *cardStream) Complete(ctx context.Context, message channel.OutboundMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateMessage(message); err != nil {
		return err
	}
	if err := s.updateLocked(ctx, message.Text, true); err != nil {
		return err
	}
	if err := s.throttle(ctx); err != nil {
		return err
	}
	s.state.Sequence++
	if err := s.commit(ctx, "closing"); err != nil {
		return err
	}
	uuid := streamUUID(s.delivery, s.state, "settings")
	request := larkcard.NewSettingsCardReqBuilder().CardId(s.state.CardID).
		Body(larkcard.NewSettingsCardReqBodyBuilder().Uuid(uuid).Sequence(s.state.Sequence).
			Settings(`{"config":{"streaming_mode":false}}`).Build()).Build()
	response, err := s.owner.client.Cardkit.V1.Card.Settings(ctx, request)
	if err != nil {
		return s.remoteFailure(ctx, fmt.Errorf("close CardKit streaming mode (outcome may be unknown): %w", err))
	}
	if response == nil || !response.Success() {
		return s.remoteFailure(ctx, fmt.Errorf("close CardKit streaming mode failed: %v", response))
	}
	return s.commit(ctx, "complete")
}

func (s *cardStream) commit(ctx context.Context, phase string) error {
	encoded, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	manager := channel.DeliveryManager{Store: s.owner.deliveries}
	var updated channel.Delivery
	switch phase {
	case "creating", "sending", "updating", "closing":
		updated, err = manager.Intent(ctx, s.delivery, phase, encoded)
	case "created", "active":
		updated, err = manager.Confirm(ctx, s.delivery, false, encoded)
	case "complete":
		updated, err = manager.Confirm(ctx, s.delivery, true, encoded)
	case "unknown":
		updated, err = manager.Unknown(ctx, s.delivery)
	default:
		return fmt.Errorf("invalid CardKit delivery phase %q", phase)
	}
	if err != nil {
		s.blocked = true
		return fmt.Errorf("persist CardKit stream phase %s: %w", phase, err)
	}
	s.delivery = updated
	return nil
}

func (s *cardStream) remoteFailure(ctx context.Context, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.commit(cleanupCtx, "unknown"); err != nil {
		return errors.Join(cause, err)
	}
	s.blocked = true
	return cause
}

func (s *cardStream) throttle(ctx context.Context) error {
	remaining := streamUpdateInterval - time.Since(s.lastSent)
	if s.lastSent.IsZero() || remaining <= 0 {
		return nil
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func streamUUID(delivery channel.Delivery, state StreamState, operation string) string {
	hash := sha256.Sum256([]byte(delivery.Session.BindingID + "\x00" + delivery.ID + "\x00" + operation + "\x00" + fmt.Sprint(state.Sequence)))
	return hex.EncodeToString(hash[:16])
}

var _ channel.StreamingChannel = (*Channel)(nil)
var _ channel.ReplyStream = (*cardStream)(nil)
