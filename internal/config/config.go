package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"github.com/aiomni/dune/internal/wire"
	"gopkg.in/yaml.v3"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Gateway     string `yaml:"gateway"`
	Listen      string `yaml:"listen"`
	Token       string `yaml:"token"`
	Target      string `yaml:"target"`
	Certificate string `yaml:"certificate"`
	Key         string `yaml:"key"`
	LogLevel    string `yaml:"log_level"`
	// SessionDir stores machine configuration and identifies the private tmux server.
	SessionDir string `yaml:"session_dir,omitempty"`
}

func DefaultPath() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config/dune/config.yaml")
}
func Load(path string) (Config, error) {
	var c Config
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	d := yaml.NewDecoder(strings.NewReader(string(b)))
	d.KnownFields(true)
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	if c.SessionDir == "" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return c, err
		}
		c.SessionDir = filepath.Join(filepath.Dir(absolute), "sessions")
	}
	return c, c.Validate()
}
func gatewayURL(address string) (*url.URL, error) {
	u, e := url.Parse(address)
	if e != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Path != "/tunnel" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("gateway must be ws://HOST:PORT/tunnel (or wss://)")
	}
	if u.Port() != "" {
		port, e := strconv.Atoi(u.Port())
		if e != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("gateway port must be 1..65535")
		}
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsUnspecified() {
		return nil, fmt.Errorf("gateway must use a reachable address, not a wildcard")
	}
	return u, nil
}
func listenAddress(address string) error {
	host, port, e := net.SplitHostPort(address)
	if e != nil || net.ParseIP(host) == nil {
		return fmt.Errorf("listen must be IP:PORT, for example 0.0.0.0:7443")
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 1 || n > 65535 {
		return fmt.Errorf("listen port must be 1..65535")
	}
	return nil
}
func (c Config) Validate() error {
	_, e := gatewayURL(c.Gateway)
	if e != nil {
		return e
	}
	if c.Listen != "" {
		if e := listenAddress(c.Listen); e != nil {
			return e
		}
	}
	if len(c.Token) < 32 || c.Target == "" {
		return fmt.Errorf("token (>=32 characters) and target required")
	}
	if c.SessionDir != "" && !filepath.IsAbs(c.SessionDir) {
		return fmt.Errorf("session_dir must be absolute")
	}
	return nil
}
func (c Config) ValidateServer() error {
	if e := c.Validate(); e != nil {
		return e
	}
	if c.Listen == "" {
		return fmt.Errorf("Gateway requires listen")
	}
	if strings.HasPrefix(c.Gateway, "wss://") && (c.Key == "" || c.Certificate == "") {
		return fmt.Errorf("wss Gateway requires certificate and key")
	}
	return nil
}
func (c Config) TLS() (*tls.Config, error) {
	if strings.HasPrefix(c.Gateway, "ws://") {
		return nil, nil
	}
	if c.Certificate == "" {
		return &tls.Config{MinVersion: tls.VersionTLS12}, nil
	}
	b, e := os.ReadFile(c.Certificate)
	if e != nil {
		return nil, e
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("invalid trust certificate")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}
func Init(path, listen string) error { return InitWithGateway(path, listen, "") }
func InitWithGateway(path, listen, gateway string) error {
	if e := listenAddress(listen); e != nil {
		return e
	}
	if gateway == "" {
		host, _, _ := net.SplitHostPort(listen)
		if net.ParseIP(host).IsUnspecified() {
			return fmt.Errorf("wildcard listen requires --gateway ws://HOST:PORT/tunnel")
		}
		gateway = "ws://" + listen + "/tunnel"
	}
	endpoint, e := gatewayURL(gateway)
	if e != nil {
		return e
	}

	absolute, e := filepath.Abs(path)
	if e != nil {
		return e
	}
	path = absolute
	if _, e := os.Stat(path); e == nil {
		return fmt.Errorf("configuration already exists")
	}
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	if endpoint.Scheme == "ws" {
		return writeConfig(path, Config{Gateway: gateway, Listen: listen, Token: wire.ID() + wire.ID(), Target: "local", LogLevel: "info"})
	}
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return e
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if e != nil {
		return e
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Dune local"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}}
	if ip := net.ParseIP(endpoint.Hostname()); ip != nil {
		cert.IPAddresses = append(cert.IPAddresses, ip)
	} else {
		cert.DNSNames = append(cert.DNSNames, endpoint.Hostname())
	}
	der, e := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if e != nil {
		return e
	}
	kb, e := x509.MarshalECPrivateKey(key)
	if e != nil {
		return e
	}
	id := wire.ID()
	cp := filepath.Join(dir, "tls-"+id+".crt")
	kp := filepath.Join(dir, "tls-"+id+".key")
	c := Config{Gateway: gateway, Listen: listen, Token: wire.ID() + wire.ID(), Target: "local", Certificate: cp, Key: kp, LogLevel: "info"}
	if e = c.Validate(); e != nil {
		return e
	}
	for p, b := range map[string][]byte{cp: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), kp: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})} {
		if e = os.WriteFile(p, b, 0600); e != nil {
			return e
		}
	}
	return writeConfig(path, c)
}
func writeConfig(path string, c Config) error {
	b, e := yaml.Marshal(c)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	_, e = f.Write(b)
	return e
}

// Create writes a new machine configuration without overwriting an existing one.
func Create(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return writeConfig(path, c)
}
