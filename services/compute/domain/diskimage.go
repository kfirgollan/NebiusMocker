package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
)

// DiskFormat is the container format of an image payload. Nebius imports
// .qcow2, .vmdk, .vhd and .raw files (docs: compute/storage/import-image).
type DiskFormat string

// Supported formats.
const (
	FormatRaw   DiskFormat = "raw"
	FormatQCOW2 DiskFormat = "qcow2"
	FormatVMDK  DiskFormat = "vmdk"
	FormatVHD   DiskFormat = "vhd"
)

const (
	gib        = int64(1) << 30
	sectorSize = 512
	headLen    = 512 // bytes kept from the start of the payload
	footerLen  = 512 // VHD footer size
)

var (
	qcow2Magic = []byte{'Q', 'F', 'I', 0xfb}
	vmdkMagic  = []byte("KDMV") // sparse extent magic 0x564d444b, little-endian
	vhdCookie  = []byte("conectix")
)

// Inspection is what Inspector learned from a payload.
type Inspection struct {
	Format DiskFormat
	// Size is the number of payload bytes (Nebius status.storage_size_bytes).
	Size int64
	// VirtualSize is the size of the disk the payload describes.
	VirtualSize int64
	// SHA256 is the lowercase hex digest of the payload.
	SHA256 string
}

// MinDiskSizeBytes is the smallest disk the image fits on: its virtual size
// rounded up to a whole GiB (Nebius disk sizes are allocated in GiB).
func (in Inspection) MinDiskSizeBytes() int64 {
	return roundUp(max(in.VirtualSize, 1), gib)
}

// Inspector is an io.Writer that hashes a payload and detects its format and
// virtual size from the first and last bytes, so a single streaming pass is
// enough. The zero value is not usable; call NewInspector.
type Inspector struct {
	h    hash.Hash
	n    int64
	head []byte
	tail []byte // last footerLen bytes seen
}

// NewInspector returns an empty Inspector.
func NewInspector() *Inspector { return &Inspector{h: sha256.New()} }

// Write implements io.Writer; it never fails.
func (i *Inspector) Write(p []byte) (int, error) {
	i.h.Write(p)
	i.n += int64(len(p))
	if need := headLen - len(i.head); need > 0 {
		i.head = append(i.head, p[:min(need, len(p))]...)
	}
	i.tail = append(i.tail, p...)
	if extra := len(i.tail) - footerLen; extra > 0 {
		i.tail = append(i.tail[:0], i.tail[extra:]...)
	}
	return len(p), nil
}

// Result returns the inspection of everything written so far.
func (i *Inspector) Result() Inspection {
	in := Inspection{Format: FormatRaw, Size: i.n, VirtualSize: i.n, SHA256: hex.EncodeToString(i.h.Sum(nil))}
	switch {
	case len(i.head) >= 32 && bytes.HasPrefix(i.head, qcow2Magic):
		in.Format = FormatQCOW2
		in.VirtualSize = int64(binary.BigEndian.Uint64(i.head[24:32])) //nolint:gosec // header field; overflow yields a bogus but harmless size
	case len(i.head) >= 20 && bytes.HasPrefix(i.head, vmdkMagic):
		in.Format = FormatVMDK
		in.VirtualSize = int64(binary.LittleEndian.Uint64(i.head[12:20])) * sectorSize //nolint:gosec // header field
	case len(i.head) >= 56 && bytes.HasPrefix(i.head, vhdCookie): // dynamic VHD: footer copy at the start
		in.Format = FormatVHD
		in.VirtualSize = int64(binary.BigEndian.Uint64(i.head[48:56])) //nolint:gosec // header field
	case len(i.tail) == footerLen && bytes.HasPrefix(i.tail, vhdCookie): // fixed VHD: footer only at the end
		in.Format = FormatVHD
		in.VirtualSize = int64(binary.BigEndian.Uint64(i.tail[48:56])) //nolint:gosec // header field
	}
	if in.VirtualSize <= 0 {
		in.VirtualSize = in.Size
	}
	return in
}

// SyntheticQCOW2 builds a small, deterministic qcow2 payload describing a disk
// of virtualSize bytes. It has a valid qcow2 v2 header (so the image pipeline
// detects it like a real upload) followed by filler derived from seed. It is
// not bootable: the mocker never runs images.
func SyntheticQCOW2(virtualSize int64, payloadSize int, seed string) []byte {
	const clusterBits = 16
	buf := make([]byte, max(payloadSize, 4096))
	copy(buf, qcow2Magic)
	binary.BigEndian.PutUint32(buf[4:], 2)                    // version
	binary.BigEndian.PutUint32(buf[20:], clusterBits)         // cluster_bits
	binary.BigEndian.PutUint64(buf[24:], uint64(virtualSize)) //nolint:gosec // callers pass positive sizes
	binary.BigEndian.PutUint64(buf[48:], 1<<clusterBits)      // refcount_table_offset (nominal)
	block := sha256.Sum256([]byte(seed))
	for off := 4096; off < len(buf); off += len(block) {
		block = sha256.Sum256(block[:])
		copy(buf[off:], block[:])
	}
	return buf
}

func roundUp(n, unit int64) int64 {
	return (n + unit - 1) / unit * unit
}
