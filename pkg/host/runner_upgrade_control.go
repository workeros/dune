package host

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aiomni/dune/internal/upgradecontrol"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/upgrade"
)

func (a *App) serveUpgradeControl(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeUpgradeControl(w, http.StatusMethodNotAllowed, upgradecontrol.Response{ErrorCode: "METHOD_NOT_ALLOWED"})
		return
	}
	select {
	case a.upgradeControls <- struct{}{}:
		defer func() { <-a.upgradeControls }()
	default:
		writeUpgradeControl(w, http.StatusServiceUnavailable, upgradecontrol.Response{ErrorCode: "RESOURCE_EXHAUSTED"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer controller.SetReadDeadline(time.Time{})
	credential, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || len(credential) > 4096 {
		writeUpgradeControl(w, http.StatusUnauthorized, upgradecontrol.Response{ErrorCode: "UNAUTHORIZED"})
		return
	}
	actual, err := a.authorizer.MachineUpgradeBinding(ctx, credential, runner.Binding{})
	if err != nil {
		writeControlFailure(w, err)
		return
	}
	var request upgradecontrol.Request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
		writeUpgradeControl(w, http.StatusBadRequest, upgradecontrol.Response{ErrorCode: "INVALID_ARGUMENT"})
		return
	}
	if request.Binding != (runner.Binding{}) && request.Binding != actual {
		writeControlFailure(w, runner.ErrBindingChanged)
		return
	}
	response := upgradecontrol.Response{Binding: actual}
	switch request.Action {
	case "binding":
	case "release":
		if a.upgradeSource == nil {
			writeUpgradeControl(w, http.StatusServiceUnavailable, upgradecontrol.Response{ErrorCode: "UPGRADE_SOURCE_UNAVAILABLE"})
			return
		}
		if request.Binding != actual || !request.Platform.Valid() || !upgrade.ValidSHA256(request.Release.ManifestSHA256) {
			writeUpgradeControl(w, http.StatusBadRequest, upgradecontrol.Response{ErrorCode: "INVALID_ARGUMENT"})
			return
		}
		manifest, err := a.upgradeSource.Resolve(ctx, request.Release, request.Platform)
		if err != nil {
			writeUpgradeControl(w, http.StatusBadGateway, upgradecontrol.Response{ErrorCode: "RELEASE_UNAVAILABLE"})
			return
		}
		if !manifest.Matches(request.Release) || manifest.Platform != request.Platform {
			writeUpgradeControl(w, http.StatusConflict, upgradecontrol.Response{ErrorCode: "RELEASE_CHANGED"})
			return
		}
		response.Release = &manifest
	case "confirm":
		if request.Probe.Binding != actual || request.Binding != actual {
			writeControlFailure(w, runner.ErrBindingChanged)
			return
		}
		proof, err := a.verifyUpgradeRoute(ctx, credential, request.Probe)
		if err != nil {
			writeControlFailure(w, err)
			return
		}
		response.Proof = &proof
	default:
		writeUpgradeControl(w, http.StatusBadRequest, upgradecontrol.Response{ErrorCode: "INVALID_ARGUMENT"})
		return
	}
	if _, err := a.authorizer.MachineUpgradeBinding(ctx, credential, actual); err != nil {
		writeControlFailure(w, err)
		return
	}
	writeUpgradeControl(w, http.StatusOK, response)
}

func (a *App) verifyUpgradeRoute(ctx context.Context, credential string, probe upgrade.Probe) (upgrade.Proof, error) {
	grant, handler, err := a.authorizer.UpgradeVerification(ctx, credential, probe.Binding)
	if err != nil {
		return upgrade.Proof{}, err
	}
	connection, close, err := a.connectGrantedRunner(ctx, probe.Binding, "runner.upgrade.probe", grant, handler)
	if err != nil {
		return upgrade.Proof{}, err
	}
	defer close()
	var proof upgrade.Proof
	if err := connection.Call(ctx, "runner.upgrade.probe", probe, &proof); err != nil {
		return proof, err
	}
	if proof.Binding != probe.Binding || proof.InstallationID != probe.InstallationID || proof.OperationID != probe.OperationID || proof.AttemptID != probe.AttemptID || proof.Challenge != probe.Challenge || proof.Incarnation != connection.Binding.Incarnation || proof.ConnectionGeneration != connection.Binding.Generation || proof.RouteEpoch != connection.Binding.RouteEpoch {
		return upgrade.Proof{}, &api.Error{Code: "UPGRADE_PROOF_MISMATCH", Detail: "routed response belongs to another attempt or connection"}
	}
	// These facts are asserted only here, after the normal authorized route has
	// completed a full round trip. The Runner's local receipt cannot assert them.
	proof.GatewayAccepted, proof.Routed = true, true
	return proof, nil
}

func writeUpgradeControl(w http.ResponseWriter, status int, result upgradecontrol.Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(result)
}

func writeControlFailure(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "UPGRADE_CONTROL_UNAVAILABLE"
	if errors.Is(err, identity.ErrUnauthorized) {
		status, code = http.StatusUnauthorized, "UNAUTHORIZED"
	}
	if errors.Is(err, runner.ErrBindingChanged) {
		status, code = http.StatusConflict, "STALE_BINDING"
	}
	var failure *api.Error
	if errors.As(err, &failure) && api.ValidateSubmissionID(failure.Code) == nil {
		code = failure.Code
	}
	writeUpgradeControl(w, status, upgradecontrol.Response{ErrorCode: code})
}
