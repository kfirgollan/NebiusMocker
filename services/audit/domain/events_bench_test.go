package domain_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/audit"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/services/audit/domain"
)

const (
	logSize     = 50_000
	benchFilter = `service.name='IAM' AND action:'DEL' AND regex(resource.metadata.name, '^grp-1.*')`
	maxListTime = 100 * time.Millisecond
)

// fillLog50k returns a full log of varied events, one per second from t0.
func fillLog50k(tb testing.TB) *audit.Log {
	tb.Helper()
	services := []string{"IAM", "COMPUTE", "VPC"}
	actions := []string{"CREATE", "UPDATE", "DELETE"}
	specs := make([]eventSpec, logSize)
	for i := range specs {
		name := fmt.Sprintf("vm-%d", i)
		if i%2 == 0 {
			name = fmt.Sprintf("grp-%d", i)
		}
		specs[i] = eventSpec{
			id:        fmt.Sprintf("event-%d", i),
			at:        t0.Add(time.Duration(i) * time.Second),
			service:   services[i%len(services)],
			action:    actions[(i/len(services))%len(actions)],
			resID:     fmt.Sprintf("res-%d", i),
			resName:   name,
			hierarchy: []string{tenantID, projectID},
		}
	}
	return newLog(tb, logSize, specs...)
}

func list50k(tb testing.TB) (*domain.EventService, domain.ListEventsQuery) {
	tb.Helper()
	svc, err := domain.NewEventService(fillLog50k(tb), newFakeHierarchy(), knownRegions)
	if err != nil {
		tb.Fatal(err)
	}
	return svc, domain.ListEventsQuery{
		Region:   region,
		ParentID: tenantID, Start: t0, End: t0.Add(24 * time.Hour), PageSize: 100, Filter: benchFilter,
	}
}

func BenchmarkList50k(b *testing.B) {
	svc, q := list50k(b)
	b.ResetTimer()
	for b.Loop() {
		got, _, err := svc.List(b.Context(), q)
		if err != nil || len(got) == 0 {
			b.Fatalf("List = %d events, %v", len(got), err)
		}
	}
}

func TestList50kIsFast(t *testing.T) {
	t.Parallel()
	svc, q := list50k(t)
	clk := clock.Real{}
	start := clk.Now()
	got, _, err := svc.List(t.Context(), q)
	elapsed := clk.Now().Sub(start)
	if err != nil || len(got) == 0 {
		t.Fatalf("List = %d events, %v", len(got), err)
	}
	if elapsed > maxListTime {
		t.Fatalf("List over %d events took %s, want < %s", logSize, elapsed, maxListTime)
	}
}
