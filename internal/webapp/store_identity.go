package webapp

import (
	"context"
	"fmt"

	"github.com/aiomni/dune/internal/identity"
)

var _ identity.Repository = (*Store)(nil)

func (s *Store) RegisterAccount(ctx context.Context, account identity.Account, hash string, expires int64) error {
	return s.update(func(data *metadata) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, a := range data.Accounts {
			if a.Email == account.Email {
				return fmt.Errorf("email already registered")
			}
		}
		if _, exists := data.Accounts[account.ID]; exists {
			return fmt.Errorf("account already exists")
		}
		data.Accounts[account.ID] = account
		data.Sessions[hash] = loginSession{UserID: account.ID, ExpiresAt: expires}
		return nil
	})
}

func (s *Store) ReadAccount(ctx context.Context, email string) (identity.Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return identity.Account{}, err
	}
	if s.lock == nil {
		return identity.Account{}, fmt.Errorf("account store is closed")
	}
	for _, a := range s.data.Accounts {
		if a.Email == email {
			return a, nil
		}
	}
	return identity.Account{}, ErrUnauthorized
}

func (s *Store) CreateSession(ctx context.Context, userID, hash string, expires int64, limit int) error {
	return s.update(func(data *metadata) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, ok := data.Accounts[userID]; !ok {
			return ErrUnauthorized
		}
		count := 0
		for _, session := range data.Sessions {
			if session.UserID == userID {
				count++
			}
		}
		if count >= limit {
			return identity.ErrSessionLimit
		}
		data.Sessions[hash] = loginSession{UserID: userID, ExpiresAt: expires}
		return nil
	})
}

func (s *Store) ReadSession(ctx context.Context, hash string, now int64) (User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return User{}, err
	}
	if s.lock == nil {
		return User{}, fmt.Errorf("account store is closed")
	}
	session, ok := s.data.Sessions[hash]
	if !ok || session.ExpiresAt <= now {
		return User{}, ErrUnauthorized
	}
	a, ok := s.data.Accounts[session.UserID]
	if !ok {
		return User{}, ErrUnauthorized
	}
	return a.User, nil
}

func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	return s.update(func(data *metadata) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		delete(data.Sessions, hash)
		return nil
	})
}
