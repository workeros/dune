package webapp

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserTicketProtocol(t *testing.T) {
	ticket := strings.Repeat("a", 43)
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set("Sec-WebSocket-Protocol", browserTicketProtocolPrefix+ticket)
	protocol, got, ok := browserTicketProtocol(request)
	if !ok || protocol != browserTicketProtocolPrefix+ticket || got != ticket {
		t.Fatal(protocol, got, ok)
	}
	for _, invalid := range []string{"", browserTicketProtocolPrefix + "short", browserTicketProtocolPrefix + ticket + ",other", browserTicketProtocolPrefix + strings.Repeat("!", 43)} {
		request.Header.Set("Sec-WebSocket-Protocol", invalid)
		if _, _, ok := browserTicketProtocol(request); ok {
			t.Fatalf("accepted invalid protocol %q", invalid)
		}
	}
}
