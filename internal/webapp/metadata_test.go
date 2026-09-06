package webapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
)

func TestMetadataFailureResponses(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: private driver details", metadata.ErrCommitUnknown), 503, "RESULT_UNKNOWN"},
		{errors.New("private driver details"), 503, "METADATA_UNAVAILABLE"},
		{metadata.ErrConflict, 409, "CONFLICT"},
		{metadata.ErrNotFound, 404, "NOT_FOUND"},
		{identity.ErrUnauthorized, 401, "UNAUTHORIZED"},
	} {
		r := httptest.NewRecorder()
		writeMetadataError(r, tc.err)
		if r.Code != tc.status || !strings.Contains(r.Body.String(), tc.code) || strings.Contains(r.Body.String(), "private driver details") {
			t.Fatalf("failure response: %d %s", r.Code, r.Body)
		}
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "metadata"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := newTestServer(context.Background(), Options{DialGateway: noGateway, PublicURL: "http://dune.example.test"}, store, identity.NewLocal(store, true))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for route, body := range map[string]string{
		"auth/login":    `{"email":"owner@example.test","password":"strong-test-password"}`,
		"auth/register": `{"email":"owner@example.test","password":"strong-test-password"}`,
		"enroll":        `{"token":"` + strings.Repeat("a", 64) + `","os":"linux","arch":"amd64"}`,
	} {
		r := httptest.NewRequest("POST", "/api/"+route, bytes.NewBufferString(body))
		r.Header.Set("Origin", app.urls.Origin)
		r.Header.Set("X-Dune-Request", "1")
		r.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		app.ServeHTTP(out, r)
		if out.Code != 503 || !strings.Contains(out.Body.String(), "METADATA_UNAVAILABLE") {
			t.Fatalf("database failure on %s: %d %s", route, out.Code, out.Body)
		}
	}
}
