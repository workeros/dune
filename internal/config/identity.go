package config

import (
	"context"
	"fmt"
	"time"

	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/identity/oidc"
)

// Identity constructs the official OIDC adapter from private startup config.
// Other identity protocols are injected directly through the public Go host.
func Identity(ctx context.Context, path string) (identity.Options, error) {
	var value struct {
		OIDC struct {
			Issuer       string `yaml:"issuer"`
			ClientID     string `yaml:"client_id"`
			ClientSecret string `yaml:"client_secret"`
		} `yaml:"oidc"`
		Lifetime string `yaml:"session_lifetime"`
	}
	if err := privateYAML(path, "identity", &value); err != nil {
		return identity.Options{}, err
	}
	lifetime := 8 * time.Hour
	if value.Lifetime != "" {
		var err error
		lifetime, err = time.ParseDuration(value.Lifetime)
		if err != nil || lifetime < time.Minute || lifetime > 24*time.Hour {
			return identity.Options{}, fmt.Errorf("external session lifetime must be 1 minute..24 hours")
		}
	}
	provider, err := oidc.Open(ctx, oidc.Config{Issuer: value.OIDC.Issuer, ClientID: value.OIDC.ClientID, ClientSecret: value.OIDC.ClientSecret})
	if err != nil {
		return identity.Options{}, err
	}
	return identity.Options{Provider: provider, SessionLifetime: lifetime}, nil
}
