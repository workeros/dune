package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/aiomni/dune/internal/lifecycle"
	managedmodule "github.com/aiomni/dune/internal/managed"
	"github.com/aiomni/dune/pkg/deployment"
	"github.com/aiomni/dune/pkg/fabric"
)

// ManagedProviders contains the capabilities required for each configured
// Fabric namespace. Adapter instances retain private SDK configuration and
// credentials; Dune copies only these interface maps at startup.
type ManagedProviders struct {
	Availability map[string]fabric.AvailabilityProvider
	Create       map[string]fabric.CreateProvider
	Bootstrap    map[string]fabric.BootstrapProvider
	Inspect      map[string]fabric.InspectProvider
	Renew        map[string]fabric.RenewProvider
	Destroy      map[string]fabric.DestroyProvider
	Candidate    map[string]fabric.CandidateProvider
}

// ManagedWorkerOptions controls durable lifecycle recovery. Start from
// DefaultManagedWorkerOptions and then override explicit values. Zero is valid
// only for FirstConnectionGrace and DestroyAccessCloseTimeout.
type ManagedWorkerOptions struct {
	PollInterval, LeaseTTL, CallTimeout time.Duration
	EnrollmentLifetime                  time.Duration
	RenewalEnabled                      bool
	ExtendBy, RenewBefore               time.Duration
	FirstConnectionGrace                time.Duration
	RenewalPolicyVersion                string
	BootstrapVersion                    string
	DestroyAccessCloseTimeout           time.Duration
	HistoryRetention                    time.Duration
}

func DefaultManagedWorkerOptions() ManagedWorkerOptions {
	return ManagedWorkerOptions{
		PollInterval: time.Second, LeaseTTL: 30 * time.Second, CallTimeout: 20 * time.Second,
		EnrollmentLifetime: 10 * time.Minute, RenewalEnabled: true, ExtendBy: time.Hour,
		RenewBefore: 10 * time.Minute, FirstConnectionGrace: 5 * time.Minute,
		RenewalPolicyVersion: "personal-v1", DestroyAccessCloseTimeout: time.Minute,
		HistoryRetention: 30 * 24 * time.Hour,
	}
}

// ManagedOptions enables the complete Managed lifecycle. Templates are public
// user choices. Every Fabric referenced by a template must have a read-only
// availability check and every lifecycle capability. Every configured provider
// namespace must be complete so existing resources remain recoverable.
type ManagedOptions struct {
	Templates []fabric.Template
	Providers ManagedProviders
	Worker    ManagedWorkerOptions
}

type managedAssembly struct {
	catalog             *fabric.Catalog
	availability        map[string]fabric.AvailabilityProvider
	providers           managedmodule.ProviderSet
	worker              managedmodule.WorkerConfig
	destroyCloseTimeout time.Duration
	fingerprint         string
}

func providerKeys(providers ManagedProviders) ([]string, error) {
	sets := []map[string]bool{{}, {}, {}, {}, {}, {}, {}}
	for id, provider := range providers.Availability {
		sets[0][id] = provider != nil
	}
	for id, provider := range providers.Create {
		sets[1][id] = provider != nil
	}
	for id, provider := range providers.Bootstrap {
		sets[2][id] = provider != nil
	}
	for id, provider := range providers.Inspect {
		sets[3][id] = provider != nil
	}
	for id, provider := range providers.Renew {
		sets[4][id] = provider != nil
	}
	for id, provider := range providers.Destroy {
		sets[5][id] = provider != nil
	}
	for id, provider := range providers.Candidate {
		sets[6][id] = provider != nil
	}
	if len(sets[0]) == 0 {
		return nil, fmt.Errorf("Managed requires at least one complete provider")
	}
	keys := make([]string, 0, len(sets[0]))
	for id, valid := range sets[0] {
		if !valid {
			return nil, fmt.Errorf("Managed provider %q is nil", id)
		}
		for _, set := range sets[1:] {
			if len(set) != len(sets[0]) || !set[id] {
				return nil, fmt.Errorf("Managed provider %q does not implement the complete lifecycle", id)
			}
		}
		keys = append(keys, id)
	}
	sort.Strings(keys)
	return keys, nil
}

func prepareManaged(options *ManagedOptions, urls deployment.URLs) (*managedAssembly, error) {
	if options == nil {
		return nil, nil
	}
	catalog, err := fabric.NewCatalog(options.Templates)
	if err != nil {
		return nil, err
	}
	keys, err := providerKeys(options.Providers)
	if err != nil {
		return nil, err
	}
	configured := make(map[string]bool, len(keys))
	for _, id := range keys {
		configured[id] = true
	}
	for _, template := range options.Templates {
		if !configured[template.FabricID] {
			return nil, fmt.Errorf("Managed template references provider %q without a complete lifecycle", template.FabricID)
		}
	}
	workerOptions := options.Worker
	renewal := lifecycle.RenewalConfig{
		Enabled: workerOptions.RenewalEnabled, ExtendBy: workerOptions.ExtendBy,
		RenewBefore: workerOptions.RenewBefore, FirstConnectionGrace: workerOptions.FirstConnectionGrace,
	}
	if workerOptions.DestroyAccessCloseTimeout < 0 || workerOptions.DestroyAccessCloseTimeout > 10*time.Minute || workerOptions.EnrollmentLifetime < time.Second || workerOptions.EnrollmentLifetime > 10*time.Minute ||
		workerOptions.HistoryRetention < 24*time.Hour || workerOptions.HistoryRetention > 366*24*time.Hour {
		return nil, fmt.Errorf("Managed enrollment, destroy wait and history retention require bounded durations")
	}
	worker := managedmodule.WorkerConfig{
		PollInterval: workerOptions.PollInterval, LeaseTTL: workerOptions.LeaseTTL, CallTimeout: workerOptions.CallTimeout,
		Bootstrap: managedmodule.BootstrapConfig{PublicURL: urls.PublicURL, GatewayURL: urls.GatewayURL, Version: workerOptions.BootstrapVersion, EnrollmentLifetime: workerOptions.EnrollmentLifetime},
		Renewal:   renewal, RenewalPolicyVersion: workerOptions.RenewalPolicyVersion,
		HistoryRetention: workerOptions.HistoryRetention,
	}
	// NewWorker performs the final provider-name and duration validation after
	// the shared Store exists. This nonsecret snapshot is also cluster identity.
	description := struct {
		Catalog, BootstrapVersion, RenewalPolicyVersion string
		Providers                                       []string
		PollInterval, LeaseTTL, CallTimeout             time.Duration
		EnrollmentLifetime, DestroyCloseTimeout         time.Duration
		HistoryRetention                                time.Duration
		Renewal                                         lifecycle.RenewalConfig
	}{
		Catalog: catalog.Fingerprint(), Providers: keys, BootstrapVersion: workerOptions.BootstrapVersion,
		RenewalPolicyVersion: workerOptions.RenewalPolicyVersion, PollInterval: workerOptions.PollInterval,
		LeaseTTL: workerOptions.LeaseTTL, CallTimeout: workerOptions.CallTimeout,
		EnrollmentLifetime: workerOptions.EnrollmentLifetime, DestroyCloseTimeout: workerOptions.DestroyAccessCloseTimeout,
		HistoryRetention: workerOptions.HistoryRetention,
		Renewal:          renewal,
	}
	encoded, err := json.Marshal(description)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	return &managedAssembly{
		catalog:      catalog,
		availability: options.Providers.Availability,
		providers: managedmodule.ProviderSet{
			Create: options.Providers.Create, Bootstrap: options.Providers.Bootstrap,
			Inspect: options.Providers.Inspect, Renew: options.Providers.Renew, Destroy: options.Providers.Destroy, Candidate: options.Providers.Candidate,
		},
		worker: worker, destroyCloseTimeout: workerOptions.DestroyAccessCloseTimeout,
		fingerprint: hex.EncodeToString(sum[:]),
	}, nil
}
