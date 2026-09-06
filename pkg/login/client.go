package login

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/deployment"
	"github.com/aiomni/dune/pkg/sdk"
)

type Options struct {
	Site      string
	TLSConfig *tls.Config
}

type Client struct {
	site string
	http *http.Client
	tls  *tls.Config
}

func secureURL(u *url.URL) bool {
	return u.Scheme == "https" || u.Scheme == "wss" || ((u.Scheme == "http" || u.Scheme == "ws") && net.ParseIP(u.Hostname()).IsLoopback())
}

func New(options Options) (*Client, error) {
	u, err := deployment.Public(options.Site)
	if err != nil || !secureURL(u) {
		return nil, errors.New("Dune login requires HTTPS, except numeric loopback HTTP")
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if options.TLSConfig != nil {
		tc = options.TLSConfig.Clone()
		if tc.InsecureSkipVerify {
			return nil, errors.New("Dune login requires verified TLS")
		}
		if tc.MinVersion < tls.VersionTLS12 {
			tc.MinVersion = tls.VersionTLS12
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tc
	return &Client{site: u.String(), tls: tc, http: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

type APIError struct {
	Status int
	Code   string
}

func (e *APIError) Error() string {
	if e.Code == "RESULT_UNKNOWN" {
		return "Dune result is unknown; do not automatically retry"
	}
	if e.Status == 401 {
		return "Dune login is invalid, expired, cancelled or already consumed; start a new login"
	}
	return fmt.Sprintf("Dune request failed (HTTP %d, %s)", e.Status, e.Code)
}

func (c *Client) request(ctx context.Context, method, path, token string, body, out any) (int, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return 0, err
		}
	}
	r, err := http.NewRequestWithContext(ctx, method, c.site+path, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Dune-Request", "1")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.http.Do(r)
	if err != nil {
		return 0, errors.New("Dune request failed without a confirmed result; do not automatically retry")
	}
	defer response.Body.Close()
	data, err = io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return response.StatusCode, errors.New("Dune response incomplete or too large; result may be unknown")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(data, &failure)
		if len(failure.Code) > 64 || strings.ContainsFunc(failure.Code, func(r rune) bool { return r != '_' && (r < 'A' || r > 'Z') }) {
			failure.Code = "FAILED"
		}
		return response.StatusCode, &APIError{Status: response.StatusCode, Code: failure.Code}
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return response.StatusCode, errors.New("invalid Dune response; result may be unknown")
	}
	return response.StatusCode, nil
}

// Attempt keeps the local proof out of the browser URL. Consume must only be
// repeated after an explicit pending response, never after an uncertain result.
type Attempt struct {
	Request
	client *Client
	proof  string
}

func (c *Client) Begin(ctx context.Context) (*Attempt, error) {
	proof := wire.ID() + wire.ID()
	hash := sha256.Sum256([]byte(proof))
	var request Request
	_, err := c.request(ctx, "POST", "api/auth/cli/start", "", map[string]string{"challenge": hex.EncodeToString(hash[:])}, &request)
	if err != nil {
		return nil, err
	}
	id, err := hex.DecodeString(request.ID)
	if err != nil || len(id) != 16 || request.VerificationURL != c.site+"?cli_login="+request.ID || request.Code != strings.ToUpper(request.ID[:8]) || request.ExpiresAt <= time.Now().Unix() || request.ExpiresAt > time.Now().Add(11*time.Minute).Unix() {
		return nil, errors.New("invalid Dune login challenge")
	}
	return &Attempt{Request: request, client: c, proof: proof}, nil
}

func (a *Attempt) Consume(ctx context.Context) (Session, bool, error) {
	var session Session
	status, err := a.client.request(ctx, "POST", "api/auth/cli/consume", "", map[string]string{"id": a.ID, "verifier": a.proof}, &session)
	if err != nil {
		return Session{}, false, err
	}
	if status == http.StatusAccepted {
		return Session{}, true, nil
	}
	if err := a.client.validateSession(session); err != nil {
		return Session{}, false, err
	}
	if session.ExpiresAt <= time.Now().Unix() || session.ExpiresAt > time.Now().Add(8*time.Hour+time.Minute).Unix() {
		return Session{}, false, errors.New("invalid CLI session lifetime")
	}
	return session, false, nil
}

func (a *Attempt) Wait(ctx context.Context) (Session, error) {
	ctx, cancel := context.WithDeadline(ctx, time.Unix(a.ExpiresAt, 0))
	defer cancel()
	for {
		session, pending, err := a.Consume(ctx)
		if err != nil {
			return Session{}, err
		}
		if !pending {
			return session, nil
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Session{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) validateSession(session Session) error {
	raw := strings.TrimPrefix(session.Token, "dune_cli_")
	decoded, err := hex.DecodeString(raw)
	if session.Site != c.site || !strings.HasPrefix(session.Token, "dune_cli_") || err != nil || len(decoded) != 32 || session.PrincipalID == "" {
		return errors.New("invalid Dune CLI session")
	}
	return nil
}

func (c *Client) Machines(ctx context.Context, session Session) ([]Machine, error) {
	if err := c.validateSession(session); err != nil {
		return nil, err
	}
	var machines []Machine
	_, err := c.request(ctx, "GET", "api/cli/machines", session.Token, nil, &machines)
	return machines, err
}

func (c *Client) Logout(ctx context.Context, session Session) error {
	if err := c.validateSession(session); err != nil {
		return err
	}
	_, err := c.request(ctx, "POST", "api/cli/logout", session.Token, struct{}{}, nil)
	return err
}

func (c *Client) Dial(ctx context.Context, session Session, target string) (*sdk.Client, error) {
	if err := c.validateSession(session); err != nil {
		return nil, err
	}
	if target == "" {
		return nil, errors.New("Dune target is required")
	}
	var access Access
	_, err := c.request(ctx, "POST", "api/cli/access", session.Token, map[string]string{"target": target}, &access)
	if err != nil {
		return nil, err
	}
	u, err := deployment.Gateway(access.Gateway)
	if err != nil || !secureURL(u) || access.Target != target || access.ExpiresAt <= time.Now().Unix() || access.Credential == "" {
		return nil, errors.New("invalid Dune access credential")
	}
	return sdk.Dial(ctx, sdk.Options{Gateway: access.Gateway, Token: access.Credential, Target: target, TLSConfig: c.tls})
}
