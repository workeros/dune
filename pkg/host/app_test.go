package host_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/host"
)

func await(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("application did not finish closing")
	}
}

func TestParentCancellationClosesOwnedListenersAndReleasesStore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	options := host.Options{DataDir: filepath.Join(t.TempDir(), "accounts"), PublicURL: "http://example.test/dune/"}
	app, err := host.Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if other, err := host.Open(ctx, options); err == nil {
		other.Close()
		t.Fatal("two applications opened the same metadata directory")
	}
	done := make(chan error, 2)
	for range 2 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { done <- app.Serve(listener) }()
		response, err := http.Get("http://" + listener.Addr().String() + "/dune/api/bootstrap")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("bootstrap status %d", response.StatusCode)
		}
	}
	cancel()
	await(t, app.Done())
	for range 2 {
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Fatalf("listener close: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("owned listener remained open")
		}
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := host.Open(context.Background(), options)
	if err != nil {
		t.Fatalf("closed application retained store: %v", err)
	}
	reopened.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve after Close: %v", err)
	}
	if conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); err == nil {
		conn.Close()
		t.Fatal("rejected listener was not closed")
	}
}

type observedBody struct {
	io.ReadCloser
	read chan struct{}
}

func (b *observedBody) Read(p []byte) (int, error) {
	select {
	case <-b.read:
	default:
		close(b.read)
	}
	return b.ReadCloser.Read(p)
}

func TestCloseInterruptsPartialRequestWithoutClosingExternalServer(t *testing.T) {
	options := host.Options{DataDir: filepath.Join(t.TempDir(), "accounts"), PublicURL: "http://example.test/dune/"}
	app, err := host.Open(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	reading := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("host alive")) })
	mux.HandleFunc("/dune/", func(w http.ResponseWriter, r *http.Request) {
		r.Body = &observedBody{ReadCloser: r.Body, read: reading}
		app.ServeHTTP(w, r)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The client leaves a JSON body unfinished. Cancelling a request context
	// alone cannot release the blocked net/http body reader.
	fmt.Fprintf(conn, "POST /dune/api/auth/register HTTP/1.1\r\nHost: example.test\r\nContent-Type: application/json\r\nX-Dune-Request: 1\r\nContent-Length: 100\r\n\r\n{")
	await(t, reading)
	go app.Close()
	await(t, app.Done())
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = http.ReadResponse(bufio.NewReader(conn), nil)
	response, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(body) != "host alive" {
		t.Fatal("closing Dune affected the host's own route")
	}
	response, err = http.Get(server.URL + "/dune/api/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("closed Dune handler admitted a request")
	}
}
