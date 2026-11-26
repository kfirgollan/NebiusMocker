package domain_test

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/services/dns/domain"
)

// fakeGate records the claims and runs apply, unless refuse is set.
type fakeGate struct {
	refuse error
	claims []peer.QuotaClaim
}

func (g *fakeGate) Admit(ctx context.Context, c peer.QuotaClaim, apply func(ctx context.Context) error) error {
	g.claims = append(g.claims, c)
	if g.refuse != nil {
		return g.refuse
	}
	if apply == nil {
		return nil
	}
	return apply(ctx)
}

func TestCreateZoneClaimsQuotaOfItsProject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		project string
		network string
	}{
		{"seeded project", projectID, networkID},
		{"other project", otherProjectID, foreignNetwork},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			done(t)(e.svc.CreateZone(t.Context(), domain.CreateZoneCmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: tt.project, Name: "z"}, Spec: zoneSpec("my.example.", tt.network),
			}))
			want := []peer.QuotaClaim{{ContainerID: tt.project, Amounts: map[string]int64{quota.DNSZoneCount: 1}}}
			if diff := cmp.Diff(want, e.gate.claims); diff != "" {
				t.Errorf("claims (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRefusedZoneCreateStoresNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.gate.refuse = errs.QuotaExceeded("limit reached")
	op, err := e.svc.CreateZone(t.Context(), domain.CreateZoneCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "z"}, Spec: zoneSpec("my.example.", networkID),
	})
	if err == nil {
		err = op.Err
	}
	wantKind(t, "CreateZone", err, errs.KindQuotaExceeded)
	if len(e.zones.items) != 0 || len(e.records.items) != 0 {
		t.Errorf("zones = %d, records = %d after a refused create, want none", len(e.zones.items), len(e.records.items))
	}
	if len(e.networks.bound) != 0 {
		t.Errorf("bound = %v after a refused create, want the binding released", e.networks.bound)
	}
}

func TestQuotaUsageCountsZonesPerProject(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	create := func(project, name, domainName, network string) string {
		return done(t)(e.svc.CreateZone(t.Context(), domain.CreateZoneCmd{
			Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: name}, Spec: zoneSpec(domainName, network),
		}))
	}
	if got, err := e.svc.QuotaUsage(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("empty usage = %v, %v, want none", got, err)
	}
	create(projectID, "a", "a.example.", networkID)
	create(projectID, "b", "b.example.", network2ID)
	other := create(otherProjectID, "c", "c.example.", foreignNetwork)
	want := []peer.QuotaUsage{
		{ContainerID: projectID, Name: quota.DNSZoneCount, Amount: 2},
		{ContainerID: otherProjectID, Name: quota.DNSZoneCount, Amount: 1},
	}
	got, err := e.svc.QuotaUsage(t.Context())
	if err != nil {
		t.Fatalf("QuotaUsage: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("usage (-want +got):\n%s", diff)
	}
	done(t)(e.svc.DeleteZone(t.Context(), other, 0, nil))
	got, err = e.svc.QuotaUsage(t.Context())
	if err != nil || len(got) != 1 || got[0].Amount != 2 {
		t.Errorf("usage after delete = %v, %v, want the seeded project's 2 zones only", got, err)
	}
}
