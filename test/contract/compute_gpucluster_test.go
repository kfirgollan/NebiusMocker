package contract

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk"
	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

// gpuClients are the SDK clients of one principal against one mocker.
type gpuClients struct {
	clusters computesvc.GpuClusterService
	inst     *testkit.Instance
	sdk      *gosdk.SDK
	project  string
}

func newGpuClients(t *testing.T, inst *testkit.Instance, token string) gpuClients {
	t.Helper()
	sdk := inst.SDKAs(t, token)
	return gpuClients{clusters: computesvc.NewGpuClusterService(sdk), sdk: sdk, inst: inst, project: inst.ProjectID}
}

func startGpuClusters(t *testing.T, opts ...testkit.Option) gpuClients {
	t.Helper()
	env := startImages(t, opts...)
	return newGpuClients(t, env.Inst, config.DefaultDevToken)
}

func (c gpuClients) create(ctx context.Context, parent, name, fabric string) (string, error) {
	op, err := c.clusters.Create(ctx, &computepb.CreateGpuClusterRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name},
		Spec:     &computepb.GpuClusterSpec{InfinibandFabric: fabric},
	})
	if err != nil {
		return "", err
	}
	if _, err := op.Wait(ctx); err != nil {
		return "", err
	}
	return op.ResourceID(), nil
}

func (c gpuClients) must(ctx context.Context, t *testing.T, name, fabric string) *computepb.GpuCluster {
	t.Helper()
	id, err := c.create(ctx, c.project, name, fabric)
	if err != nil {
		t.Fatalf("create gpu cluster %s: %v", name, err)
	}
	g, err := c.clusters.Get(ctx, &computepb.GetGpuClusterRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func (c gpuClients) update(ctx context.Context, g *computepb.GpuCluster, change func(*computepb.GpuCluster)) error {
	next := proto.Clone(g).(*computepb.GpuCluster)
	change(next)
	op, err := c.clusters.Update(ctx, &computepb.UpdateGpuClusterRequest{Metadata: next.GetMetadata(), Spec: next.GetSpec()})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func (c gpuClients) delete(ctx context.Context, id string) error {
	op, err := c.clusters.Delete(ctx, &computepb.DeleteGpuClusterRequest{Id: id})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func (c gpuClients) opDescs(t *testing.T, pageSize int64, token string) (descs []string, next string) {
	t.Helper()
	ops, err := c.clusters.ListOperationsByParent(t.Context(), &computepb.ListOperationsByParentRequest{
		ParentId: c.project, PageSize: pageSize, PageToken: token,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range ops.GetOperations() {
		descs = append(descs, strings.ToLower(o.GetDescription()))
	}
	return descs, ops.GetNextPageToken()
}

func assertInvalid(t *testing.T, err error, contains string) {
	t.Helper()
	imagescenario.AssertCode(t, err, codes.InvalidArgument)
	if msg := status.Convert(err).Message(); !strings.Contains(msg, contains) {
		t.Fatalf("message %q does not mention %q", msg, contains)
	}
}

func TestGpuClusterCRUD(t *testing.T) {
	t.Parallel()
	c := startGpuClusters(t)
	ctx := t.Context()

	g := c.must(ctx, t, "train", "fabric-3")
	md := g.GetMetadata()
	if g.GetSpec().GetInfinibandFabric() != "fabric-3" || g.GetStatus().GetReconciling() || len(g.GetStatus().GetInstances()) != 0 ||
		!strings.HasPrefix(md.GetId(), "computegpucluster") || md.GetResourceVersion() != 1 || md.GetParentId() != c.project ||
		md.GetName() != "train" || md.GetCreatedAt() == nil || md.GetUpdatedAt() == nil {
		t.Fatalf("created cluster = %v", g)
	}
	id := md.GetId()

	byName, err := c.clusters.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: c.project, Name: "train"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}

	// Name and labels.
	err = c.update(ctx, g, func(n *computepb.GpuCluster) {
		n.GetMetadata().Name = "train-2"
		n.GetMetadata().Labels = map[string]string{"team": "a"}
	})
	if err != nil {
		t.Fatalf("update name+labels: %v", err)
	}
	got, err := c.clusters.Get(ctx, &computepb.GetGpuClusterRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetMetadata().GetName() != "train-2" || got.GetMetadata().GetLabels()["team"] != "a" ||
		got.GetMetadata().GetResourceVersion() <= md.GetResourceVersion() || got.GetSpec().GetInfinibandFabric() != "fabric-3" {
		t.Fatalf("after update: %v", got)
	}

	// Sending the same fabric again is fine.
	if err := c.update(ctx, got, func(n *computepb.GpuCluster) { n.GetSpec().InfinibandFabric = "fabric-3" }); err != nil {
		t.Fatalf("update with same fabric: %v", err)
	}

	// A reset mask on labels clears them.
	maskCtx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust("metadata.labels"))
	if err != nil {
		t.Fatal(err)
	}
	cur, err := c.clusters.Get(ctx, &computepb.GetGpuClusterRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	cleared := proto.Clone(cur).(*computepb.GpuCluster)
	cleared.GetMetadata().Labels = nil
	op, err := c.clusters.Update(maskCtx, &computepb.UpdateGpuClusterRequest{Metadata: cleared.GetMetadata(), Spec: cleared.GetSpec()})
	if err != nil {
		t.Fatalf("update with reset mask: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := c.clusters.Get(ctx, &computepb.GetGpuClusterRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.GetMetadata().GetLabels()) != 0 || after.GetMetadata().GetName() != "train-2" || after.GetSpec().GetInfinibandFabric() != "fabric-3" {
		t.Fatalf("after reset mask: %v", after)
	}

	// The fabric cannot change.
	err = c.update(ctx, after, func(n *computepb.GpuCluster) { n.GetSpec().InfinibandFabric = "fabric-4" })
	assertInvalid(t, err, "spec.infiniband_fabric")

	if err := c.delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	_, err = c.clusters.Get(ctx, &computepb.GetGpuClusterRequest{Id: id})
	imagescenario.AssertCode(t, err, codes.NotFound)

	// Operations of the deleted cluster remain, newest first and paged.
	descs, _ := c.opDescs(t, 0, "")
	if len(descs) < 4 || !strings.HasPrefix(descs[0], "delete") || !strings.HasPrefix(descs[len(descs)-1], "create") {
		t.Fatalf("ListOperationsByParent = %v, want delete first and create last", descs)
	}
	for _, d := range descs[1 : len(descs)-1] {
		if !strings.HasPrefix(d, "update") {
			t.Fatalf("ListOperationsByParent = %v, want updates between delete and create", descs)
		}
	}
	var paged []string
	token := ""
	for range len(descs) + 1 {
		page, next := c.opDescs(t, 1, token)
		if len(page) != 1 {
			t.Fatalf("page of size 1 = %v", page)
		}
		paged = append(paged, page...)
		if next == "" {
			break
		}
		token = next
	}
	if strings.Join(paged, ",") != strings.Join(descs, ",") {
		t.Fatalf("paged operations = %v, want %v", paged, descs)
	}
}

func TestGpuClusterListPaging(t *testing.T) {
	t.Parallel()
	c := startGpuClusters(t)
	ctx := t.Context()
	want := map[string]bool{}
	for _, n := range []string{"a", "b", "c"} {
		want[c.must(ctx, t, n, "fabric-3").GetMetadata().GetId()] = true
	}

	got := map[string]bool{}
	token := ""
	pages := 0
	for {
		resp, err := c.clusters.List(ctx, &computepb.ListGpuClustersRequest{ParentId: c.project, PageSize: 1, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.GetItems()) > 1 {
			t.Fatalf("page of size 1 has %d items", len(resp.GetItems()))
		}
		for _, it := range resp.GetItems() {
			got[it.GetMetadata().GetId()] = true
		}
		pages++
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
		if pages > 10 {
			t.Fatal("paging does not terminate")
		}
	}
	if len(got) != 3 || pages != 3 {
		t.Fatalf("paged %d clusters over %d pages, want 3 over 3", len(got), pages)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("cluster %s missing from paged list", id)
		}
	}

	_, err := c.clusters.List(ctx, &computepb.ListGpuClustersRequest{ParentId: c.project, Filter: "name=a"}) //nolint:staticcheck // the deprecated filter must be rejected
	assertInvalid(t, err, "filter")
}

func TestGpuClusterCreateValidation(t *testing.T) {
	t.Parallel()
	c := startGpuClusters(t)
	ctx := t.Context()

	for _, tc := range []struct{ name, fabric string }{
		{"missing fabric", ""},
		{"unknown fabric", "fabric-1"},
		{"fabric of another region", "fabric-5"},
		{"fabric of another region, new-style name", "us-central1-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := c.create(ctx, c.project, "bad", tc.fabric)
			assertInvalid(t, err, "spec.infiniband_fabric")
		})
	}

	t.Run("metadata.id set", func(t *testing.T) {
		t.Parallel()
		_, err := c.clusters.Create(ctx, &computepb.CreateGpuClusterRequest{
			Metadata: &commonpb.ResourceMetadata{Id: "computegpucluster-e00custom", ParentId: c.project, Name: "withid"},
			Spec:     &computepb.GpuClusterSpec{InfinibandFabric: "fabric-3"},
		})
		assertInvalid(t, err, "metadata.id")
	})

	t.Run("missing parent", func(t *testing.T) {
		t.Parallel()
		_, err := c.create(ctx, "", "noparent", "fabric-3")
		imagescenario.AssertCode(t, err, codes.InvalidArgument)
	})

	t.Run("unknown parent", func(t *testing.T) {
		t.Parallel()
		_, err := c.create(ctx, "project-e00doesnotexist0000", "orphan", "fabric-3")
		imagescenario.AssertCode(t, err, codes.NotFound)
	})

	t.Run("duplicate name", func(t *testing.T) {
		t.Parallel()
		c.must(ctx, t, "dup", "fabric-3")
		_, err := c.create(ctx, c.project, "dup", "fabric-4")
		imagescenario.AssertCode(t, err, codes.AlreadyExists)
	})

	t.Run("region follows project", func(t *testing.T) {
		t.Parallel()
		projects := iamv2svc.NewProjectService(c.sdk)
		op, err := projects.Create(ctx, &iamv2pb.CreateProjectRequest{
			Metadata: &commonpb.ResourceMetadata{ParentId: c.inst.TenantID, Name: "west"},
			Spec:     &iamv2pb.ProjectSpec{Region: "eu-west1"},
		})
		if err != nil {
			t.Fatalf("create eu-west1 project: %v", err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		west := op.ResourceID()

		_, err = c.create(ctx, west, "wrong", "fabric-3")
		assertInvalid(t, err, "spec.infiniband_fabric")
		id, err := c.create(ctx, west, "right", "fabric-5")
		if err != nil {
			t.Fatalf("create fabric-5 in eu-west1: %v", err)
		}
		g, err := c.clusters.Get(ctx, &computepb.GetGpuClusterRequest{Id: id})
		if err != nil || g.GetSpec().GetInfinibandFabric() != "fabric-5" || g.GetMetadata().GetParentId() != west {
			t.Fatalf("Get = %v, %v", g, err)
		}
	})
}

func TestGpuClusterMissing(t *testing.T) {
	t.Parallel()
	c := startGpuClusters(t)
	ctx := t.Context()
	const missing = "computegpucluster-e00doesnotexist"

	_, err := c.clusters.Get(ctx, &computepb.GetGpuClusterRequest{Id: missing})
	imagescenario.AssertCode(t, err, codes.NotFound)
	_, err = c.clusters.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: c.project, Name: "nope"})
	imagescenario.AssertCode(t, err, codes.NotFound)
	imagescenario.AssertCode(t, c.delete(ctx, missing), codes.NotFound)
	_, err = c.clusters.Update(ctx, &computepb.UpdateGpuClusterRequest{
		Metadata: &commonpb.ResourceMetadata{Id: missing, ParentId: c.project, Name: "x"},
		Spec:     &computepb.GpuClusterSpec{InfinibandFabric: "fabric-3"},
	})
	imagescenario.AssertCode(t, err, codes.NotFound)
}

func TestGpuClusterAuthz(t *testing.T) {
	t.Parallel()
	env := startImages(t, testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired())
	admin := newGpuClients(t, env.Inst, config.DefaultDevToken)
	fixture := admin.must(t.Context(), t, "fixture", "fabric-3")
	fixtureID := fixture.GetMetadata().GetId()

	t.Run("viewer", func(t *testing.T) {
		t.Parallel()
		v := newGpuClients(t, env.Inst, config.DefaultViewerToken)
		ctx := t.Context()
		if _, err := v.clusters.Get(ctx, &computepb.GetGpuClusterRequest{Id: fixtureID}); err != nil {
			t.Errorf("Get: %v", err)
		}
		if _, err := v.clusters.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: v.project, Name: "fixture"}); err != nil {
			t.Errorf("GetByName: %v", err)
		}
		if _, err := v.clusters.List(ctx, &computepb.ListGpuClustersRequest{ParentId: v.project}); err != nil {
			t.Errorf("List: %v", err)
		}
		if _, err := v.clusters.ListOperationsByParent(ctx, &computepb.ListOperationsByParentRequest{ParentId: v.project}); err != nil {
			t.Errorf("ListOperationsByParent: %v", err)
		}
		_, err := v.create(ctx, v.project, "by-viewer", "fabric-3")
		imagescenario.AssertCode(t, err, codes.PermissionDenied)
		err = v.update(ctx, fixture, func(n *computepb.GpuCluster) { n.GetMetadata().Labels = map[string]string{"x": "y"} })
		imagescenario.AssertCode(t, err, codes.PermissionDenied)
		imagescenario.AssertCode(t, v.delete(ctx, fixtureID), codes.PermissionDenied)
		if _, err := admin.clusters.Get(ctx, &computepb.GetGpuClusterRequest{Id: fixtureID}); err != nil {
			t.Fatalf("fixture disappeared: %v", err)
		}
	})

	for name, token := range map[string]string{"editor": config.DefaultEditorToken, "admin": config.DefaultDevToken} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newGpuClients(t, env.Inst, token)
			ctx := t.Context()
			g := p.must(t.Context(), t, "by-"+name, "fabric-4")
			err := p.update(ctx, g, func(n *computepb.GpuCluster) { n.GetMetadata().Labels = map[string]string{"x": "y"} })
			if err != nil {
				t.Fatalf("%s update: %v", name, err)
			}
			if err := p.delete(ctx, g.GetMetadata().GetId()); err != nil {
				t.Fatalf("%s delete: %v", name, err)
			}
		})
	}
}

// TestGpuClusterCustomFabricCatalog replaces the built-in catalog through the
// setting behind NEBIUS_MOCKER_COMPUTE_FABRICS_FILE.
func TestGpuClusterCustomFabricCatalog(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fabrics.yaml")
	const catalog = "fabrics: [{name: lab-fabric, region: eu-north1, platforms: [gpu-h100-sxm]}]\n"
	if err := os.WriteFile(path, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	c := startGpuClusters(t, testkit.WithConfig(func(cfg *config.Config) { cfg.ComputeFabricsFile = path }))

	g := c.must(t.Context(), t, "lab", "lab-fabric")
	if g.GetSpec().GetInfinibandFabric() != "lab-fabric" {
		t.Fatalf("created cluster = %v", g)
	}
	_, err := c.create(t.Context(), c.project, "builtin", "fabric-3")
	assertInvalid(t, err, "spec.infiniband_fabric")
}
