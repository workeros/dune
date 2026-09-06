package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHTTPWildcard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if e := InitWithGateway(path, "0.0.0.0:7443", "ws://gateway.example.test:7443/tunnel"); e != nil {
		t.Fatal(e)
	}
	c, e := Load(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.ValidateServer(); e != nil {
		t.Fatal(e)
	}
	tc, e := c.TLS()
	if e != nil || tc != nil || c.Certificate != "" || c.Key != "" {
		t.Fatal("HTTP unexpectedly requires certificates", e)
	}
	entries, e := os.ReadDir(filepath.Dir(path))
	if e != nil || len(entries) != 1 {
		t.Fatal("HTTP init created extra files", e)
	}
	c.Listen = ""
	if e = c.Validate(); e != nil {
		t.Fatal("client config rejected", e)
	}
	if e = c.ValidateServer(); e == nil {
		t.Fatal("server requires listen")
	}
}

func TestEndpointValidation(t *testing.T) {
	for _, address := range []string{"ws://0.0.0.0:7443/tunnel", "ws://host:0/tunnel", "ws://host:65536/tunnel", "http://host:7443/tunnel", "ws://user@host:7443/tunnel", "ws://host:7443/tunnel?token=secret"} {
		if _, e := gatewayURL(address); e == nil {
			t.Errorf("accepted %s", address)
		}
	}
	if e := Init(filepath.Join(t.TempDir(), "config.yaml"), "0.0.0.0:7443"); e == nil {
		t.Fatal("wildcard needs an advertised endpoint")
	}
}
