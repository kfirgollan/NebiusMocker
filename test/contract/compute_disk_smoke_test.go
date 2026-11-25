package contract

import (
	"testing"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
)

// TestDiskSmoke is the realistic flow: disk from a public image family →
// snapshot → disk from the snapshot → teardown.
func TestDiskSmoke(t *testing.T) {
	t.Parallel()
	env := startImages(t)
	ctx := t.Context()
	sdk := env.Inst.SDK(t)
	disks, snaps := computesvc.NewDiskService(sdk), computesvc.NewDiskSnapshotService(sdk)

	op, err := disks.Create(ctx, &computepb.CreateDiskRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: config.DefaultProjectID, Name: "boot"},
		Spec: &computepb.DiskSpec{
			Type: computepb.DiskSpec_NETWORK_SSD,
			Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 32},
			Source: &computepb.DiskSpec_SourceImageFamily{SourceImageFamily: &computepb.SourceImageFamily{
				ImageFamily: "ubuntu24.04-driverless",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	boot, err := disks.Get(ctx, &computepb.GetDiskRequest{Id: op.ResourceID()})
	if err != nil {
		t.Fatal(err)
	}
	if boot.GetStatus().GetState() != computepb.DiskStatus_READY || boot.GetStatus().GetSizeBytes() != 32<<30 ||
		boot.GetStatus().GetSourceImageId() == "" || boot.GetStatus().GetBlockSizeBytes() != 4096 {
		t.Fatalf("boot disk = %v", boot)
	}

	sop, err := snaps.Create(ctx, &computepb.CreateDiskSnapshotRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: config.DefaultProjectID, Name: "snap"},
		Spec:     &computepb.DiskSnapshotSpec{SourceDiskId: boot.GetMetadata().GetId()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = snaps.Create(ctx, &computepb.CreateDiskSnapshotRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: config.DefaultProjectID, Name: "orphan"},
		Spec:     &computepb.DiskSnapshotSpec{SourceDiskId: "computedisk-e00missing0000000000"},
	})
	imagescenario.AssertCode(t, err, codes.NotFound)

	_, err = disks.Create(ctx, &computepb.CreateDiskRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: config.DefaultProjectID, Name: "too-small"},
		Spec: &computepb.DiskSpec{
			Type:   computepb.DiskSpec_NETWORK_SSD,
			Size:   &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 16},
			Source: &computepb.DiskSpec_SourceSnapshotId{SourceSnapshotId: sop.ResourceID()},
		},
	})
	imagescenario.AssertCode(t, err, codes.InvalidArgument)

	cop, err := disks.Create(ctx, &computepb.CreateDiskRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: config.DefaultProjectID, Name: "restored"},
		Spec: &computepb.DiskSpec{
			Type:   computepb.DiskSpec_NETWORK_SSD,
			Size:   &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 64},
			Source: &computepb.DiskSpec_SourceSnapshotId{SourceSnapshotId: sop.ResourceID()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{cop.ResourceID(), boot.GetMetadata().GetId()} {
		dop, err := disks.Delete(ctx, &computepb.DeleteDiskRequest{Id: id})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dop.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	dop, err := snaps.Delete(ctx, &computepb.DeleteDiskSnapshotRequest{Id: sop.ResourceID()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
