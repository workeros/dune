package metadata

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"

	"github.com/aiomni/dune/internal/identity"
)

// Legacy is a read-only import source, never a runtime backend. It holds the
// original account-directory lock until Close and never rewrites source data.
type Legacy struct {
	lock *os.File
	data legacyData
}

type legacySession struct {
	UserID    string `json:"user_id"`
	ExpiresAt int64  `json:"expires_at"`
}
type legacyEnrollment struct {
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	ExpiresAt int64  `json:"expires_at"`
}

// Keep the legacy wire shape frozen when the current Machine view grows.
type legacyMachine struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	CreatedAt      int64  `json:"created_at"`
	OwnerID        string `json:"owner_id"`
	CredentialHash string `json:"credential_hash"`
}
type legacyData struct {
	Version     int                         `json:"version"`
	Accounts    map[string]identity.Account `json:"accounts"`
	Sessions    map[string]legacySession    `json:"sessions"`
	Enrollments map[string]legacyEnrollment `json:"enrollments"`
	Machines    map[string]legacyMachine    `json:"machines"`
}

type ImportCounts struct{ Accounts, Sessions, Enrollments, Machines int }

func ReadLegacy(ctx context.Context, dir string) (*Legacy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Do not create a directory when the user mistypes an import source.
	if _, err := os.Lstat(dir); err != nil {
		return nil, err
	}
	lock, err := lockDirectory(dir)
	if err != nil {
		return nil, err
	}
	source := &Legacy{lock: lock}
	f, err := os.OpenFile(filepath.Join(dir, "accounts.json"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		source.Close()
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || int(stat.Uid) != os.Getuid() || stat.Nlink != 1 {
			err = fmt.Errorf("legacy metadata must be a private, owned regular file without links")
		}
	}
	var contents []byte
	if err == nil {
		contents, err = io.ReadAll(io.LimitReader(f, 32*1024*1024+1))
	}
	if err == nil && len(contents) > 32*1024*1024 {
		err = fmt.Errorf("legacy metadata exceeds 32 MiB")
	}
	if err == nil {
		err = uniqueJSON(json.NewDecoder(bytes.NewReader(contents)), 0)
	}
	if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(contents))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&source.data)
		if err == nil {
			var extra any
			if decoder.Decode(&extra) != io.EOF {
				err = fmt.Errorf("one legacy JSON object required")
			}
		}
	}
	if err == nil {
		err = source.data.validate()
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		source.Close()
		return nil, err
	}
	return source, nil
}

// encoding/json alone silently accepts duplicate keys. An import must not lose
// records before uniqueness and reference validation can examine them.
func uniqueJSON(dec *json.Decoder, depth int) error {
	if depth > 64 {
		return fmt.Errorf("legacy JSON nesting exceeds 64 levels")
	}
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim == '{' {
		seen := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			// Legacy keys are lowercase. Reject encoding/json's case-insensitive
			// field aliases as well, so a later "Accounts" cannot replace records.
			if !ok || name != strings.ToLower(name) || seen[name] {
				return fmt.Errorf("duplicate or invalid JSON object key")
			}
			seen[name] = true
			if err := uniqueJSON(dec, depth+1); err != nil {
				return err
			}
		}
	} else if delim == '[' {
		for dec.More() {
			if err := uniqueJSON(dec, depth+1); err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("invalid JSON structure")
	}
	_, err = dec.Token()
	return err
}

func hexValue(value string, size int) bool {
	if len(value) != size {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func validName(name string) bool {
	return name != "" && len(name) <= 120 && name == strings.TrimSpace(name) && !strings.ContainsFunc(name, unicode.IsControl)
}

func (d legacyData) validate() error {
	if d.Version != 1 || d.Accounts == nil || d.Sessions == nil || d.Enrollments == nil || d.Machines == nil {
		return fmt.Errorf("unsupported or incomplete legacy metadata")
	}
	emails := map[string]bool{}
	for id, a := range d.Accounts {
		address, err := mail.ParseAddress(a.Email)
		if !hexValue(id, 32) || a.ID != id || err != nil || address.Address != a.Email || len(a.Email) > 254 || a.Email != strings.ToLower(strings.TrimSpace(a.Email)) || emails[a.Email] || !hexValue(a.Salt, 32) || !hexValue(a.PasswordHash, 64) {
			return fmt.Errorf("invalid or duplicate legacy account")
		}
		emails[a.Email] = true
	}
	for hash, s := range d.Sessions {
		if _, ok := d.Accounts[s.UserID]; !ok || !hexValue(hash, 64) || s.ExpiresAt <= 0 {
			return fmt.Errorf("invalid legacy session or principal reference")
		}
	}
	for hash, e := range d.Enrollments {
		if _, ok := d.Accounts[e.UserID]; !ok || !hexValue(hash, 64) || e.ExpiresAt <= 0 || !validName(e.Name) {
			return fmt.Errorf("invalid legacy enrollment or principal reference")
		}
	}
	credentials := map[string]bool{}
	for id, m := range d.Machines {
		_, owner := d.Accounts[m.OwnerID]
		if !owner || !hexValue(id, 32) || id != m.ID || !validName(m.Name) || m.CreatedAt <= 0 || !hexValue(m.CredentialHash, 64) || credentials[m.CredentialHash] || (m.OS != "linux" && m.OS != "darwin") || (m.Arch != "amd64" && m.Arch != "arm64") {
			return fmt.Errorf("invalid legacy machine, credential or owner reference")
		}
		credentials[m.CredentialHash] = true
	}
	return nil
}

func (l *Legacy) Close() error { return l.lock.Close() }

func (s *Store) ImportLegacy(ctx context.Context, source *Legacy) (ImportCounts, error) {
	d := source.data
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.emptyImportTarget(ctx, tx); err != nil {
			return err
		}
		for _, a := range d.Accounts {
			if _, err := tx.ExecContext(ctx, `INSERT INTO dune_principals(id,email) VALUES($1,$2)`, a.ID, a.Email); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO dune_local_accounts(principal_id,email,salt,password_hash) VALUES($1,$2,$3,$4)`, a.ID, a.Email, a.Salt, a.PasswordHash); err != nil {
				return err
			}
		}
		for hash, s := range d.Sessions {
			if _, err := tx.ExecContext(ctx, `INSERT INTO dune_sessions(hash,principal_id,expires_at) VALUES($1,$2,$3)`, hash, s.UserID, s.ExpiresAt); err != nil {
				return err
			}
		}
		for hash, e := range d.Enrollments {
			if _, err := tx.ExecContext(ctx, `INSERT INTO dune_enrollments(hash,principal_id,name,expires_at) VALUES($1,$2,$3,$4)`, hash, e.UserID, e.Name, e.ExpiresAt); err != nil {
				return err
			}
		}
		for _, m := range d.Machines {
			if _, err := tx.ExecContext(ctx, `INSERT INTO dune_runners(id,owner_id,name,kind,fabric_id,binding_revision,created_at) VALUES($1,$2,$3,'attached','attached',1,$4)`, m.ID, m.OwnerID, m.Name, m.CreatedAt); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO dune_machines(id,runner_id,credential_hash,os,arch) VALUES($1,$1,$2,$3,$4)`, m.ID, m.CredentialHash, m.OS, m.Arch); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return ImportCounts{Accounts: len(d.Accounts), Sessions: len(d.Sessions), Enrollments: len(d.Enrollments), Machines: len(d.Machines)}, nil
}
