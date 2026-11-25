package domain_test

import (
	"strings"
	"testing"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

func TestParseS3Class(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    storagepb.StorageClass
		errPart string // empty means success
	}{
		{"STANDARD", storagepb.StorageClass_STANDARD, ""},
		{"ENHANCED_THROUGHPUT", storagepb.StorageClass_ENHANCED_THROUGHPUT, ""},
		{"INTELLIGENT", 0, "not supported"},
		{"GLACIER", 0, "invalid storage class"},
		{"standard", 0, "invalid storage class"},
		{"", 0, "invalid storage class"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := domain.ParseS3Class(tt.in)
			if tt.errPart == "" {
				if err != nil || got != tt.want {
					t.Fatalf("ParseS3Class(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
				}
				return
			}
			if !errs.IsKind(err, errs.KindBadRequest) || !strings.Contains(err.Error(), tt.errPart) {
				t.Fatalf("ParseS3Class(%q) error = %v, want a bad request containing %q", tt.in, err, tt.errPart)
			}
		})
	}
}

func TestStorageClassMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		class     storagepb.StorageClass
		s3Name    string
		backend   string
		roundTrip storagepb.StorageClass
	}{
		{"unspecified is standard", storagepb.StorageClass_STORAGE_CLASS_UNSPECIFIED, "STANDARD", "STANDARD", storagepb.StorageClass_STANDARD},
		{"standard", storagepb.StorageClass_STANDARD, "STANDARD", "STANDARD", storagepb.StorageClass_STANDARD},
		{"enhanced throughput", storagepb.StorageClass_ENHANCED_THROUGHPUT, "ENHANCED_THROUGHPUT", "REDUCED_REDUNDANCY", storagepb.StorageClass_ENHANCED_THROUGHPUT},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := domain.S3ClassName(tt.class); got != tt.s3Name {
				t.Errorf("S3ClassName = %q, want %q", got, tt.s3Name)
			}
			be := domain.BackendClass(tt.class)
			if be != tt.backend {
				t.Errorf("BackendClass = %q, want %q", be, tt.backend)
			}
			if got := domain.NebiusClass(be); got != tt.roundTrip {
				t.Errorf("NebiusClass(%q) = %v, want %v", be, got, tt.roundTrip)
			}
			if got := domain.BackendToS3Class(be); got != domain.S3ClassName(tt.roundTrip) {
				t.Errorf("BackendToS3Class(%q) = %q", be, got)
			}
		})
	}
	if got := domain.NebiusClass("GLACIER"); got != storagepb.StorageClass_STANDARD {
		t.Errorf("NebiusClass(unknown) = %v, want STANDARD", got)
	}
	if got := domain.BackendToS3Class("REDUCED_REDUNDANCY"); got != "ENHANCED_THROUGHPUT" {
		t.Errorf("BackendToS3Class(REDUCED_REDUNDANCY) = %q, want ENHANCED_THROUGHPUT", got)
	}
}
