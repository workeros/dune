package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/transport/peer"
)

// ClusterConfig is the official application's private listener configuration.
// Go hosts supply host.ClusterOptions and own their listeners directly.
type ClusterConfig struct {
	Listen             string
	RecoveryGeneration string
	Peer               peer.Config
}

func Cluster(path string) (ClusterConfig, error) {
	var value struct {
		RecoveryGeneration string `yaml:"recovery_generation"`
		Peer               struct {
			Listen      string `yaml:"listen"`
			Address     string `yaml:"address"`
			Certificate string `yaml:"certificate"`
			Key         string `yaml:"key"`
			CA          string `yaml:"ca"`
		} `yaml:"peer"`
	}
	if err := privateYAML(path, "cluster", &value); err != nil {
		return ClusterConfig{}, err
	}
	if !wire.ValidID(value.RecoveryGeneration) {
		return ClusterConfig{}, fmt.Errorf("cluster requires a valid recovery generation")
	}
	if err := listenAddress(value.Peer.Listen); err != nil {
		return ClusterConfig{}, fmt.Errorf("invalid peer listener: %w", err)
	}
	read := func(name string, private bool) ([]byte, error) {
		if name == "" {
			return nil, fmt.Errorf("peer certificate, key and CA files required")
		}
		if !filepath.IsAbs(name) {
			name = filepath.Join(filepath.Dir(path), name)
		}
		return peerPEM(name, private)
	}
	certPEM, err := read(value.Peer.Certificate, false)
	if err != nil {
		return ClusterConfig{}, err
	}
	keyPEM, err := read(value.Peer.Key, true)
	if err != nil {
		return ClusterConfig{}, err
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return ClusterConfig{}, fmt.Errorf("invalid peer certificate or private key")
	}
	caPEM, err := read(value.Peer.CA, false)
	if err != nil {
		return ClusterConfig{}, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return ClusterConfig{}, fmt.Errorf("invalid peer CA file")
	}
	result := ClusterConfig{Listen: value.Peer.Listen, RecoveryGeneration: value.RecoveryGeneration, Peer: peer.Config{Address: value.Peer.Address, Certificate: certificate, Roots: roots}}
	if _, err := peer.New(result.Peer); err != nil {
		return ClusterConfig{}, err
	}
	return result, nil
}

func peerPEM(path string, private bool) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot open peer certificate material: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Size() > 64*1024 || (private && (info.Mode().Perm()&0077 != 0 || int(stat.Uid) != os.Getuid())) {
		return nil, fmt.Errorf("peer PEM must be a regular file of at most 64 KiB; private keys must be owned and private")
	}
	data, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 64*1024 {
		return nil, fmt.Errorf("peer PEM exceeds 64 KiB")
	}
	return data, nil
}
