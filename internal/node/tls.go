// Package node carries the coordinator-to-node link.
//
// A node establishes an outbound connection to the coordinator and keeps it
// open. A single persistent bidirectional channel multiplexes heartbeats,
// execution commands, events, permission responses, and reconnect
// synchronization.
//
// The coordinator owns logical state; the node owns the live execution.
package node

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// CertificateFile and KeyFile are the node link certificate names inside the
// data directory.
const (
	CertificateFile = "node-cert.pem"
	KeyFile         = "node-key.pem"
)

// LoadOrCreateCertificate returns the node link certificate, generating a
// self-signed one on first use.
//
// The loopback link uses TLS like any other, so the one-machine path is not a
// special case that can rot while the multi-machine path is the only one
// exercised.
func LoadOrCreateCertificate(dir string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, CertificateFile)
	keyPath := filepath.Join(dir, KeyFile)

	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return cert, nil
	}

	cert, certPEM, keyPEM, err := generateCertificate()
	if err != nil {
		return tls.Certificate{}, err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("node: create %s: %w", dir, err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("node: write certificate: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("node: write key: %w", err)
	}
	return cert, nil
}

func generateCertificate() (tls.Certificate, []byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("node: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("node: generate serial: %w", err)
	}

	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "hive-node"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("node: create certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("node: marshal key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("node: load generated pair: %w", err)
	}
	return cert, certPEM, keyPEM, nil
}

// ClientTLSConfig trusts the local node certificate.
//
// v1 uses one locally generated certificate for the whole cluster, so the node
// link is encrypted and authenticated against that certificate rather than
// against a public CA. Node identity is a separate concern: the coordinator
// records the node identity from the handshake and authorizes it explicitly.
func ClientTLSConfig(cert tls.Certificate) *tls.Config {
	pool := x509.NewCertPool()
	if len(cert.Certificate) > 0 {
		if parsed, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
			pool.AddCert(parsed)
		}
	}
	return &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}
}

// ServerTLSConfig serves the node link with the local certificate.
func ServerTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}
