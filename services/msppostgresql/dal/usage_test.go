package dal_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	resourcepb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1/resource"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/dal"
)

func sized(id, parent, preset string, hosts, diskGiB int64) *pgpb.Cluster {
	c := cluster(id, parent, id)
	c.Spec.Config.Template = &pgpb.TemplateSpec{
		Resources: &resourcepb.ResourcesSpec{Platform: "cpu-e2", Preset: preset},
		Hosts:     &resourcepb.HostSpec{Count: hosts},
		Disk:      &resourcepb.DiskSpec{Type: "network-ssd", SizeGibibytes: diskGiB},
	}
	return c
}

func TestQuotaUsageSumsClustersPerProject(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	clusters := dal.NewClusterRepo(st)
	for _, c := range []*pgpb.Cluster{
		sized("postgresql-1", project, "2vcpu-8gb", 1, 20),
		sized("postgresql-2", project, "4vcpu-16gb", 3, 50),
		sized("postgresql-3", project2, "2vcpu-8gb", 2, 10),
	} {
		if err := clusters.Create(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := dal.NewQuotaUsage(st).QuotaUsage(t.Context())
	if err != nil {
		t.Fatalf("QuotaUsage: %v", err)
	}
	const gib = 1 << 30
	want := []peer.QuotaUsage{
		{ContainerID: project, Name: quota.MSPPostgreSQLClusterCount, Amount: 2},
		{ContainerID: project, Name: quota.MSPPostgreSQLVCPU, Amount: 2 + 12},
		{ContainerID: project, Name: quota.MSPPostgreSQLMemory, Amount: (8 + 48) * gib},
		{ContainerID: project, Name: quota.MSPPostgreSQLDiskSize, Amount: (20 + 150) * gib},
		{ContainerID: project2, Name: quota.MSPPostgreSQLClusterCount, Amount: 1},
		{ContainerID: project2, Name: quota.MSPPostgreSQLVCPU, Amount: 4},
		{ContainerID: project2, Name: quota.MSPPostgreSQLMemory, Amount: 16 * gib},
		{ContainerID: project2, Name: quota.MSPPostgreSQLDiskSize, Amount: 20 * gib},
	}
	less := func(a, b peer.QuotaUsage) bool { return a.ContainerID+a.Name < b.ContainerID+b.Name }
	if diff := cmp.Diff(want, got, cmpopts.SortSlices(less)); diff != "" {
		t.Fatalf("usage mismatch (-want +got):\n%s", diff)
	}
}
