package domain_test

import (
	"context"
	"maps"
	"slices"
	"sync"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	resourcepb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1/resource"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
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

func TestQuotaAmounts(t *testing.T) {
	t.Parallel()
	spec := validSpec()
	spec.Config.Template.Hosts.Count = 3
	want := map[string]int64{
		quota.MSPPostgreSQLClusterCount: 1,
		quota.MSPPostgreSQLVCPU:         3 * 2,
		quota.MSPPostgreSQLMemory:       3 * 8 * gib,
		quota.MSPPostgreSQLDiskSize:     3 * 20 * gib,
	}
	if got := domain.QuotaAmounts(spec); !maps.Equal(got, want) {
		t.Errorf("QuotaAmounts = %v, want %v", got, want)
	}
}

func TestCreateAndRestoreChargeQuota(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", nil)
	claims := e.quota.all()
	if len(claims) != 1 || claims[0].ContainerID != projectID || !maps.Equal(claims[0].Amounts, domain.QuotaAmounts(validSpec())) {
		t.Fatalf("create claims = %+v", claims)
	}

	backupID := e.backupsOf(t, c.GetMetadata().GetId())[0].Backup.GetId()
	done(t)(e.svc.RestoreCluster(t.Context(), domain.RestoreClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "restored"}, Spec: validSpec(),
		SourceClusterID: c.GetMetadata().GetId(), BackupID: backupID,
	}))
	if claims := e.quota.all(); len(claims) != 2 || claims[1].Amounts[quota.MSPPostgreSQLClusterCount] != 1 {
		t.Fatalf("restore claims = %+v, want a second cluster charged", claims)
	}

	// Over the limit: nothing is stored and the network is released.
	e.quota.set(quota.MSPPostgreSQLClusterCount)
	_, err := e.svc.CreateCluster(t.Context(), domain.CreateClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "refused"}, Spec: validSpec(),
	})
	wantKind(t, "create over quota", err, errs.KindQuotaExceeded)
	if _, err := e.svc.GetClusterByName(t.Context(), projectID, "refused"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("refused cluster stored: %v", err)
	}
	if page, _ := e.svc.ListClusters(t.Context(), projectID, 0, ""); len(page.Items) != 2 {
		t.Errorf("clusters after a refused create = %d, want 2", len(page.Items))
	}
	e.vpc.mu.Lock()
	bound := len(e.vpc.bound)
	e.vpc.mu.Unlock()
	if bound != 2 {
		t.Errorf("network bindings after a refused create = %d, want the 2 clusters'", bound)
	}
}

func TestUpdateChargesGrowthOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.create(t, "main", nil).GetMetadata().GetId()
	grow := func(preset string, hosts, disk int64) error {
		return errOf(e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{
			Metadata: &commonpb.ResourceMetadata{Id: id},
			Spec: &pgpb.ClusterSpec{Config: &pgpb.ConfigSpec{Template: &pgpb.TemplateSpec{
				Resources: &resourcepb.ResourcesSpec{Platform: "cpu-e2", Preset: preset},
				Hosts:     &resourcepb.HostSpec{Count: hosts},
				Disk:      &resourcepb.DiskSpec{Type: "network-ssd", SizeGibibytes: disk},
			}}},
		}))
	}

	// 1×(2 vCPU, 8 GiB, 20 GiB) → 3×(4 vCPU, 16 GiB, 50 GiB): the delta.
	if err := grow("4vcpu-16gb", 3, 50); err != nil {
		t.Fatalf("grow: %v", err)
	}
	claims := e.quota.all()
	want := map[string]int64{
		quota.MSPPostgreSQLClusterCount: 0,
		quota.MSPPostgreSQLVCPU:         12 - 2,
		quota.MSPPostgreSQLMemory:       (48 - 8) * gib,
		quota.MSPPostgreSQLDiskSize:     (150 - 20) * gib,
	}
	if len(claims) != 2 || !maps.Equal(claims[1].Amounts, want) {
		t.Fatalf("update claims = %+v, want %v", claims, want)
	}

	// A larger preset than the vCPU quota holds: refused, the spec stays.
	e.quota.set(quota.MSPPostgreSQLVCPU)
	wantKind(t, "grow over quota", grow("8vcpu-32gb", 3, 50), errs.KindQuotaExceeded)
	if got := e.get(t, id).GetSpec().GetConfig().GetTemplate(); got.GetHosts().GetCount() != 3 || got.GetResources().GetPreset() != "4vcpu-16gb" {
		t.Errorf("template after a refused update = %v", got)
	}
	// A shrink frees vCPUs and always passes, even with the quota exhausted.
	if err := grow("2vcpu-8gb", 3, 50); err != nil {
		t.Fatalf("shrink: %v", err)
	}
	// A label change claims nothing.
	n := len(e.quota.all())
	done(t)(e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Labels: map[string]string{"env": "dev"}},
	}))
	if len(e.quota.all()) != n {
		t.Error("a label change went through the quota gate")
	}
}
