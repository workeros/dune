package identity

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const CLIPrefix = "dune_cli_"
const CLISessionLifetime = 8 * time.Hour

var ErrPending = errors.New("browser confirmation pending")

func ValidProof(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ValidCLIToken(token string) bool {
	return strings.HasPrefix(token, CLIPrefix) && ValidProof(strings.TrimPrefix(token, CLIPrefix))
}

func authenticateCLI(ctx context.Context, store interface {
	ReadSession(context.Context, string, int64) (User, error)
}, namespace, token string) (User, error) {
	if !ValidCLIToken(token) {
		return User{}, ErrUnauthorized
	}
	user, err := store.ReadSession(ctx, digest(token), time.Now().Unix())
	if err == nil && (user.Kind != "cli" || user.Namespace != namespace) {
		return User{}, ErrUnauthorized
	}
	return user, err
}
