// Package testca mints a throwaway certificate authority and one server
// certificate for tests that run NebiusMocker with TLS
// (NEBIUS_MOCKER_TLS_CERT_FILE / _KEY_FILE), the way a user would with their
// own local CA.
package testca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

// CA is a CA plus a server certificate it signed, written as PEM files.
type CA struct {
	// Dir holds ca.crt, tls.crt and tls.key.
	Dir string
	// CAFile is the CA certificate clients trust.
	CAFile string
	// CertFile and KeyFile are the server certificate and key.
	CertFile, KeyFile string
	// Pool trusts the CA.
	Pool *x509.CertPool
}

// New creates the CA and a server certificate valid for dnsNames and ips, in
// a new temp dir. Files are world-readable so containers can mount them.
func New(tb testing.TB, dnsNames []string, ips []net.IP) *CA {
	tb.Helper()
	now := clock.Real{}.Now()
	caKey := newKey(tb)
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(tb),
		Subject:               pkix.Name{CommonName: "NebiusMocker test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		tb.Fatalf("testca: CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		tb.Fatalf("testca: parse CA: %v", err)
	}
	key := newKey(tb)
	leaf := &x509.Certificate{
		SerialNumber: serial(tb),
		Subject:      pkix.Name{CommonName: "nebius-mocker"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		DNSNames:     dnsNames,
		IPAddresses:  ips,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		tb.Fatalf("testca: server cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		tb.Fatalf("testca: key: %v", err)
	}
	dir := tb.TempDir()
	c := &CA{
		Dir: dir, CAFile: filepath.Join(dir, "ca.crt"),
		CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"),
		Pool: x509.NewCertPool(),
	}
	c.Pool.AddCert(caCert)
	writePEM(tb, c.CAFile, "CERTIFICATE", caDER)
	writePEM(tb, c.CertFile, "CERTIFICATE", leafDER)
	writePEM(tb, c.KeyFile, "EC PRIVATE KEY", keyDER)
	return c
}

func newKey(tb testing.TB) *ecdsa.PrivateKey {
	tb.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatalf("testca: key: %v", err)
	}
	return k
}

func serial(tb testing.TB) *big.Int {
	tb.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		tb.Fatalf("testca: serial: %v", err)
	}
	return n
}

func writePEM(tb testing.TB, path, typ string, der []byte) {
	tb.Helper()
	// 0644: containers running as another user mount these test-only files.
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o644); err != nil { //nolint:gosec // throwaway test certificates
		tb.Fatalf("testca: write %s: %v", path, err)
	}
}
