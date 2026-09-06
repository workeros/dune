package identity

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/wire"
	public "github.com/aiomni/dune/pkg/identity"
)

const LoginLifetime = 10 * time.Minute

type LoginTransaction struct {
	StateHash, BrowserHash, Namespace, RedirectURL, Nonce, Verifier string
	ExpiresAt                                                       int64
}

type ExternalRepository interface {
	ReadSession(context.Context, string, int64) (User, error)
	DeleteSession(context.Context, string) error
	BeginLogin(context.Context, LoginTransaction) error
	ConsumeLogin(context.Context, string, string, string, string, int64) (LoginTransaction, error)
	ExternalLogin(context.Context, string, public.Subject, string, string, int64, int) (User, error)
}

type External struct {
	store     ExternalRepository
	provider  public.Provider
	namespace string
	lifetime  time.Duration
}

func NewExternal(store ExternalRepository, options public.Options) (*External, error) {
	if options.Provider == nil {
		return nil, fmt.Errorf("identity provider required")
	}
	namespace := options.Provider.Namespace()
	if namespace == "" || len(namespace) > 2048 || !utf8.ValidString(namespace) || strings.ContainsFunc(namespace, unicode.IsControl) {
		return nil, fmt.Errorf("stable identity namespace required")
	}
	lifetime := options.SessionLifetime
	if lifetime == 0 {
		lifetime = 8 * time.Hour
	}
	if lifetime < time.Minute || lifetime > 24*time.Hour {
		return nil, fmt.Errorf("external session lifetime must be 1 minute..24 hours")
	}
	return &External{store: store, provider: options.Provider, namespace: namespace, lifetime: lifetime}, nil
}

func (e *External) Begin(ctx context.Context, redirect string) (string, string, error) {
	c := public.Challenge{State: wire.ID() + wire.ID(), Nonce: wire.ID() + wire.ID(), Verifier: wire.ID() + wire.ID(), RedirectURL: redirect}
	proof := wire.ID() + wire.ID()
	location, err := e.provider.Begin(ctx, c)
	if err != nil {
		return "", "", err
	}
	u, err := url.Parse(location)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return "", "", fmt.Errorf("invalid identity redirect")
	}
	err = e.store.BeginLogin(ctx, LoginTransaction{StateHash: digest(c.State), BrowserHash: digest(proof), Namespace: e.namespace, RedirectURL: redirect, Nonce: c.Nonce, Verifier: c.Verifier, ExpiresAt: time.Now().Add(LoginLifetime).Unix()})
	if err != nil {
		return "", "", err
	}
	return location, proof, nil
}

func (e *External) Finish(ctx context.Context, state, proof, code, redirect string) (User, string, error) {
	if len(state) != 64 || len(proof) != 64 || code == "" || len(code) > 8192 {
		return User{}, "", ErrUnauthorized
	}
	login, err := e.store.ConsumeLogin(ctx, digest(state), digest(proof), e.namespace, redirect, time.Now().Unix())
	if err != nil {
		return User{}, "", err
	}
	// Consume before the external exchange. A timeout or lost response never
	// replays an upstream code; the user must explicitly start a new login.
	subject, err := e.provider.Verify(ctx, public.Challenge{State: state, Nonce: login.Nonce, Verifier: login.Verifier, RedirectURL: redirect}, code)
	if err != nil {
		return User{}, "", ErrUnauthorized
	}
	if subject.ID == "" || len(subject.ID) > 512 || len(subject.Email) > 254 || !utf8.ValidString(subject.ID+subject.Email) || strings.ContainsFunc(subject.ID+subject.Email, unicode.IsControl) {
		return User{}, "", ErrUnauthorized
	}
	token := wire.ID() + wire.ID()
	user, err := e.store.ExternalLogin(ctx, e.namespace, subject, wire.ID(), digest(token), time.Now().Add(e.lifetime).Unix(), maxSessions)
	if err != nil {
		return User{}, "", err
	}
	return user, token, nil
}

func (e *External) SessionLifetime() time.Duration { return e.lifetime }

func (e *External) Authenticate(ctx context.Context, token string) (User, error) {
	if len(token) != 64 {
		return User{}, ErrUnauthorized
	}
	user, err := e.store.ReadSession(ctx, digest(token), time.Now().Unix())
	if err == nil && user.Namespace != e.namespace {
		return User{}, ErrUnauthorized
	}
	return user, err
}
func (e *External) Logout(ctx context.Context, token string) error {
	return e.store.DeleteSession(ctx, digest(token))
}
func (*External) RegistrationAllowed() bool { return false }
func (*External) Register(context.Context, string, string) (User, string, error) {
	return User{}, "", ErrRegistrationDisabled
}
func (*External) Login(context.Context, string, string) (User, string, error) {
	return User{}, "", ErrUnauthorized
}
