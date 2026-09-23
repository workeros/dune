package upgrade

import (
	"strings"
	"testing"
)

func testManifest() Manifest {
	m := Manifest{ID: "release", Platform: Platform{OS: "darwin", Arch: "arm64"}, ArchiveURL: "https://releases.example/dune.tar.gz", ArchiveSHA256: strings.Repeat("a", 64), StateContract: strings.Repeat("b", 64)}
	for _, path := range []string{"dune", "tmux", "rg", "licenses/NOTICE"} {
		mode := uint32(0700)
		if strings.HasPrefix(path, "licenses/") {
			mode = 0600
		}
		m.Components = append(m.Components, Component{Path: path, SHA256: strings.Repeat("c", 64), Bytes: 42, Mode: mode})
	}
	return m
}

func TestManifestDigestFreezesEveryComponentAndIgnoresListOrder(t *testing.T) {
	m := testManifest()
	digest, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	ref := ReleaseRef{ID: m.ID, ManifestSHA256: digest}
	if !m.Matches(ref) {
		t.Fatal("manifest did not match own digest")
	}
	m.Components[0], m.Components[3] = m.Components[3], m.Components[0]
	if !m.Matches(ref) {
		t.Fatal("component list order changed release identity")
	}
	m.Components[0].SHA256 = strings.Repeat("d", 64)
	if m.Matches(ref) {
		t.Fatal("license change retained old manifest identity")
	}
	m.Components[0].Path = "licenses/../dune"
	if _, err := m.Digest(); err == nil {
		t.Fatal("path escape has a release identity")
	}
}
