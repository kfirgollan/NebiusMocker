package domain_test

import (
	"context"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

const gib = 1 << 30

// fakeQuota is a quota gate that records every claim and refuses the ones
// that add to the quota named refuse (none by default).
type fakeQuota struct {
	mu     sync.Mutex
	refuse string
	claims []peer.QuotaClaim
}

func (q *fakeQuota) Admit(ctx context.Context, c peer.QuotaClaim, apply func(context.Context) error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.claims = append(q.claims, peer.QuotaClaim{ContainerID: c.ContainerID, Region: c.Region, Amounts: maps.Clone(c.Amounts)})
	if q.refuse != "" && c.Amounts[q.refuse] > 0 {
		return errs.QuotaExceeded("quota exhausted", errs.QuotaViolation{Quota: q.refuse})
	}
	return apply(ctx)
}

func (q *fakeQuota) set(refuse string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.refuse = refuse
}

func (q *fakeQuota) all() []peer.QuotaClaim {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.claims)
}

func TestCreateChargesQuota(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	spec := validSpec()
	spec.Size = "large"
	c := e.create(t, "main", spec)
	want := map[string]int64{quota.MSPMLflowClusterCount: 1, quota.MSPMLflowVCPU: 16, quota.MSPMLflowMemory: 64 * gib}
	claims := e.quota.all()
	if len(claims) != 1 || claims[0].ContainerID != projectID || !maps.Equal(claims[0].Amounts, want) {
		t.Fatalf("claims = %+v, want %v", claims, want)
	}
	if got := e.svc.QuotaAmounts(c); !maps.Equal(got, want) {
		t.Errorf("QuotaAmounts(stored) = %v, want %v", got, want)
	}
	// The default size is charged too.
	if got := e.svc.QuotaAmounts(e.create(t, "default-size", validSpec())); got[quota.MSPMLflowVCPU] != 8 || got[quota.MSPMLflowMemory] != 32*gib {
		t.Errorf("default size amounts = %v", got)
	}

	// Over the limit: nothing is stored, the network and internals are released.
	e.quota.set(quota.MSPMLflowMemory)
	_, err := e.svc.Create(t.Context(), createCmd("refused", validSpec()))
	if errs.KindOf(err) != errs.KindQuotaExceeded {
		t.Fatalf("create over quota: %v, want quota_exceeded", err)
	}
	if _, err := e.svc.GetByName(t.Context(), projectID, "refused"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("refused cluster stored: %v", err)
	}
	e.peers.networks.mu.Lock()
	bound := len(e.peers.networks.bound)
	e.peers.networks.mu.Unlock()
	if bound != 2 {
		t.Errorf("network bindings after a refused create = %d, want the 2 clusters'", bound)
	}
}
