package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTTPWildcard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if e := InitWithGateway(path, "0.0.0.0:7443", "ws://gateway.example.test:7443/api/v1/ws/tunnel"); e != nil {
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
	for _, address := range []string{"ws://0.0.0.0:7443/api/v1/ws/tunnel", "ws://host:0/api/v1/ws/tunnel", "ws://host:65536/api/v1/ws/tunnel", "http://host:7443/api/v1/ws/tunnel", "ws://user@host:7443/api/v1/ws/tunnel", "ws://host:7443/tunnel", "ws://host:7443/api/v1/ws/tunnel?token=secret"} {
		if _, e := gatewayURL(address); e == nil {
			t.Errorf("accepted %s", address)
		}
	}
	if e := Init(filepath.Join(t.TempDir(), "config.yaml"), "0.0.0.0:7443"); e == nil {
		t.Fatal("wildcard needs an advertised endpoint")
	}
}

func TestServiceConfigurationRejectsRelativeCertificatePaths(t *testing.T) {
	for _, field := range []string{"certificate", "key"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			data := "gateway: wss://gateway.example.test/api/v1/ws/tunnel\ntoken: " + strings.Repeat("x", 32) + "\ntarget: test\n" + field + ": local.pem\n"
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), field+" must be absolute") {
				t.Fatalf("relative path would change meaning under the service working directory: %v", err)
			}
		})
	}
}
