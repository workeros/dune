// Package peer provides the mutually authenticated HTTPS/WebSocket transport
// for one-hop Gateway connections. It does not grant any user permissions.
package peer

import (
	"bytes"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Config is loaded by the application from private deployment configuration.
// Certificate is a leaf key pair for this instance's advertised hostname, with
// both server and client authentication usages. Roots is the dedicated cluster
// trust set, never a user's browser or machine credential. Configuration changes
// use coordinated restart; roots may overlap during a CA/certificate rotation.
// The private key must support concurrent signing and must not be mutated.
type Config struct {
	Address     string
	Certificate tls.Certificate
	Roots       *x509.CertPool
}

type Transport struct {
	address     *url.URL
	certificate tls.Certificate
	roots       *x509.CertPool
	expires     time.Time
}

func address(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || u.Path == "" || path.Clean(u.Path) != u.Path || u.String() != value {
		return nil, fmt.Errorf("canonical HTTPS instance endpoint required")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if ip.IsUnspecified() || ip.IsMulticast() {
			return nil, fmt.Errorf("peer endpoint must name one instance")
		}
	} else if !validHostname(u.Hostname()) {
		return nil, fmt.Errorf("invalid peer hostname")
	}
	if port := u.Port(); port != "" {
		if number, err := strconv.ParseUint(port, 10, 16); err != nil || number == 0 {
			return nil, fmt.Errorf("invalid peer port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, fmt.Errorf("invalid peer port")
	}
	return u, nil
}

func validHostname(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// New validates and copies the immutable configuration before opening sockets.
// The certificate must verify for both peer usages and this instance's address.
func New(config Config) (*Transport, error) {
	u, err := address(config.Address)
	if err != nil {
		return nil, err
	}
	if config.Roots == nil || len(config.Certificate.Certificate) == 0 {
		return nil, fmt.Errorf("cluster roots and leaf certificate required")
	}
	cert := cloneCertificate(config.Certificate)
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("invalid peer certificate: %w", err)
	}
	key, ok := cert.PrivateKey.(crypto.Signer)
	if !ok || leaf.IsCA || leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return nil, fmt.Errorf("peer leaf signing key required")
	}
	public, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil || !bytes.Equal(public, leaf.RawSubjectPublicKeyInfo) {
		return nil, fmt.Errorf("peer certificate and private key differ")
	}
	roots := config.Roots.Clone()
	chain, err := parseChain(cert.Certificate)
	if err != nil {
		return nil, err
	}
	expires, err := verify(chain, roots, u.Hostname(), x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, fmt.Errorf("peer server identity: %w", err)
	}
	if _, err := verify(chain, roots, "", x509.ExtKeyUsageClientAuth); err != nil {
		return nil, fmt.Errorf("peer client identity: %w", err)
	}
	cert.Leaf = nil
	return &Transport{address: u, certificate: cert, roots: roots, expires: expires}, nil
}

// Address is the complete, directly reachable advertised endpoint.
func (t *Transport) Address() string { return t.address.String() }

// Ready reports whether this transport's own verified certificate chain is
// still within its validity period. It does not probe remote peers or listeners.
func (t *Transport) Ready() bool { return time.Now().Before(t.expires) }

// ServerTLSConfig is for a dedicated peer listener owned by the application.
// Public browser listeners must not require peer client certificates. The HTTP
// handler also verifies certificates independently, so forwarded TLS headers or
// a different listener trust configuration cannot bypass the peer trust set.
func (t *Transport) ServerTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cloneCertificate(t.certificate)}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: t.roots.Clone(), NextProtos: []string{"http/1.1"}}
}

func cloneCertificate(c tls.Certificate) tls.Certificate {
	c.Certificate = cloneBytes(c.Certificate)
	c.OCSPStaple = bytes.Clone(c.OCSPStaple)
	c.SignedCertificateTimestamps = cloneBytes(c.SignedCertificateTimestamps)
	c.Leaf = nil
	return c
}
func cloneBytes(values [][]byte) [][]byte {
	copy := make([][]byte, len(values))
	for i, value := range values {
		copy[i] = bytes.Clone(value)
	}
	return copy
}
func parseChain(values [][]byte) ([]*x509.Certificate, error) {
	if len(values) == 0 || len(values) > 8 {
		return nil, fmt.Errorf("bounded certificate chain required")
	}
	chain := make([]*x509.Certificate, len(values))
	for i, value := range values {
		cert, err := x509.ParseCertificate(value)
		if err != nil {
			return nil, err
		}
		chain[i] = cert
	}
	return chain, nil
}
func verify(chain []*x509.Certificate, roots *x509.CertPool, host string, usage x509.ExtKeyUsage) (time.Time, error) {
	if len(chain) == 0 || len(chain) > 8 {
		return time.Time{}, fmt.Errorf("peer certificate required")
	}
	if chain[0].IsCA || chain[0].KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return time.Time{}, fmt.Errorf("peer leaf signing certificate required")
	}
	intermediates := x509.NewCertPool()
	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}
	verified, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: host, KeyUsages: []x509.ExtKeyUsage{usage}})
	if err != nil {
		return time.Time{}, err
	}
	return chainExpiry(verified[0]), nil
}
func chainExpiry(chain []*x509.Certificate) time.Time {
	var expires time.Time
	for _, cert := range chain {
		if expires.IsZero() || cert.NotAfter.Before(expires) {
			expires = cert.NotAfter
		}
	}
	return expires
}
