package peer_test

import (
	"context"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

type denyGate struct{ claims []peer.QuotaClaim }

func (g *denyGate) Admit(_ context.Context, c peer.QuotaClaim, _ func(context.Context) error) error {
	g.claims = append(g.claims, c)
	return context.Canceled // any error: apply must not run
}

func TestAdmitQuotaWithoutGateRunsApply(t *testing.T) {
	t.Parallel()
	ran := false
	err := peer.AdmitQuota(t.Context(), peer.NewDirectory(), peer.QuotaClaim{}, func(context.Context) error {
		ran = true
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("AdmitQuota = %v, ran = %v", err, ran)
	}
	if err := peer.AdmitQuota(t.Context(), nil, peer.QuotaClaim{}, nil); err != nil {
		t.Fatalf("nil apply: %v", err)
	}
}

func TestAdmitQuotaUsesGate(t *testing.T) {
	t.Parallel()
	d := peer.NewDirectory()
	g := &denyGate{}
	if err := peer.Provide[peer.QuotaGate](d, peer.QuotaGateKey, g); err != nil {
		t.Fatal(err)
	}
	c := peer.QuotaClaim{ContainerID: "project-1", Amounts: map[string]int64{"vpc.network.count": 1}}
	err := peer.AdmitQuota(t.Context(), d, c, func(context.Context) error {
		t.Error("apply ran although the gate refused")
		return nil
	})
	if err == nil || len(g.claims) != 1 || g.claims[0].ContainerID != "project-1" {
		t.Fatalf("err = %v, claims = %v", err, g.claims)
	}
}

func TestQuotaUsageSources(t *testing.T) {
	t.Parallel()
	d := peer.NewDirectory()
	src := peer.QuotaFunc(func(context.Context) ([]peer.QuotaUsage, error) {
		return []peer.QuotaUsage{{ContainerID: "project-1", Name: "vpc.network.count", Amount: 2}}, nil
	})
	if err := peer.Provide[peer.QuotaUsageSource](d, peer.QuotaUsageKey("vpc"), src); err != nil {
		t.Fatal(err)
	}
	if err := peer.Provide[greeter](d, greeterKey, english{}); err != nil {
		t.Fatal(err)
	}
	got := peer.QuotaUsageSources(d)
	if len(got) != 1 || got["vpc"] == nil {
		t.Fatalf("sources = %v", got)
	}
	u, err := got["vpc"].QuotaUsage(t.Context())
	if err != nil || len(u) != 1 || u[0].Amount != 2 {
		t.Fatalf("usage = %v, %v", u, err)
	}
	if len(peer.QuotaUsageSources(nil)) != 0 {
		t.Fatal("nil directory has sources")
	}
}
