package host_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/observe"
)

type observedAccessChecker struct{ allowed bool }

func (c observedAccessChecker) Check(_ context.Context, request access.Request) (access.Decision, error) {
	return access.Decision{Allowed: c.allowed, Reason: "OBSERVED", ID: request.RequestID, ValidUntil: time.Now().Add(time.Minute)}, nil
}

func TestHostEmitsStructuredAccessObservation(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		t.Run(map[bool]string{true: "allowed", false: "denied"}[allowed], func(t *testing.T) {
			events := make(chan observe.Event, 4)
			app, err := host.Open(context.Background(), host.Options{
				DataDir: filepath.Join(t.TempDir(), "metadata"), PublicURL: "http://example.test/dune/",
				AccessChecker: observedAccessChecker{allowed: allowed},
				Observer:      observe.SinkFunc(func(_ context.Context, event observe.Event) { events <- event }),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			request := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				if method != http.MethodGet {
					r.Header.Set("X-Dune-Request", "1")
					r.Header.Set("Origin", "http://example.test")
				}
				if cookie != nil {
					r.AddCookie(cookie)
				}
				out := httptest.NewRecorder()
				app.ServeHTTP(out, r)
				return out
			}
			registered := request(http.MethodPost, "/dune/api/auth/register", `{"email":"observe@example.test","password":"observe-test-password"}`, nil)
			if registered.Code != http.StatusOK || len(registered.Result().Cookies()) != 1 {
				t.Fatal("registration failed", registered.Code, registered.Body.String())
			}
			response := request(http.MethodPost, "/dune/api/enrollments", `{"name":"observed machine"}`, registered.Result().Cookies()[0])
			wantStatus, wantOutcome := http.StatusOK, "allowed"
			if !allowed {
				wantStatus, wantOutcome = http.StatusForbidden, "denied"
			}
			if response.Code != wantStatus {
				t.Fatal("unexpected enrollment result", response.Code, response.Body.String())
			}
			select {
			case event := <-events:
				if event.Name != observe.AccessCheck || event.Outcome != wantOutcome || event.PrincipalID == "" || event.Namespace != "" || event.Operation != "runner.create" || event.Suboperation != "attached" || event.RequestID == "" || event.DurationMicros < 1 {
					t.Fatal("incomplete access event", event)
				}
				if event.DecisionID == "" && allowed {
					t.Fatal("allowed event lost decision identity", event)
				}
			case <-time.After(time.Second):
				t.Fatal("access event was not delivered")
			}
			if status := app.ObservationStatus(); status.Dropped != 0 {
				t.Fatal("healthy sink dropped events", status)
			}
		})
	}
}
