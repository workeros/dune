// Package identity defines the authentication boundary used by Dune Web.
// Dune ships a local password implementation; enterprise deployments provide
// their own implementation and validate their own session cookie directly.
package identity

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalidArgument      = errors.New("invalid argument")
	ErrUnauthorized         = errors.New("invalid credentials or expired session")
	ErrRegistrationDisabled = errors.New("此站点未开放本地注册，请使用已有账号登录。")
	ErrSessionLimit         = errors.New("active login session limit reached; log out of another browser or wait for expiry")
	ErrLoginLimit           = errors.New("login service busy; retry later")
)

// User is the verified identity used for authorization. ID must be stable and
// unique within Namespace. Enterprise adapters should return the enterprise
// directory's user ID directly; Dune does not create a shadow principal.
type User struct {
	ID        string `json:"id"`
	Email     string `json:"email,omitempty"`
	Namespace string `json:"-"`
	Subject   string `json:"-"`
	Kind      string `json:"-"`
}

// Authentication is one successful credential verification. ExpiresAt is the
// actual credential expiry, not a cache TTL or a property of the user.
type Authentication struct {
	User      User
	ExpiresAt time.Time
}

func (a Authentication) Valid() bool {
	return a.User.ID != "" && time.Now().Before(a.ExpiresAt)
}

// LoginMethod is advertised by /api/v1/bootstrap. URL may point outside Dune when
// a host owns the enterprise login flow.
type LoginMethod struct {
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

// Service validates the host's opaque browser credential and returns its actual
// expiry. Every Authenticate call must use the freshest facts the identity source
// exposes and honor cancellation. Logout delegates revocation where the source
// supports it; offline JWT verification cannot observe upstream logout.
// Dune never persists enterprise sessions or user records.
type Service interface {
	Authenticate(context.Context, string) (Authentication, error)
	Logout(context.Context, string) error
	Namespace() string
	LoginMethods(publicURL string) []LoginMethod
}

// PasswordService is the optional local-login surface. Web publishes its
// register/login endpoints only for implementations of this interface.
type PasswordService interface {
	Service
	Register(context.Context, string, string) (User, string, error)
	Login(context.Context, string, string) (User, string, error)
	RegistrationAllowed() bool
}
