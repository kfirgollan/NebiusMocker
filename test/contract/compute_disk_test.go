package contract

import (
	"context"
	"slices"
	"strconv"
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

// diskClients are the SDK clients of one principal against one mocker.
type diskClients struct {
	disks   computesvc.DiskService
	snaps   computesvc.DiskSnapshotService
	project string
	tenant  string
}

func newDiskClients(t *testing.T, inst *testkit.Instance, token string) diskClients {
	t.Helper()
	sdk := inst.SDKAs(t, token)
	return diskClients{
		disks: computesvc.NewDiskService(sdk), snaps: computesvc.NewDiskSnapshotService(sdk),
		project: inst.ProjectID, tenant: inst.TenantID,
	}
}

func startDisks(t *testing.T) diskClients {
	t.Helper()
	env := startImages(t)
	return newDiskClients(t, env.Inst, config.DefaultDevToken)
}

func ssdSpec(gib int64) *computepb.DiskSpec {
	return &computepb.DiskSpec{
		Type: computepb.DiskSpec_NETWORK_SSD,
		Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: gib},
	}
}

func (c diskClients) createDisk(ctx context.Context, name string, spec *computepb.DiskSpec) (string, error) {
	op, err := c.disks.Create(ctx, &computepb.CreateDiskRequest{
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

func (c diskClients) mustDisk(ctx context.Context, t *testing.T, name string, spec *computepb.DiskSpec) *computepb.Disk {
	t.Helper()
	id, err := c.createDisk(ctx, name, spec)
	if err != nil {
		t.Fatalf("create disk %s: %v", name, err)
	}
	d, err := c.disks.Get(ctx, &computepb.GetDiskRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (c diskClients) createSnap(ctx context.Context, parent, name, diskID, description string) (string, error) {
	op, err := c.snaps.Create(ctx, &computepb.CreateDiskSnapshotRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name},
		Spec:     &computepb.DiskSnapshotSpec{SourceDiskId: diskID, Description: description},
	})
	if err != nil {
		return "", err
	}
	if _, err := op.Wait(ctx); err != nil {
		return "", err
	}
	return op.ResourceID(), nil
}

func (c diskClients) mustSnap(t *testing.T, name, diskID string) *computepb.DiskSnapshot {
	t.Helper()
	id, err := c.createSnap(t.Context(), c.project, name, diskID, "")
	if err != nil {
		t.Fatalf("create snapshot %s: %v", name, err)
	}
	s, err := c.snaps.Get(t.Context(), &computepb.GetDiskSnapshotRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// updateDisk resends the disk with the given changes applied and waits.
func (c diskClients) updateDisk(ctx context.Context, d *computepb.Disk, change func(*computepb.Disk)) error {
	next := proto.Clone(d).(*computepb.Disk)
	change(next)
	op, err := c.disks.Update(ctx, &computepb.UpdateDiskRequest{Metadata: next.GetMetadata(), Spec: next.GetSpec()})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func (c diskClients) deleteDisk(ctx context.Context, id string) error {
	op, err := c.disks.Delete(ctx, &computepb.DeleteDiskRequest{Id: id})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func (c diskClients) deleteSnap(ctx context.Context, id string) error {
	op, err := c.snaps.Delete(ctx, &computepb.DeleteDiskSnapshotRequest{Id: id})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func TestDiskCRUD(t *testing.T) {
	t.Parallel()
	c := startDisks(t)
	ctx := t.Context()

	d := c.mustDisk(ctx, t, "data", ssdSpec(10))
	if d.GetStatus().GetState() != computepb.DiskStatus_READY || d.GetStatus().GetSizeBytes() != 10<<30 ||
		d.GetStatus().GetBlockSizeBytes() != 4096 || d.GetStatus().GetReconciling() ||
		d.GetMetadata().GetParentId() != c.project || d.GetSpec().GetSizeGibibytes() != 10 {
		t.Fatalf("created disk = %v", d)
	}
	id := d.GetMetadata().GetId()

	byName, err := c.disks.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: c.project, Name: "data"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}

	// Labels: resource_version bumps.
	err = c.updateDisk(ctx, d, func(n *computepb.Disk) { n.GetMetadata().Labels = map[string]string{"team": "a", "env": "dev"} })
	if err != nil {
		t.Fatalf("update labels: %v", err)
	}
	got, err := c.disks.Get(ctx, &computepb.GetDiskRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetMetadata().GetLabels()["team"] != "a" || got.GetMetadata().GetResourceVersion() <= d.GetMetadata().GetResourceVersion() {
		t.Fatalf("after label update: %v (was version %d)", got.GetMetadata(), d.GetMetadata().GetResourceVersion())
	}

	// Grow.
	err = c.updateDisk(ctx, got, func(n *computepb.Disk) { n.GetSpec().Size = &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 20} })
	if err != nil {
		t.Fatalf("grow: %v", err)
	}
	grown, err := c.disks.Get(ctx, &computepb.GetDiskRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if grown.GetStatus().GetSizeBytes() != 20<<30 || grown.GetStatus().GetState() != computepb.DiskStatus_READY ||
		grown.GetMetadata().GetResourceVersion() <= got.GetMetadata().GetResourceVersion() {
		t.Fatalf("after grow: %v", grown)
	}

	// A reset mask on labels with no labels clears them.
	maskCtx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust("metadata.labels"))
	if err != nil {
		t.Fatal(err)
	}
	cleared := proto.Clone(grown).(*computepb.Disk)
	cleared.GetMetadata().Labels = nil
	op, err := c.disks.Update(maskCtx, &computepb.UpdateDiskRequest{Metadata: cleared.GetMetadata(), Spec: cleared.GetSpec()})
	if err != nil {
		t.Fatalf("update with reset mask: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := c.disks.Get(ctx, &computepb.GetDiskRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.GetMetadata().GetLabels()) != 0 || after.GetStatus().GetSizeBytes() != 20<<30 {
		t.Fatalf("after reset mask: %v", after)
	}

	if err := c.deleteDisk(ctx, id); err != nil {
		t.Fatal(err)
	}
	_, err = c.disks.Get(ctx, &computepb.GetDiskRequest{Id: id})
	imagescenario.AssertCode(t, err, codes.NotFound)

	// Operations, newest first.
	ops, err := c.disks.ListOperationsByParent(ctx, &computepb.ListOperationsByParentRequest{ParentId: c.project})
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

func TestDiskListPaging(t *testing.T) {
	t.Parallel()
	c := startDisks(t)
	ctx := t.Context()
	var want []string
	for i := range 5 {
		want = append(want, c.mustDisk(ctx, t, "page-"+strconv.Itoa(i), ssdSpec(1)).GetMetadata().GetId())
	}
	var got []string
	token, pages := "", 0
	for {
		resp, err := c.disks.List(ctx, &computepb.ListDisksRequest{ParentId: c.project, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(resp.GetItems()) > 2 {
			t.Fatalf("page %d has %d items, want <= 2", pages, len(resp.GetItems()))
		}
		for _, d := range resp.GetItems() {
			got = append(got, d.GetMetadata().GetId())
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3", pages)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paged ids = %v, want creation order %v", got, want)
	}
	_, err := c.disks.List(ctx, &computepb.ListDisksRequest{ParentId: c.project, Filter: "name = 'x'"}) //nolint:staticcheck // filter is deprecated upstream; the mocker must reject it
	imagescenario.AssertCode(t, err, codes.InvalidArgument)
}

func TestDiskCreateValidation(t *testing.T) {
	t.Parallel()
	c := startDisks(t)
	ctx := t.Context()

	nrd := func(gib int64) *computepb.DiskSpec {
		return &computepb.DiskSpec{
			Type: computepb.DiskSpec_NETWORK_SSD_NON_REPLICATED,
			Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: gib},
		}
	}
	block := func(size int64) *computepb.DiskSpec {
		s := ssdSpec(1)
		s.BlockSizeBytes = size
		return s
	}
	typed := func(typ computepb.DiskSpec_DiskType) *computepb.DiskSpec {
		s := ssdSpec(1)
		s.Type = typ
		return s
	}
	encrypted := func(typ computepb.DiskSpec_DiskType, gib int64) *computepb.DiskSpec {
		return &computepb.DiskSpec{
			Type: typ, Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: gib},
			DiskEncryption: &computepb.DiskEncryption{Type: computepb.DiskEncryption_DISK_ENCRYPTION_MANAGED},
		}
	}
	fromFamily := func(family string, gib int64) *computepb.DiskSpec {
		s := ssdSpec(gib)
		s.Source = &computepb.DiskSpec_SourceImageFamily{SourceImageFamily: &computepb.SourceImageFamily{ImageFamily: family}}
		return s
	}
	fromImage := func(id string) *computepb.DiskSpec {
		s := ssdSpec(30)
		s.Source = &computepb.DiskSpec_SourceImageId{SourceImageId: id}
		return s
	}

	tests := []struct {
		name string
		spec *computepb.DiskSpec
		want codes.Code
	}{
		{"no size unit", &computepb.DiskSpec{Type: computepb.DiskSpec_NETWORK_SSD}, codes.InvalidArgument},
		{"zero gibibytes", ssdSpec(0), codes.InvalidArgument},
		{"bytes not 4MiB multiple", &computepb.DiskSpec{
			Type: computepb.DiskSpec_NETWORK_SSD, Size: &computepb.DiskSpec_SizeBytes{SizeBytes: 4<<20 + 512},
		}, codes.InvalidArgument},
		{"ssd over 8192 GiB", ssdSpec(8193), codes.InvalidArgument},
		{"nrd 94 GiB", nrd(94), codes.InvalidArgument},
		{"block size 3000", block(3000), codes.InvalidArgument},
		{"block size 262144", block(262144), codes.InvalidArgument},
		{"hdd", typed(computepb.DiskSpec_NETWORK_HDD), codes.InvalidArgument},
		{"unspecified type", typed(computepb.DiskSpec_UNSPECIFIED), codes.InvalidArgument},
		{"managed encryption on ssd", encrypted(computepb.DiskSpec_NETWORK_SSD, 1), codes.InvalidArgument},
		{"missing image", fromImage("computeimage-e00missing0000000000"), codes.NotFound},
		{"deprecated family", fromFamily("ubuntu22.04-driverless", 30), codes.FailedPrecondition},
		{"unknown family", fromFamily("no-such-family", 30), codes.NotFound},
		{"smaller than image min", fromFamily("ubuntu24.04-driverless", 10), codes.InvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := c.createDisk(ctx, "v-"+strings.ReplaceAll(tc.name, " ", "-"), tc.spec)
			imagescenario.AssertCode(t, err, tc.want)
		})
	}

	t.Run("nrd 93 GiB ok", func(t *testing.T) {
		t.Parallel()
		d := c.mustDisk(ctx, t, "nrd-93", nrd(93))
		if d.GetStatus().GetSizeBytes() != 93<<30 {
			t.Fatalf("size = %d", d.GetStatus().GetSizeBytes())
		}
	})
	t.Run("managed encryption on io_m3", func(t *testing.T) {
		t.Parallel()
		d := c.mustDisk(ctx, t, "enc", encrypted(computepb.DiskSpec_NETWORK_SSD_IO_M3, 93))
		if d.GetSpec().GetDiskEncryption().GetType() != computepb.DiskEncryption_DISK_ENCRYPTION_MANAGED {
			t.Fatalf("encryption = %v", d.GetSpec().GetDiskEncryption())
		}
	})
	t.Run("duplicate name", func(t *testing.T) {
		t.Parallel()
		c.mustDisk(ctx, t, "dup", ssdSpec(1))
		_, err := c.createDisk(ctx, "dup", ssdSpec(1))
		imagescenario.AssertCode(t, err, codes.AlreadyExists)
	})
}

func TestDiskUpdateValidation(t *testing.T) {
	t.Parallel()
	c := startDisks(t)
	ctx := t.Context()
	d := c.mustDisk(ctx, t, "immutable", ssdSpec(8))

	tests := []struct {
		name   string
		change func(*computepb.Disk)
	}{
		{"shrink", func(n *computepb.Disk) { n.GetSpec().Size = &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 4} }},
		{"type change", func(n *computepb.Disk) { n.GetSpec().Type = computepb.DiskSpec_NETWORK_SSD_NON_REPLICATED }},
		{"block size change", func(n *computepb.Disk) { n.GetSpec().BlockSizeBytes = 8192 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			imagescenario.AssertCode(t, c.updateDisk(ctx, d, tc.change), codes.InvalidArgument)
		})
	}

	t.Run("stale resource_version", func(t *testing.T) {
		t.Parallel()
		fresh := c.mustDisk(ctx, t, "versioned", ssdSpec(1))
		err := c.updateDisk(ctx, fresh, func(n *computepb.Disk) { n.GetMetadata().Labels = map[string]string{"a": "b"} })
		if err != nil {
			t.Fatal(err)
		}
		// fresh still carries the old resource_version.
		err = c.updateDisk(ctx, fresh, func(n *computepb.Disk) { n.GetMetadata().Labels = map[string]string{"a": "c"} })
		imagescenario.AssertCode(t, err, codes.Aborted)
	})

	t.Run("forbid deletion", func(t *testing.T) {
		t.Parallel()
		spec := ssdSpec(1)
		spec.ForbidDeletion = true
		g := c.mustDisk(ctx, t, "guarded", spec)
		gid := g.GetMetadata().GetId()
		imagescenario.AssertCode(t, c.deleteDisk(ctx, gid), codes.FailedPrecondition)
		if err := c.updateDisk(ctx, g, func(n *computepb.Disk) { n.GetSpec().ForbidDeletion = false }); err != nil {
			t.Fatal(err)
		}
		if err := c.deleteDisk(ctx, gid); err != nil {
			t.Fatalf("delete after clearing forbid_deletion: %v", err)
		}
		_, err := c.disks.Get(ctx, &computepb.GetDiskRequest{Id: gid})
		imagescenario.AssertCode(t, err, codes.NotFound)
	})
}

func TestDiskSnapshots(t *testing.T) {
	t.Parallel()
	c := startDisks(t)
	ctx := t.Context()
	a := c.mustDisk(ctx, t, "snap-a", ssdSpec(10))
	b := c.mustDisk(ctx, t, "snap-b", ssdSpec(10))
	aID, bID := a.GetMetadata().GetId(), b.GetMetadata().GetId()

	var aSnaps, bSnaps []string
	for i := range 3 {
		aSnaps = append(aSnaps, c.mustSnap(t, "a-"+strconv.Itoa(i), aID).GetMetadata().GetId())
	}
	for i := range 2 {
		bSnaps = append(bSnaps, c.mustSnap(t, "b-"+strconv.Itoa(i), bID).GetMetadata().GetId())
	}

	snap, err := c.snaps.Get(ctx, &computepb.GetDiskSnapshotRequest{Id: aSnaps[0]})
	if err != nil {
		t.Fatal(err)
	}
	if snap.GetStatus().GetState() != computepb.DiskSnapshotStatus_READY ||
		snap.GetStatus().GetContentSizeBytes() != a.GetStatus().GetSizeBytes() ||
		snap.GetSpec().GetSourceDiskId() != aID || snap.GetMetadata().GetParentId() != c.project {
		t.Fatalf("snapshot = %v", snap)
	}
	byName, err := c.snaps.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: c.project, Name: "a-0"})
	if err != nil || byName.GetMetadata().GetId() != aSnaps[0] {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}

	walk := func(list func(token string) ([]*computepb.DiskSnapshot, string, error)) []string {
		t.Helper()
		var ids []string
		token := ""
		for {
			items, next, err := list(token)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) > 2 {
				t.Fatalf("page has %d items, want <= 2", len(items))
			}
			for _, s := range items {
				ids = append(ids, s.GetMetadata().GetId())
			}
			if token = next; token == "" {
				return ids
			}
		}
	}
	all := walk(func(token string) ([]*computepb.DiskSnapshot, string, error) {
		r, err := c.snaps.List(ctx, &computepb.ListDiskSnapshotsRequest{ParentId: c.project, PageSize: 2, PageToken: token})
		return r.GetItems(), r.GetNextPageToken(), err
	})
	if want := slices.Concat(aSnaps, bSnaps); !slices.Equal(all, want) {
		t.Fatalf("List = %v, want %v", all, want)
	}
	for diskID, want := range map[string][]string{aID: aSnaps, bID: bSnaps} {
		got := walk(func(token string) ([]*computepb.DiskSnapshot, string, error) {
			r, err := c.snaps.ListByDisk(ctx, &computepb.ListDiskSnapshotsByDiskRequest{DiskId: diskID, PageSize: 2, PageToken: token})
			return r.GetItems(), r.GetNextPageToken(), err
		})
		if !slices.Equal(got, want) {
			t.Fatalf("ListByDisk(%s) = %v, want %v", diskID, got, want)
		}
	}
	_, err = c.snaps.ListByDisk(ctx, &computepb.ListDiskSnapshotsByDiskRequest{DiskId: "computedisk-e00missing0000000000"})
	imagescenario.AssertCode(t, err, codes.NotFound)
}

func TestDiskSnapshotValidation(t *testing.T) {
	t.Parallel()
	c := startDisks(t)
	ctx := t.Context()
	d := c.mustDisk(ctx, t, "src", ssdSpec(10))
	id := d.GetMetadata().GetId()

	_, err := c.createSnap(ctx, c.project, "orphan", "computedisk-e00missing0000000000", "")
	imagescenario.AssertCode(t, err, codes.NotFound)
	_, err = c.createSnap(ctx, c.tenant, "wrong-parent", id, "")
	imagescenario.AssertCode(t, err, codes.InvalidArgument)
	_, err = c.createSnap(ctx, c.project, "long-desc", id, strings.Repeat("x", 1025))
	imagescenario.AssertCode(t, err, codes.InvalidArgument)

	snap := c.mustSnap(t, "valid", id)
	snapID := snap.GetMetadata().GetId()

	// Update description and labels; the source disk is immutable.
	update := func(change func(*computepb.DiskSnapshot)) error {
		cur, err := c.snaps.Get(ctx, &computepb.GetDiskSnapshotRequest{Id: snapID})
		if err != nil {
			t.Fatal(err)
		}
		next := proto.Clone(cur).(*computepb.DiskSnapshot)
		change(next)
		op, err := c.snaps.Update(ctx, &computepb.UpdateDiskSnapshotRequest{Metadata: next.GetMetadata(), Spec: next.GetSpec()})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	err = update(func(n *computepb.DiskSnapshot) {
		n.GetSpec().Description = "nightly"
		n.GetMetadata().Labels = map[string]string{"k": "v"}
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := c.snaps.Get(ctx, &computepb.GetDiskSnapshotRequest{Id: snapID})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSpec().GetDescription() != "nightly" || got.GetMetadata().GetLabels()["k"] != "v" ||
		got.GetMetadata().GetResourceVersion() <= snap.GetMetadata().GetResourceVersion() {
		t.Fatalf("after update: %v", got)
	}
	err = update(func(n *computepb.DiskSnapshot) { n.GetSpec().SourceDiskId = "computedisk-e00other00000000000" })
	imagescenario.AssertCode(t, err, codes.InvalidArgument)

	// Disks from the snapshot: too small is rejected, large enough works.
	fromSnap := func(gib int64) *computepb.DiskSpec {
		s := ssdSpec(gib)
		s.Source = &computepb.DiskSpec_SourceSnapshotId{SourceSnapshotId: snapID}
		return s
	}
	_, err = c.createDisk(ctx, "too-small", fromSnap(5))
	imagescenario.AssertCode(t, err, codes.InvalidArgument)
	restored := c.mustDisk(ctx, t, "restored", fromSnap(10))
	if restored.GetStatus().GetSizeBytes() != 10<<30 {
		t.Fatalf("restored = %v", restored)
	}
	_, err = c.createDisk(ctx, "no-snap", func() *computepb.DiskSpec {
		s := ssdSpec(10)
		s.Source = &computepb.DiskSpec_SourceSnapshotId{SourceSnapshotId: "computesnapshot-e00missing000000"}
		return s
	}())
	imagescenario.AssertCode(t, err, codes.NotFound)

	// Deleting the source disk keeps the snapshot, with a stale source id.
	if err := c.deleteDisk(ctx, id); err != nil {
		t.Fatalf("delete source disk: %v", err)
	}
	kept, err := c.snaps.Get(ctx, &computepb.GetDiskSnapshotRequest{Id: snapID})
	if err != nil {
		t.Fatal(err)
	}
	if kept.GetStatus().GetState() != computepb.DiskSnapshotStatus_READY || kept.GetSpec().GetSourceDiskId() != id {
		t.Fatalf("snapshot after source delete = %v", kept)
	}

	if err := c.deleteSnap(ctx, snapID); err != nil {
		t.Fatalf("delete snapshot: %v", err)
	}
	_, err = c.snaps.Get(ctx, &computepb.GetDiskSnapshotRequest{Id: snapID})
	imagescenario.AssertCode(t, err, codes.NotFound)
}

func TestDiskAuthz(t *testing.T) {
	t.Parallel()
	env := startImages(t, testkit.WithAuthz(authz.ModeEnforce), testkit.WithAuthnRequired())
	admin := newDiskClients(t, env.Inst, config.DefaultDevToken)
	fixture := admin.mustDisk(t.Context(), t, "fixture", ssdSpec(1))
	fixtureID := fixture.GetMetadata().GetId()
	fixtureSnap := admin.mustSnap(t, "fixture-snap", fixtureID)
	snapID := fixtureSnap.GetMetadata().GetId()

	reads := func(t *testing.T, c diskClients) {
		t.Helper()
		ctx := t.Context()
		check := func(call string, err error) {
			t.Helper()
			if err != nil {
				t.Errorf("%s: %v, want success", call, err)
			}
		}
		_, err := c.disks.Get(ctx, &computepb.GetDiskRequest{Id: fixtureID})
		check("disk Get", err)
		_, err = c.disks.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: c.project, Name: "fixture"})
		check("disk GetByName", err)
		_, err = c.disks.List(ctx, &computepb.ListDisksRequest{ParentId: c.project})
		check("disk List", err)
		_, err = c.disks.ListOperationsByParent(ctx, &computepb.ListOperationsByParentRequest{ParentId: c.project})
		check("disk ListOperationsByParent", err)
		_, err = c.snaps.Get(ctx, &computepb.GetDiskSnapshotRequest{Id: snapID})
		check("snapshot Get", err)
		_, err = c.snaps.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: c.project, Name: "fixture-snap"})
		check("snapshot GetByName", err)
		_, err = c.snaps.List(ctx, &computepb.ListDiskSnapshotsRequest{ParentId: c.project})
		check("snapshot List", err)
		_, err = c.snaps.ListByDisk(ctx, &computepb.ListDiskSnapshotsByDiskRequest{DiskId: fixtureID})
		check("snapshot ListByDisk", err)
	}

	t.Run("viewer", func(t *testing.T) {
		t.Parallel()
		v := newDiskClients(t, env.Inst, config.DefaultViewerToken)
		ctx := t.Context()
		reads(t, v)
		_, err := v.createDisk(ctx, "by-viewer", ssdSpec(1))
		imagescenario.AssertCode(t, err, codes.PermissionDenied)
		err = v.updateDisk(ctx, fixture, func(n *computepb.Disk) { n.GetMetadata().Labels = map[string]string{"x": "y"} })
		imagescenario.AssertCode(t, err, codes.PermissionDenied)
		imagescenario.AssertCode(t, v.deleteDisk(ctx, fixtureID), codes.PermissionDenied)
		_, err = v.createSnap(ctx, v.project, "by-viewer", fixtureID, "")
		imagescenario.AssertCode(t, err, codes.PermissionDenied)
		_, err = v.snaps.Update(ctx, &computepb.UpdateDiskSnapshotRequest{Metadata: fixtureSnap.GetMetadata(), Spec: fixtureSnap.GetSpec()})
		imagescenario.AssertCode(t, err, codes.PermissionDenied)
		imagescenario.AssertCode(t, v.deleteSnap(ctx, snapID), codes.PermissionDenied)
		if _, err := admin.disks.Get(ctx, &computepb.GetDiskRequest{Id: fixtureID}); err != nil {
			t.Fatalf("fixture disappeared: %v", err)
		}
	})

	t.Run("editor", func(t *testing.T) {
		t.Parallel()
		e := newDiskClients(t, env.Inst, config.DefaultEditorToken)
		ctx := t.Context()
		reads(t, e)
		d := e.mustDisk(ctx, t, "by-editor", ssdSpec(1))
		id := d.GetMetadata().GetId()
		err := e.updateDisk(ctx, d, func(n *computepb.Disk) { n.GetMetadata().Labels = map[string]string{"x": "y"} })
		if err != nil {
			t.Fatalf("editor update disk: %v", err)
		}
		s := e.mustSnap(t, "by-editor", id)
		op, err := e.snaps.Update(ctx, &computepb.UpdateDiskSnapshotRequest{
			Metadata: s.GetMetadata(), Spec: &computepb.DiskSnapshotSpec{SourceDiskId: id, Description: "edited"},
		})
		if err != nil {
			t.Fatalf("editor update snapshot: %v", err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		if err := e.deleteSnap(ctx, s.GetMetadata().GetId()); err != nil {
			t.Fatalf("editor delete snapshot: %v", err)
		}
		if err := e.deleteDisk(ctx, id); err != nil {
			t.Fatalf("editor delete disk: %v", err)
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		t.Parallel()
		u := newDiskClients(t, env.Inst, "")
		ctx := t.Context()
		_, err := u.disks.Get(ctx, &computepb.GetDiskRequest{Id: fixtureID})
		imagescenario.AssertCode(t, err, codes.Unauthenticated)
		_, err = u.disks.List(ctx, &computepb.ListDisksRequest{ParentId: u.project})
		imagescenario.AssertCode(t, err, codes.Unauthenticated)
		_, err = u.createDisk(ctx, "anon", ssdSpec(1))
		imagescenario.AssertCode(t, err, codes.Unauthenticated)
		_, err = u.snaps.Get(ctx, &computepb.GetDiskSnapshotRequest{Id: snapID})
		imagescenario.AssertCode(t, err, codes.Unauthenticated)
		_, err = u.createSnap(ctx, u.project, "anon", fixtureID, "")
		imagescenario.AssertCode(t, err, codes.Unauthenticated)
	})
}
