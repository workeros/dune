package config

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/aiomni/dune/internal/testcert"
	"github.com/aiomni/dune/internal/wire"
)

func TestPrivateClusterConfiguration(t *testing.T) {
	dir := t.TempDir()
	ca := testcert.New(t)
	certificate := ca.Issue(t, "127.0.0.1", nil)
	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"cert.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}),
		"key.pem":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
		"ca.pem":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Certificate.Raw}),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(dir, "cluster.yaml")
	valid := "recovery_generation: " + wire.ID() + "\npeer:\n  listen: 127.0.0.1:9443\n  address: https://127.0.0.1:9443/private/peer\n  certificate: cert.pem\n  key: key.pem\n  ca: ca.pem\n"
	write := func(source string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(valid)
	value, err := Cluster(file)
	if err != nil || value.Listen != "127.0.0.1:9443" {
		t.Fatal("valid relative-path configuration rejected", err)
	}
	for _, source := range []string{
		strings.Replace(valid, "listen: 127.0.0.1:9443", "listen: example.test:9443", 1),
		strings.Replace(valid, "https://127.0.0.1:9443", "http://127.0.0.1:9443", 1),
		strings.Replace(valid, "https://127.0.0.1:9443", "https://127.0.0.2:9443", 1),
		strings.Replace(valid, "recovery_generation:", "unknown:", 1),
		valid + "---\nprivate: private-password\n",
	} {
		write(source)
		if _, err := Cluster(file); err == nil || strings.Contains(err.Error(), "private-password") {
			t.Fatal("invalid configuration accepted or disclosed content", err)
		}
	}
	write(valid)
	if err := os.Chmod(filepath.Join(dir, "key.pem"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Cluster(file); err == nil {
		t.Fatal("public private key accepted")
	}
	if err := os.Chmod(filepath.Join(dir, "key.pem"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("key.pem", filepath.Join(dir, "linked.pem")); err != nil {
		t.Fatal(err)
	}
	write(strings.Replace(valid, "key: key.pem", "key: linked.pem", 1))
	if _, err := Cluster(file); err == nil {
		t.Fatal("symlink key accepted")
	}
	write(valid)
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Cluster(file); err == nil {
		t.Fatal("shared cluster configuration accepted")
	}
}

func TestPeerPEMRejectsPipeWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := peerPEM(path, true); err == nil {
		t.Fatal("pipe accepted as private key")
	}
}
