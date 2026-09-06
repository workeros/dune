// Package oidc provides Authorization Code + PKCE identity verification using
// go-oidc and oauth2. It never persists upstream tokens.
package oidc

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/aiomni/dune/pkg/identity"
	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type Config struct {
	Issuer, ClientID, ClientSecret string
	// HTTPClient optionally supplies private trust/transport configuration.
	// Dune copies the client and bounds HTTP requests to ten seconds.
	HTTPClient *http.Client
}

type Provider struct {
	issuer   string
	oauth    oauth2.Config
	verifier *coreoidc.IDTokenVerifier
	client   *http.Client
}

func validURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback())
}

// Open discovers a configured issuer. HTTPS is required except numeric loopback
// HTTP for local development. There are no options to skip token verification.
func Open(ctx context.Context, config Config) (*Provider, error) {
	issuer, err := url.Parse(config.Issuer)
	if err != nil || !validURL(config.Issuer) || issuer.RawQuery != "" || config.ClientID == "" {
		return nil, fmt.Errorf("valid OIDC issuer and client ID required")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if config.HTTPClient != nil {
		*client = *config.HTTPClient
		if client.Timeout <= 0 || client.Timeout > 10*time.Second {
			client.Timeout = 10 * time.Second
		}
	}
	discovered, err := coreoidc.NewProvider(coreoidc.ClientContext(ctx, client), config.Issuer)
	if err != nil {
		return nil, errors.New("OIDC discovery failed")
	}
	var metadata struct {
		Keys        string   `json:"jwks_uri"`
		AuthMethods []string `json:"token_endpoint_auth_methods_supported"`
	}
	if discovered.Claims(&metadata) != nil || !validURL(metadata.Keys) || !validURL(discovered.Endpoint().AuthURL) || !validURL(discovered.Endpoint().TokenURL) {
		return nil, errors.New("invalid OIDC endpoints")
	}
	endpoint := discovered.Endpoint()
	// Never use oauth2's automatic auth-style probing: it retries the same
	// authorization code after an ambiguous token response.
	endpoint.AuthStyle = oauth2.AuthStyleInHeader
	if config.ClientSecret == "" {
		endpoint.AuthStyle = oauth2.AuthStyleInParams
	} else if len(metadata.AuthMethods) > 0 {
		basic, post := false, false
		for _, method := range metadata.AuthMethods {
			basic = basic || method == "client_secret_basic"
			post = post || method == "client_secret_post"
		}
		if !basic && !post {
			return nil, errors.New("unsupported OIDC token authentication method")
		}
		if !basic {
			endpoint.AuthStyle = oauth2.AuthStyleInParams
		}
	}
	return &Provider{issuer: config.Issuer, client: client, oauth: oauth2.Config{ClientID: config.ClientID, ClientSecret: config.ClientSecret, Endpoint: endpoint, Scopes: []string{coreoidc.ScopeOpenID, "email"}}, verifier: discovered.VerifierContext(coreoidc.ClientContext(ctx, client), &coreoidc.Config{ClientID: config.ClientID})}, nil
}

func (p *Provider) Namespace() string { return p.issuer }

func (p *Provider) Begin(ctx context.Context, c identity.Challenge) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !validURL(c.RedirectURL) || c.State == "" || c.Nonce == "" || len(c.Verifier) < 43 || len(c.Verifier) > 128 {
		return "", errors.New("invalid login challenge")
	}
	config := p.oauth
	config.RedirectURL = c.RedirectURL
	return config.AuthCodeURL(c.State, coreoidc.Nonce(c.Nonce), oauth2.S256ChallengeOption(c.Verifier)), nil
}

func (p *Provider) Verify(ctx context.Context, c identity.Challenge, code string) (identity.Subject, error) {
	if _, err := p.Begin(ctx, c); err != nil {
		return identity.Subject{}, err
	}
	if code == "" || len(code) > 8192 {
		return identity.Subject{}, errors.New("invalid authorization code")
	}
	config := p.oauth
	config.RedirectURL = c.RedirectURL
	exchangeClient := *p.client
	exchangeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	token, err := config.Exchange(coreoidc.ClientContext(ctx, &exchangeClient), code, oauth2.VerifierOption(c.Verifier))
	if err != nil {
		return identity.Subject{}, errors.New("OIDC code exchange failed; start a new login")
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return identity.Subject{}, errors.New("OIDC ID token missing")
	}
	id, err := p.verifier.Verify(ctx, raw)
	if err != nil || id.Issuer != p.issuer || subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(c.Nonce)) != 1 {
		return identity.Subject{}, errors.New("OIDC ID token verification failed")
	}
	var claims struct {
		Email           string `json:"email"`
		AuthorizedParty string `json:"azp"`
	}
	if id.Claims(&claims) != nil || id.Subject == "" || id.IssuedAt.IsZero() || (len(id.Audience) > 1 && claims.AuthorizedParty != config.ClientID) || (claims.AuthorizedParty != "" && claims.AuthorizedParty != config.ClientID) || id.IssuedAt.After(time.Now().Add(time.Minute)) {
		return identity.Subject{}, errors.New("invalid OIDC claims")
	}
	if id.AccessTokenHash != "" {
		if err := id.VerifyAccessToken(token.AccessToken); err != nil {
			return identity.Subject{}, errors.New("invalid OIDC access-token binding")
		}
	}
	return identity.Subject{ID: id.Subject, Email: claims.Email}, nil
}
