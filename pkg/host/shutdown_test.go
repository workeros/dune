package host_test

import (
	"bufio"
	"context"
	"encoding/json"
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

func waitReadiness(t *testing.T, app *host.App, predicate func(host.Readiness) bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for !predicate(app.Readiness()) {
		select {
		case <-deadline:
			t.Fatal("readiness did not change", app.Readiness())
		case <-time.After(time.Millisecond):
		}
	}
}

func TestShutdownFinishesOrCancelsAcceptedHTTP(t *testing.T) {
	for _, owned := range []bool{false, true} {
		for _, complete := range []bool{false, true} {
			t.Run(fmt.Sprintf("owned=%v/complete=%v", owned, complete), func(t *testing.T) {
				options := host.Options{DataDir: filepath.Join(t.TempDir(), "accounts"), PublicURL: "http://example.test/dune/"}
				app, err := host.Open(context.Background(), options)
				if err != nil {
					t.Fatal(err)
				}
				defer app.Close()
				var address string
				if owned {
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					address = listener.Addr().String()
					go app.Serve(listener)
				} else {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/host" {
							w.Write([]byte("host alive"))
							return
						}
						app.ServeHTTP(w, r)
					}))
					defer server.Close()
					address = server.Listener.Addr().String()
				}
				conn, err := net.DialTimeout("tcp", address, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				body := `{"email":"shutdown@example.test","password":"shutdown-test-password"}`
				fmt.Fprintf(conn, "POST /dune/api/auth/register HTTP/1.1\r\nHost: example.test\r\nContent-Type: application/json\r\nX-Dune-Request: 1\r\nContent-Length: %d\r\n\r\n%s", len(body), body[:1])
				waitReadiness(t, app, func(r host.Readiness) bool { return r.Requests == 1 })
				budget := time.Second
				if complete {
					budget = 6 * time.Second
				}
				ctx, cancel := context.WithTimeout(context.Background(), budget)
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- app.Shutdown(ctx) }()
				waitReadiness(t, app, func(r host.Readiness) bool { return r.Draining })
				for path, want := range map[string]int{"/dune/health/ready": 503, "/dune/health/live": 200, "/dune/api/bootstrap": 503} {
					response, err := http.Get("http://" + address + path)
					if err != nil {
						t.Fatal(err)
					}
					response.Body.Close()
					if response.StatusCode != want {
						t.Fatal("draining response", path, response.StatusCode)
					}
				}
				if err := app.SetUserEnabled(context.Background(), "late", false); err == nil {
					t.Fatal("drain admitted administration")
				}
				if complete {
					if _, err := io.WriteString(conn, body[1:]); err != nil {
						t.Fatal(err)
					}
					response, err := http.ReadResponse(bufio.NewReader(conn), nil)
					if err != nil {
						t.Fatal("accepted response interrupted", err)
					}
					var user struct{ ID string }
					err = json.NewDecoder(response.Body).Decode(&user)
					response.Body.Close()
					if err != nil || response.StatusCode != 200 || user.ID == "" {
						t.Fatal("accepted registration not completed", response.StatusCode, err)
					}
				}
				select {
				case err := <-done:
					if (complete && err != nil) || (!complete && !errors.Is(err, context.DeadlineExceeded)) {
						t.Fatal("shutdown result", err)
					}
				case <-time.After(8 * time.Second):
					t.Fatal("shutdown did not release partial request")
				}
				await(t, app.Done())
				if app.Readiness().Serving || app.Readiness().Accepting || app.Readiness().Requests != 0 {
					t.Fatal("closed app reports serving", app.Readiness())
				}
				if !owned {
					response, err := http.Get("http://" + address + "/host")
					if err != nil {
						t.Fatal("external host interrupted", err)
					}
					response.Body.Close()
					if response.StatusCode != 200 {
						t.Fatal("external host closed")
					}
				}
				reopened, err := host.Open(context.Background(), options)
				if err != nil {
					t.Fatal("shutdown retained storage", err)
				}
				reopened.Close()
			})
		}
	}
}

func TestReadinessPrefixMethodsAndIdleShutdown(t *testing.T) {
	app, err := host.Open(context.Background(), host.Options{DataDir: filepath.Join(t.TempDir(), "accounts"), PublicURL: "http://example.test/tools/dune/"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	for _, test := range []struct {
		method, path string
		status       int
	}{{"GET", "/tools/dune/health/ready", 200}, {"HEAD", "/tools/dune/health/live", 200}, {"POST", "/tools/dune/health/ready", 405}, {"GET", "/health/ready", 404}} {
		out := httptest.NewRecorder()
		app.ServeHTTP(out, httptest.NewRequest(test.method, test.path, nil))
		if out.Code != test.status || (test.method == "HEAD" && out.Body.Len() != 0) {
			t.Fatal("health response", test, out.Code, out.Body)
		}
	}
	// Concurrent shutdown and immediate close share one resource release.
	done := make(chan error, 3)
	go func() { done <- app.Shutdown(context.Background()) }()
	go func() { done <- app.Shutdown(context.Background()) }()
	go func() { done <- app.Close() }()
	for range 3 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
