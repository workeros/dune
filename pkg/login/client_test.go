package login

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientProofAndNoReplay(t *testing.T) {
	for _, mode := range []string{"success", "failure", "redirect", "lost-response"} {
		t.Run(mode, func(t *testing.T) {
			var attempts, redirected atomic.Int32
			var challenge string
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Dune-Request") != "1" {
					t.Error("request proof header missing")
				}
				site := server.URL + "/tools/"
				switch r.URL.Path {
				case "/tools/api/auth/cli/start":
					var body struct{ Challenge string }
					json.NewDecoder(r.Body).Decode(&body)
					challenge = body.Challenge
					json.NewEncoder(w).Encode(Request{ID: strings.Repeat("a", 32), Code: "AAAAAAAA", VerificationURL: site + "?cli_login=" + strings.Repeat("a", 32), ExpiresAt: time.Now().Add(10 * time.Minute).Unix()})
				case "/tools/api/auth/cli/consume":
					attempts.Add(1)
					var body struct{ ID, Verifier string }
					json.NewDecoder(r.Body).Decode(&body)
					sum := sha256.Sum256([]byte(body.Verifier))
					if body.ID != strings.Repeat("a", 32) || challenge != hex.EncodeToString(sum[:]) || len(body.Verifier) != 64 {
						t.Error("CLI proof changed")
					}
					switch mode {
					case "failure":
						w.WriteHeader(503)
						w.Write([]byte(`{"code":"RESULT_UNKNOWN","error":"private upstream token"}`))
					case "redirect":
						http.Redirect(w, r, "/trap", http.StatusTemporaryRedirect)
					case "lost-response":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
					default:
						json.NewEncoder(w).Encode(Session{Site: site, Token: "dune_cli_" + strings.Repeat("b", 64), PrincipalID: "principal", ExpiresAt: time.Now().Add(time.Hour).Unix()})
					}
				case "/trap":
					redirected.Add(1)
					w.WriteHeader(500)
				case "/tools/api/cli/logout":
					if r.Header.Get("Authorization") != "Bearer dune_cli_"+strings.Repeat("b", 64) || r.URL.RawQuery != "" {
						t.Error("CLI credential binding changed")
					}
					w.Write([]byte(`{"ok":true}`))
				default:
					t.Error("unexpected endpoint", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			client, err := New(Options{Site: server.URL + "/tools"})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			attempt, err := client.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(attempt.VerificationURL, attempt.proof) {
				t.Fatal("local proof put in browser URL")
			}
			session, err := attempt.Wait(context.Background())
			if mode == "success" {
				if err != nil {
					t.Fatal(err)
				}
				if err := client.Logout(context.Background(), session); err != nil {
					t.Fatal(err)
				}
			} else if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal("uncertain result accepted or secret disclosed", err)
			}
			if attempts.Load() != 1 || redirected.Load() != 0 {
				t.Fatal("CLI exchange was replayed", attempts.Load(), redirected.Load())
			}
		})
	}
}

func TestClientRequiresVerifiedTransport(t *testing.T) {
	for _, options := range []Options{{Site: "http://example.test/"}, {Site: "https://example.test/", TLSConfig: &tls.Config{InsecureSkipVerify: true}}, {Site: "https://user:secret@example.test/"}} {
		if _, err := New(options); err == nil {
			t.Fatal("unsafe login endpoint accepted")
		}
	}
}
