package domain

import (
	"fmt"
	"math"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Filesystem rules from https://docs.nebius.com/compute/storage/types
// (verified 2026-11):
//   - Only the Network SSD filesystem type is listed. NETWORK_HDD, WEKA and
//     VAST are in the proto enum but not documented as available, so they are
//     rejected rather than given invented limits.
//   - Network SSD filesystems are 1 GiB to 100 PiB. No size multiple is
//     documented, so none is enforced.
//   - The block size rules are the disk ones (EffectiveBlockSize).
//   - Filesystems grow (hot resize) but never shrink; the name, type and
//     block size are fixed after creation.
const (
	minFilesystemBytes = 1 * gib
	maxFilesystemBytes = 104857600 * gib // 100 PiB
)

// FilesystemSizeBytes normalizes the size oneof of a filesystem spec to bytes.
func FilesystemSizeBytes(spec *computepb.FilesystemSpec) (int64, error) {
	var (
		n     int64
		unit  int64
		field string
	)
	switch s := spec.GetSize().(type) {
	case *computepb.FilesystemSpec_SizeBytes:
		n, unit, field = s.SizeBytes, 1, "size_bytes"
	case *computepb.FilesystemSpec_SizeKibibytes:
		n, unit, field = s.SizeKibibytes, kib, "size_kibibytes"
	case *computepb.FilesystemSpec_SizeMebibytes:
		n, unit, field = s.SizeMebibytes, mib, "size_mebibytes"
	case *computepb.FilesystemSpec_SizeGibibytes:
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

// checkFilesystemType validates the filesystem type.
func checkFilesystemType(t computepb.FilesystemSpec_FilesystemType) error {
	switch t {
	case computepb.FilesystemSpec_NETWORK_SSD:
		return nil
	case computepb.FilesystemSpec_UNSPECIFIED:
		return errs.BadRequest(errs.Violation{Field: "spec.type", Message: "required"})
	case computepb.FilesystemSpec_NETWORK_HDD, computepb.FilesystemSpec_WEKA, computepb.FilesystemSpec_VAST:
		return errs.BadRequest(errs.Violation{Field: "spec.type", Message: fmt.Sprintf("filesystem type %s is not available", t)})
	default:
		return errs.BadRequest(errs.Violation{Field: "spec.type", Message: fmt.Sprintf("filesystem type %s is not available", t)})
	}
}

// checkFilesystemSize validates a normalized size against the type limits.
func checkFilesystemSize(size int64) error {
	if size < minFilesystemBytes || size > maxFilesystemBytes {
		return errs.BadRequest(errs.Violation{
			Field: "spec.size", Message: fmt.Sprintf("NETWORK_SSD filesystems must be between %d and %d GiB, got %d bytes",
				minFilesystemBytes/gib, maxFilesystemBytes/gib, size),
		})
	}
	return nil
}
