// Package localca keeps a local certificate authority for NebiusMocker's TLS
// listeners (e.g. the AI endpoint data plane), so users can trust the mock's
// https:// URLs the way they would trust a real cloud's: once, with
// `curl --cacert <dir>/ca.pem` or their OS trust store.
//
// The CA lives in a directory and survives restarts:
//
//	<dir>/ca.pem      the CA certificate (give this to clients)
//	<dir>/ca-key.pem  the CA certificate and its private key (mocker only)
//
// Leaf certificates are issued in memory on every start.
package localca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

const (
	// CertFile is the CA certificate clients trust.
	CertFile = "ca.pem"
	// keyFile holds the CA certificate and key; only the mocker reads it.
	keyFile = "ca-key.pem"

	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 365 * 24 * time.Hour
	// backdate tolerates small clock skew between the mock and its clients.
	backdate = time.Hour
)

// CA is a loaded certificate authority.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	clk     clock.Clock
}

// LoadOrCreate loads the CA stored in dir, creating it (and dir) on first use.
// Concurrent processes sharing dir agree on one CA: the key file is created
// exclusively, and a loser of that race loads the winner's.
func LoadOrCreate(dir string, clk clock.Clock) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("localca: %w", err)
	}
	keyPath := filepath.Join(dir, keyFile)
	bundle, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		bundle, err = create(dir, keyPath, clk)
	}
	if err != nil {
		return nil, fmt.Errorf("localca: %w", err)
	}
	ca, err := parse(bundle, clk)
	if err != nil {
		return nil, fmt.Errorf("localca: %s: %w", keyPath, err)
	}
	// ca.pem is derived; every process writes the same bytes atomically.
	if err := writeAtomic(filepath.Join(dir, CertFile), ca.certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("localca: %w", err)
	}
	return ca, nil
}

// CertPEM returns the CA certificate in PEM form.
func (ca *CA) CertPEM() []byte { return append([]byte(nil), ca.certPEM...) }

// Pool returns a certificate pool holding the CA.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// Issue returns a server certificate for hosts (DNS names, wildcards such as
// "*.ai.localhost", or IP addresses), signed by the CA.
func (ca *CA) Issue(hosts ...string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("localca: leaf key: %w", err)
	}
	now := ca.clk.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: hosts[0], Organization: []string{"NebiusMocker"}},
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("localca: issue: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key}, nil
}

func create(dir, keyPath string, clk clock.Clock) ([]byte, error) {
	bundle, err := newBundle(clk)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".ca-key-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // the link below keeps the data
	if _, err := tmp.Write(bundle); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	// Link fails if keyPath exists: exactly one process creates the CA.
	if err := os.Link(tmp.Name(), keyPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ReadFile(keyPath)
		}
		return nil, err
	}
	return bundle, nil
}

func newBundle(clk clock.Clock) ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := clk.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "NebiusMocker local CA", Organization: []string{"NebiusMocker"}},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})...), nil
}

func parse(bundle []byte, clk clock.Clock) (*CA, error) {
	ca := &CA{clk: clk}
	for rest := bundle; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		switch b.Type {
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return nil, err
			}
			ca.cert, ca.certPEM = c, pem.EncodeToMemory(b)
		case "EC PRIVATE KEY":
			k, err := x509.ParseECPrivateKey(b.Bytes)
			if err != nil {
				return nil, err
			}
			ca.key = k
		}
	}
	if ca.cert == nil || ca.key == nil {
		return nil, errors.New("CA file must hold a certificate and an EC private key")
	}
	return ca, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return big.NewInt(1) // crypto/rand does not fail on supported platforms
	}
	return n
}
