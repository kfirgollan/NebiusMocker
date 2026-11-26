package mock

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

func TestEnsureCluster(t *testing.T) {
	t.Parallel()
	b := New(clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	ctx := t.Context()
	c := domain.BackendCluster{ID: "mk8scluster-e00a", Version: "1.35", PublicEndpoint: true}
	ep, err := b.EnsureCluster(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if ep.PublicEndpoint != "https://mk8scluster-e00a.mk8s.mocker.localhost" ||
		ep.PrivateEndpoint != "https://mk8scluster-e00a.private.mk8s.mocker.localhost" || ep.KubernetesVersion != "1.35.0" {
		t.Fatalf("endpoint = %+v", ep)
	}
	block, _ := pem.Decode([]byte(ep.CACertificate))
	if block == nil {
		t.Fatal("CA is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !cert.IsCA {
		t.Fatalf("CA: %v, IsCA=%v", err, cert != nil && cert.IsCA)
	}

	c.PublicEndpoint = false
	again, err := b.EnsureCluster(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if again.CACertificate != ep.CACertificate || again.PublicEndpoint != "" {
		t.Fatalf("reconcile changed the CA or kept the public endpoint: %+v", again)
	}
	if err := b.DeleteCluster(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteCluster(ctx, c.ID); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestEnsureNodesAllReady(t *testing.T) {
	t.Parallel()
	b := New(clock.NewManual(time.Unix(0, 0)))
	got, err := b.EnsureNodes(t.Context(), domain.BackendNodeGroup{Nodes: []domain.BackendNode{{Name: "a"}, {Name: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Ready || !got[1].Ready || got[1].Name != "b" {
		t.Fatalf("readiness = %+v", got)
	}
}
