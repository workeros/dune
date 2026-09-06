// Package sdk is the default Dune execution client, combining the connection
// protocol with authenticated WebSocket dialing. Hosts supplying their own
// connections can use pkg/client without a dependency on HTTP or WebSocket.
package sdk

import (
	"context"
	"github.com/aiomni/dune/pkg/client"
	"net"
)

// Client, Stream, and Port preserve the default SDK's execution API.
type Client = client.Client
type Stream = client.Stream
type Port = client.Port

// Connect transfers ownership of conn to the execution protocol client.
// ctx bounds the handshake; Client.Close controls the established lifetime.
func Connect(ctx context.Context, conn net.Conn, target string) (*Client, error) {
	return client.Connect(ctx, conn, target)
}
