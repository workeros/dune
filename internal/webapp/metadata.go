package webapp

import (
	"errors"
	"net/http"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/runner"
)

// Database failures must not masquerade as bad credentials or missing objects.
// In particular, a lost commit acknowledgement cannot authorize a write retry.
func writeMetadataError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, access.ErrUnavailable):
		writeError(w, 503, "ACCESS_UNAVAILABLE", "access checker unavailable")
	case errors.Is(err, access.ErrDenied):
		writeError(w, 403, "ACCESS_DENIED", "access denied")
	case errors.Is(err, fabric.ErrProviderUnavailable):
		writeError(w, 503, "MANAGED_PROVIDER_UNAVAILABLE", "managed provider is not accepting new resources")
	case errors.Is(err, metadata.ErrCommitUnknown):
		writeError(w, 503, "RESULT_UNKNOWN", "提交结果未知，请先核对当前状态，不要自动重试此操作。")
	case errors.Is(err, identity.ErrRegistrationDisabled):
		writeError(w, 403, "REGISTRATION_DISABLED", err.Error())
	case errors.Is(err, identity.ErrUnauthorized):
		writeError(w, 401, "UNAUTHORIZED", identity.ErrUnauthorized.Error())
	case errors.Is(err, runner.ErrBindingChanged):
		writeError(w, 409, "BINDING_CHANGED", runner.ErrBindingChanged.Error())
	case errors.Is(err, lifecycle.ErrBusy), errors.Is(err, lifecycle.ErrIntentConflict):
		writeError(w, 409, "LIFECYCLE_CONFLICT", err.Error())
	case errors.Is(err, fabric.ErrTemplateNotFound):
		writeError(w, 404, "NOT_FOUND", "resource not found")
	case errors.Is(err, metadata.ErrNotFound), errors.Is(err, authorization.ErrNotFound):
		writeError(w, 404, "NOT_FOUND", "resource not found")
	case errors.Is(err, metadata.ErrConflict):
		writeError(w, 409, "CONFLICT", "metadata conflict")
	case errors.Is(err, identity.ErrSessionLimit):
		writeError(w, 429, "SESSION_LIMIT", err.Error())
	case errors.Is(err, identity.ErrLoginLimit):
		writeError(w, 429, "LOGIN_LIMIT", err.Error())
	case errors.Is(err, identity.ErrInvalidArgument), errors.Is(err, metadata.ErrInvalidArgument), errors.Is(err, fabric.ErrInvalidParameters), errors.Is(err, fabric.ErrInvalidTemplate):
		writeError(w, 400, "INVALID_ARGUMENT", err.Error())
	default:
		writeError(w, 503, "METADATA_UNAVAILABLE", "metadata service unavailable")
	}
}
