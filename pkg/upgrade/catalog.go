package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/aiomni/dune/pkg/api"
)

const MaxCatalogBytes = 4 << 20

// Catalog is an immutable allowlist loaded by the host, never by a Runner's
// submitted URL. Replace it through host configuration when publishing releases.
type Catalog struct{ releases map[ReleaseRef]Manifest }

func NewCatalog(manifests []Manifest) (*Catalog, error) {
	if len(manifests) == 0 || len(manifests) > 256 {
		return nil, fmt.Errorf("catalog requires between 1 and 256 releases")
	}
	catalog := &Catalog{releases: make(map[ReleaseRef]Manifest, len(manifests))}
	identities := make(map[string]bool)
	for _, m := range manifests {
		if err := m.ValidateDownload(); err != nil {
			return nil, err
		}
		identity := m.ID + "/" + m.Platform.OS + "/" + m.Platform.Arch
		if identities[identity] {
			return nil, fmt.Errorf("duplicate release identity and platform")
		}
		identities[identity] = true
		digest, err := m.Digest()
		if err != nil {
			return nil, err
		}
		m.Components = slices.Clone(m.Components)
		catalog.releases[ReleaseRef{ID: m.ID, ManifestSHA256: digest}] = m
	}
	return catalog, nil
}

// ReadCatalog accepts the bounded JSON array emitted by package-release.py.
func ReadCatalog(reader io.Reader) (*Catalog, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxCatalogBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxCatalogBytes {
		return nil, fmt.Errorf("release catalog exceeds size limit")
	}
	var manifests []Manifest
	if err := json.Unmarshal(data, &manifests); err != nil {
		return nil, err
	}
	return NewCatalog(manifests)
}

func (c *Catalog) Resolve(ctx context.Context, ref ReleaseRef, platform Platform) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	m, exists := c.releases[ref]
	if !exists || m.Platform != platform {
		return Manifest{}, &api.Error{Code: "RELEASE_NOT_APPROVED", Detail: "release reference or platform is not in the configured catalog"}
	}
	m.Components = slices.Clone(m.Components)
	return m, nil
}
