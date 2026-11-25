package domain

import (
	"fmt"
	"math"
	"math/bits"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Disk rules from https://docs.nebius.com/compute/storage/types and
// https://docs.nebius.com/compute/storage/manage (verified 2026-11):
//   - Network SSD: 1–8192 GiB.
//   - Network SSD NRD and IO M3: 93–262,074 GiB, in multiples of 93 GiB.
//   - Sizes are multiples of both 4 MiB and the block size; a disk holds at
//     most 2^32 blocks (so 16 TiB with the default 4 KiB blocks).
//   - Block size: a power of two from 4 KiB to 128 KiB, default 4 KiB, immutable.
//   - Boot disks (created from an image) cannot exceed 30,720 GiB.
//   - Disks only grow.
//   - DISK_ENCRYPTION_MANAGED is available for NRD and IO M3 only (proto
//     comment); Network SSD is always encrypted by the platform.
//
// NETWORK_HDD is in the proto enum ("the list of available types will be
// clarified later") but the docs list no HDD disk type, so it is rejected as
// unavailable rather than given invented limits.
const (
	kib = int64(1) << 10
	mib = int64(1) << 20
	// gib is declared in diskimage.go.

	// DefaultBlockSizeBytes is the block size of a disk that does not set one.
	DefaultBlockSizeBytes = 4 * kib
	minBlockSizeBytes     = 4 * kib
	maxBlockSizeBytes     = 128 * kib
	sizeGranularity       = 4 * mib
	maxBlocks             = int64(1) << 32
	maxBootDiskBytes      = 30720 * gib
	nrdNamespaceBytes     = 93 * gib
)

// typeLimits are the size limits of a disk type.
type typeLimits struct {
	min, max, multiple int64
	encryptable        bool
}

var diskTypeLimits = map[computepb.DiskSpec_DiskType]typeLimits{
	computepb.DiskSpec_NETWORK_SSD:                {min: 1 * gib, max: 8192 * gib},
	computepb.DiskSpec_NETWORK_SSD_NON_REPLICATED: {min: 93 * gib, max: 262074 * gib, multiple: nrdNamespaceBytes, encryptable: true},
	computepb.DiskSpec_NETWORK_SSD_IO_M3:          {min: 93 * gib, max: 262074 * gib, multiple: nrdNamespaceBytes, encryptable: true},
}

// SizeBytes normalizes the size oneof of a disk spec to bytes.
func SizeBytes(spec *computepb.DiskSpec) (int64, error) {
	var (
		n     int64
		unit  int64
		field string
	)
	switch s := spec.GetSize().(type) {
	case *computepb.DiskSpec_SizeBytes:
		n, unit, field = s.SizeBytes, 1, "size_bytes"
	case *computepb.DiskSpec_SizeKibibytes:
		n, unit, field = s.SizeKibibytes, kib, "size_kibibytes"
	case *computepb.DiskSpec_SizeMebibytes:
		n, unit, field = s.SizeMebibytes, mib, "size_mebibytes"
	case *computepb.DiskSpec_SizeGibibytes:
		n, unit, field = s.SizeGibibytes, gib, "size_gibibytes"
	default:
		return 0, errs.BadRequest(errs.Violation{
			Field: "spec.size", Message: "exactly one of size_bytes, size_kibibytes, size_mebibytes, size_gibibytes is required",
		})
	}
	if n <= 0 {
		return 0, errs.BadRequest(errs.Violation{Field: "spec." + field, Message: "must be positive"})
	}
	if n > math.MaxInt64/unit {
		return 0, errs.BadRequest(errs.Violation{Field: "spec." + field, Message: "is too large"})
	}
	return n * unit, nil
}

// EffectiveBlockSize validates spec.block_size_bytes and applies the default.
func EffectiveBlockSize(requested int64) (int64, error) {
	if requested == 0 {
		return DefaultBlockSizeBytes, nil
	}
	if requested < minBlockSizeBytes || requested > maxBlockSizeBytes || bits.OnesCount64(uint64(requested)) != 1 {
		return 0, errs.BadRequest(errs.Violation{
			Field:   "spec.block_size_bytes",
			Message: fmt.Sprintf("must be a power of two between %d and %d, got %d", minBlockSizeBytes, maxBlockSizeBytes, requested),
		})
	}
	return requested, nil
}

// checkType validates the disk type and its encryption setting.
func checkType(spec *computepb.DiskSpec) error {
	t := spec.GetType()
	if t == computepb.DiskSpec_UNSPECIFIED {
		return errs.BadRequest(errs.Violation{Field: "spec.type", Message: "required"})
	}
	lim, ok := diskTypeLimits[t]
	if !ok {
		return errs.BadRequest(errs.Violation{Field: "spec.type", Message: fmt.Sprintf("disk type %s is not available", t)})
	}
	switch e := spec.GetDiskEncryption().GetType(); e {
	case computepb.DiskEncryption_DISK_ENCRYPTION_UNSPECIFIED:
	case computepb.DiskEncryption_DISK_ENCRYPTION_MANAGED:
		if !lim.encryptable {
			return errs.BadRequest(errs.Violation{
				Field:   "spec.disk_encryption.type",
				Message: fmt.Sprintf("DISK_ENCRYPTION_MANAGED is available for NETWORK_SSD_NON_REPLICATED and NETWORK_SSD_IO_M3 disks only, not %s", t),
			})
		}
	default:
		return errs.BadRequest(errs.Violation{Field: "spec.disk_encryption.type", Message: fmt.Sprintf("unknown encryption type %d", e)})
	}
	return nil
}

// checkSize validates a normalized size against the type, block size and boot limits.
func checkSize(t computepb.DiskSpec_DiskType, blockSize, size int64, boot bool) error {
	lim := diskTypeLimits[t]
	bad := func(format string, a ...any) error {
		return errs.BadRequest(errs.Violation{Field: "spec.size", Message: fmt.Sprintf(format, a...)})
	}
	switch {
	case size < lim.min || size > lim.max:
		return bad("%s disks must be between %d and %d GiB, got %d bytes", t, lim.min/gib, lim.max/gib, size)
	case lim.multiple != 0 && size%lim.multiple != 0:
		return bad("%s disk size must be a multiple of %d GiB, got %d bytes", t, lim.multiple/gib, size)
	case size%sizeGranularity != 0:
		return bad("disk size must be a multiple of 4 MiB, got %d bytes", size)
	case size%blockSize != 0:
		return bad("disk size must be a multiple of the block size %d, got %d bytes", blockSize, size)
	case size/blockSize > maxBlocks:
		return bad("a disk holds at most %d blocks: with %d-byte blocks the maximum size is %d GiB", maxBlocks, blockSize, maxBlocks*blockSize/gib)
	case boot && size > maxBootDiskBytes:
		return bad("boot disks cannot exceed %d GiB", maxBootDiskBytes/gib)
	}
	return nil
}
