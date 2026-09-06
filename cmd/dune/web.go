package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/pkg/deployment"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/transport/ws"
)

func runWeb(ctx context.Context, c config.Config, options host.Options, webListen string) error {
	if err := c.ValidateServer(); err != nil {
		return err
	}
	if options.PublicURL == "" {
		if !strings.HasSuffix(c.Gateway, "/tunnel") {
			return fmt.Errorf("public browser URL required when configured gateway uses a nonstandard connection path")
		}
		options.PublicURL = strings.Replace(strings.TrimSuffix(c.Gateway, "/tunnel"), "ws", "http", 1)
	}
	addresses, err := deployment.NewURLs(options.PublicURL, options.GatewayURL)
	if err != nil {
		return err
	}
	options.PublicURL = addresses.PublicURL
	options.DataDir, err = filepath.Abs(options.DataDir)
	if err != nil {
		return err
	}
	tc, err := c.TLS()
	if err != nil {
		return err
	}
	// The CLI hosts Web and Gateway together. Use the local listener, while
	// retaining the advertised TLS identity and pinned certificate verification.
	local, err := deployment.Gateway(c.Gateway)
	if err != nil {
		return err
	}
	local.Path = addresses.Path + "tunnel"
	if tc != nil {
		tc.ServerName = local.Hostname()
	}
	listenHost, listenPort, _ := net.SplitHostPort(c.Listen)
	if ip := net.ParseIP(listenHost); ip.IsUnspecified() {
		listenHost = "::1"
		if ip.To4() != nil {
			listenHost = "127.0.0.1"
		}
	}
	local.Host = net.JoinHostPort(listenHost, listenPort)
	options.DialGateway = func(ctx context.Context, token string) (net.Conn, error) {
		return ws.Dial(ctx, local.String(), token, tc)
	}
	app, err := host.Open(ctx, options)
	if err != nil {
		return err
	}
	defer app.Close()
	primary, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	defer primary.Close()
	if strings.HasPrefix(c.Gateway, "wss://") {
		cert, err := tls.LoadX509KeyPair(c.Certificate, c.Key)
		if err != nil {
			return err
		}
		primary = tls.NewListener(primary, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	}
	listeners := []net.Listener{primary}
	if webListen != "" {
		host, _, err := net.SplitHostPort(webListen)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			return fmt.Errorf("extra web listener must bind a loopback IP")
		}
		ln, err := net.Listen("tcp", webListen)
		if err != nil {
			return err
		}
		defer ln.Close()
		listeners = append(listeners, ln)
	}
	errors := make(chan error, len(listeners))
	for _, ln := range listeners {
		go func() { errors <- app.Serve(ln) }()
	}
	fmt.Printf("Dune Web: %s\n", addresses.PublicURL)
	select {
	case <-ctx.Done():
	case err = <-errors:
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}
