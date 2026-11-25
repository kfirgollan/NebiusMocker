package domain

import (
	"math"
	"testing"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func TestSizeBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		spec     *computepb.DiskSpec
		want     int64
		wantKind errs.Kind
	}{
		{name: "bytes", spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeBytes{SizeBytes: 12345}}, want: 12345},
		{name: "kibibytes", spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeKibibytes{SizeKibibytes: 3}}, want: 3 * kib},
		{name: "mebibytes", spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeMebibytes{SizeMebibytes: 3}}, want: 3 * mib},
		{name: "gibibytes", spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: 3}}, want: 3 * gib},
		{name: "none set", spec: &computepb.DiskSpec{}, wantKind: errs.KindBadRequest},
		{name: "nil spec", spec: nil, wantKind: errs.KindBadRequest},
		{name: "zero bytes", spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeBytes{}}, wantKind: errs.KindBadRequest},
		{name: "zero gibibytes", spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeGibibytes{}}, wantKind: errs.KindBadRequest},
		{name: "negative bytes", spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeBytes{SizeBytes: -1}}, wantKind: errs.KindBadRequest},
		{name: "negative gibibytes", spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: -5}}, wantKind: errs.KindBadRequest},
		{name: "max bytes", spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeBytes{SizeBytes: math.MaxInt64}}, want: math.MaxInt64},
		{name: "kibibytes overflow", spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeKibibytes{SizeKibibytes: math.MaxInt64}}, wantKind: errs.KindBadRequest},
		{name: "mebibytes overflow", spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeMebibytes{SizeMebibytes: math.MaxInt64 / 2}}, wantKind: errs.KindBadRequest},
		{name: "gibibytes overflow", spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: math.MaxInt64}}, wantKind: errs.KindBadRequest},
		{name: "gibibytes largest", spec: &computepb.DiskSpec{Size: &computepb.DiskSpec_SizeGibibytes{SizeGibibytes: math.MaxInt64 / gib}}, want: (math.MaxInt64 / gib) * gib},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := SizeBytes(tt.spec)
			if k := errs.KindOf(err); k != tt.wantKind {
				t.Fatalf("SizeBytes kind = %v, want %v (err: %v)", k, tt.wantKind, err)
			}
			if err == nil && got != tt.want {
				t.Fatalf("SizeBytes = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEffectiveBlockSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		requested int64
		want      int64
		wantKind  errs.Kind
	}{
		{name: "default", requested: 0, want: 4096},
		{name: "minimum", requested: 4096, want: 4096},
		{name: "8 KiB", requested: 8192, want: 8192},
		{name: "maximum", requested: 131072, want: 131072},
		{name: "below minimum", requested: 2048, wantKind: errs.KindBadRequest},
		{name: "above maximum", requested: 262144, wantKind: errs.KindBadRequest},
		{name: "not a power of two", requested: 3000, wantKind: errs.KindBadRequest},
		{name: "between powers of two", requested: 12288, wantKind: errs.KindBadRequest},
		{name: "negative", requested: -4096, wantKind: errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := EffectiveBlockSize(tt.requested)
			if k := errs.KindOf(err); k != tt.wantKind {
				t.Fatalf("EffectiveBlockSize(%d) kind = %v, want %v (err: %v)", tt.requested, k, tt.wantKind, err)
			}
			if err == nil && got != tt.want {
				t.Fatalf("EffectiveBlockSize(%d) = %d, want %d", tt.requested, got, tt.want)
			}
		})
	}
}

func TestCheckType(t *testing.T) {
	t.Parallel()
	managed := &computepb.DiskEncryption{Type: computepb.DiskEncryption_DISK_ENCRYPTION_MANAGED}
	tests := []struct {
		name     string
		spec     *computepb.DiskSpec
		wantKind errs.Kind
	}{
		{name: "ssd", spec: &computepb.DiskSpec{Type: computepb.DiskSpec_NETWORK_SSD}},
		{name: "nrd", spec: &computepb.DiskSpec{Type: computepb.DiskSpec_NETWORK_SSD_NON_REPLICATED}},
		{name: "io m3", spec: &computepb.DiskSpec{Type: computepb.DiskSpec_NETWORK_SSD_IO_M3}},
		{name: "hdd rejected", spec: &computepb.DiskSpec{Type: computepb.DiskSpec_NETWORK_HDD}, wantKind: errs.KindBadRequest},
		{name: "unspecified", spec: &computepb.DiskSpec{}, wantKind: errs.KindBadRequest},
		{name: "nil spec", spec: nil, wantKind: errs.KindBadRequest},
		{name: "managed encryption on ssd", spec: &computepb.DiskSpec{Type: computepb.DiskSpec_NETWORK_SSD, DiskEncryption: managed}, wantKind: errs.KindBadRequest},
		{name: "managed encryption on nrd", spec: &computepb.DiskSpec{Type: computepb.DiskSpec_NETWORK_SSD_NON_REPLICATED, DiskEncryption: managed}},
		{name: "managed encryption on io m3", spec: &computepb.DiskSpec{Type: computepb.DiskSpec_NETWORK_SSD_IO_M3, DiskEncryption: managed}},
		{
			name: "unspecified encryption on ssd",
			spec: &computepb.DiskSpec{Type: computepb.DiskSpec_NETWORK_SSD, DiskEncryption: &computepb.DiskEncryption{}},
		},
		{
			name: "unknown encryption type",
			spec: &computepb.DiskSpec{
				Type:           computepb.DiskSpec_NETWORK_SSD_NON_REPLICATED,
				DiskEncryption: &computepb.DiskEncryption{Type: computepb.DiskEncryption_DiskEncryptionType(99)},
			},
			wantKind: errs.KindBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if k := errs.KindOf(checkType(tt.spec)); k != tt.wantKind {
				t.Fatalf("checkType kind = %v, want %v", k, tt.wantKind)
			}
		})
	}
}

func TestCheckSize(t *testing.T) {
	t.Parallel()
	const (
		ssd = computepb.DiskSpec_NETWORK_SSD
		nrd = computepb.DiskSpec_NETWORK_SSD_NON_REPLICATED
		m3  = computepb.DiskSpec_NETWORK_SSD_IO_M3
	)
	tests := []struct {
		name     string
		typ      computepb.DiskSpec_DiskType
		block    int64
		size     int64
		boot     bool
		wantKind errs.Kind
	}{
		{name: "ssd minimum", typ: ssd, block: 4096, size: 1 * gib},
		{name: "ssd below minimum", typ: ssd, block: 4096, size: 1*gib - 4*mib, wantKind: errs.KindBadRequest},
		{name: "ssd maximum", typ: ssd, block: 4096, size: 8192 * gib},
		{name: "ssd above maximum", typ: ssd, block: 4096, size: 8192*gib + 4*mib, wantKind: errs.KindBadRequest},
		{name: "ssd zero", typ: ssd, block: 4096, size: 0, wantKind: errs.KindBadRequest},
		{name: "ssd not a multiple of 4 MiB", typ: ssd, block: 4096, size: 1*gib + 1*mib, wantKind: errs.KindBadRequest},
		{name: "ssd not a multiple of block size", typ: ssd, block: 3000, size: 1 * gib, wantKind: errs.KindBadRequest},
		{name: "ssd 128 KiB blocks", typ: ssd, block: 131072, size: 100 * gib},
		{name: "nrd minimum", typ: nrd, block: 4096, size: 93 * gib},
		{name: "nrd below minimum", typ: nrd, block: 4096, size: 92 * gib, wantKind: errs.KindBadRequest},
		{name: "nrd 186 GiB", typ: nrd, block: 4096, size: 186 * gib},
		{name: "nrd 94 GiB", typ: nrd, block: 4096, size: 94 * gib, wantKind: errs.KindBadRequest},
		{name: "nrd 100 GiB", typ: nrd, block: 4096, size: 100 * gib, wantKind: errs.KindBadRequest},
		{name: "nrd maximum", typ: nrd, block: 65536, size: 262074 * gib},
		{name: "nrd above maximum", typ: nrd, block: 65536, size: 262074*gib + 93*gib, wantKind: errs.KindBadRequest},
		{name: "nrd 16 TiB class multiple with 4 KiB blocks", typ: nrd, block: 4096, size: 177 * 93 * gib, wantKind: errs.KindBadRequest},
		{name: "nrd 16 TiB class multiple with 8 KiB blocks", typ: nrd, block: 8192, size: 177 * 93 * gib},
		{name: "nrd below the block cap with 4 KiB blocks", typ: nrd, block: 4096, size: 176 * 93 * gib},
		{name: "io m3 minimum", typ: m3, block: 4096, size: 93 * gib},
		{name: "io m3 94 GiB", typ: m3, block: 4096, size: 94 * gib, wantKind: errs.KindBadRequest},
		{name: "io m3 block cap with 4 KiB blocks", typ: m3, block: 4096, size: 177 * 93 * gib, wantKind: errs.KindBadRequest},
		{name: "nrd boot at the cap", typ: nrd, block: 8192, size: 330 * 93 * gib, boot: true},
		{name: "nrd boot above the cap", typ: nrd, block: 8192, size: 331 * 93 * gib, boot: true, wantKind: errs.KindBadRequest},
		{name: "nrd non-boot at the same size", typ: nrd, block: 8192, size: 331 * 93 * gib},
		{name: "ssd boot unaffected below the cap", typ: ssd, block: 4096, size: 8192 * gib, boot: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkSize(tt.typ, tt.block, tt.size, tt.boot)
			if k := errs.KindOf(err); k != tt.wantKind {
				t.Fatalf("checkSize(%v, %d, %d, boot=%v) kind = %v, want %v (err: %v)", tt.typ, tt.block, tt.size, tt.boot, k, tt.wantKind, err)
			}
		})
	}
}
