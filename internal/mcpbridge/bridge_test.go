package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testToken = "dune_agent_bridge_test_secret"

func bridgeHost(t *testing.T, intercept func(http.ResponseWriter, *http.Request, string) bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "host", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "agents_prompt", Description: "Submit once", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(request.Params.Arguments)}}, StructuredContent: map[string]any{"operation_ref": "accepted"}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken || r.URL.Path != "/prefix/api/v1/agent-mcp" {
			t.Error("request did not use the fixed authenticated endpoint")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var envelope struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &envelope)
		if intercept != nil && intercept(w, r, envelope.Method) {
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(host.Close)
	return host, &calls
}

func connectBridge(t *testing.T, endpoint string) (*mcp.ClientSession, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	bridge, native := net.Pipe()
	t.Cleanup(func() { bridge.Close(); native.Close() })
	done := make(chan error, 1)
	go func() { done <- Run(ctx, endpoint, testToken, bridge, bridge) }()
	connectCtx, connectCancel := context.WithTimeout(ctx, 5*time.Second)
	defer connectCancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "native", Version: "1"}, nil).Connect(connectCtx, &mcp.IOTransport{Reader: native, Writer: native}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session, cancel, done
}

func waitBridge(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not stop")
	}
}

func TestBridgeStdioCatalogCallAndEOF(t *testing.T) {
	host, calls := bridgeHost(t, nil)
	session, _, done := connectBridge(t, host.URL+"/prefix/api/v1/agent-mcp")
	catalog, err := session.ListTools(t.Context(), nil)
	if err != nil || len(catalog.Tools) != 1 || catalog.Tools[0].Name != "agents_prompt" || catalog.Tools[0].Description != "Submit once" {
		t.Fatalf("catalog: %+v, %v", catalog, err)
	}
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "agents_prompt", Arguments: map[string]any{"text": "hello"}})
	if err != nil || result.IsError || calls.Load() != 1 {
		t.Fatalf("call: %+v, %v, count %d", result, err, calls.Load())
	}
	encoded, _ := json.Marshal(result)
	if !bytes.Contains(encoded, []byte("accepted")) || !bytes.Contains(encoded, []byte("hello")) || bytes.Contains(encoded, []byte(testToken)) {
		t.Fatalf("unexpected reply: %s", encoded)
	}
	_ = session.Close()
	waitBridge(t, done)
}

func TestBridgeCancellationClosesStdio(t *testing.T) {
	host, _ := bridgeHost(t, nil)
	_, cancel, done := connectBridge(t, host.URL+"/prefix/api/v1/agent-mcp")
	cancel()
	waitBridge(t, done)
}

func TestBridgeUnknownCallIsNotReplayed(t *testing.T) {
	var received atomic.Int32
	host, _ := bridgeHost(t, func(w http.ResponseWriter, r *http.Request, method string) bool {
		if method != "tools/call" {
			return false
		}
		received.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return true
		}
		conn.Close()
		return true
	})
	session, _, _ := connectBridge(t, host.URL+"/prefix/api/v1/agent-mcp")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "agents_prompt", Arguments: map[string]any{"text": "only once"}})
	if err != nil || !result.IsError || received.Load() != 1 {
		t.Fatalf("unknown result: %+v, %v, calls %d", result, err, received.Load())
	}
	encoded, _ := json.Marshal(result)
	if !bytes.Contains(encoded, []byte("RESULT_UNKNOWN")) || bytes.Contains(encoded, []byte(testToken)) || bytes.Contains(encoded, []byte(host.URL)) {
		t.Fatalf("unsafe unknown result: %s", encoded)
	}
}

func TestBridgeRefusesRedirectWithoutCredentialForwarding(t *testing.T) {
	var forwarded atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded.Add(1) }))
	defer destination.Close()
	host, _ := bridgeHost(t, func(w http.ResponseWriter, r *http.Request, _ string) bool {
		http.Redirect(w, r, destination.URL+"/"+testToken, http.StatusTemporaryRedirect)
		return true
	})
	bridge, native := net.Pipe()
	defer bridge.Close()
	defer native.Close()
	err := Run(t.Context(), host.URL+"/prefix/api/v1/agent-mcp", testToken, bridge, bridge)
	if err == nil || strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), host.URL) || forwarded.Load() != 0 {
		t.Fatalf("redirect: %v, forwarded %d", err, forwarded.Load())
	}
}
