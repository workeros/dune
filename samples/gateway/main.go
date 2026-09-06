// A Gateway embedded in a standard HTTP server, using only public Dune packages.
// Run with DUNE_TOKEN and DUNE_TARGET; point fabricd and sdk.Dial at /tunnel.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/transport/tunnel"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	token, target := os.Getenv("DUNE_TOKEN"), os.Getenv("DUNE_TARGET")
	if token == "" || target == "" {
		return fmt.Errorf("DUNE_TOKEN and DUNE_TARGET are required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	g := gateway.New()
	defer g.Close()
	mux := http.NewServeMux()
	mux.Handle("GET /tunnel", tunnel.NewHandler(ctx, g, func(presented string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
		if subtle.ConstantTimeCompare([]byte(token), []byte(presented)) != 1 {
			return gateway.BindingContext{}, nil, fmt.Errorf("unauthorized")
		}
		return (access.Grant{Target: target, Role: gateway.RoleEither}).Bind()
	}))
	server := &http.Server{Addr: "127.0.0.1:7443", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		g.Close()
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
