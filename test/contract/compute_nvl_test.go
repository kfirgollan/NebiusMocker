package contract

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

// nvlClients are the SDK clients of one principal against one mocker.
type nvlClients struct {
	groups  computesvc.NVLInstanceGroupService
	project string
}

func newNVLClients(t *testing.T, inst *testkit.Instance, token string) nvlClients {
	t.Helper()
	return nvlClients{groups: computesvc.NewNVLInstanceGroupService(inst.SDKAs(t, token)), project: inst.ProjectID}
}

func startNVL(t *testing.T) nvlClients {
	t.Helper()
	env := startImages(t)
	return newNVLClients(t, env.Inst, config.DefaultDevToken)
}

func (c nvlClients) create(ctx context.Context, name string, typ computepb.NVLInstanceGroupSpec_NVLInstanceGroupType, size int64) (string, error) {
	op, err := c.groups.Create(ctx, &computepb.CreateNVLInstanceGroupRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: c.project, Name: name},
		Spec:     &computepb.NVLInstanceGroupSpec{Type: typ, Size: size},
	})
	if err != nil {
		return "", err
	}
	if _, err := op.Wait(ctx); err != nil {
		return "", err
	}
	return op.ResourceID(), nil
}

func (c nvlClients) get(ctx context.Context, id string) (*computepb.NVLInstanceGroup, error) {
	return c.groups.Get(ctx, &computepb.GetNVLInstanceGroupRequest{Id: id})
}

func (c nvlClients) must(ctx context.Context, t *testing.T, name string, typ computepb.NVLInstanceGroupSpec_NVLInstanceGroupType) *computepb.NVLInstanceGroup {
	t.Helper()
	id, err := c.create(ctx, name, typ, 2)
	if err != nil {
		t.Fatalf("create nvl group %s: %v", name, err)
	}
	g, err := c.get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func (c nvlClients) update(ctx context.Context, g *computepb.NVLInstanceGroup, change func(*computepb.NVLInstanceGroup)) error {
	next := proto.Clone(g).(*computepb.NVLInstanceGroup)
	change(next)
	op, err := c.groups.Update(ctx, &computepb.UpdateNVLInstanceGroupRequest{Metadata: next.GetMetadata(), Spec: next.GetSpec()})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func (c nvlClients) delete(ctx context.Context, id string) error {
	op, err := c.groups.Delete(ctx, &computepb.DeleteNVLInstanceGroupRequest{Id: id})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func TestNVLInstanceGroupCRUD(t *testing.T) {
	t.Parallel()
	c := startNVL(t)
	ctx := t.Context()

	g := c.must(ctx, t, "domain", computepb.NVLInstanceGroupSpec_GB200)
	md := g.GetMetadata()
	if g.GetSpec().GetType() != computepb.NVLInstanceGroupSpec_GB200 || g.GetSpec().GetSize() != 2 ||
		g.GetStatus().GetReconciling() || len(g.GetStatus().GetInstances()) != 0 ||
		!strings.HasPrefix(md.GetId(), "computenvlinstancegroup") || md.GetResourceVersion() != 1 || md.GetParentId() != c.project ||
		md.GetName() != "domain" || md.GetCreatedAt() == nil || md.GetUpdatedAt() == nil {
		t.Fatalf("created group = %v", g)
	}
	id := md.GetId()

	byName, err := c.groups.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: c.project, Name: "domain"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}

	// Name and labels.
	err = c.update(ctx, g, func(n *computepb.NVLInstanceGroup) {
		n.GetMetadata().Name = "domain-2"
		n.GetMetadata().Labels = map[string]string{"team": "a"}
	})
	if err != nil {
		t.Fatalf("update name+labels: %v", err)
	}
	got, err := c.get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetMetadata().GetName() != "domain-2" || got.GetMetadata().GetLabels()["team"] != "a" ||
		got.GetMetadata().GetResourceVersion() <= md.GetResourceVersion() ||
		got.GetSpec().GetType() != computepb.NVLInstanceGroupSpec_GB200 || got.GetSpec().GetSize() != 2 {
		t.Fatalf("after update: %v", got)
	}

	// A reset mask on labels clears them.
	maskCtx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust("metadata.labels"))
	if err != nil {
		t.Fatal(err)
	}
	cleared := proto.Clone(got).(*computepb.NVLInstanceGroup)
	cleared.GetMetadata().Labels = nil
	op, err := c.groups.Update(maskCtx, &computepb.UpdateNVLInstanceGroupRequest{Metadata: cleared.GetMetadata(), Spec: cleared.GetSpec()})
	if err != nil {
		t.Fatalf("update with reset mask: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := c.get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.GetMetadata().GetLabels()) != 0 || after.GetMetadata().GetName() != "domain-2" || after.GetSpec().GetSize() != 2 {
		t.Fatalf("after reset mask: %v", after)
	}

	// Type and size cannot change.
	err = c.update(ctx, after, func(n *computepb.NVLInstanceGroup) { n.GetSpec().Type = computepb.NVLInstanceGroupSpec_GB300 })
	assertInvalid(t, err, "spec.type")
	err = c.update(ctx, after, func(n *computepb.NVLInstanceGroup) { n.GetSpec().Size = 4 })
	assertInvalid(t, err, "spec.size")

	// A stale resource_version conflicts.
	err = c.update(ctx, after, func(n *computepb.NVLInstanceGroup) {
		n.GetMetadata().ResourceVersion = md.GetResourceVersion()
		n.GetMetadata().Name = "stale"
	})
	imagescenario.AssertCode(t, err, codes.Aborted)

	if err := c.delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	_, err = c.get(ctx, id)
	imagescenario.AssertCode(t, err, codes.NotFound)
}

func TestNVLInstanceGroupListPaging(t *testing.T) {
	t.Parallel()
	c := startNVL(t)
	ctx := t.Context()
	want := map[string]bool{}
	for _, n := range []string{"a", "b", "c"} {
		want[c.must(ctx, t, n, computepb.NVLInstanceGroupSpec_GB200).GetMetadata().GetId()] = true
	}

	got := map[string]bool{}
	token := ""
	pages := 0
	for {
		resp, err := c.groups.List(ctx, &computepb.ListNVLInstanceGroupsRequest{ParentId: c.project, PageSize: 1, PageToken: token})
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
		t.Fatalf("paged %d groups over %d pages, want 3 over 3", len(got), pages)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("group %s missing from paged list", id)
		}
	}
}

func TestNVLInstanceGroupCreateValidation(t *testing.T) {
	t.Parallel()
	c := startNVL(t)
	ctx := t.Context()

	_, err := c.create(ctx, "bad", computepb.NVLInstanceGroupSpec_UNSPECIFIED, 2)
	assertInvalid(t, err, "spec.type")
	_, err = c.create(ctx, "bad", computepb.NVLInstanceGroupSpec_GB200, 0)
	assertInvalid(t, err, "spec.size")
	_, err = c.create(ctx, "bad", computepb.NVLInstanceGroupSpec_GB200, -1)
	imagescenario.AssertCode(t, err, codes.InvalidArgument)

	c.must(ctx, t, "dup", computepb.NVLInstanceGroupSpec_GB200)
	_, err = c.create(ctx, "dup", computepb.NVLInstanceGroupSpec_GB300, 2)
	imagescenario.AssertCode(t, err, codes.AlreadyExists)
}

func TestNVLInstanceGroupMissing(t *testing.T) {
	t.Parallel()
	c := startNVL(t)
	ctx := t.Context()
	const missing = "computenvlinstancegroup-e00doesnotexist"

	_, err := c.get(ctx, missing)
	imagescenario.AssertCode(t, err, codes.NotFound)
	_, err = c.groups.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: c.project, Name: "nope"})
	imagescenario.AssertCode(t, err, codes.NotFound)
	imagescenario.AssertCode(t, c.delete(ctx, missing), codes.NotFound)
	_, err = c.groups.Update(ctx, &computepb.UpdateNVLInstanceGroupRequest{
		Metadata: &commonpb.ResourceMetadata{Id: missing, ParentId: c.project, Name: "x"},
		Spec:     &computepb.NVLInstanceGroupSpec{Type: computepb.NVLInstanceGroupSpec_GB200, Size: 2},
	})
	imagescenario.AssertCode(t, err, codes.NotFound)
}

func TestNVLInstanceGroupAuthz(t *testing.T) {
	t.Parallel()
	env := startImages(t, testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired())
	admin := newNVLClients(t, env.Inst, config.DefaultDevToken)
	fixture := admin.must(t.Context(), t, "fixture", computepb.NVLInstanceGroupSpec_GB200)
	fixtureID := fixture.GetMetadata().GetId()

	t.Run("viewer", func(t *testing.T) {
		t.Parallel()
		v := newNVLClients(t, env.Inst, config.DefaultViewerToken)
		ctx := t.Context()
		if _, err := v.get(ctx, fixtureID); err != nil {
			t.Errorf("Get: %v", err)
		}
		if _, err := v.groups.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: v.project, Name: "fixture"}); err != nil {
			t.Errorf("GetByName: %v", err)
		}
		if _, err := v.groups.List(ctx, &computepb.ListNVLInstanceGroupsRequest{ParentId: v.project}); err != nil {
			t.Errorf("List: %v", err)
		}
		_, err := v.create(ctx, "by-viewer", computepb.NVLInstanceGroupSpec_GB200, 2)
		imagescenario.AssertCode(t, err, codes.PermissionDenied)
		err = v.update(ctx, fixture, func(n *computepb.NVLInstanceGroup) { n.GetMetadata().Labels = map[string]string{"x": "y"} })
		imagescenario.AssertCode(t, err, codes.PermissionDenied)
		imagescenario.AssertCode(t, v.delete(ctx, fixtureID), codes.PermissionDenied)
		if _, err := admin.get(ctx, fixtureID); err != nil {
			t.Fatalf("fixture disappeared: %v", err)
		}
	})

	for name, token := range map[string]string{"editor": config.DefaultEditorToken, "admin": config.DefaultDevToken} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newNVLClients(t, env.Inst, token)
			ctx := t.Context()
			g := p.must(ctx, t, "by-"+name, computepb.NVLInstanceGroupSpec_GB300)
			err := p.update(ctx, g, func(n *computepb.NVLInstanceGroup) { n.GetMetadata().Labels = map[string]string{"x": "y"} })
			if err != nil {
				t.Fatalf("%s update: %v", name, err)
			}
			if err := p.delete(ctx, g.GetMetadata().GetId()); err != nil {
				t.Fatalf("%s delete: %v", name, err)
			}
		})
	}
}
