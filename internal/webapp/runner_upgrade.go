package webapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/upgrade"
)

// All five actions use the existing authenticated Runner selection and CSRF
// rules. The Go service also checks current permissions, including offline reads.
func (s *Server) runnerUpgrade(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	binding, ok := selectedBinding(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	if action != "inspect" && action != "preview" && action != "start" && action != "get" && action != "list" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown Runner upgrade action")
		return
	}
	resource, _, err := s.access.Resource(r.Context(), user, binding.MachineID, true, "runner.upgrade."+action)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	if resource.Runner.Binding == nil || *resource.Runner.Binding != binding {
		writeMetadataError(w, runner.ErrBindingChanged)
		return
	}
	if s.options.RunnerUpgrades == nil {
		writeError(w, http.StatusServiceUnavailable, "UPGRADE_UNSUPPORTED", "Runner upgrade service is not configured")
		return
	}
	var body json.RawMessage
	if !readJSON(w, r, &body) {
		return
	}
	scope := upgrade.Scope{Principal: user, OwnerID: resource.OwnerID, Binding: binding}
	var result any
	switch action {
	case "inspect":
		result, err = s.options.RunnerUpgrades.InspectRunner(r.Context(), scope)
	case "preview":
		var request upgrade.PreviewRequest
		if decodeUpgradeBody(body, &request) != nil || request.Binding != binding {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "preview must retain the selected binding")
			return
		}
		result, err = s.options.RunnerUpgrades.PreviewUpgrade(r.Context(), scope, request)
	case "start":
		var request upgrade.Request
		if decodeUpgradeBody(body, &request) != nil || request.Binding != binding || request.Validate() != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "complete original upgrade request required")
			return
		}
		result, err = s.options.RunnerUpgrades.StartUpgrade(r.Context(), scope, request)
	case "get":
		var query upgrade.Query
		if decodeUpgradeBody(body, &query) != nil || query.Binding != binding || query.Validate() != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "original upgrade selector required")
			return
		}
		result, err = s.options.RunnerUpgrades.GetUpgrade(r.Context(), scope, query)
	case "list":
		var request upgrade.ListRequest
		if decodeUpgradeBody(body, &request) != nil || request.Binding != binding || request.Validate() != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "original upgrade history scope required")
			return
		}
		result, err = s.options.RunnerUpgrades.ListUpgrades(r.Context(), scope, request)
	}
	if err != nil {
		status, code, message := metadataError(err)
		var protocol *api.Error
		if errors.As(err, &protocol) {
			status, code, message = http.StatusServiceUnavailable, protocol.Code, protocol.Detail
			if code == "INVALID_ARGUMENT" {
				status = http.StatusBadRequest
			}
			if code == "SUBMISSION_CONFLICT" || code == "INSTALLATION_CHANGED" {
				status = http.StatusConflict
			}
			if code == "UPGRADE_NOT_FOUND" {
				status = http.StatusNotFound
			}
		}
		writeJSON(w, status, struct {
			Code   string `json:"code"`
			Error  string `json:"error"`
			Result any    `json:"result"`
		}{code, message, result})
		return
	}
	status := http.StatusOK
	if action == "start" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, result)
}

func decodeUpgradeBody(body []byte, result any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("one upgrade request required")
	}
	return nil
}
