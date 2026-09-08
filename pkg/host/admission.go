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
	"github.com/aiomni/dune/pkg/observe"
)

func configurationFingerprint(options Options, urls deployment.URLs, service identity.Service, managedFingerprint string) (string, error) {
	revision := options.ConfigurationVersion
	if len(revision) > 128 || !utf8.ValidString(revision) || strings.ContainsFunc(revision, unicode.IsControl) {
		return "", fmt.Errorf("configuration version must be a nonsecret identifier of at most 128 bytes")
	}
	if (options.Identity != nil || options.AccessChecker != nil || options.Managed != nil) && revision == "" {
		return "", fmt.Errorf("PostgreSQL hosts with custom identity, policy or Managed providers require ConfigurationVersion")
	}
	// Origins and direct peer addresses identify instances and may differ. The
	// mounted path and behavior must agree. Opaque provider/checker configuration
	// is represented by the version supplied by the application, never by secrets.
	description := struct {
		Protocol, Path, Namespace, Policy, Version string
		// Omitting the empty addition preserves the admission fingerprint of
		// deployments that have not enabled Managed.
		Managed               string `json:",omitempty"`
		Lifetime              time.Duration
		Registration, Cluster bool
	}{Protocol: api.Version, Path: urls.Path, Namespace: service.Namespace(), Lifetime: identity.SessionLifetime, Registration: options.Identity == nil && !options.DisableRegistration, Cluster: options.Cluster != nil, Policy: "owner-v1", Version: revision, Managed: managedFingerprint}
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

func registerInstance(ctx context.Context, store *metadata.Store, options Options, urls deployment.URLs, service identity.Service, managedFingerprint string) (*gateway.AdmissionLease, metadata.InstanceConfig, error) {
	fingerprint, err := configurationFingerprint(options, urls, service, managedFingerprint)
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
			if a.ctx.Err() == nil {
				a.observer.emit(observe.Event{Name: observe.HostAdmissionRenewal, Outcome: "expired", OwnerID: config.BootID})
			}
			a.cancel()
			return
		case <-ticker.C:
			started := time.Now()
			bounded, cancel := context.WithTimeout(a.ctx, min(time.Second, a.admission.Remaining()))
			duration, err := a.store.RenewInstance(bounded, config)
			expired := bounded.Err()
			cancel()
			suboperation := "store"
			if expired != nil {
				suboperation = "deadline"
			}
			leaseErr := error(nil)
			if err == nil && expired == nil {
				leaseErr = a.admission.Renew(started.Add(duration))
				if leaseErr != nil {
					suboperation = "local_lease"
				}
			}
			if err != nil || expired != nil || leaseErr != nil {
				// An uncertain renewal cannot extend local authority. This boot stops;
				// its database reservation expires naturally instead of being replayed.
				if a.ctx.Err() == nil {
					a.observer.emit(observe.Event{Name: observe.HostAdmissionRenewal, Outcome: "failed", Suboperation: suboperation, OwnerID: config.BootID, DurationMicros: observationMicros(started)})
				}
				a.cancel()
				return
			}
		}
	}
}
