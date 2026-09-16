package feishu

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/aiomni/dune/im/channel"
)

// ServiceStore is the persistence contract needed to run Feishu bindings. A
// single-host deployment may use im/sqlite; a clustered host must implement
// the same atomic semantics on shared storage.
type ServiceStore interface {
	channel.BindingStore
	channel.WorkQueue
	channel.ConversationStore
	channel.DeliveryStore
	channel.StatusStore
	channel.IssueStore
	channel.RecoveryStore
}

type CredentialResolver interface {
	ResolveCredentials(context.Context, channel.BotBinding) ([]byte, error)
}

type Service struct {
	store       ServiceStore
	credentials CredentialResolver
	agents      channel.AgentBackend
	provider    Provider
	lifetime    context.Context
	cancel      context.CancelFunc
	mu          sync.RWMutex
	bindings    map[string]*runningBinding
	closed      bool
	running     bool
	wg          sync.WaitGroup
}

type runningBinding struct {
	binding channel.BotBinding
	config  Config
	channel *Channel
	cancel  context.CancelFunc
	err     error
}

// BindingStatus contains no credentials or raw event text. A WebSocket state
// of connected is reported only after the SDK's Ready/Reconnected callback;
// callback_ready means the local handler is mounted, not publicly reachable.
type BindingStatus struct {
	TenantID       string               `json:"tenant_id"`
	BindingID      string               `json:"binding_id"`
	Revision       int64                `json:"revision"`
	ReceiveMode    string               `json:"receive_mode"`
	ReplyMode      string               `json:"reply_mode"`
	TransportState string               `json:"transport_state"`
	Stats          channel.BindingStats `json:"stats"`
}

func (s *Service) Status(ctx context.Context, tenantID, bindingID string) (BindingStatus, error) {
	binding, found, err := s.store.GetBinding(ctx, tenantID, bindingID)
	if err != nil {
		return BindingStatus{}, err
	}
	if !found || binding.Provider != Kind {
		return BindingStatus{}, errors.New("Feishu Binding not found in Tenant")
	}
	var config Config
	if err := strictJSON(binding.Config, &config); err != nil {
		return BindingStatus{}, fmt.Errorf("invalid persisted Feishu Binding config: %w", err)
	}
	if config.ReplyMode == "" {
		config.ReplyMode = ReplyFinalText
	}
	stats, err := s.store.BindingStats(ctx, bindingID)
	if err != nil {
		return BindingStatus{}, err
	}
	status := BindingStatus{TenantID: tenantID, BindingID: bindingID, Revision: binding.Revision,
		ReceiveMode: config.ReceiveMode, ReplyMode: config.ReplyMode, Stats: stats, TransportState: "inactive"}
	if !binding.Enabled {
		status.TransportState = "disabled"
		return status, nil
	}
	s.mu.RLock()
	entry := s.bindings[bindingID]
	if entry != nil && entry.binding.TenantID == tenantID && entry.binding.Revision == binding.Revision {
		status.TransportState = entry.channel.TransportState()
		if entry.err != nil {
			status.TransportState = "failed"
		}
	}
	s.mu.RUnlock()
	return status, nil
}

// Issues returns bounded, read-only recovery context for a Tenant-owned bot.
// It deliberately cannot authorize or perform a replay of an uncertain turn.
func (s *Service) Issues(ctx context.Context, tenantID, bindingID string, perCategoryLimit int) (channel.BindingIssues, error) {
	binding, found, err := s.store.GetBinding(ctx, tenantID, bindingID)
	if err != nil {
		return channel.BindingIssues{}, err
	}
	if !found || binding.Provider != Kind {
		return channel.BindingIssues{}, errors.New("Feishu Binding not found in Tenant")
	}
	return s.store.ListIssues(ctx, bindingID, perCategoryLimit)
}

// ReconcileConfirmedDelivery clears unfinished bookkeeping left after this
// turn's delivery was durably confirmed complete. A running turn also needs
// an expired worker lease. It never resends a message or Agent prompt and
// remains Tenant-scoped.
func (s *Service) ReconcileConfirmedDelivery(ctx context.Context, tenantID string, key channel.SessionKey, eventID string) error {
	if tenantID == "" || key.TenantID != tenantID || key.BindingID == "" {
		return errors.New("Feishu recovery scope does not match Tenant or Binding")
	}
	binding, found, err := s.store.GetBinding(ctx, tenantID, key.BindingID)
	if err != nil {
		return err
	}
	if !found || binding.Provider != Kind {
		return errors.New("Feishu Binding not found in Tenant")
	}
	return s.store.ReconcileConfirmedDelivery(ctx, key, eventID)
}

func NewService(store ServiceStore, credentials CredentialResolver, agents channel.AgentBackend) (*Service, error) {
	if store == nil || credentials == nil || agents == nil {
		return nil, errors.New("Feishu service requires store, credential resolver and Agent backend")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &Service{store: store, credentials: credentials, agents: agents, lifetime: lifetime, cancel: cancel,
		provider: Provider{Deliveries: store}, bindings: map[string]*runningBinding{}}, nil
}

// LoadTenant synchronizes all Feishu Bindings for a Tenant. The caller owns
// tenant discovery and authorization; no IM sender can add a Binding here.
func (s *Service) LoadTenant(ctx context.Context, tenantID string) error {
	bindings, err := s.store.ListBindings(ctx, tenantID)
	if err != nil {
		return err
	}
	wanted := make(map[string]struct{})
	for _, binding := range bindings {
		if binding.Enabled && binding.Provider == Kind {
			wanted[binding.ID] = struct{}{}
			if err := s.Activate(ctx, binding); err != nil {
				return fmt.Errorf("activate Feishu Binding %q: %w", binding.ID, err)
			}
		}
	}
	s.mu.RLock()
	var obsolete []*runningBinding
	for id, entry := range s.bindings {
		if entry.binding.TenantID == tenantID {
			if _, keep := wanted[id]; !keep {
				obsolete = append(obsolete, entry)
			}
		}
	}
	s.mu.RUnlock()
	for _, entry := range obsolete {
		if err := s.deactivate(ctx, tenantID, entry.binding.ID, entry); err != nil {
			return err
		}
	}
	return nil
}

// Deactivate stops exactly one Tenant-owned Binding. It is idempotent for a
// Binding that is already inactive, but never touches another Tenant's bot.
func (s *Service) Deactivate(ctx context.Context, tenantID, bindingID string) error {
	return s.deactivate(ctx, tenantID, bindingID, nil)
}

func (s *Service) deactivate(ctx context.Context, tenantID, bindingID string, expected *runningBinding) error {
	if tenantID == "" || bindingID == "" {
		return errors.New("Feishu Tenant and Binding IDs are required")
	}
	s.mu.Lock()
	entry := s.bindings[bindingID]
	if entry != nil && entry.binding.TenantID != tenantID {
		s.mu.Unlock()
		return errors.New("Feishu Binding belongs to another Tenant")
	}
	if expected != nil && entry != expected {
		s.mu.Unlock()
		return nil // superseded by a newer activation
	}
	if entry != nil {
		delete(s.bindings, bindingID)
		entry.cancel()
	}
	s.mu.Unlock()
	if entry == nil {
		return nil
	}
	return entry.channel.Stop(ctx)
}

// Activate replaces one Binding revision. The new channel is validated before
// replacing the old one; the old transport is stopped before the new WebSocket
// starts, avoiding two live event receivers for one bot.
func (s *Service) Activate(ctx context.Context, binding channel.BotBinding) error {
	if binding.Provider != Kind || !binding.Enabled || binding.ID == "" || binding.TenantID == "" {
		return errors.New("enabled Feishu Binding is required")
	}
	stored, found, err := s.store.GetBinding(ctx, binding.TenantID, binding.ID)
	if err != nil {
		return err
	}
	if !found || stored.Revision != binding.Revision || !stored.Enabled {
		return errors.New("Feishu Binding is stale or disabled")
	}
	// Only persisted configuration is authoritative. The caller identifies a
	// revision, but must not be able to substitute credentials or Agent target.
	binding = stored
	credentials, err := s.credentials.ResolveCredentials(ctx, binding)
	if err != nil {
		return fmt.Errorf("resolve Feishu credentials: %w", err)
	}
	config, _, err := parseConfig(binding.Config, credentials)
	if err != nil {
		return err
	}
	opened, err := s.provider.Open(ctx, binding, credentials, channel.Ingress{Inbox: s.store})
	if err != nil {
		return err
	}
	ch := opened.(*Channel)
	entryCtx, cancel := context.WithCancel(s.lifetime)
	entry := &runningBinding{binding: binding, config: config, channel: ch, cancel: cancel}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return errors.New("Feishu service is closed")
	}
	previous := s.bindings[binding.ID]
	if previous != nil && previous.binding.TenantID != binding.TenantID {
		s.mu.Unlock()
		cancel()
		return errors.New("Feishu Binding ID already belongs to another Tenant")
	}
	if previous != nil && previous.binding.Revision > binding.Revision {
		s.mu.Unlock()
		cancel()
		return errors.New("Feishu Binding revision moved backwards")
	}
	// Hold the registry lock while stopping the old transport so callbacks
	// never resolve a half-replaced Binding. Stop is bounded by caller ctx.
	if previous != nil {
		previous.cancel()
		if err := previous.channel.Stop(ctx); err != nil {
			s.mu.Unlock()
			cancel()
			return err
		}
	}
	s.bindings[binding.ID] = entry
	if config.ReceiveMode == ReceiveWebSocket {
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if config.ReceiveMode == ReceiveWebSocket {
		go func() {
			defer s.wg.Done()
			err := ch.Start(entryCtx)
			s.mu.Lock()
			entry.err = err
			s.mu.Unlock()
		}()
	}
	return nil
}

// CallbackHandler is Feishu-specific; the common channel.EventSink interface
// remains the sole inbound boundary. Mount this handler at a Binding-specific
// public route and keep that route free of credentials.
func (s *Service) CallbackHandler(tenantID, bindingID string) (http.Handler, error) {
	s.mu.RLock()
	entry := s.bindings[bindingID]
	if entry == nil || entry.binding.TenantID != tenantID || entry.config.ReceiveMode != ReceiveCallback {
		s.mu.RUnlock()
		return nil, errors.New("Feishu callback Binding is not active")
	}
	s.mu.RUnlock()
	// Resolve the active Channel on each request so an already mounted route
	// follows credential rotation and never keeps the old handler alive.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		current := s.bindings[bindingID]
		if current == nil || current.binding.TenantID != tenantID || current.config.ReceiveMode != ReceiveCallback || current.err != nil {
			http.Error(w, "Feishu callback Binding is not active", http.StatusServiceUnavailable)
			return
		}
		current.channel.CallbackHandler().ServeHTTP(w, r)
	}), nil
}

func (s *Service) LookupBinding(ctx context.Context, bindingID string) (channel.ActiveBinding, error) {
	s.mu.RLock()
	entry := s.bindings[bindingID]
	s.mu.RUnlock()
	if entry == nil {
		stored, found, err := s.store.GetBindingByID(ctx, bindingID)
		if err != nil {
			return channel.ActiveBinding{}, err
		}
		if found && !stored.Enabled {
			return channel.ActiveBinding{Binding: stored}, nil
		}
		return channel.ActiveBinding{}, errors.New("Feishu Binding is not active")
	}
	stored, found, err := s.store.GetBinding(ctx, entry.binding.TenantID, bindingID)
	if err != nil {
		return channel.ActiveBinding{}, err
	}
	if !found {
		return channel.ActiveBinding{}, errors.New("Feishu Binding no longer exists")
	}
	if !stored.Enabled {
		return channel.ActiveBinding{Binding: stored}, nil
	}
	if stored.Revision != entry.binding.Revision {
		return channel.ActiveBinding{}, errors.New("Feishu Binding revision changed; activate current revision before processing")
	}
	s.mu.RLock()
	current := s.bindings[bindingID] == entry && entry.err == nil
	s.mu.RUnlock()
	if !current {
		return channel.ActiveBinding{}, errors.New("Feishu Binding is not active")
	}
	return channel.ActiveBinding{Binding: entry.binding, Channel: entry.channel,
		Subjects: RootResolver{Conversations: s.store, Messages: entry.channel}, Admission: entry.channel,
		Streaming: entry.config.ReplyMode == ReplyStreaming, ReplyMode: string(entry.config.ReplyMode)}, nil
}

// Run processes accepted events until cancellation. The worker count is
// bounded; each worker relies on durable claim and conversation leases rather
// than a process-local mutex for correctness.
func (s *Service) Run(ctx context.Context, workers int, onError func(error)) error {
	if workers < 1 || workers > 16 {
		return errors.New("Feishu worker count must be 1..16")
	}
	s.mu.Lock()
	if s.closed || s.running {
		s.mu.Unlock()
		return errors.New("Feishu service is closed or already running")
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()
	runCtx, cancelRun := context.WithCancel(ctx)
	stopOnServiceClose := context.AfterFunc(s.lifetime, cancelRun)
	defer stopOnServiceClose()
	defer cancelRun()
	processor := channel.Processor{Work: s.store, Conversations: s.store, Deliveries: s.store, Bindings: s, Agents: s.agents}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for runCtx.Err() == nil {
				found, err := processor.ProcessOne(runCtx)
				if err != nil && onError != nil && runCtx.Err() == nil {
					onError(err)
				}
				if err != nil || !found {
					timer := time.NewTimer(250 * time.Millisecond)
					select {
					case <-runCtx.Done():
						timer.Stop()
					case <-timer.C:
					}
				}
			}
		}()
	}
	<-runCtx.Done()
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopErr := s.Stop(stopCtx)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return stopErr
	case <-stopCtx.Done():
		return errors.Join(stopErr, stopCtx.Err())
	}
}

func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	entries := make([]*runningBinding, 0, len(s.bindings))
	for _, entry := range s.bindings {
		entries = append(entries, entry)
	}
	s.bindings = map[string]*runningBinding{}
	s.mu.Unlock()
	var result error
	for _, entry := range entries {
		entry.cancel()
		result = errors.Join(result, entry.channel.Stop(ctx))
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return result
	case <-ctx.Done():
		return errors.Join(result, ctx.Err())
	}
}

var _ channel.ActiveBindings = (*Service)(nil)
