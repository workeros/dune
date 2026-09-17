// Package feishu implements the Feishu transport and message protocol behind
// channel.EventSink. HTTP callbacks and WebSocket events share one event path.
package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aiomni/dune/im/channel"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

const (
	Kind             = "feishu"
	ReceiveWebSocket = "websocket"
	ReceiveCallback  = "callback"
	ReplyStreaming   = "streaming_card"
	ReplyFinalCard   = "final_card"
	ReplyFinalText   = "final_text"
)

type Config struct {
	AppID       string `json:"app_id"`
	Site        string `json:"site"`
	ReceiveMode string `json:"receive_mode"`
	ReplyMode   string `json:"reply_mode"`
}

type Credentials struct {
	AppSecret         string `json:"app_secret"`
	EncryptKey        string `json:"encrypt_key"`
	VerificationToken string `json:"verification_token"`
}

type Provider struct {
	// Deliveries must be durable for every reply mode.
	Deliveries channel.DeliveryStore
	wsDomain   string // package-local test endpoint; production uses Site
}

func (Provider) Kind() string { return Kind }

func (Provider) Validate(config json.RawMessage, credentials []byte) error {
	_, _, err := parseConfig(config, credentials)
	return err
}

func parseConfig(config json.RawMessage, credentials []byte) (Config, Credentials, error) {
	var cfg Config
	var secret Credentials
	if err := strictJSON(config, &cfg); err != nil {
		return cfg, secret, fmt.Errorf("feishu config: %w", err)
	}
	if err := strictJSON(credentials, &secret); err != nil {
		return cfg, secret, fmt.Errorf("feishu credentials: %w", err)
	}
	if !strings.HasPrefix(cfg.AppID, "cli_") || secret.AppSecret == "" {
		return cfg, secret, errors.New("feishu App ID and App Secret are required")
	}
	if cfg.Site == "" {
		cfg.Site = "feishu"
	}
	if cfg.Site != "feishu" && cfg.Site != "lark" {
		return cfg, secret, errors.New("feishu site must be feishu or lark")
	}
	if cfg.ReceiveMode != ReceiveWebSocket && cfg.ReceiveMode != ReceiveCallback {
		return cfg, secret, errors.New("feishu receive mode must be websocket or callback")
	}
	if cfg.ReplyMode == "" {
		cfg.ReplyMode = ReplyFinalText
	}
	if cfg.ReplyMode != ReplyFinalText && cfg.ReplyMode != ReplyFinalCard && cfg.ReplyMode != ReplyStreaming {
		return cfg, secret, errors.New("invalid feishu reply mode")
	}
	if cfg.ReceiveMode == ReceiveCallback && (secret.EncryptKey == "" || secret.VerificationToken == "") {
		return cfg, secret, errors.New("callback mode requires Encrypt Key and Verification Token")
	}
	return cfg, secret, nil
}

func strictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("expected exactly one JSON value")
	}
	return nil
}

// Open constructs exactly one transport for the binding. The returned channel
// additionally exposes CallbackHandler in callback mode; this is Feishu-only
// and is intentionally absent from the provider-independent channel package.
func (p Provider) Open(_ context.Context, binding channel.BotBinding, credentials []byte, sink channel.EventSink) (channel.Channel, error) {
	if sink == nil || binding.Provider != Kind || binding.ID == "" || binding.TenantID == "" {
		return nil, errors.New("invalid Feishu binding or event sink")
	}
	if binding.ConfigVersion != 1 {
		return nil, fmt.Errorf("unsupported Feishu config version %d", binding.ConfigVersion)
	}
	cfg, secret, err := parseConfig(binding.Config, credentials)
	if err != nil {
		return nil, err
	}
	if p.Deliveries == nil {
		return nil, errors.New("Feishu replies require durable DeliveryStore")
	}
	baseURL := lark.FeishuBaseUrl
	if cfg.Site == "lark" {
		baseURL = lark.LarkBaseUrl
	}
	wsDomain := baseURL
	if p.wsDomain != "" {
		wsDomain = p.wsDomain
	}
	c := &Channel{
		binding:    binding,
		config:     cfg,
		secret:     secret,
		sink:       sink,
		deliveries: p.Deliveries,
		client:     lark.NewClient(cfg.AppID, secret.AppSecret, lark.WithOpenBaseUrl(baseURL)),
		stopped:    make(chan struct{}),
		drained:    make(chan struct{}),
	}
	c.transportState = "callback_ready"
	if cfg.ReceiveMode == ReceiveWebSocket {
		c.transportState = "connecting"
		dispatch := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(c.onMessage)
		c.ws = larkws.NewClient(cfg.AppID, secret.AppSecret,
			larkws.WithDomain(wsDomain), larkws.WithEventHandler(dispatch),
			larkws.WithOnReady(func() { c.setTransportState("connected") }),
			larkws.WithOnError(func(error) { c.setTransportState("connection_error") }),
			larkws.WithOnReconnecting(func() { c.setTransportState("reconnecting") }),
			larkws.WithOnReconnected(func() { c.setTransportState("connected") }),
			larkws.WithOnDisconnected(func() { c.setTransportState("disconnected") }))
	}
	return c, nil
}

type Channel struct {
	binding              channel.BotBinding
	config               Config
	secret               Credentials
	sink                 channel.EventSink
	deliveries           channel.DeliveryStore
	client               *lark.Client
	ws                   *larkws.Client
	stopped              chan struct{}
	drained              chan struct{}
	stop                 sync.Once
	ingressMu            sync.RWMutex
	transportMu          sync.RWMutex
	transportState       string
	botMu                sync.Mutex
	botOpenID            string
	botIdentityFetchedAt time.Time
}

func (c *Channel) Start(ctx context.Context) error {
	select {
	case <-c.stopped:
		return errors.New("Feishu channel already stopped")
	default:
	}
	if c.ws != nil {
		err := c.ws.Start(ctx)
		if err != nil && ctx.Err() == nil {
			c.setTransportState("failed")
		}
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.stopped:
		return nil
	}
}

func (c *Channel) Stop(ctx context.Context) error {
	c.stop.Do(func() {
		close(c.stopped)
		c.setTransportState("stopped")
		// Event handlers that entered before Stop must finish before a
		// successful Stop returns. New handlers observe stopped under the
		// same ingress lock and cannot accept another event.
		go func() {
			c.ingressMu.Lock()
			c.ingressMu.Unlock()
			close(c.drained)
		}()
		if c.ws != nil {
			c.ws.Close()
		}
	})
	select {
	case <-c.drained:
	case <-ctx.Done():
		return fmt.Errorf("wait for Feishu inbound events to drain: %w", ctx.Err())
	}
	if c.ws != nil {
		return c.ws.CloseAndWait(ctx)
	}
	return nil
}

func (c *Channel) TransportState() string {
	c.transportMu.RLock()
	defer c.transportMu.RUnlock()
	return c.transportState
}

func (c *Channel) setTransportState(state string) {
	c.transportMu.Lock()
	defer c.transportMu.Unlock()
	select {
	case <-c.stopped:
		c.transportState = "stopped"
	default:
		c.transportState = state
	}
}

// CallbackHandler is deliberately provider-specific. It is available only
// when ReceiveMode is callback and never exposed through channel.Channel.
func (c *Channel) CallbackHandler() http.Handler {
	if c.config.ReceiveMode != ReceiveCallback {
		return nil
	}
	return http.HandlerFunc(c.serveCallback)
}

func (c *Channel) onMessage(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	c.ingressMu.RLock()
	defer c.ingressMu.RUnlock()
	select {
	case <-c.stopped:
		return errors.New("Feishu channel is stopped")
	default:
	}
	message, ok, err := c.normalize(event)
	if err != nil || !ok {
		return err
	}
	return c.sink.Accept(ctx, message)
}

// ensure the transport and normalized handler remain compile-time compatible.
var _ channel.Provider = Provider{}
var _ channel.Channel = (*Channel)(nil)
var _ channel.GroupAdmission = (*Channel)(nil)

// AddressedToBot admits a new group topic only when the structured mention
// targets this application's bot, not just any bot in the same group.
func (c *Channel) AddressedToBot(ctx context.Context, message channel.InboundMessage) (bool, error) {
	if message.BindingID != c.binding.ID || message.ChatKind != channel.ChatGroup {
		return false, errors.New("Feishu group admission does not match binding")
	}
	if len(message.MentionedIDs) == 0 {
		return false, nil
	}
	c.botMu.Lock()
	defer c.botMu.Unlock()
	if c.botOpenID == "" || time.Since(c.botIdentityFetchedAt) >= time.Hour {
		response, err := c.client.Get(ctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
		if err != nil {
			return false, fmt.Errorf("get Feishu bot identity: %w", err)
		}
		if response == nil || response.StatusCode != http.StatusOK {
			return false, fmt.Errorf("get Feishu bot identity: unexpected response %v", response)
		}
		var result struct {
			Code int `json:"code"`
			Bot  struct {
				OpenID string `json:"open_id"`
			} `json:"bot"`
		}
		if err := json.Unmarshal(response.RawBody, &result); err != nil {
			return false, fmt.Errorf("decode Feishu bot identity: %w", err)
		}
		if result.Code != 0 || result.Bot.OpenID == "" {
			return false, fmt.Errorf("Feishu bot identity unavailable: code %d", result.Code)
		}
		c.botOpenID, c.botIdentityFetchedAt = result.Bot.OpenID, time.Now()
	}
	for _, mentioned := range message.MentionedIDs {
		if mentioned == c.botOpenID {
			return true, nil
		}
	}
	return false, nil
}
