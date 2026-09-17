package webapp

import (
	"errors"
	"net/http"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/managed"
	"github.com/aiomni/dune/pkg/profiles"
	"github.com/aiomni/dune/pkg/runner"
)

// Database failures must not masquerade as bad credentials or missing objects.
// In particular, a lost commit acknowledgement cannot authorize a write retry.
func writeMetadataError(w http.ResponseWriter, err error) {
	status, code, message := metadataError(err)
	writeError(w, status, code, message)
}

func metadataError(err error) (int, string, string) {
	switch {
	case errors.Is(err, access.ErrUnavailable):
		return 503, "ACCESS_UNAVAILABLE", "access checker unavailable"
	case errors.Is(err, access.ErrDenied):
		return 403, "ACCESS_DENIED", "access denied"
	case errors.Is(err, fabric.ErrProviderUnavailable):
		return 503, "MANAGED_PROVIDER_UNAVAILABLE", "managed provider is not accepting new resources"
	case errors.Is(err, metadata.ErrCommitUnknown), errors.Is(err, profiles.ErrCommitUnknown):
		return 503, "RESULT_UNKNOWN", "提交结果未知，请先核对当前状态，不要自动重试此操作。"
	case errors.Is(err, identity.ErrRegistrationDisabled):
		return 403, "REGISTRATION_DISABLED", err.Error()
	case errors.Is(err, identity.ErrUnauthorized):
		return 401, "UNAUTHORIZED", identity.ErrUnauthorized.Error()
	case errors.Is(err, runner.ErrBindingChanged):
		return 409, "BINDING_CHANGED", runner.ErrBindingChanged.Error()
	case errors.Is(err, managed.ErrBusy), errors.Is(err, managed.ErrIntentConflict):
		return 409, "LIFECYCLE_CONFLICT", err.Error()
	case errors.Is(err, managed.ErrForbidden):
		return 403, "ACCESS_DENIED", "access denied"
	case errors.Is(err, managed.ErrNotFound):
		return 404, "NOT_FOUND", "resource not found"
	case errors.Is(err, managed.ErrInvalid):
		return 400, "INVALID_ARGUMENT", err.Error()
	case errors.Is(err, fabric.ErrTemplateNotFound):
		return 404, "NOT_FOUND", "resource not found"
	case errors.Is(err, profiles.ErrNotFound), errors.Is(err, metadata.ErrNotFound), errors.Is(err, authorization.ErrNotFound):
		return 404, "NOT_FOUND", "resource not found"
	case errors.Is(err, profiles.ErrConflict), errors.Is(err, metadata.ErrConflict):
		return 409, "CONFLICT", "metadata conflict"
	case errors.Is(err, identity.ErrSessionLimit):
		return 429, "SESSION_LIMIT", err.Error()
	case errors.Is(err, identity.ErrLoginLimit):
		return 429, "LOGIN_LIMIT", err.Error()
	case errors.Is(err, profiles.ErrInvalid), errors.Is(err, identity.ErrInvalidArgument), errors.Is(err, metadata.ErrInvalidArgument), errors.Is(err, fabric.ErrInvalidParameters), errors.Is(err, fabric.ErrInvalidTemplate):
		return 400, "INVALID_ARGUMENT", err.Error()
	default:
		return 503, "METADATA_UNAVAILABLE", "metadata service unavailable"
	}
}
