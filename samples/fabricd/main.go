// A minimal external execution host. DUNE_GATEWAY is a complete WS(S) URL;
// DUNE_TOKEN, DUNE_TARGET, and DUNE_STATE_DIR supply this instance's parameters.
// This sample serves one connection. Supervisors may reconnect with ServeConn
// on the same engine to retain its execution incarnation.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/aiomni/dune/pkg/fabricd"
	"github.com/aiomni/dune/pkg/transport/ws"
)

func main() {
	if code, handled := fabricd.RunHelper(os.Args[1:]); handled {
		os.Exit(code)
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	endpoint, token, target := os.Getenv("DUNE_GATEWAY"), os.Getenv("DUNE_TOKEN"), os.Getenv("DUNE_TARGET")
	if endpoint == "" || token == "" || target == "" {
		return fmt.Errorf("DUNE_GATEWAY, DUNE_TOKEN, and DUNE_TARGET are required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	engine, err := fabricd.Open(ctx, os.Getenv("DUNE_STATE_DIR"))
	if err != nil {
		return err
	}
	defer engine.Close()
	conn, err := ws.Dial(ctx, endpoint, token, &tls.Config{MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	return engine.ServeConn(ctx, conn, target)
}
