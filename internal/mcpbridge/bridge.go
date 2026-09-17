// Package mcpbridge adapts native stdio MCP clients to the host's HTTP tools.
// It has no Agent execution logic and never replays a failed tool call.
package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	Command  = "__agent-mcp"
	URLEnv   = "DUNE_AGENT_MCP_URL"
	TokenEnv = "DUNE_AGENT_MCP_TOKEN"
)

type bearerTransport struct {
	endpoint *url.URL
	token    string
}

func (b bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != b.endpoint.Scheme || request.URL.Host != b.endpoint.Host || request.URL.Path != b.endpoint.Path || request.URL.RawQuery != "" {
		return nil, errors.New("MCP bridge refused a different endpoint")
	}
	request = request.Clone(request.Context())
	request.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(request)
}

// Run owns the stdio connection until EOF/cancellation. Secrets are supplied by
// the launcher through environment variables, never command-line arguments.
func Run(ctx context.Context, endpoint, token string, input io.ReadCloser, output io.WriteCloser) error {
	address, err := url.Parse(endpoint)
	if err != nil || address.Host == "" || (address.Scheme != "http" && address.Scheme != "https") || address.User != nil || address.RawQuery != "" || address.Fragment != "" || len(endpoint) > 4096 || token == "" || len(token) > 1024 || strings.ContainsAny(token, " \r\n\t") {
		return errors.New("invalid MCP bridge configuration")
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "dune-mcp-bridge", Version: "1"}, nil)
	httpClient := &http.Client{Transport: bearerTransport{endpoint: address, token: token}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	remote, err := client.Connect(connectCtx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	cancel()
	if err != nil {
		return errors.New("MCP bridge could not authenticate with its configured host")
	}
	defer remote.Close()
	catalogCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	tools, err := remote.ListTools(catalogCtx, nil)
	cancel()
	if err != nil || tools.NextCursor != "" || len(tools.Tools) > 64 {
		return errors.New("MCP bridge could not load its host tool catalog")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "dune-agents", Version: "1"}, nil)
	for _, tool := range tools.Tools {
		server.AddTool(tool, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			result, err := remote.CallTool(ctx, &mcp.CallToolParams{Name: request.Params.Name, Arguments: request.Params.Arguments})
			if err == nil {
				return result, nil
			}
			// A broken reply can follow an accepted write. Do not reconnect and
			// repeat it, or expose private HTTP/credential details in the error.
			failure := map[string]any{"code": "RESULT_UNKNOWN", "error": "MCP tool result is unknown; it was not replayed. Query any operation_ref already received."}
			encoded, _ := json.Marshal(failure)
			return &mcp.CallToolResult{IsError: true, StructuredContent: failure, Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, nil
		})
	}
	if err := server.Run(ctx, &mcp.IOTransport{Reader: input, Writer: output, MaxLineLength: 256 * 1024}); err != nil && ctx.Err() == nil {
		return errors.New("MCP bridge stdio connection closed unexpectedly")
	}
	return nil
}
