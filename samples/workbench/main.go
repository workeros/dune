// This standalone host embeds Dune alongside an application-owned HTTP route.
// Copy it into a separate Go module and add github.com/aiomni/dune as a dependency.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aiomni/dune/pkg/deployment"
	"github.com/aiomni/dune/pkg/host"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:7443", "host HTTP listen address")
	public := flag.String("url", "http://127.0.0.1:7443/dune/", "public Dune deployment directory")
	data := flag.String("data", "", "absolute private Dune metadata directory")
	assets := flag.String("assets", "", "built Dune workbench directory")
	binaries := flag.String("binaries", "", "Dune installation archives directory")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	addresses, err := deployment.NewURLs(*public, "")
	if err != nil {
		return err
	}
	app, err := host.Open(ctx, host.Options{DataDir: *data, PublicURL: addresses.PublicURL, Assets: *assets, Binaries: *binaries})
	if err != nil {
		return err
	}
	defer app.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("host alive")) })
	// Preserve the prefix; do not use StripPrefix or copy any Dune routes.
	mux.Handle(addresses.Path, app)
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16 * 1024}
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Printf("Dune workbench: %s\n", addresses.PublicURL)
	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		return err
	}
}
