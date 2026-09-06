// Package identity owns local authentication and session policy. Persistence
// implements atomic domain operations; HTTP cookies and routes belong to Web.
package identity

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/aiomni/dune/internal/wire"
)

var (
	ErrInvalidArgument      = errors.New("invalid argument")
	ErrUnauthorized         = errors.New("invalid credentials or expired session")
	ErrRegistrationDisabled = errors.New("此站点未开放本地注册，请使用已有账号登录。")
	ErrSessionLimit         = errors.New("active login session limit reached; log out of another browser or wait for expiry")
	ErrLoginLimit           = errors.New("login service busy; retry later")
)

const SessionLifetime = 7 * 24 * time.Hour
const maxSessions = 32

type User struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Namespace string `json:"-"`
}

type Service interface {
	Register(context.Context, string, string) (User, string, error)
	Login(context.Context, string, string) (User, string, error)
	Authenticate(context.Context, string) (User, error)
	Logout(context.Context, string) error
	RegistrationAllowed() bool
	Namespace() string
}

type Account struct {
	User
	Salt         string `json:"salt"`
	PasswordHash string `json:"password_hash"`
}

// Repository is internal to Dune's modules. The host cannot combine independent
// domain stores. Each mutation below commits atomically on the selected backend.
type Repository interface {
	RegisterAccount(context.Context, Account, string, int64) error
	ReadAccount(context.Context, string) (Account, error)
	CreateSession(context.Context, string, string, int64, int) error
	ReadSession(context.Context, string, int64) (User, error)
	DeleteSession(context.Context, string) error
}

type Local struct {
	store        Repository
	registration bool
}

func NewLocal(store Repository, registration bool) *Local {
	return &Local{store: store, registration: registration}
}

func (l *Local) RegistrationAllowed() bool { return l.registration }
func (*Local) Namespace() string           { return "" }

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func passwordHash(password, salt string) (string, error) {
	b, err := pbkdf2.Key(sha256.New, password, []byte(salt), 600000, 32)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func normalizedEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 254 {
		return "", fmt.Errorf("%w: valid email address required", ErrInvalidArgument)
	}
	return email, nil
}

func (l *Local) Register(ctx context.Context, email, password string) (User, string, error) {
	if !l.registration {
		return User{}, "", ErrRegistrationDisabled
	}
	if err := ctx.Err(); err != nil {
		return User{}, "", err
	}
	email, err := normalizedEmail(email)
	if err != nil {
		return User{}, "", err
	}
	if len(password) < 12 || len(password) > 256 {
		return User{}, "", fmt.Errorf("%w: password must be 12..256 bytes", ErrInvalidArgument)
	}
	account := Account{User: User{ID: wire.ID(), Email: email}, Salt: wire.ID()}
	account.PasswordHash, err = passwordHash(password, account.Salt)
	if err != nil {
		return User{}, "", err
	}
	token := wire.ID() + wire.ID()
	if err := l.store.RegisterAccount(ctx, account, digest(token), time.Now().Add(SessionLifetime).Unix()); err != nil {
		return User{}, "", err
	}
	return account.User, token, nil
}

func (l *Local) Login(ctx context.Context, email, password string) (User, string, error) {
	if err := ctx.Err(); err != nil {
		return User{}, "", err
	}
	if len(password) > 256 {
		return User{}, "", ErrUnauthorized
	}
	email, err := normalizedEmail(email)
	if err != nil {
		return User{}, "", ErrUnauthorized
	}
	account, err := l.store.ReadAccount(ctx, email)
	if err != nil && !errors.Is(err, ErrUnauthorized) {
		return User{}, "", err
	}
	salt := account.Salt
	if salt == "" {
		salt = "dune-nonexistent-account-salt"
	}
	hash, err := passwordHash(password, salt)
	if err != nil {
		return User{}, "", err
	}
	if account.ID == "" || subtle.ConstantTimeCompare([]byte(hash), []byte(account.PasswordHash)) != 1 {
		return User{}, "", ErrUnauthorized
	}
	token := wire.ID() + wire.ID()
	if err := l.store.CreateSession(ctx, account.ID, digest(token), time.Now().Add(SessionLifetime).Unix(), maxSessions); err != nil {
		return User{}, "", err
	}
	return account.User, token, nil
}

func (l *Local) Authenticate(ctx context.Context, token string) (User, error) {
	if len(token) != 64 {
		return User{}, ErrUnauthorized
	}
	user, err := l.store.ReadSession(ctx, digest(token), time.Now().Unix())
	if err == nil && user.Namespace != "" {
		return User{}, ErrUnauthorized
	}
	return user, err
}

func (l *Local) Logout(ctx context.Context, token string) error {
	return l.store.DeleteSession(ctx, digest(token))
}
