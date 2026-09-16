package feishu

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const maxCallbackBytes = 1 << 20
const callbackTimeWindow = 5 * time.Minute

func (c *Channel) serveCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	select {
	case <-c.stopped:
		http.Error(w, "channel stopped", http.StatusServiceUnavailable)
		return
	default:
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCallbackBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid body", http.StatusBadRequest)
		}
		return
	}
	var envelope larkevent.EventEncryptMsg
	if json.Unmarshal(data, &envelope) != nil || envelope.Encrypt == "" {
		http.Error(w, "encrypted event required", http.StatusBadRequest)
		return
	}
	signed := freshSignedCallback(r.Header, data, c.secret.EncryptKey, time.Now())
	if r.Header.Get(larkevent.EventSignature) != "" && !signed {
		http.Error(w, "invalid signature or timestamp", http.StatusUnauthorized)
		return
	}
	plain, err := safeEventDecrypt(envelope.Encrypt, c.secret.EncryptKey)
	if err != nil {
		http.Error(w, "invalid encrypted event", http.StatusUnauthorized)
		return
	}
	var header struct {
		Type      string `json:"type"`
		Token     string `json:"token"`
		Challenge string `json:"challenge"`
		Header    struct {
			AppID     string `json:"app_id"`
			Token     string `json:"token"`
			EventType string `json:"event_type"`
		} `json:"header"`
	}
	if json.Unmarshal(plain, &header) != nil {
		http.Error(w, "invalid event", http.StatusBadRequest)
		return
	}
	token := header.Header.Token
	if token == "" {
		token = header.Token
	}
	if !secureEqual(token, c.secret.VerificationToken) {
		http.Error(w, "invalid event token", http.StatusUnauthorized)
		return
	}
	if header.Header.AppID != "" && header.Header.AppID != c.config.AppID {
		http.Error(w, "invalid app ID", http.StatusUnauthorized)
		return
	}
	if header.Type == "url_verification" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": header.Challenge})
		return
	}
	if header.Header.AppID == "" || !signed {
		http.Error(w, "invalid signature or timestamp", http.StatusUnauthorized)
		return
	}
	if header.Header.EventType != "im.message.receive_v1" {
		w.WriteHeader(http.StatusOK)
		return
	}
	var event larkim.P2MessageReceiveV1
	if err := json.Unmarshal(plain, &event); err != nil {
		http.Error(w, "invalid message event", http.StatusBadRequest)
		return
	}
	if err := c.onMessage(r.Context(), &event); err != nil {
		// A non-2xx response allows Feishu to redeliver when durable
		// acceptance failed. The sink must deduplicate by event ID.
		http.Error(w, "event not accepted", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// The upstream SDK may panic when decrypting attacker-controlled ciphertext
// with a wrong key because it slices between unvalidated brace offsets.
func safeEventDecrypt(encrypted, key string) (plain []byte, err error) {
	defer func() {
		if recover() != nil {
			plain, err = nil, errors.New("invalid encrypted event")
		}
	}()
	return larkevent.EventDecrypt(encrypted, key)
}

func freshSignedCallback(header http.Header, body []byte, key string, current time.Time) bool {
	timestamp := header.Get(larkevent.EventRequestTimestamp)
	nonce := header.Get(larkevent.EventRequestNonce)
	signature := header.Get(larkevent.EventSignature)
	if timestamp == "" || nonce == "" || signature == "" {
		return false
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || seconds < 0 || seconds > current.Unix()+int64(callbackTimeWindow.Seconds()) || seconds < current.Unix()-int64(callbackTimeWindow.Seconds()) {
		return false
	}
	expected := larkevent.Signature(timestamp, nonce, key, string(body))
	return secureEqual(signature, expected)
}

func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
