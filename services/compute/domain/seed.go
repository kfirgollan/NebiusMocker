package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
)

// syntheticPayloadSize is the size of each seeded public image blob: small,
// but a real object with a real checksum.
const syntheticPayloadSize = 32 << 10

// SeedDeps are what seeding the public catalog needs.
type SeedDeps struct {
	Images ImageRepository
	Blobs  Blobs
	Clock  clock.Clock
}

// SeedPublicCatalog creates every catalog version as a READY public image in
// each region's public-images project, with a synthetic qcow2 payload in the
// image store. It is idempotent: existing images and blobs with the right
// checksum are kept. IDs are derived from (region, family, version), so they
// are stable across restarts.
func SeedPublicCatalog(ctx context.Context, d SeedDeps, cat Catalog, regions []Region) error {
	if err := cat.CheckRegions(regions); err != nil {
		return err
	}
	for _, r := range regions {
		for i := range cat.Families {
			f := &cat.Families[i]
			if !f.inRegion(r.Name) {
				continue
			}
			for _, v := range f.Versions {
				if err := seedVersion(ctx, d, r, f, v); err != nil {
					return fmt.Errorf("seed image %s/%s in %s: %w", f.Name, v.Version, r.Name, err)
				}
			}
		}
	}
	return nil
}

func seedVersion(ctx context.Context, d SeedDeps, r Region, f *Family, v Version) error {
	id, err := nid.Deterministic(KindImage, r.RoutingCode, f.Name+"/"+v.Version)
	if err != nil {
		return err
	}
	minDisk := max(f.MinDiskSizeGiB, 1) * gib
	payload := SyntheticQCOW2(minDisk, syntheticPayloadSize, f.Name+"/"+v.Version)
	insp := NewInspector()
	_, _ = insp.Write(payload) // never fails
	in := insp.Result()
	if err := ensureBlob(ctx, d.Blobs, id, payload, in); err != nil {
		return err
	}
	if _, err := d.Images.Get(ctx, id); err == nil {
		return nil
	} else if !errs.IsKind(err, errs.KindNotFound) {
		return err
	}
	return d.Images.Create(ctx, publicImage(id, r, f, v, in, d.Clock.Now()))
}

func ensureBlob(ctx context.Context, blobs Blobs, id string, payload []byte, in Inspection) error {
	info, err := blobs.StatImage(ctx, id)
	switch {
	case err == nil && info.Size == in.Size && info.Metadata[MetaSHA256] == in.SHA256:
		return nil // already stored by an earlier boot (persistent object store)
	case err != nil && !errs.IsKind(err, errs.KindNotFound):
		return err
	}
	stored, err := blobs.PutImage(ctx, id, bytes.NewReader(payload), in.Size, blobMetadata(id, in))
	if err != nil {
		return err
	}
	if stored.Size != in.Size {
		return errors.New("stored payload size mismatch")
	}
	return nil
}

func publicImage(id string, r Region, f *Family, v Version, in Inspection, now time.Time) *computepb.Image {
	arch, _ := parseArch(f.CPUArchitecture) // validated by ParseCatalog
	created := timestamppb.New(v.releasedAt(now))
	img := &computepb.Image{
		Metadata: &commonpb.ResourceMetadata{
			Id:              id,
			ParentId:        PublicProjectID(r.RoutingCode),
			Name:            f.Name + "-" + v.Version,
			Labels:          maps.Clone(f.Labels),
			ResourceVersion: 1,
			CreatedAt:       created,
			UpdatedAt:       created,
		},
		Spec: &computepb.ImageSpec{
			Description:              f.Description,
			ImageFamily:              f.Name,
			Version:                  v.Version,
			CpuArchitecture:          arch,
			ImageFamilyHumanReadable: f.HumanReadable,
			RecommendedPlatforms:     slices.Clone(f.RecommendedPlatforms),
			UnsupportedPlatforms:     maps.Clone(f.UnsupportedPlatforms),
			// No source: public images are published by Nebius, not imported.
		},
		Status: &computepb.ImageStatus{
			State:            computepb.ImageStatus_READY,
			StorageSizeBytes: in.Size,
			MinDiskSizeBytes: in.MinDiskSizeBytes(),
		},
	}
	dep := v.Deprecation
	if dep == nil {
		dep = f.Deprecation
	}
	if dep != nil {
		img.Status.ImageFamilyDeprecation = &computepb.ImageStatus_ImageFamilyDeprecationStatus{
			DeprecatedAt: timestamppb.New(dep.At), Message: dep.Message,
		}
	}
	return img
}
