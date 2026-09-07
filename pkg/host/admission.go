package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/deployment"
	"github.com/aiomni/dune/pkg/gateway"
)

func configurationFingerprint(options Options, urls deployment.URLs, service identity.Service) (string, error) {
	revision := options.ConfigurationVersion
	if len(revision) > 128 || !utf8.ValidString(revision) || strings.ContainsFunc(revision, unicode.IsControl) {
		return "", fmt.Errorf("configuration version must be a nonsecret identifier of at most 128 bytes")
	}
	if (options.Identity != nil || options.AccessChecker != nil) && revision == "" {
		return "", fmt.Errorf("PostgreSQL hosts with custom identity or policy require ConfigurationVersion")
	}
	// Origins and direct peer addresses identify instances and may differ. The
	// mounted path and behavior must agree. Opaque provider/checker configuration
	// is represented by the version supplied by the application, never by secrets.
	description := struct {
		Protocol, Path, Namespace, Policy, Version string
		Lifetime                                   time.Duration
		Registration, Cluster                      bool
	}{Protocol: api.Version, Path: urls.Path, Namespace: service.Namespace(), Lifetime: identity.SessionLifetime, Registration: options.Identity == nil && !options.DisableRegistration, Cluster: options.Cluster != nil, Policy: "owner-v1", Version: revision}
	if options.Identity != nil {
		description.Lifetime = options.Identity.SessionLifetime
		if description.Lifetime == 0 {
			description.Lifetime = 8 * time.Hour
		}
	}
	if options.AccessChecker != nil {
		description.Policy = "custom"
	}
	data, err := json.Marshal(description)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func registerInstance(ctx context.Context, store *metadata.Store, options Options, urls deployment.URLs, service identity.Service) (*gateway.AdmissionLease, metadata.InstanceConfig, error) {
	fingerprint, err := configurationFingerprint(options, urls, service)
	if err != nil {
		return nil, metadata.InstanceConfig{}, err
	}
	config := metadata.InstanceConfig{BootID: wire.ID(), Fingerprint: fingerprint}
	if options.Cluster != nil {
		config.RecoveryGeneration = options.Cluster.RecoveryGeneration
	}
	started := time.Now()
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	duration, err := store.RegisterInstance(bounded, config)
	if err != nil {
		return nil, config, err
	}
	if bounded.Err() != nil {
		return nil, config, bounded.Err()
	}
	lease, err := gateway.NewAdmissionLease(started.Add(duration))
	return lease, config, err
}

func (a *App) renewAdmission(config metadata.InstanceConfig) {
	defer a.active.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-a.admission.Done():
			a.cancel()
			return
		case <-ticker.C:
			started := time.Now()
			bounded, cancel := context.WithTimeout(a.ctx, min(time.Second, a.admission.Remaining()))
			duration, err := a.store.RenewInstance(bounded, config)
			expired := bounded.Err()
			cancel()
			if err != nil || expired != nil || a.admission.Renew(started.Add(duration)) != nil {
				// An uncertain renewal cannot extend local authority. This boot stops;
				// its database reservation expires naturally instead of being replayed.
				a.cancel()
				return
			}
		}
	}
}
