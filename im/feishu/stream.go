package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aiomni/dune/im/channel"
	larkcard "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
)

const streamElementID = "answer"
const streamUpdateInterval = 110 * time.Millisecond // below CardKit's 10 updates/s/card ceiling
const maxStreamUpdateRequestBytes = 28 * 1024       // conservative local budget, not a documented CardKit API limit

// StreamState is version 2 of Feishu's opaque provider_state. The common
// delivery record owns identity, address, phase and revision.
type StreamState struct {
	CardID            string       `json:"card_id,omitempty"`
	MessageID         string       `json:"message_id,omitempty"`
	ConfirmedText     string       `json:"confirmed_text,omitempty"`
	PendingText       string       `json:"pending_text,omitempty"`
	Sequence          int          `json:"sequence"`
	Parts             []StreamPart `json:"parts,omitempty"`
	NeedsContinuation bool         `json:"needs_continuation,omitempty"`
}

type StreamPart struct {
	CardID    string `json:"card_id"`
	MessageID string `json:"message_id"`
	Text      string `json:"text"`
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
		Address: address, Mode: string(ReplyStreaming), ProviderStateVersion: 2,
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
		if delivery.Phase == "active" && !state.NeedsContinuation && state.CardID != "" && state.MessageID != "" {
			return &cardStream{owner: c, delivery: delivery, state: state, to: to}, nil
		}
		return nil, fmt.Errorf("Feishu stream %q is %s; reconcile before retry", initial.DeliveryID, delivery.Phase)
	}
	stream := &cardStream{owner: c, delivery: delivery, state: state, to: to}
	if err := stream.startCard(ctx); err != nil {
		return nil, err
	}
	return stream, nil
}

func (s *cardStream) startCard(ctx context.Context) error {
	s.state.CardID, s.state.MessageID, s.state.ConfirmedText, s.state.PendingText = "", "", "", ""
	s.state.Sequence = 0
	s.state.NeedsContinuation = false
	if err := s.commit(ctx, "creating"); err != nil {
		return err
	}
	cardJSON, err := streamCardJSON("", true)
	if err != nil {
		return err
	}
	response, err := s.owner.client.Cardkit.V1.Card.Create(ctx, larkcard.NewCreateCardReqBuilder().
		Body(larkcard.NewCreateCardReqBodyBuilder().Type("card_json").Data(cardJSON).Build()).Build())
	if err != nil {
		return s.remoteFailure(ctx, fmt.Errorf("create CardKit entity (outcome may be unknown): %w", err))
	}
	if response == nil || !response.Success() || response.Data == nil || value(response.Data.CardId) == "" {
		return s.remoteFailure(ctx, fmt.Errorf("create CardKit entity failed: %v", response))
	}
	s.state.CardID = value(response.Data.CardId)
	if err := s.commit(ctx, "created"); err != nil {
		return err
	}
	content, err := json.Marshal(map[string]any{"type": "card", "data": map[string]string{"card_id": s.state.CardID}})
	if err != nil {
		return err
	}
	if err := s.commit(ctx, "sending"); err != nil {
		return err
	}
	operation := "card-message"
	if len(s.state.Parts) > 0 {
		operation = fmt.Sprintf("card-message-%d", len(s.state.Parts)+1)
	}
	messageID, err := s.owner.sendContent(ctx, s.to, "interactive", string(content), outboundUUID(s.delivery.Session.BindingID, s.delivery.ID, operation))
	if err != nil {
		return s.remoteFailure(ctx, fmt.Errorf("send CardKit entity (outcome may be unknown): %w", err))
	}
	s.state.MessageID = messageID
	s.lastSent = time.Time{}
	return s.commit(ctx, "active")
}

func streamCardJSON(text string, streaming bool) (string, error) {
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"streaming_mode": streaming, "update_multi": true},
		"body": map[string]any{"elements": []any{
			map[string]string{"tag": "markdown", "element_id": streamElementID, "content": text},
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
	if s.state.NeedsContinuation || s.state.CardID == "" || s.state.MessageID == "" {
		return errors.New("CardKit stream has no active card; reconcile before continuing")
	}
	first, fits := streamChunkPrefix(fullText)
	if !final && !fits {
		fullText = first
	}
	if fullText == s.state.ConfirmedText {
		return nil
	}
	if !final && !strings.HasPrefix(fullText, s.state.ConfirmedText) {
		return errors.New("CardKit update must extend confirmed cumulative text")
	}
	if final && !fits {
		return errors.New("CardKit stream update request exceeds the local 28 KiB budget")
	}
	if err := s.throttle(ctx); err != nil {
		return err
	}
	next := s.state
	next.Sequence++
	if next.Sequence < 1 || next.Sequence > math.MaxInt32 {
		return errors.New("CardKit stream sequence exceeds the API range")
	}
	uuid := streamUUID(s.delivery, next, "content")
	body := larkcard.NewContentCardElementReqBodyBuilder().Uuid(uuid).Sequence(next.Sequence).Content(fullText).Build()
	encoded, err := json.Marshal(body)
	if err != nil || len(encoded) > maxStreamUpdateRequestBytes {
		return errors.New("CardKit stream update request exceeds the local 28 KiB budget")
	}
	s.state.PendingText = fullText
	s.state.Sequence = next.Sequence
	if err := s.commit(ctx, "updating"); err != nil {
		return err
	}
	request := larkcard.NewContentCardElementReqBuilder().CardId(s.state.CardID).ElementId(streamElementID).
		Body(body).Build()
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
	s.delivery.AgentTurnCompleted = message.AgentTurnCompleted
	chunks := streamChunks(message.Text)
	for index, chunk := range chunks {
		if index > 0 {
			if err := s.startCard(ctx); err != nil {
				return err
			}
		}
		if err := s.updateLocked(ctx, chunk, true); err != nil {
			return err
		}
		if err := s.closeCard(ctx, index == len(chunks)-1); err != nil {
			return err
		}
	}
	return nil
}

func (s *cardStream) closeCard(ctx context.Context, complete bool) error {
	cardJSON, err := streamCardJSON(s.state.ConfirmedText, false)
	if err != nil {
		return err
	}
	if err := s.throttle(ctx); err != nil {
		return err
	}
	if s.state.Sequence >= math.MaxInt32 {
		return errors.New("CardKit stream sequence exceeds the API range")
	}
	s.state.Sequence++
	if err := s.commit(ctx, "closing"); err != nil {
		return err
	}
	// Finalize the card body and streaming mode in one confirmed operation.
	uuid := streamUUID(s.delivery, s.state, "final-card")
	request := larkcard.NewUpdateCardReqBuilder().CardId(s.state.CardID).
		Body(larkcard.NewUpdateCardReqBodyBuilder().Uuid(uuid).Sequence(s.state.Sequence).
			Card(larkcard.NewCardBuilder().Type("card_json").Data(cardJSON).Build()).Build()).Build()
	response, err := s.owner.client.Cardkit.V1.Card.Update(ctx, request)
	if err != nil {
		return s.remoteFailure(ctx, fmt.Errorf("finalize CardKit card (outcome may be unknown): %w", err))
	}
	if response == nil || !response.Success() {
		return s.remoteFailure(ctx, fmt.Errorf("finalize CardKit card failed: %v", response))
	}
	if complete {
		return s.commit(ctx, "complete")
	}
	s.state.Parts = append(s.state.Parts, StreamPart{CardID: s.state.CardID, MessageID: s.state.MessageID, Text: s.state.ConfirmedText})
	s.state.NeedsContinuation = true
	return s.commit(ctx, "active")
}

// streamChunks splits on rune boundaries by the encoded update request size.
// CardKit's content field is JSON escaped; raw text bytes alone undercount
// quotes, backslashes and control characters.
func streamChunks(text string) []string {
	if text == "" {
		return []string{text}
	}
	chunks := make([]string, 0, len(text)/maxStreamUpdateRequestBytes+1)
	for text != "" {
		part, fits := streamChunkPrefix(text)
		chunks = append(chunks, part)
		if fits {
			break
		}
		text = text[len(part):]
	}
	return chunks
}

func streamChunkPrefix(text string) (string, bool) {
	maxBody := larkcard.NewContentCardElementReqBodyBuilder().Uuid(strings.Repeat("0", 32)).Sequence(math.MaxInt32).Content("").Build()
	base, _ := json.Marshal(maxBody) // fixed string/int fields cannot fail JSON encoding
	budget := maxStreamUpdateRequestBytes - len(base)
	size := 0
	for index := 0; index < len(text); {
		r, width := utf8.DecodeRuneInString(text[index:])
		runeBytes := jsonEscapedRuneBytes(r)
		if r == utf8.RuneError && width == 1 {
			// encoding/json replaces each invalid input byte with \ufffd.
			runeBytes = 6
		}
		if size+runeBytes > budget {
			return text[:index], false
		}
		size += runeBytes
		index += width
	}
	return text, true
}

// Match encoding/json's default string escaping without allocating a JSON
// string for every rune of a potentially one-megabyte Agent answer.
func jsonEscapedRuneBytes(r rune) int {
	switch r {
	case '"', '\\', '\b', '\f', '\n', '\r', '\t':
		return 2
	case '<', '>', '&', '\u2028', '\u2029':
		return 6
	}
	if r < 0x20 {
		return 6
	}
	return utf8.RuneLen(r)
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
		updated, err = s.confirm(ctx, manager, false, encoded)
	case "complete":
		updated, err = s.confirm(ctx, manager, true, encoded)
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

// A confirmed CardKit response can still be followed by request cancellation
// before its local state is recorded. Retrying only the durable confirmation
// preserves that evidence without issuing another platform operation.
func (s *cardStream) confirm(ctx context.Context, manager channel.DeliveryManager, complete bool, state json.RawMessage) (channel.Delivery, error) {
	updated, err := manager.Confirm(ctx, s.delivery, complete, state)
	if err == nil {
		return updated, nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return manager.Confirm(cleanupCtx, s.delivery, complete, state)
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
	hash := sha256.Sum256([]byte(delivery.Session.BindingID + "\x00" + delivery.ID + "\x00" + state.CardID + "\x00" + operation + "\x00" + fmt.Sprint(state.Sequence)))
	return hex.EncodeToString(hash[:16])
}

var _ channel.StreamingChannel = (*Channel)(nil)
var _ channel.ReplyStream = (*cardStream)(nil)
