package contract

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

// fsClients are the SDK clients of one principal against one mocker.
type fsClients struct {
	fs      computesvc.FilesystemService
	project string
}

func newFsClients(t *testing.T, inst *testkit.Instance, token string) fsClients {
	t.Helper()
	return fsClients{fs: computesvc.NewFilesystemService(inst.SDKAs(t, token)), project: inst.ProjectID}
}

func fsSpecGiB(gib int64) *computepb.FilesystemSpec {
	return &computepb.FilesystemSpec{
		Type: computepb.FilesystemSpec_NETWORK_SSD,
		Size: &computepb.FilesystemSpec_SizeGibibytes{SizeGibibytes: gib},
	}
}

func (c fsClients) create(ctx context.Context, name string, spec *computepb.FilesystemSpec) (string, error) {
	op, err := c.fs.Create(ctx, &computepb.CreateFilesystemRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: c.project, Name: name}, Spec: spec,
	})
	if err != nil {
		return "", err
	}
	if _, err := op.Wait(ctx); err != nil {
		return "", err
	}
	return op.ResourceID(), nil
}

func (c fsClients) must(ctx context.Context, t *testing.T, name string, spec *computepb.FilesystemSpec) *computepb.Filesystem {
	t.Helper()
	id, err := c.create(ctx, name, spec)
	if err != nil {
		t.Fatalf("create filesystem %s: %v", name, err)
	}
	f, err := c.fs.Get(ctx, &computepb.GetFilesystemRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// update resends the filesystem with the change applied and waits.
func (c fsClients) update(ctx context.Context, f *computepb.Filesystem, change func(*computepb.Filesystem)) error {
	next := proto.Clone(f).(*computepb.Filesystem)
	change(next)
	op, err := c.fs.Update(ctx, &computepb.UpdateFilesystemRequest{Metadata: next.GetMetadata(), Spec: next.GetSpec()})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func (c fsClients) delete(ctx context.Context, id string) error {
	op, err := c.fs.Delete(ctx, &computepb.DeleteFilesystemRequest{Id: id})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func TestFilesystemCRUD(t *testing.T) {
	t.Parallel()
	env := startImages(t)
	c := newFsClients(t, env.Inst, config.DefaultDevToken)
	ctx := t.Context()

	f := c.must(ctx, t, "shared", fsSpecGiB(10))
	if f.GetStatus().GetState() != computepb.FilesystemStatus_READY || f.GetStatus().GetSizeBytes() != 10<<30 ||
		f.GetStatus().GetBlockSizeBytes() != 4096 || f.GetStatus().GetReconciling() ||
		f.GetMetadata().GetParentId() != c.project || f.GetSpec().GetSizeGibibytes() != 10 || f.GetSpec().GetBlockSizeBytes() != 0 {
		t.Fatalf("created filesystem = %v", f)
	}
	id := f.GetMetadata().GetId()
	if empty, err := env.Store.BucketEmpty(ctx, "nebius-mocker-fs-"+id); err != nil || !empty {
		t.Fatalf("filesystem bucket: empty=%v err=%v, want an existing empty bucket", empty, err)
	}

	byName, err := c.fs.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: c.project, Name: "shared"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}

	// Labels: resource_version bumps.
	err = c.update(ctx, f, func(n *computepb.Filesystem) { n.GetMetadata().Labels = map[string]string{"team": "a"} })
	if err != nil {
		t.Fatalf("update labels: %v", err)
	}
	got, err := c.fs.Get(ctx, &computepb.GetFilesystemRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetMetadata().GetLabels()["team"] != "a" || got.GetMetadata().GetResourceVersion() <= f.GetMetadata().GetResourceVersion() {
		t.Fatalf("after label update: %v", got.GetMetadata())
	}

	// Hot resize.
	err = c.update(ctx, got, func(n *computepb.Filesystem) {
		n.GetSpec().Size = &computepb.FilesystemSpec_SizeGibibytes{SizeGibibytes: 20}
	})
	if err != nil {
		t.Fatalf("grow: %v", err)
	}
	grown, err := c.fs.Get(ctx, &computepb.GetFilesystemRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if grown.GetStatus().GetSizeBytes() != 20<<30 || grown.GetStatus().GetState() != computepb.FilesystemStatus_READY ||
		grown.GetMetadata().GetResourceVersion() <= got.GetMetadata().GetResourceVersion() {
		t.Fatalf("after grow: %v", grown)
	}

	// A reset mask on labels with no labels clears them.
	maskCtx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust("metadata.labels"))
	if err != nil {
		t.Fatal(err)
	}
	cleared := proto.Clone(grown).(*computepb.Filesystem)
	cleared.GetMetadata().Labels = nil
	op, err := c.fs.Update(maskCtx, &computepb.UpdateFilesystemRequest{Metadata: cleared.GetMetadata(), Spec: cleared.GetSpec()})
	if err != nil {
		t.Fatalf("update with reset mask: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := c.fs.Get(ctx, &computepb.GetFilesystemRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.GetMetadata().GetLabels()) != 0 || after.GetStatus().GetSizeBytes() != 20<<30 || after.GetMetadata().GetName() != "shared" {
		t.Fatalf("after reset mask: %v", after)
	}

	if err := c.delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	_, err = c.fs.Get(ctx, &computepb.GetFilesystemRequest{Id: id})
	imagescenario.AssertCode(t, err, codes.NotFound)
	if _, err := env.Store.BucketEmpty(ctx, "nebius-mocker-fs-"+id); !errors.Is(err, objstore.ErrBucketNotFound) {
		t.Fatalf("filesystem bucket after delete: err = %v, want ErrBucketNotFound", err)
	}

	// Operations, newest first, including the ones of the deleted filesystem.
	ops, err := c.fs.ListOperationsByParent(ctx, &computepb.ListOperationsByParentRequest{ParentId: c.project})
	if err != nil {
		t.Fatal(err)
	}
	var descs []string
	for _, o := range ops.GetOperations() {
		if o.GetResourceId() != id {
			t.Fatalf("operation %v is for another resource", o)
		}
		descs = append(descs, strings.ToLower(o.GetDescription()))
	}
	// create, 3 updates (labels, grow, reset), delete.
	if len(descs) != 5 || !strings.HasPrefix(descs[0], "delete") || !strings.HasPrefix(descs[1], "update") ||
		!strings.HasPrefix(descs[3], "update") || !strings.HasPrefix(descs[4], "create") {
		t.Fatalf("ListOperationsByParent = %v, want delete, 3x update, create", descs)
	}
}

func TestFilesystemListPaging(t *testing.T) {
	t.Parallel()
	c := newFsClients(t, startImages(t).Inst, config.DefaultDevToken)
	ctx := t.Context()
	var want []string
	for i := range 5 {
		want = append(want, c.must(ctx, t, "page-"+strconv.Itoa(i), fsSpecGiB(1)).GetMetadata().GetId())
	}
	var got []string
	token, pages := "", 0
	for {
		resp, err := c.fs.List(ctx, &computepb.ListFilesystemsRequest{ParentId: c.project, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(resp.GetItems()) > 2 {
			t.Fatalf("page %d has %d items, want <= 2", pages, len(resp.GetItems()))
		}
		for _, f := range resp.GetItems() {
			got = append(got, f.GetMetadata().GetId())
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	if pages != 3 || !slices.Equal(got, want) {
		t.Fatalf("pages = %d, ids = %v, want 3 pages in creation order %v", pages, got, want)
	}

	ops1, err := c.fs.ListOperationsByParent(ctx, &computepb.ListOperationsByParentRequest{ParentId: c.project, PageSize: 3})
	if err != nil || len(ops1.GetOperations()) != 3 || ops1.GetNextPageToken() == "" {
		t.Fatalf("operations page 1 = %v, %v", ops1, err)
	}
	ops2, err := c.fs.ListOperationsByParent(ctx, &computepb.ListOperationsByParentRequest{
		ParentId: c.project, PageSize: 3, PageToken: ops1.GetNextPageToken(),
	})
	if err != nil || len(ops2.GetOperations()) != 2 || ops2.GetNextPageToken() != "" {
		t.Fatalf("operations page 2 = %v, %v", ops2, err)
	}

	_, err = c.fs.List(ctx, &computepb.ListFilesystemsRequest{ParentId: c.project, Filter: "name = 'x'"}) //nolint:staticcheck // filter is deprecated upstream; the mocker must reject it
	imagescenario.AssertCode(t, err, codes.InvalidArgument)
}

func TestFilesystemErrors(t *testing.T) {
	t.Parallel()
	c := newFsClients(t, startImages(t).Inst, config.DefaultDevToken)
	ctx := t.Context()

	hdd := fsSpecGiB(10)
	hdd.Type = computepb.FilesystemSpec_NETWORK_HDD
	noType := fsSpecGiB(10)
	noType.Type = computepb.FilesystemSpec_UNSPECIFIED
	badBlock := fsSpecGiB(10)
	badBlock.BlockSizeBytes = 5000
	for name, spec := range map[string]*computepb.FilesystemSpec{
		"too small": fsSpecGiB(0), "too large": fsSpecGiB(104857601), "hdd": hdd, "no type": noType, "bad block size": badBlock,
		"no size": {Type: computepb.FilesystemSpec_NETWORK_SSD},
	} {
		_, err := c.create(ctx, "bad", spec)
		if code := status.Code(err); code != codes.InvalidArgument {
			t.Errorf("create with %s: code = %v (err %v), want InvalidArgument", name, code, err)
		}
	}
	_, err := c.fs.Create(ctx, &computepb.CreateFilesystemRequest{Metadata: &commonpb.ResourceMetadata{Name: "x"}, Spec: fsSpecGiB(1)})
	imagescenario.AssertCode(t, err, codes.InvalidArgument)
	_, err = c.fs.Create(ctx, &computepb.CreateFilesystemRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: "project-e00nonexistent00000", Name: "x"}, Spec: fsSpecGiB(1),
	})
	imagescenario.AssertCode(t, err, codes.NotFound)

	f := c.must(ctx, t, "one", fsSpecGiB(10))
	id := f.GetMetadata().GetId()
	_, err = c.create(ctx, "one", fsSpecGiB(1))
	imagescenario.AssertCode(t, err, codes.AlreadyExists)

	_, err = c.fs.Get(ctx, &computepb.GetFilesystemRequest{Id: "computefilesystem-e00doesnotexist000"})
	imagescenario.AssertCode(t, err, codes.NotFound)
	_, err = c.fs.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: c.project, Name: "missing"})
	imagescenario.AssertCode(t, err, codes.NotFound)

	// Immutables and shrinking.
	imagescenario.AssertCode(t, c.update(ctx, f, func(n *computepb.Filesystem) { n.GetMetadata().Name = "renamed" }), codes.InvalidArgument)
	imagescenario.AssertCode(t, c.update(ctx, f, func(n *computepb.Filesystem) { n.GetSpec().BlockSizeBytes = 8192 }), codes.InvalidArgument)
	imagescenario.AssertCode(t, c.update(ctx, f, func(n *computepb.Filesystem) {
		n.GetSpec().Size = &computepb.FilesystemSpec_SizeGibibytes{SizeGibibytes: 5}
	}), codes.InvalidArgument)

	// Deletion protection.
	if err := c.update(ctx, f, func(n *computepb.Filesystem) { n.GetSpec().ForbidDeletion = true }); err != nil {
		t.Fatalf("set forbid_deletion: %v", err)
	}
	imagescenario.AssertCode(t, c.delete(ctx, id), codes.FailedPrecondition)
	protected, err := c.fs.Get(ctx, &computepb.GetFilesystemRequest{Id: id})
	if err != nil || protected.GetStatus().GetState() != computepb.FilesystemStatus_READY {
		t.Fatalf("filesystem after a refused delete = %v, %v", protected, err)
	}
	if err := c.update(ctx, protected, func(n *computepb.Filesystem) { n.GetSpec().ForbidDeletion = false }); err != nil {
		t.Fatal(err)
	}
	if err := c.delete(ctx, id); err != nil {
		t.Fatalf("delete after clearing forbid_deletion: %v", err)
	}
	imagescenario.AssertCode(t, c.delete(ctx, id), codes.NotFound)
}

func TestFilesystemAuthz(t *testing.T) {
	t.Parallel()
	env := startImages(t, testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired())
	admin := newFsClients(t, env.Inst, config.DefaultDevToken)
	fixture := admin.must(t.Context(), t, "fixture", fsSpecGiB(1))
	fixtureID := fixture.GetMetadata().GetId()

	reads := func(t *testing.T, c fsClients) {
		t.Helper()
		ctx := t.Context()
		if _, err := c.fs.Get(ctx, &computepb.GetFilesystemRequest{Id: fixtureID}); err != nil {
			t.Errorf("Get: %v, want success", err)
		}
		if _, err := c.fs.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: c.project, Name: "fixture"}); err != nil {
			t.Errorf("GetByName: %v, want success", err)
		}
		if _, err := c.fs.List(ctx, &computepb.ListFilesystemsRequest{ParentId: c.project}); err != nil {
			t.Errorf("List: %v, want success", err)
		}
		if _, err := c.fs.ListOperationsByParent(ctx, &computepb.ListOperationsByParentRequest{ParentId: c.project}); err != nil {
			t.Errorf("ListOperationsByParent: %v, want success", err)
		}
	}

	t.Run("viewer", func(t *testing.T) {
		t.Parallel()
		v := newFsClients(t, env.Inst, config.DefaultViewerToken)
		ctx := t.Context()
		reads(t, v)
		_, err := v.create(ctx, "by-viewer", fsSpecGiB(1))
		imagescenario.AssertCode(t, err, codes.PermissionDenied)
		err = v.update(ctx, fixture, func(n *computepb.Filesystem) { n.GetMetadata().Labels = map[string]string{"x": "y"} })
		imagescenario.AssertCode(t, err, codes.PermissionDenied)
		imagescenario.AssertCode(t, v.delete(ctx, fixtureID), codes.PermissionDenied)
		if _, err := admin.fs.Get(ctx, &computepb.GetFilesystemRequest{Id: fixtureID}); err != nil {
			t.Fatalf("fixture disappeared: %v", err)
		}
	})

	t.Run("editor", func(t *testing.T) {
		t.Parallel()
		e := newFsClients(t, env.Inst, config.DefaultEditorToken)
		ctx := t.Context()
		reads(t, e)
		f := e.must(ctx, t, "by-editor", fsSpecGiB(1))
		if err := e.update(ctx, f, func(n *computepb.Filesystem) { n.GetMetadata().Labels = map[string]string{"x": "y"} }); err != nil {
			t.Fatalf("editor update: %v", err)
		}
		if err := e.delete(ctx, f.GetMetadata().GetId()); err != nil {
			t.Fatalf("editor delete: %v", err)
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		t.Parallel()
		u := newFsClients(t, env.Inst, "")
		ctx := t.Context()
		_, err := u.fs.Get(ctx, &computepb.GetFilesystemRequest{Id: fixtureID})
		imagescenario.AssertCode(t, err, codes.Unauthenticated)
		_, err = u.create(ctx, "anon", fsSpecGiB(1))
		imagescenario.AssertCode(t, err, codes.Unauthenticated)
	})
}
