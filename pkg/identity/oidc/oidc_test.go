package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/identity"
	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

// Signed protocol fixtures exercise verification failures. Actual-provider
// acceptance is separate; this server is not an enterprise identity source.
func TestOIDCCodePKCEAndVerification(t *testing.T) {
	first, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	second, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var key atomic.Pointer[rsa.PrivateKey]
	key.Store(first)
	var requests, keys, redirected atomic.Int32
	c := identity.Challenge{State: strings.Repeat("s", 64), Nonce: strings.Repeat("n", 64), Verifier: strings.Repeat("v", 64), RedirectURL: "https://dune.example.test/tools/dune/api/auth/external/callback"}
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token", "jwks_uri": server.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}, "token_endpoint_auth_methods_supported": []string{"client_secret_basic"}})
		case "/keys":
			keys.Add(1)
			current := key.Load()
			kid := "first"
			if current == second {
				kid = "second"
			}
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &current.PublicKey, KeyID: kid, Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			requests.Add(1)
			if r.ParseForm() != nil {
				t.Error("token form unreadable")
				w.WriteHeader(400)
				return
			}
			client, secret, _ := r.BasicAuth()
			if client != "dune-client" || secret != "private-test-secret" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code_verifier") != c.Verifier || r.Form.Get("redirect_uri") != c.RedirectURL {
				t.Error("code exchange lost its client/PKCE/redirect binding")
				w.WriteHeader(400)
				return
			}
			code := r.Form.Get("code")
			if code == "lost-response" {
				http.Error(w, "private upstream details", 500)
				return
			}
			if code == "redirect" {
				http.Redirect(w, r, server.URL+"/unexpected", http.StatusTemporaryRedirect)
				return
			}
			if code == "rotate" {
				key.Store(second)
			}
			claims := map[string]any{"iss": server.URL, "sub": "stable-subject", "aud": "dune-client", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": c.Nonce, "email": "person@example.test"}
			switch code {
			case "missing-subject":
				delete(claims, "sub")
			case "missing-issued-at":
				delete(claims, "iat")
			case "nonce":
				claims["nonce"] = "another-nonce"
			case "issuer":
				claims["iss"] = "https://wrong-issuer.test"
			case "audience":
				claims["aud"] = "another-client"
			case "expired":
				claims["exp"] = time.Now().Add(-time.Hour).Unix()
			case "not-before":
				claims["nbf"] = time.Now().Add(time.Hour).Unix()
			case "issued-future":
				claims["iat"] = time.Now().Add(time.Hour).Unix()
			case "authorized-party":
				claims["azp"] = "another-client"
			case "multiple-audiences":
				claims["aud"] = []string{"dune-client", "other"}
			}
			current := key.Load()
			kid := "first"
			if current == second {
				kid = "second"
			}
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: current}, (&jose.SignerOptions{}).WithHeader("kid", kid))
			if err != nil {
				t.Error(err)
				return
			}
			payload, err := json.Marshal(claims)
			if err != nil {
				t.Error(err)
				return
			}
			signed, err := signer.Sign(payload)
			if err != nil {
				t.Error(err)
				return
			}
			raw, err := signed.CompactSerialize()
			if err != nil {
				t.Error(err)
				return
			}
			if code == "signature" {
				parts := strings.Split(raw, ".")
				parts[2] = strings.Repeat("A", len(parts[2]))
				raw = strings.Join(parts, ".")
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "private-access-token", "token_type": "Bearer", "id_token": raw, "expires_in": 3600})
		case "/unexpected":
			redirected.Add(1)
			w.WriteHeader(500)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	p, err := Open(context.Background(), Config{Issuer: server.URL, ClientID: "dune-client", ClientSecret: "private-test-secret", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	authorize, err := p.Begin(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(authorize)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("state") != c.State || q.Get("nonce") != c.Nonce || q.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(c.Verifier) || q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" {
		t.Fatal("authorization challenge not bound")
	}
	for _, code := range []string{"valid", "rotate"} {
		subject, err := p.Verify(context.Background(), c, code)
		if err != nil || subject.ID != "stable-subject" {
			t.Fatal("valid or rotated token rejected", err)
		}
	}
	if keys.Load() != 2 {
		t.Fatalf("key rotation did not refresh cached keys: %d", keys.Load())
	}
	for _, code := range []string{"missing-subject", "missing-issued-at", "nonce", "issuer", "audience", "expired", "not-before", "issued-future", "authorized-party", "multiple-audiences", "signature", "lost-response", "redirect"} {
		t.Run(code, func(t *testing.T) {
			before := requests.Load()
			if _, err := p.Verify(context.Background(), c, code); err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal("invalid identity accepted or secret disclosed", err)
			}
			if requests.Load() != before+1 {
				t.Fatal("code exchange was replayed")
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("token exchange followed redirect")
	}
}
