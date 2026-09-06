package webapp

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
)

var ErrUnauthorized = identity.ErrUnauthorized
var ErrNotFound = errors.New("not found")

type User = identity.User

type Machine struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CreatedAt int64  `json:"created_at"`
}

type account = identity.Account

type loginSession struct {
	UserID    string `json:"user_id"`
	ExpiresAt int64  `json:"expires_at"`
}
type enrollment struct {
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	ExpiresAt int64  `json:"expires_at"`
}
type machineRecord struct {
	Machine
	OwnerID        string `json:"owner_id"`
	CredentialHash string `json:"credential_hash"`
}

// Deliberately no project, command, environment, output or conversation fields.
// This is a small single-process metadata store, not a content database.
type metadata struct {
	Version     int                      `json:"version"`
	Accounts    map[string]account       `json:"accounts"`
	Sessions    map[string]loginSession  `json:"sessions"`
	Enrollments map[string]enrollment    `json:"enrollments"`
	Machines    map[string]machineRecord `json:"machines"`
}

type Store struct {
	mu   sync.RWMutex
	dir  string
	lock *os.File
	data metadata
}

func OpenStore(dir string) (*Store, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == "/" {
		return nil, fmt.Errorf("account data directory must be private and absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || int(stat.Uid) != os.Getuid() {
		return nil, fmt.Errorf("account data directory must be owned by this user and mode 0700, not a symlink")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "accounts.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("account store is already open: %w", err)
	}
	s := &Store{dir: dir, lock: lock, data: metadata{Version: 1, Accounts: map[string]account{}, Sessions: map[string]loginSession{}, Enrollments: map[string]enrollment{}, Machines: map[string]machineRecord{}}}
	f, err := os.OpenFile(filepath.Join(dir, "accounts.json"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err == nil {
		dec := json.NewDecoder(io.LimitReader(f, 32*1024*1024))
		dec.DisallowUnknownFields()
		err = dec.Decode(&s.data)
		if err == nil {
			var extra any
			if dec.Decode(&extra) != io.EOF {
				err = fmt.Errorf("invalid trailing account data")
			}
		}
		f.Close()
	} else if os.IsNotExist(err) {
		err = nil
	}
	if err == nil && (s.data.Version != 1 || s.data.Accounts == nil || s.data.Sessions == nil || s.data.Enrollments == nil || s.data.Machines == nil) {
		err = fmt.Errorf("unsupported or corrupt account store")
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	err := s.lock.Close()
	s.lock = nil
	return err
}

// Commit a copy, then replace memory only after an atomic durable file write.
// Failed persistence must not consume a binding token or half-create an account.
func (s *Store) update(change func(*metadata) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return fmt.Errorf("account store is closed")
	}
	b, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	var next metadata
	if err = json.Unmarshal(b, &next); err != nil {
		return err
	}
	now := time.Now().Unix()
	for key, sess := range next.Sessions {
		if sess.ExpiresAt <= now {
			delete(next.Sessions, key)
		}
	}
	for key, token := range next.Enrollments {
		if token.ExpiresAt <= now {
			delete(next.Enrollments, key)
		}
	}
	if err = change(&next); err != nil {
		return err
	}
	b, err = json.Marshal(next)
	if err != nil {
		return err
	}
	if len(b) > 32*1024*1024 {
		return fmt.Errorf("account metadata capacity exceeded")
	}
	f, err := os.CreateTemp(s.dir, ".accounts-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(s.dir, "accounts.json")); err != nil {
		return err
	}
	s.data = next
	// The rename is committed even if the subsequent directory fsync fails.
	// Report this as an uncertain durable outcome, never roll back in memory.
	dir, err := os.Open(s.dir)
	if err != nil {
		return fmt.Errorf("metadata committed but durability uncertain: %w", err)
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return fmt.Errorf("metadata committed but durability uncertain: %w", err)
	}
	return nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func randomToken() string { return wire.ID() + wire.ID() }

func (s *Store) IssueEnrollment(userID, name string) (string, int64, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 || strings.ContainsFunc(name, unicode.IsControl) {
		return "", 0, fmt.Errorf("machine name must be 1..120 bytes without control characters")
	}
	token := randomToken()
	expires := time.Now().Add(10 * time.Minute).Unix()
	err := s.update(func(data *metadata) error {
		if _, ok := data.Accounts[userID]; !ok {
			return ErrUnauthorized
		}
		count := 0
		for _, pending := range data.Enrollments {
			if pending.UserID == userID {
				count++
			}
		}
		if count >= 5 {
			return fmt.Errorf("at most five pending binding commands; existing commands expire after ten minutes")
		}
		data.Enrollments[tokenHash(token)] = enrollment{UserID: userID, Name: name, ExpiresAt: expires}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	return token, expires, nil
}

func (s *Store) Enroll(token, osName, arch string) (Machine, string, error) {
	if len(token) != 64 {
		return Machine{}, "", ErrUnauthorized
	}
	if (osName != "linux" && osName != "darwin") || (arch != "amd64" && arch != "arm64") {
		return Machine{}, "", fmt.Errorf("supported platforms: Linux/macOS on amd64/arm64")
	}
	credential := randomToken()
	machine := Machine{ID: wire.ID(), OS: osName, Arch: arch, CreatedAt: time.Now().Unix()}
	err := s.update(func(data *metadata) error {
		pending, ok := data.Enrollments[tokenHash(token)]
		if !ok || pending.ExpiresAt <= time.Now().Unix() {
			return ErrUnauthorized
		}
		count := 0
		for _, m := range data.Machines {
			if m.OwnerID == pending.UserID {
				count++
			}
		}
		if count >= 32 {
			return fmt.Errorf("machine limit reached (32 per account)")
		}
		machine.Name = pending.Name
		data.Machines[machine.ID] = machineRecord{Machine: machine, OwnerID: pending.UserID, CredentialHash: tokenHash(credential)}
		delete(data.Enrollments, tokenHash(token))
		return nil
	})
	if err != nil {
		return Machine{}, "", err
	}
	return machine, credential, nil
}

func (s *Store) Machines(userID string) []Machine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Machine{}
	if s.lock == nil {
		return out
	}
	for _, m := range s.data.Machines {
		if m.OwnerID == userID {
			out = append(out, m.Machine)
		}
	}
	return out
}

func (s *Store) Owns(userID, machineID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lock == nil {
		return false
	}
	m, ok := s.data.Machines[machineID]
	return ok && m.OwnerID == userID
}

func (s *Store) MachineCredential(token string) (string, bool) {
	if len(token) != 64 {
		return "", false
	}
	hash := tokenHash(token)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lock == nil {
		return "", false
	}
	for _, m := range s.data.Machines {
		if subtle.ConstantTimeCompare([]byte(hash), []byte(m.CredentialHash)) == 1 {
			return m.ID, true
		}
	}
	return "", false
}

func (s *Store) Revoke(userID, machineID string) error {
	return s.update(func(data *metadata) error {
		m, ok := data.Machines[machineID]
		if !ok || m.OwnerID != userID {
			return ErrNotFound
		}
		delete(data.Machines, machineID)
		return nil
	})
}
