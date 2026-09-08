package managed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/deployment"
	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/runner"
)

type BootstrapConfig struct {
	PublicURL          string
	GatewayURL         string
	Version            string
	EnrollmentLifetime time.Duration
}

type bootstrapTarget struct {
	Endpoint         string `json:"endpoint"`
	GatewayURL       string `json:"gateway_url"`
	Version          string `json:"version"`
	EnrollmentMillis int64  `json:"enrollment_millis"`
}

func normalizeBootstrapConfig(config BootstrapConfig) (bootstrapTarget, string, error) {
	urls, err := deployment.NewURLs(config.PublicURL, config.GatewayURL)
	if err != nil || config.Version == "" || len(config.Version) > 128 || !utf8.ValidString(config.Version) || strings.TrimSpace(config.Version) != config.Version || strings.ContainsFunc(config.Version, unicode.IsControl) || config.EnrollmentLifetime < time.Second || config.EnrollmentLifetime > 10*time.Minute {
		return bootstrapTarget{}, "", fmt.Errorf("invalid managed bootstrap configuration")
	}
	target := bootstrapTarget{Endpoint: urls.PublicURL, GatewayURL: urls.GatewayURL, Version: config.Version, EnrollmentMillis: config.EnrollmentLifetime.Milliseconds()}
	encoded, err := json.Marshal(target)
	if err != nil {
		return bootstrapTarget{}, "", err
	}
	sum := sha256.Sum256(encoded)
	return target, hex.EncodeToString(sum[:]), nil
}

type BootstrapExecutor struct {
	store     *metadata.Store
	providers map[string]fabric.BootstrapProvider
	target    bootstrapTarget
	digest    string
}

func NewBootstrapExecutor(store *metadata.Store, providers map[string]fabric.BootstrapProvider, config BootstrapConfig) (*BootstrapExecutor, error) {
	if store == nil {
		return nil, fmt.Errorf("managed bootstrap executor requires shared metadata")
	}
	target, digest, err := normalizeBootstrapConfig(config)
	if err != nil {
		return nil, err
	}
	copy := make(map[string]fabric.BootstrapProvider, len(providers))
	for id, provider := range providers {
		if id == "" || id == "attached" || len(id) > 128 || !utf8.ValidString(id) || strings.TrimSpace(id) != id || strings.ContainsFunc(id, unicode.IsControl) || provider == nil {
			return nil, fmt.Errorf("invalid managed bootstrap provider configuration")
		}
		copy[id] = provider
	}
	return &BootstrapExecutor{store: store, providers: copy, target: target, digest: digest}, nil
}

func (e *BootstrapExecutor) boundMachine(ctx context.Context, claimed lifecycle.Operation) (string, error) {
	selected, err := e.store.Runner(ctx, claimed.PrincipalID, claimed.RunnerID)
	if err != nil {
		return "", err
	}
	if selected.Kind != "managed" {
		return "", lifecycle.ErrIntentConflict
	}
	if selected.Binding == nil {
		return "", nil
	}
	if selected.Binding.RunnerID != claimed.RunnerID || selected.Binding.FabricID != claimed.FabricID || selected.Binding.Revision != claimed.BindingRevision {
		return "", runner.ErrBindingChanged
	}
	return selected.Binding.MachineID, nil
}

func (e *BootstrapExecutor) recordBoundMachine(ctx context.Context, claimed lifecycle.Operation, action lifecycle.ProviderAction) (bool, error) {
	machineID, err := e.boundMachine(ctx, claimed)
	if err != nil || machineID == "" {
		return false, err
	}
	return true, e.store.RecordProviderAction(ctx, claimed, action.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: action.ResourceRef})
}

// Execute advances one claimed Bootstrap stage. Only the first confirmed grant
// can carry enrollment material to Bootstrap. Every later claimant either uses
// the consumed grant's durable machine binding or reconciles the original call.
func (e *BootstrapExecutor) Execute(ctx context.Context, claimed lifecycle.Operation) error {
	return e.execute(ctx, ctx, claimed)
}

func (e *BootstrapExecutor) execute(ctx, providerCtx context.Context, claimed lifecycle.Operation) error {
	creation, err := e.store.ManagedCreation(ctx, claimed.PrincipalID, claimed.RequestKey)
	if err != nil {
		return err
	}
	if creation.Operation.ID != claimed.ID || creation.Operation.Intent != claimed.Intent {
		return lifecycle.ErrIntentConflict
	}
	existing, existingErr := e.store.ProviderAction(ctx, claimed.ID, "bootstrap")
	if existingErr == nil {
		if !existing.CompletedAt.IsZero() {
			return nil
		}
		if bound, err := e.recordBoundMachine(ctx, claimed, existing); err != nil || bound {
			return err
		}
	} else if !errors.Is(existingErr, metadata.ErrNotFound) {
		return existingErr
	}
	provider, ok := e.providers[claimed.FabricID]
	if !ok {
		return ErrProviderUnavailable
	}
	grant := lifecycle.BootstrapGrant{}
	dispatch := false
	if existingErr == nil {
		grant.Action = existing
	} else {
		grant, dispatch, err = e.store.BeginManagedBootstrap(ctx, claimed, e.digest, time.Duration(e.target.EnrollmentMillis)*time.Millisecond)
		if err != nil {
			return err
		}
	}
	if !grant.Action.CompletedAt.IsZero() {
		return nil
	}
	if !dispatch {
		if bound, err := e.recordBoundMachine(ctx, claimed, grant.Action); err != nil || bound {
			return err
		}
	}
	var observed fabric.Observation
	if dispatch {
		if err := e.store.CheckProviderAction(ctx, claimed, grant.Action.ID); err != nil {
			return err
		}
		observed, err = provider.Bootstrap(providerCtx, fabric.BootstrapCall{
			Action: providerAction(claimed, grant.Action), EnrollmentToken: grant.Token, EnrollmentExpiresAt: grant.ExpiresAt,
			Endpoint: e.target.Endpoint, GatewayURL: e.target.GatewayURL, Version: e.target.Version,
		})
	} else {
		observed, err = provider.ReconcileBootstrap(providerCtx, fabric.BootstrapReconcileCall{Action: providerAction(claimed, grant.Action)})
	}
	result, resultErr := providerObservation(observed, err)
	if resultErr != nil {
		return resultErr
	}
	return e.store.RecordProviderAction(ctx, claimed, grant.Action.ID, result)
}
