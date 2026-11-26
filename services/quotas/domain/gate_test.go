package domain

import (
	"context"
	"errors"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

const instances = quota.ComputeInstanceCount

// setLimit sets the allowance of name in region for a container.
func setLimit(t *testing.T, f *fixture, container, region string, limit uint64) {
	t.Helper()
	a, err := f.svc.GetByName(t.Context(), container, instances, region)
	if err != nil {
		t.Fatalf("GetByName(%s): %v", container, err)
	}
	if _, err := f.svc.Update(t.Context(), updateCmd(a.GetMetadata().GetId(), 0, "", proto.Uint64(limit), mask.New())); err != nil {
		t.Fatalf("Update(%s): %v", container, err)
	}
}

func claim(container string, delta int64) peer.QuotaClaim {
	return peer.QuotaClaim{ContainerID: container, Amounts: map[string]int64{instances: delta}}
}

func TestAdmitWithinLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	setLimit(t, f, project1, "", 3)
	f.usage(peer.QuotaUsage{ContainerID: project1, Name: instances, Amount: 2})
	ran := false
	if err := f.svc.Admit(t.Context(), claim(project1, 1), func(context.Context) error { ran = true; return nil }); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !ran {
		t.Error("apply did not run for a claim within the limit")
	}
}

func TestAdmitApplyErrorPropagates(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	want := errs.Internal("boom")
	if err := f.svc.Admit(t.Context(), claim(project1, 1), func(context.Context) error { return want }); !errors.Is(err, want) {
		t.Errorf("Admit = %v, want the apply error", err)
	}
}

func quotaViolations(t *testing.T, err error) []errs.QuotaViolation {
	t.Helper()
	if !errs.IsKind(err, errs.KindQuotaExceeded) {
		t.Fatalf("err = %v, want KindQuotaExceeded", err)
	}
	e, ok := errors.AsType[*errs.Error](err)
	if !ok {
		t.Fatalf("err %v is not an *errs.Error", err)
	}
	return e.QuotaViolations
}

func TestAdmitProjectExceeded(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	setLimit(t, f, project1, "", 3)
	f.usage(peer.QuotaUsage{ContainerID: project1, Name: instances, Amount: 2})
	ran := false
	err := f.svc.Admit(t.Context(), claim(project1, 2), func(context.Context) error { ran = true; return nil })
	vs := quotaViolations(t, err)
	if ran {
		t.Error("apply ran although the quota is exceeded")
	}
	if len(vs) != 1 {
		t.Fatalf("violations = %v, want one", vs)
	}
	if v := vs[0]; v.Quota != instances || v.Limit != "3" || v.Requested != "2" || v.Region != regionEU || v.Message == "" {
		t.Errorf("violation = %+v", v)
	}
}

func TestAdmitTenantExceeded(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	setLimit(t, f, tenantID, regionEU, 4)
	f.usage(
		peer.QuotaUsage{ContainerID: project1, Name: instances, Amount: 3},
		peer.QuotaUsage{ContainerID: project2, Name: instances, Amount: 1},
		peer.QuotaUsage{ContainerID: projectUS, Name: instances, Amount: 100}, // another region
	)
	ran := false
	err := f.svc.Admit(t.Context(), claim(project1, 1), func(context.Context) error { ran = true; return nil })
	vs := quotaViolations(t, err)
	if ran {
		t.Error("apply ran although the tenant quota is exceeded")
	}
	if len(vs) != 1 || vs[0].Limit != "4" || vs[0].Requested != "1" || vs[0].Region != regionEU {
		t.Fatalf("violations = %+v, want the tenant limit 4 only", vs)
	}
	// One unit less fits.
	if err := f.svc.Admit(t.Context(), claim(project1, 0), nil); err != nil {
		t.Errorf("zero claim: %v", err)
	}
	f.usage(peer.QuotaUsage{ContainerID: project1, Name: instances, Amount: 2},
		peer.QuotaUsage{ContainerID: project2, Name: instances, Amount: 1})
	if err := f.svc.Admit(t.Context(), claim(project1, 1), nil); err != nil {
		t.Errorf("claim reaching the tenant limit exactly: %v", err)
	}
}

func TestAdmitExplicitRegionAndBothLevels(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	setLimit(t, f, projectUS, "", 1)
	setLimit(t, f, tenantID, regionUS, 1)
	f.usage(peer.QuotaUsage{ContainerID: projectUS, Region: regionUS, Name: instances, Amount: 1})
	c := peer.QuotaClaim{ContainerID: projectUS, Region: regionUS, Amounts: map[string]int64{instances: 1}}
	vs := quotaViolations(t, f.svc.Admit(t.Context(), c, nil))
	if len(vs) != 2 {
		t.Errorf("violations = %+v, want project and tenant", vs)
	}
}

func TestAdmitNonPositivePasses(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	setLimit(t, f, project1, "", 0)
	f.usage(peer.QuotaUsage{ContainerID: project1, Name: instances, Amount: 5})
	for _, delta := range []int64{0, -1, -100} {
		ran := false
		if err := f.svc.Admit(t.Context(), claim(project1, delta), func(context.Context) error { ran = true; return nil }); err != nil || !ran {
			t.Errorf("delta %d: err = %v, ran = %v; want pass", delta, err, ran)
		}
	}
	empty := peer.QuotaClaim{ContainerID: project1}
	if err := f.svc.Admit(t.Context(), empty, nil); err != nil {
		t.Errorf("empty claim: %v", err)
	}
}

func TestAdmitNilApplyOnlyChecks(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	setLimit(t, f, project1, "", 1)
	if err := f.svc.Admit(t.Context(), claim(project1, 1), nil); err != nil {
		t.Errorf("fitting claim: %v", err)
	}
	if err := f.svc.Admit(t.Context(), claim(project1, 2), nil); !errs.IsKind(err, errs.KindQuotaExceeded) {
		t.Errorf("exceeding claim: %v, want QuotaExceeded", err)
	}
}

func TestAdmitErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	bogus := peer.QuotaClaim{ContainerID: project1, Amounts: map[string]int64{"bogus.quota": 1}}
	if err := f.svc.Admit(t.Context(), bogus, nil); !errs.IsKind(err, errs.KindInternal) {
		t.Errorf("unknown quota: %v, want Internal", err)
	}
	if err := f.svc.Admit(t.Context(), claim("project-e00nope", 1), nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("missing project: %v, want NotFound", err)
	}
	f.sources["compute"] = peer.QuotaFunc(func(context.Context) ([]peer.QuotaUsage, error) { return nil, errs.Internal("down") })
	if err := f.svc.Admit(t.Context(), claim(project1, 1), nil); !errs.IsKind(err, errs.KindInternal) {
		t.Errorf("failing usage source: %v, want Internal", err)
	}
}

// counter is a usage source whose consumption the apply func increments.
type counter struct {
	mu sync.Mutex
	n  uint64
}

func (c *counter) QuotaUsage(context.Context) ([]peer.QuotaUsage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return []peer.QuotaUsage{{ContainerID: project1, Name: instances, Amount: c.n}}, nil
}

func (c *counter) inc(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return nil
}

func (c *counter) value() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func TestAdmitConcurrentClaimsRespectLimit(t *testing.T) {
	t.Parallel()
	const limit, workers = 5, 50
	f := newFixture(t, map[string]uint64{instances: limit})
	c := &counter{}
	f.sources["compute"] = c

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		failures  []error
	)
	for range workers {
		wg.Go(func() {
			err := f.svc.Admit(t.Context(), claim(project1, 1), c.inc)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				succeeded++
				return
			}
			failures = append(failures, err)
		})
	}
	wg.Wait()
	if succeeded != limit || c.value() != limit {
		t.Errorf("succeeded = %d, counter = %d; want %d each", succeeded, c.value(), limit)
	}
	for _, err := range failures {
		if !errs.IsKind(err, errs.KindQuotaExceeded) {
			t.Errorf("failure %v, want QuotaExceeded", err)
		}
	}
}
