// Package upgradecontrol connects an independent Runner worker to its original
// host. Machine credentials authenticate control only; the host performs the
// actual read-only probe through the normal Gateway route.
package upgradecontrol

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/upgrade"
)

type Request struct {
	Action   string             `json:"action"`
	Binding  runner.Binding     `json:"binding"`
	Release  upgrade.ReleaseRef `json:"release,omitempty"`
	Platform upgrade.Platform   `json:"platform,omitempty"`
	Probe    upgrade.Probe      `json:"probe,omitempty"`
}

type Response struct {
	Binding    runner.Binding      `json:"binding"`
	Release    *upgrade.Manifest   `json:"release,omitempty"`
	Inspection *upgrade.Inspection `json:"inspection,omitempty"`
	Proof      *upgrade.Proof      `json:"proof,omitempty"`
	ErrorCode  string              `json:"error_code,omitempty"`
}

type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

func New(endpoint, token string, trust *tls.Config) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || len(token) < 32 {
		return nil, fmt.Errorf("configured original upgrade control endpoint and machine credential required")
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: trust}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{endpoint: endpoint, token: token, http: client}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) call(ctx context.Context, request Request) (Response, error) {
	var result Response
	body, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	call, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	call.Header.Set("Authorization", "Bearer "+c.token)
	call.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(call)
	if err != nil {
		return result, &api.Error{Code: "UPGRADE_CONTROL_UNREACHABLE", Detail: "original host control endpoint is unreachable"}
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(response.Body, 256<<10))
	if err := decoder.Decode(&result); err != nil {
		return result, &api.Error{Code: "UPGRADE_CONTROL_INVALID", Detail: "invalid host control response"}
	}
	if response.StatusCode != http.StatusOK || result.ErrorCode != "" {
		code := result.ErrorCode
		if api.ValidateSubmissionID(code) != nil {
			code = "UPGRADE_CONTROL_REJECTED"
		}
		return result, &api.Error{Code: code, Detail: "original host refused upgrade control request"}
	}
	if !result.Binding.Valid() || (request.Binding.Valid() && result.Binding != request.Binding) {
		return result, &api.Error{Code: "STALE_BINDING", Detail: "host binding differs from original Runner"}
	}
	return result, nil
}

func (c *Client) Binding(ctx context.Context, expected runner.Binding) (runner.Binding, error) {
	result, err := c.call(ctx, Request{Action: "binding", Binding: expected})
	return result.Binding, err
}

func (c *Client) Resolve(ctx context.Context, binding runner.Binding, ref upgrade.ReleaseRef, platform upgrade.Platform) (upgrade.Manifest, error) {
	result, err := c.call(ctx, Request{Action: "release", Binding: binding, Release: ref, Platform: platform})
	if err != nil {
		return upgrade.Manifest{}, err
	}
	if result.Release == nil || result.Release.ValidateDownload() != nil || !result.Release.Matches(ref) || result.Release.Platform != platform {
		return upgrade.Manifest{}, &api.Error{Code: "RELEASE_CHANGED", Detail: "host release differs from frozen reference or platform"}
	}
	return *result.Release, nil
}

func (c *Client) Confirm(ctx context.Context, probe upgrade.Probe) (upgrade.Proof, error) {
	result, err := c.call(ctx, Request{Action: "confirm", Binding: probe.Binding, Probe: probe})
	if err != nil {
		return upgrade.Proof{}, err
	}
	if result.Proof == nil {
		return upgrade.Proof{}, &api.Error{Code: "UPGRADE_PROOF_MISSING", Detail: "host did not confirm a routed probe"}
	}
	return *result.Proof, nil
}

func (c *Client) Inspect(ctx context.Context, binding runner.Binding) (upgrade.Inspection, error) {
	result, err := c.call(ctx, Request{Action: "inspect", Binding: binding})
	if err != nil {
		return upgrade.Inspection{}, err
	}
	if result.Inspection == nil || result.Inspection.Binding != binding {
		return upgrade.Inspection{}, &api.Error{Code: "UPGRADE_INSPECTION_INVALID", Detail: "original Runner inspection missing"}
	}
	return *result.Inspection, nil
}
