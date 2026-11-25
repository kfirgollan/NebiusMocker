package domain

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"
)

func TestMaterialRedacted(t *testing.T) {
	t.Parallel()
	m, err := newSymMaterial(rand.Reader, kmspb.SymmetricAlgorithm_AES_256)
	if err != nil {
		t.Fatalf("newSymMaterial: %v", err)
	}
	for _, v := range []any{m, asymMaterial{der: []byte("secret-der")}} {
		if got := fmt.Sprint(v); got != "[REDACTED]" {
			t.Errorf("fmt.Sprint(%T) = %q, want [REDACTED]", v, got)
		}
		if got := fmt.Sprintf("%+v|%v", v, v); strings.Contains(got, "secret-der") {
			t.Errorf("fmt output of %T leaks material: %q", v, got)
		}
	}
	if got := m.LogValue(); got.Kind() != slog.KindString || got.String() != "[REDACTED]" {
		t.Errorf("symMaterial.LogValue() = %v, want [REDACTED]", got)
	}
	if got := (asymMaterial{}).LogValue().String(); got != "[REDACTED]" {
		t.Errorf("asymMaterial.LogValue() = %q, want [REDACTED]", got)
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "m", m)
	if !strings.Contains(buf.String(), "[REDACTED]") || strings.Contains(buf.String(), fmt.Sprintf("%v", m.Versions[0].Key)) {
		t.Errorf("slog output = %q, want redacted material", buf.String())
	}
}

func TestSealOpen(t *testing.T) {
	t.Parallel()
	m, err := newSymMaterial(rand.Reader, kmspb.SymmetricAlgorithm_AES_256)
	if err != nil {
		t.Fatal(err)
	}
	pt, aad := []byte("hello world"), []byte("ctx")
	ct, err := m.seal(rand.Reader, "k1", pt, aad)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if ct[0] != ciphertextFormat || binary.BigEndian.Uint32(ct[1:5]) != 1 {
		t.Errorf("header = % x, want format 1 version 1", ct[:5])
	}
	got, err := m.open("k1", ct, aad)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("open = %q, %v, want %q", got, err, pt)
	}

	other, _ := newSymMaterial(rand.Reader, kmspb.SymmetricAlgorithm_AES_256)
	type tc struct {
		name string
		mat  symMaterial
		id   string
		ct   []byte
		aad  []byte
	}
	tests := []tc{
		{"wrong aad", m, "k1", ct, []byte("other")},
		{"missing aad", m, "k1", ct, nil},
		{"wrong key id", m, "k2", ct, aad},
		{"wrong key", other, "k1", ct, aad},
		{"empty", m, "k1", nil, aad},
		{"truncated", m, "k1", ct[:headerSize+5], aad},
	}
	for i := range ct {
		bad := bytes.Clone(ct)
		bad[i] ^= 0x01
		tests = append(tests, tc{fmt.Sprintf("flip byte %d", i), m, "k1", bad, aad})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tt.mat.open(tt.id, tt.ct, tt.aad); err == nil {
				t.Errorf("open(%s) succeeded, want error", tt.name)
			}
		})
	}
}

func TestSymMaterialRotate(t *testing.T) {
	t.Parallel()
	m, _ := newSymMaterial(rand.Reader, kmspb.SymmetricAlgorithm_AES_256)
	old, _ := m.seal(rand.Reader, "k", []byte("old"), nil)
	m2, err := m.rotate(rand.Reader, time.Time{})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	fresh, _ := m2.seal(rand.Reader, "k", []byte("new"), nil)
	if v := binary.BigEndian.Uint32(fresh[1:5]); v != 2 {
		t.Errorf("new ciphertext version = %d, want 2", v)
	}
	if got, err := m2.open("k", old, nil); err != nil || string(got) != "old" {
		t.Errorf("open old after rotate = %q, %v, want old", got, err)
	}
	if _, err := m.open("k", fresh, nil); err == nil {
		t.Error("pre-rotation material opened a version-2 ciphertext")
	}
	if len(m.Versions) != 1 {
		t.Errorf("rotate mutated receiver: %d versions", len(m.Versions))
	}
}

func TestEncodeDecodeSymMaterial(t *testing.T) {
	t.Parallel()
	m, _ := newSymMaterial(rand.Reader, kmspb.SymmetricAlgorithm_AES_256)
	b, err := encodeSymMaterial(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSymMaterial(b)
	if err != nil || got.Primary != 1 || !bytes.Equal(got.Versions[0].Key, m.Versions[0].Key) {
		t.Errorf("round trip = %v, %v", got, err)
	}
	for _, bad := range [][]byte{nil, []byte("{"), []byte(`{"versions":[]}`)} {
		if _, err := decodeSymMaterial(bad); err == nil {
			t.Errorf("decodeSymMaterial(%q) succeeded, want error", bad)
		}
	}
}
