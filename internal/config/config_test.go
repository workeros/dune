package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/deployment"
	"gopkg.in/yaml.v3"
)

func TestMachineConfigurationIgnoresUnknownFields(t *testing.T) {
	for _, extra := range []string{
		"log_level: \"\"\n",
		"log_level: debug\n",
		"future_options:\n  enabled: true\n  values: [one, two]\n",
	} {
		t.Run(strings.SplitN(extra, "\n", 2)[0], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			data := "gateway: ws://gateway.example.test/api/v1/ws/tunnel\ntoken: " + strings.Repeat("x", 32) + "\ntarget: test\n" + extra
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := Load(path)
			if err != nil {
				t.Fatal("unknown setting blocked configuration loading", err)
			}
			want := Config{Gateway: "ws://gateway.example.test/api/v1/ws/tunnel", Token: strings.Repeat("x", 32), Target: "test", SessionDir: filepath.Join(filepath.Dir(path), "sessions")}
			if got != want {
				t.Fatal("unknown setting changed the effective configuration")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != data {
				t.Fatal("loading rewrote the original configuration", err)
			}
		})
	}
}

func TestMachineConfigurationValidatesCoreFields(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{"missing gateway", "gateway", nil},
		{"invalid gateway", "gateway", "https://example.test"},
		{"missing token", "token", nil},
		{"short token", "token", "short"},
		{"invalid token type", "token", map[string]string{"value": "secret"}},
		{"missing target", "target", nil},
		{"relative session directory", "session_dir", "sessions"},
		{"relative certificate", "certificate", "tls.crt"},
		{"relative key", "key", "tls.key"},
		{"invalid listener", "listen", "not-an-address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]any{"gateway": "ws://gateway.example.test/api/v1/ws/tunnel", "token": strings.Repeat("x", 32), "target": "test", "unrelated_setting": true}
			fields[tc.field] = tc.value
			data, err := yaml.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("invalid core configuration accepted")
			}
		})
	}
}

func TestHTTPWildcard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if e := Init(path, "0.0.0.0:7443", "ws://gateway.example.test:7443/api/v1/ws/tunnel"); e != nil {
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
		if _, e := deployment.Gateway(address); e == nil {
			t.Errorf("accepted %s", address)
		}
	}
	if e := Init(filepath.Join(t.TempDir(), "config.yaml"), "0.0.0.0:7443", ""); e == nil {
		t.Fatal("wildcard needs an advertised endpoint")
	}
}
