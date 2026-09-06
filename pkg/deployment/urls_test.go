package deployment

import "testing"

func TestPublicAddressDerivation(t *testing.T) {
	for _, tc := range []struct{ input, public, gateway, origin, cookie string }{
		{"https://Example.COM:443/tools/dune", "https://example.com/tools/dune/", "wss://example.com/tools/dune/tunnel", "https://example.com", "/tools/dune/"},
		{"http://localhost:7443", "http://localhost:7443/", "ws://localhost:7443/tunnel", "http://localhost:7443", "/"},
		{"http://[::1]:80/a//b/", "http://[::1]/a/b/", "ws://[::1]/a/b/tunnel", "http://[::1]", "/a/b/"},
		{"https://example.test/a%20b", "https://example.test/a%20b/", "wss://example.test/a%20b/tunnel", "https://example.test", "/a%20b/"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			urls, err := NewURLs(tc.input, "")
			if err != nil {
				t.Fatal(err)
			}
			if urls.PublicURL != tc.public || urls.GatewayURL != tc.gateway || urls.Origin != tc.origin || urls.CookiePath != tc.cookie {
				t.Fatalf("addresses = %+v", urls)
			}
			override, err := NewURLs(tc.input, "wss://machine.example:9443/private/connect")
			if err != nil {
				t.Fatal(err)
			}
			if override.PublicURL != urls.PublicURL || override.Path != urls.Path || override.Origin != urls.Origin || override.GatewayURL != "wss://machine.example:9443/private/connect" {
				t.Fatalf("override changed browser address: %+v", override)
			}
		})
	}
}

func TestAmbiguousAndUnsafeAddressesRejected(t *testing.T) {
	if _, err := NewURLs("https://example.test/tools/dune/", "ws://machines.test/tunnel"); err == nil {
		t.Fatal("accepted a machine transport downgrade rejected by enrollment")
	}
	for _, value := range []string{"/tools/dune", "https://user:password@example.test/", "https://example.test/?", "https://example.test/?x=1", "https://example.test/#", "https://example.test/#callback", "https://example.test:0/", "https://example.test:65536/", "https://example.test:/", "https://0.0.0.0/", "https://example.test/a/../b", "https://example.test/a%2fb", "https://example.test/%2e%2e/a", "https://example.test/a%5Cb", "https://example.test/a;b"} {
		if _, err := Public(value); err == nil {
			t.Errorf("accepted public URL %q", value)
		}
	}
	for _, value := range []string{"https://example.test/tunnel", "ws://example.test", "ws://example.test/", "wss://example.test/tunnel?", "ws://example.test/a//tunnel"} {
		if _, err := Gateway(value); err == nil {
			t.Errorf("accepted gateway URL %q", value)
		}
	}
}
