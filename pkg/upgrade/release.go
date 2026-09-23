// Package upgrade defines Runner online upgrade contracts. Hosts select trusted
// immutable releases; execution and recovery remain owned by Dune.
package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
)

const (
	MaxComponents   = 128
	MaxProgramBytes = 256 << 20
	MaxReleaseBytes = 768 << 20
	MaxArchiveBytes = 512 << 20
)

type ReleaseRef struct {
	ID             string `json:"id"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

func (p Platform) Valid() bool {
	return (p.OS == "linux" || p.OS == "darwin") && (p.Arch == "amd64" || p.Arch == "arm64")
}

// Component records all required file attributes. Executables are private
// 0700 files and notices are private 0600 files in every Dune installation.
type Component struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   uint32 `json:"mode"`
}

type Manifest struct {
	ID            string      `json:"id"`
	Platform      Platform    `json:"platform"`
	ArchiveURL    string      `json:"archive_url"`
	ArchiveSHA256 string      `json:"archive_sha256"`
	StateContract string      `json:"state_contract"`
	Components    []Component `json:"components"`
}

// Source belongs to the embedding host. It must enforce its release policy and
// honor cancellation. Runner callers cannot submit arbitrary download commands.
type Source interface {
	Resolve(context.Context, ReleaseRef, Platform) (Manifest, error)
}

func ValidSHA256(value string) bool {
	body, err := hex.DecodeString(value)
	return err == nil && len(body) == sha256.Size && hex.EncodeToString(body) == value
}

func (m Manifest) Validate() error {
	if m.ID == "" || len(m.ID) > 128 || strings.ContainsAny(m.ID, "\r\n\x00") || !m.Platform.Valid() || !ValidSHA256(m.StateContract) {
		return fmt.Errorf("complete release identity, platform and state contract required")
	}
	if m.ArchiveURL == "" && m.ArchiveSHA256 == "" {
		return ValidateComponents(m.Components)
	}
	if !ValidSHA256(m.ArchiveSHA256) {
		return fmt.Errorf("archive digest required with archive URL")
	}
	u, err := url.Parse(m.ArchiveURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" || len(m.ArchiveURL) > 4096 {
		return fmt.Errorf("invalid release archive URL")
	}
	return ValidateComponents(m.Components)
}

func ValidateComponents(components []Component) error {
	if len(components) < 4 || len(components) > MaxComponents {
		return fmt.Errorf("bounded complete release components required")
	}
	seen := make(map[string]bool)
	var total int64
	licenses := 0
	for _, c := range components {
		license := strings.HasPrefix(c.Path, "licenses/")
		if c.Path == "" || len(c.Path) > 256 || path.Clean(c.Path) != c.Path || strings.ContainsAny(c.Path, "\\\x00\r\n") || strings.HasPrefix(c.Path, "/") || strings.HasPrefix(c.Path, "../") || seen[c.Path] || (!license && c.Path != "dune" && c.Path != "tmux" && c.Path != "rg") {
			return fmt.Errorf("invalid or duplicate release component")
		}
		if !ValidSHA256(c.SHA256) || c.Bytes <= 0 || c.Bytes > MaxProgramBytes {
			return fmt.Errorf("invalid component digest or size")
		}
		wantMode := uint32(0700)
		if license {
			wantMode = 0600
			licenses++
		}
		if c.Mode != wantMode {
			return fmt.Errorf("component permissions differ from installation contract")
		}
		seen[c.Path] = true
		total += c.Bytes
	}
	if !seen["dune"] || !seen["tmux"] || !seen["rg"] || licenses == 0 || total > MaxReleaseBytes {
		return fmt.Errorf("release must include Dune, tmux, rg and licenses within size budget")
	}
	return nil
}

// Digest uses Go JSON encoding of this type with lexicographically sorted
// components. Publication tooling uses the same canonical representation.
func (m Manifest) Digest() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	m.Components = slices.Clone(m.Components)
	slices.SortFunc(m.Components, func(a, b Component) int { return strings.Compare(a.Path, b.Path) })
	body, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func (m Manifest) Matches(ref ReleaseRef) bool {
	digest, err := m.Digest()
	return err == nil && m.ID == ref.ID && digest == ref.ManifestSHA256
}

func (m Manifest) ProgramSHA256() string {
	for _, c := range m.Components {
		if c.Path == "dune" {
			return c.SHA256
		}
	}
	return ""
}

// ValidateDownload distinguishes a published target from an initial local
// installation receipt, whose contents are known without a download origin.
func (m Manifest) ValidateDownload() error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m.ArchiveURL == "" || !ValidSHA256(m.ArchiveSHA256) {
		return fmt.Errorf("published immutable archive required")
	}
	return nil
}
