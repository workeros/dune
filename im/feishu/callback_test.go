package feishu

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
)

func assertCallbackJSON(t *testing.T, response *httptest.ResponseRecorder, statuses ...int) map[string]any {
	t.Helper()
	if !slices.Contains(statuses, response.Code) || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("callback status=%d content-type=%q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body == nil {
		t.Fatalf("callback must return a JSON object: %s (%v)", response.Body, err)
	}
	if response.Code >= http.StatusBadRequest && (body["code"] != "CALLBACK_REJECTED" || body["error"] == nil || body["error"] == "" || body["challenge"] != nil) {
		t.Fatalf("invalid callback rejection: %#v", body)
	}
	return body
}

func TestCallbackJSONResponsesWithoutHostWrapper(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		alter  func(*testing.T, *Channel, *http.Request)
	}{
		{"unencrypted", http.StatusBadRequest, func(t *testing.T, c *Channel, r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(`{"type":"url_verification","challenge":"challenge","token":"` + testToken + `"}`))
		}},
		{"malformed_envelope", http.StatusBadRequest, func(t *testing.T, c *Channel, r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(`{`))
		}},
		{"invalid_ciphertext", http.StatusUnauthorized, func(t *testing.T, c *Channel, r *http.Request) {
			r.Header.Del(larkevent.EventSignature)
			r.Body = io.NopCloser(strings.NewReader(`{"encrypt":"invalid"}`))
		}},
		{"wrong_app", http.StatusUnauthorized, func(t *testing.T, c *Channel, r *http.Request) {
			plain := strings.ReplaceAll(string(testEventJSON()), testAppID, "cli_other")
			*r = *signedCallback(t, []byte(plain), strconv.FormatInt(time.Now().Unix(), 10))
		}},
		{"invalid_message", http.StatusBadRequest, func(t *testing.T, c *Channel, r *http.Request) {
			plain := `{"header":{"token":"` + testToken + `","app_id":"` + testAppID + `","event_type":"im.message.receive_v1"},"event":"invalid"}`
			*r = *signedCallback(t, []byte(plain), strconv.FormatInt(time.Now().Unix(), 10))
		}},
		{"ignored_event", http.StatusOK, func(t *testing.T, c *Channel, r *http.Request) {
			plain := strings.ReplaceAll(string(testEventJSON()), "im.message.receive_v1", "unsubscribed.event")
			*r = *signedCallback(t, []byte(plain), strconv.FormatInt(time.Now().Unix(), 10))
		}},
		{"stopped", http.StatusServiceUnavailable, func(t *testing.T, c *Channel, r *http.Request) {
			if err := c.Stop(t.Context()); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong_method", http.StatusMethodNotAllowed, func(t *testing.T, c *Channel, r *http.Request) {
			r.Method = http.MethodGet
		}},
		{"oversized", http.StatusRequestEntityTooLarge, func(t *testing.T, c *Channel, r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", maxCallbackBytes+1)))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			sink := &recordingSink{}
			c := testChannel(t, sink)
			t.Cleanup(func() { _ = c.Stop(t.Context()) })
			r := signedCallback(t, testEventJSON(), strconv.FormatInt(time.Now().Unix(), 10))
			test.alter(t, c, r)
			response := httptest.NewRecorder()
			c.CallbackHandler().ServeHTTP(response, r)
			body := assertCallbackJSON(t, response, test.status)
			if test.status == http.StatusOK && len(body) != 0 {
				t.Fatalf("ignored event acknowledgement = %#v", body)
			}
			if test.status == http.StatusMethodNotAllowed && response.Header().Get("Allow") != http.MethodPost {
				t.Fatal("missing Allow: POST")
			}
			if len(sink.messages) != 0 {
				t.Fatal("rejected or ignored callback reached the inbox")
			}
		})
	}
}

func TestCallbackWrongEncryptionKeyReturnsJSON(t *testing.T) {
	plain := []byte(`{"type":"url_verification","challenge":"challenge","token":"` + testToken + `"}`)
	encrypted, err := larkcore.EncryptedEventMsg(t.Context(), plain, "wrong-key")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"encrypt": encrypted})
	if err != nil {
		t.Fatal(err)
	}
	c := testChannel(t, &recordingSink{})
	t.Cleanup(func() { _ = c.Stop(t.Context()) })
	response := httptest.NewRecorder()
	c.CallbackHandler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/callback", strings.NewReader(string(body))))
	// Wrong-key ciphertext can fail during decryption or JSON decoding.
	assertCallbackJSON(t, response, http.StatusUnauthorized, http.StatusBadRequest)
}
