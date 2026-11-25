package domain

import (
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Storage classes. RustFS knows only the S3 classes STANDARD and
// REDUCED_REDUNDANCY, so the mocker stores Nebius ENHANCED_THROUGHPUT objects
// as REDUCED_REDUNDANCY and translates on the way in and out of the S3 front.
// Users never see the backend name. INTELLIGENT (access-based tiering) has no
// backend counterpart and is rejected by name.
const (
	backendStandard = "STANDARD"
	backendEnhanced = "REDUCED_REDUNDANCY"
)

// S3ClassName is the x-amz-storage-class value of a Nebius storage class.
func S3ClassName(c storagepb.StorageClass) string {
	if c == storagepb.StorageClass_STORAGE_CLASS_UNSPECIFIED {
		c = storagepb.StorageClass_STANDARD
	}
	return c.String()
}

// ParseS3Class parses an x-amz-storage-class header value.
func ParseS3Class(s string) (storagepb.StorageClass, error) {
	switch s {
	case "STANDARD":
		return storagepb.StorageClass_STANDARD, nil
	case "ENHANCED_THROUGHPUT":
		return storagepb.StorageClass_ENHANCED_THROUGHPUT, nil
	case "INTELLIGENT":
		return 0, errs.BadRequest(errs.Violation{Field: "x-amz-storage-class", Message: "INTELLIGENT (access-based tiering) is not supported by NebiusMocker"})
	}
	return 0, errs.BadRequest(errs.Violation{Field: "x-amz-storage-class", Message: "invalid storage class " + s})
}

// BackendClass is the object-store class that holds a Nebius storage class.
func BackendClass(c storagepb.StorageClass) string {
	if c == storagepb.StorageClass_ENHANCED_THROUGHPUT {
		return backendEnhanced
	}
	return backendStandard
}

// NebiusClass maps an object-store class back to the Nebius storage class.
func NebiusClass(backend string) storagepb.StorageClass {
	if backend == backendEnhanced {
		return storagepb.StorageClass_ENHANCED_THROUGHPUT
	}
	return storagepb.StorageClass_STANDARD
}

// BackendToS3Class maps an object-store class name to the name users see.
func BackendToS3Class(backend string) string { return S3ClassName(NebiusClass(backend)) }

// checkStorageClass validates a configured storage class.
func checkStorageClass(field string, c storagepb.StorageClass) error {
	switch c {
	case storagepb.StorageClass_STORAGE_CLASS_UNSPECIFIED, storagepb.StorageClass_STANDARD, storagepb.StorageClass_ENHANCED_THROUGHPUT:
		return nil
	case storagepb.StorageClass_INTELLIGENT:
		return errs.BadRequest(errs.Violation{Field: field, Message: "INTELLIGENT (access-based tiering) is not supported by NebiusMocker"})
	case storagepb.StorageClass_FILESYSTEM:
		return errs.BadRequest(errs.Violation{Field: field, Message: "FILESYSTEM is only for filesystem buckets"})
	}
	return errs.BadRequest(errs.Violation{Field: field, Message: "unknown storage class"})
}
