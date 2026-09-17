package webapp

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestTrustedProxySource(t *testing.T) {
	server := &Server{trustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("127.0.0.1/32")}}
	for _, tt := range []struct{ remote, forwarded, want string }{
		{"198.51.100.1:80", "1.2.3.4", "198.51.100.1"},
		{"127.0.0.1:80", "198.51.100.1, 10.1.1.1", "198.51.100.1"},
		{"127.0.0.1:80", "1.2.3.4, 198.51.100.1", "198.51.100.1"},
		{"127.0.0.1:80", "garbage, 198.51.100.1", "127.0.0.1"},
		{"[::ffff:127.0.0.1]:80", "2001:db8::1", "2001:db8::1"},
	} {
		request := httptest.NewRequest("POST", "/", nil)
		request.RemoteAddr = tt.remote
		request.Header.Set("X-Forwarded-For", tt.forwarded)
		if got := server.clientIP(request); got != tt.want {
			t.Fatalf("%s %s got %s want %s", tt.remote, tt.forwarded, got, tt.want)
		}
	}
}
func TestLoginLimitsSourceAndAccount(t *testing.T) {
	server := &Server{rates: map[string]authRate{}, hashSlots: make(chan struct{}, 4)}
	request := httptest.NewRequest("POST", "/", nil)
	request.RemoteAddr = "198.51.100.1:12"
	for i := 0; i < 20; i++ {
		if !server.authAllowed(httptest.NewRecorder(), request, "Same@Example.test") {
			t.Fatal("early limit")
		}
		<-server.hashSlots
	}
	request.RemoteAddr = "198.51.100.2:12"
	if server.authAllowed(httptest.NewRecorder(), request, " same@example.test ") {
		t.Fatal("account limit bypassed")
	}
	if !server.authAllowed(httptest.NewRecorder(), request, "another@example.test") {
		t.Fatal("independent source/account rejected")
	}
	<-server.hashSlots
	request.RemoteAddr = "198.51.100.1:12"
	if server.authAllowed(httptest.NewRecorder(), request, "another@example.test") {
		t.Fatal("source limit bypassed")
	}
}
