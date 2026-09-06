package webapp

import (
	"errors"
	"net/http"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
)

// Database failures must not masquerade as bad credentials or missing objects.
// In particular, a lost commit acknowledgement cannot authorize a write retry.
func writeMetadataError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, metadata.ErrCommitUnknown):
		writeError(w, 503, "RESULT_UNKNOWN", "提交结果未知，请先核对当前状态，不要自动重试此操作。")
	case errors.Is(err, identity.ErrRegistrationDisabled):
		writeError(w, 403, "REGISTRATION_DISABLED", err.Error())
	case errors.Is(err, identity.ErrUnauthorized):
		writeError(w, 401, "UNAUTHORIZED", identity.ErrUnauthorized.Error())
	case errors.Is(err, metadata.ErrNotFound):
		writeError(w, 404, "NOT_FOUND", "machine not found")
	case errors.Is(err, metadata.ErrConflict):
		writeError(w, 409, "CONFLICT", "metadata conflict")
	case errors.Is(err, identity.ErrSessionLimit):
		writeError(w, 429, "SESSION_LIMIT", err.Error())
	case errors.Is(err, identity.ErrLoginLimit):
		writeError(w, 429, "LOGIN_LIMIT", err.Error())
	case errors.Is(err, identity.ErrInvalidArgument), errors.Is(err, metadata.ErrInvalidArgument):
		writeError(w, 400, "INVALID_ARGUMENT", err.Error())
	default:
		writeError(w, 503, "METADATA_UNAVAILABLE", "metadata service unavailable")
	}
}
