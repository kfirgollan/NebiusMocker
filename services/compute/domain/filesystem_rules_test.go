package domain

import (
	"math"
	"testing"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func TestFilesystemSizeBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		spec    *computepb.FilesystemSpec
		want    int64
		wantErr bool
	}{
		{name: "bytes", spec: &computepb.FilesystemSpec{Size: &computepb.FilesystemSpec_SizeBytes{SizeBytes: 5}}, want: 5},
		{name: "kibibytes", spec: &computepb.FilesystemSpec{Size: &computepb.FilesystemSpec_SizeKibibytes{SizeKibibytes: 2}}, want: 2048},
		{name: "mebibytes", spec: &computepb.FilesystemSpec{Size: &computepb.FilesystemSpec_SizeMebibytes{SizeMebibytes: 3}}, want: 3 << 20},
		{name: "gibibytes", spec: &computepb.FilesystemSpec{Size: &computepb.FilesystemSpec_SizeGibibytes{SizeGibibytes: 4}}, want: 4 << 30},
		{name: "unset", spec: &computepb.FilesystemSpec{}, wantErr: true},
		{name: "zero", spec: &computepb.FilesystemSpec{Size: &computepb.FilesystemSpec_SizeGibibytes{}}, wantErr: true},
		{name: "negative", spec: &computepb.FilesystemSpec{Size: &computepb.FilesystemSpec_SizeBytes{SizeBytes: -1}}, wantErr: true},
		{name: "kibibytes overflow", spec: &computepb.FilesystemSpec{Size: &computepb.FilesystemSpec_SizeKibibytes{SizeKibibytes: math.MaxInt64}}, wantErr: true},
		{name: "mebibytes overflow", spec: &computepb.FilesystemSpec{Size: &computepb.FilesystemSpec_SizeMebibytes{SizeMebibytes: math.MaxInt64}}, wantErr: true},
		{name: "gibibytes overflow", spec: &computepb.FilesystemSpec{Size: &computepb.FilesystemSpec_SizeGibibytes{SizeGibibytes: math.MaxInt64/gib + 1}}, wantErr: true},
		{name: "bytes max", spec: &computepb.FilesystemSpec{Size: &computepb.FilesystemSpec_SizeBytes{SizeBytes: math.MaxInt64}}, want: math.MaxInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := FilesystemSizeBytes(tc.spec)
			if tc.wantErr {
				if !errs.IsKind(err, errs.KindBadRequest) {
					t.Fatalf("err = %v, want BadRequest", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("FilesystemSizeBytes = %d, %v, want %d", got, err, tc.want)
			}
		})
	}
}

func TestCheckFilesystemSize(t *testing.T) {
	t.Parallel()
	const maxBytes = 104857600 * gib // 100 PiB
	tests := []struct {
		name string
		size int64
		ok   bool
	}{
		{"1 GiB - 1", gib - 1, false},
		{"1 GiB", gib, true},
		{"1 GiB + 1 (no multiple is required)", gib + 1, true},
		{"100 PiB", maxBytes, true},
		{"100 PiB + 1", maxBytes + 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkFilesystemSize(tc.size)
			if tc.ok != (err == nil) {
				t.Fatalf("checkFilesystemSize(%d) = %v, want ok=%v", tc.size, err, tc.ok)
			}
			if err != nil && !errs.IsKind(err, errs.KindBadRequest) {
				t.Fatalf("err = %v, want BadRequest", err)
			}
		})
	}
}

func TestCheckFilesystemType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		typ computepb.FilesystemSpec_FilesystemType
		ok  bool
	}{
		{computepb.FilesystemSpec_NETWORK_SSD, true},
		{computepb.FilesystemSpec_UNSPECIFIED, false},
		{computepb.FilesystemSpec_NETWORK_HDD, false},
		{computepb.FilesystemSpec_WEKA, false},
		{computepb.FilesystemSpec_VAST, false},
		{computepb.FilesystemSpec_FilesystemType(99), false},
	}
	for _, tc := range tests {
		t.Run(tc.typ.String(), func(t *testing.T) {
			t.Parallel()
			err := checkFilesystemType(tc.typ)
			if tc.ok != (err == nil) {
				t.Fatalf("checkFilesystemType(%s) = %v, want ok=%v", tc.typ, err, tc.ok)
			}
			if err != nil && !errs.IsKind(err, errs.KindBadRequest) {
				t.Fatalf("err = %v, want BadRequest", err)
			}
		})
	}
}

func TestFilesystemBlockSize(t *testing.T) {
	t.Parallel()
	for _, n := range []int64{0, 4096, 8192, 131072} {
		if _, err := EffectiveBlockSize(n); err != nil {
			t.Errorf("EffectiveBlockSize(%d) = %v, want ok", n, err)
		}
	}
	for _, n := range []int64{-4096, 1, 2048, 4097, 12288, 262144} {
		_, err := EffectiveBlockSize(n)
		if !errs.IsKind(err, errs.KindBadRequest) {
			t.Errorf("EffectiveBlockSize(%d) = %v, want BadRequest", n, err)
		}
	}
}
