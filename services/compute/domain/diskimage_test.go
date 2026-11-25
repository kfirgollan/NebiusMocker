package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

func vmdkHeader(sectors uint64) []byte {
	b := make([]byte, 1024)
	copy(b, "KDMV")
	binary.LittleEndian.PutUint32(b[4:], 1)
	binary.LittleEndian.PutUint64(b[12:], sectors)
	return b
}

func vhdFooter(size uint64) []byte {
	b := make([]byte, footerLen)
	copy(b, "conectix")
	binary.BigEndian.PutUint64(b[48:], size)
	return b
}

func TestInspector(t *testing.T) {
	t.Parallel()
	fixed := append(bytes.Repeat([]byte{1}, 3000), vhdFooter(7<<30)...)
	tests := []struct {
		name        string
		data        []byte
		wantFormat  DiskFormat
		wantVirtual int64
		wantMinDisk int64
	}{
		{name: "qcow2", data: SyntheticQCOW2(20<<30, 8192, "s"), wantFormat: FormatQCOW2, wantVirtual: 20 << 30, wantMinDisk: 20 << 30},
		{name: "qcow2 odd size", data: SyntheticQCOW2(3<<30+5, 8192, "s"), wantFormat: FormatQCOW2, wantVirtual: 3<<30 + 5, wantMinDisk: 4 << 30},
		{name: "vmdk", data: vmdkHeader(4 << 21), wantFormat: FormatVMDK, wantVirtual: 4 << 30, wantMinDisk: 4 << 30},
		{name: "dynamic vhd", data: append(vhdFooter(2<<30), make([]byte, 600)...), wantFormat: FormatVHD, wantVirtual: 2 << 30, wantMinDisk: 2 << 30},
		{name: "fixed vhd", data: fixed, wantFormat: FormatVHD, wantVirtual: 7 << 30, wantMinDisk: 7 << 30},
		{name: "raw", data: bytes.Repeat([]byte{9}, 5000), wantFormat: FormatRaw, wantVirtual: 5000, wantMinDisk: 1 << 30},
		{name: "empty", data: nil, wantFormat: FormatRaw, wantVirtual: 0, wantMinDisk: 1 << 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			insp := NewInspector()
			for chunk := range slicesChunk(tt.data, 7) { // tiny writes cross every boundary
				_, _ = insp.Write(chunk)
			}
			got := insp.Result()
			sum := sha256.Sum256(tt.data)
			if got.Format != tt.wantFormat || got.VirtualSize != tt.wantVirtual || got.MinDiskSizeBytes() != tt.wantMinDisk ||
				got.Size != int64(len(tt.data)) || got.SHA256 != hex.EncodeToString(sum[:]) {
				t.Fatalf("Result() = %+v (min disk %d), want %s virtual %d min %d", got, got.MinDiskSizeBytes(), tt.wantFormat, tt.wantVirtual, tt.wantMinDisk)
			}
		})
	}
}

func slicesChunk(b []byte, n int) func(func([]byte) bool) {
	return func(yield func([]byte) bool) {
		for len(b) > 0 {
			k := min(n, len(b))
			if !yield(b[:k]) {
				return
			}
			b = b[k:]
		}
	}
}

func TestSyntheticQCOW2IsDeterministic(t *testing.T) {
	t.Parallel()
	a, b := SyntheticQCOW2(1<<30, 32<<10, "x"), SyntheticQCOW2(1<<30, 32<<10, "x")
	if !bytes.Equal(a, b) || len(a) != 32<<10 || bytes.Equal(a, SyntheticQCOW2(1<<30, 32<<10, "y")) {
		t.Fatal("SyntheticQCOW2 is not deterministic per seed")
	}
}

// FuzzInspector: the inspector parses untrusted upload headers; it must never
// panic and always report the exact size.
func FuzzInspector(f *testing.F) {
	f.Add(SyntheticQCOW2(1<<30, 4096, "s"))
	f.Add(vmdkHeader(1))
	f.Add(vhdFooter(1))
	f.Add([]byte("QFI\xfb"))
	f.Fuzz(func(t *testing.T, data []byte) {
		insp := NewInspector()
		_, _ = insp.Write(data)
		got := insp.Result()
		if got.Size != int64(len(data)) || got.MinDiskSizeBytes() <= 0 {
			t.Fatalf("Result() = %+v for %d bytes", got, len(data))
		}
	})
}
