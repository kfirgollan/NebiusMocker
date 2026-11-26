// Package mock is the default mk8s backend (NEBIUS_MOCKER_MK8S_BACKEND=mock):
// no Kubernetes runs. Clusters get endpoints that resolve to nothing and a
// per-cluster self-signed CA, and every node is reported ready at once. It is
// enough for Terraform, the Go SDK and the CLI, which only manage clusters;
// use the k3s backend to talk to the Kubernetes API.
package mock

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

// Name is the NEBIUS_MOCKER_MK8S_BACKEND value of this backend.
const Name = "mock"

// EndpointDomain is the DNS suffix of mock cluster endpoints. Names under
// .localhost resolve to the loopback address, where nothing serves them.
const EndpointDomain = "mk8s.mocker.localhost"

// caValidity is how long a mock cluster CA is valid (real cluster CAs are
// long-lived too).
const caValidity = 10 * 365 * 24 * time.Hour

// Backend is the mock backend. It is safe for concurrent use.
type Backend struct {
	clk clock.Clock

	mu  sync.Mutex
	cas map[string]string // cluster ID → CA PEM, stable for the process
}

var _ domain.Backend = (*Backend)(nil)

// New returns a mock backend.
func New(clk clock.Clock) *Backend {
	return &Backend{clk: clk, cas: map[string]string{}}
}

// Name implements domain.Backend.
func (*Backend) Name() string { return Name }

// CheckVersion implements domain.Backend: every catalog version is accepted.
func (*Backend) CheckVersion(string) error { return nil }

// EnsureCluster implements domain.Backend.
func (b *Backend) EnsureCluster(_ context.Context, c domain.BackendCluster) (domain.ClusterEndpoint, error) {
	ca, err := b.ca(c.ID)
	if err != nil {
		return domain.ClusterEndpoint{}, err
	}
	ep := domain.ClusterEndpoint{
		PrivateEndpoint:   "https://" + c.ID + ".private." + EndpointDomain,
		CACertificate:     ca,
		KubernetesVersion: c.Version + ".0",
	}
	if c.PublicEndpoint {
		ep.PublicEndpoint = "https://" + c.ID + "." + EndpointDomain
	}
	return ep, nil
}

// DeleteCluster implements domain.Backend.
func (b *Backend) DeleteCluster(_ context.Context, clusterID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.cas, clusterID)
	return nil
}

// EnsureNodes implements domain.Backend: every node is ready.
func (*Backend) EnsureNodes(_ context.Context, g domain.BackendNodeGroup) ([]domain.NodeReadiness, error) {
	out := make([]domain.NodeReadiness, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		out = append(out, domain.NodeReadiness{Name: n.Name, Ready: true})
	}
	return out, nil
}

// Close implements domain.Backend.
func (*Backend) Close(context.Context) error { return nil }

// ca returns the cluster's CA, creating it on first use.
func (b *Backend) ca(clusterID string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if pemCA, ok := b.cas[clusterID]; ok {
		return pemCA, nil
	}
	pemCA, err := newCA(clusterID, b.clk.Now())
	if err != nil {
		return "", fmt.Errorf("mk8s mock backend: cluster CA: %w", err)
	}
	b.cas[clusterID] = pemCA
	return pemCA, nil
}

func newCA(clusterID string, now time.Time) (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "kubernetes", OrganizationalUnit: []string{clusterID}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}
