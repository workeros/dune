package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func TestCatalogFreezesCompleteAllowedReferences(t *testing.T) {
	manifest := testManifest()
	catalog, err := NewCatalog([]Manifest{manifest})
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := manifest.Digest()
	ref := ReleaseRef{ID: manifest.ID, ManifestSHA256: digest}
	first, err := catalog.Resolve(t.Context(), ref, manifest.Platform)
	if err != nil {
		t.Fatal(err)
	}
	first.Components[0].SHA256 = "changed"
	manifest.Components[0].SHA256 = "changed"
	second, err := catalog.Resolve(t.Context(), ref, manifest.Platform)
	if err != nil || !second.Matches(ref) {
		t.Fatal("catalog mutated through caller", err)
	}
	wrong := ref
	wrong.ManifestSHA256 = "unknown"
	if _, err := catalog.Resolve(t.Context(), wrong, manifest.Platform); err == nil {
		t.Fatal("unknown ref accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := catalog.Resolve(ctx, ref, manifest.Platform); err == nil {
		t.Fatal("cancelled lookup accepted")
	}
	body, _ := json.Marshal([]Manifest{second})
	if _, err := ReadCatalog(bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCatalog([]Manifest{second, second}); err == nil {
		t.Fatal("ambiguous release identity accepted")
	}
}
