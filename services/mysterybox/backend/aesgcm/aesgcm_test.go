package aesgcm_test

import (
	"bytes"
	"testing"

	"github.com/nebius/gosdk/nid"

	"github.com/kfirgollan/NebiusMocker/services/mysterybox/backend/aesgcm"
)

func newSealer(t *testing.T) *aesgcm.Sealer {
	t.Helper()
	s, err := aesgcm.New(bytes.Repeat([]byte{7}, aesgcm.KeySize))
	if err != nil {
		t.Fatalf("aesgcm.New: %v", err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	s := newSealer(t)
	aad := []byte("mbsecver-1")
	tests := []struct {
		name  string
		plain []byte
	}{
		{name: "text", plain: []byte("hunter2")},
		{name: "empty", plain: nil},
		{name: "binary", plain: []byte{0, 0xff, 0, 1}},
		{name: "large", plain: bytes.Repeat([]byte("x"), 1<<16)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sealed, err := s.Seal(t.Context(), tt.plain, aad)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			if len(tt.plain) > 0 && bytes.Contains(sealed, tt.plain) {
				t.Error("sealed payload contains the plaintext")
			}
			got, err := s.Open(t.Context(), sealed, aad)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if !bytes.Equal(got, tt.plain) {
				t.Errorf("Open = %q, want %q", got, tt.plain)
			}
		})
	}
}

func TestOpenFails(t *testing.T) {
	t.Parallel()
	s := newSealer(t)
	aad := []byte("mbsecver-1")
	sealed, err := s.Seal(t.Context(), []byte("hunter2"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	other, err := aesgcm.New(bytes.Repeat([]byte{9}, aesgcm.KeySize))
	if err != nil {
		t.Fatalf("aesgcm.New: %v", err)
	}
	flip := func(i int) []byte {
		b := bytes.Clone(sealed)
		b[i] ^= 1
		return b
	}
	tests := []struct {
		name   string
		sealer *aesgcm.Sealer
		sealed []byte
		aad    []byte
	}{
		{name: "wrong aad", sealer: s, sealed: sealed, aad: []byte("mbsecver-2")},
		{name: "nil aad", sealer: s, sealed: sealed},
		{name: "tampered nonce", sealer: s, sealed: flip(0), aad: aad},
		{name: "tampered ciphertext", sealer: s, sealed: flip(len(sealed) - 1), aad: aad},
		{name: "truncated", sealer: s, sealed: sealed[:len(sealed)-1], aad: aad},
		{name: "too short", sealer: s, sealed: []byte{1, 2, 3}, aad: aad},
		{name: "empty", sealer: s, sealed: nil, aad: aad},
		{name: "other key", sealer: other, sealed: sealed, aad: aad},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got, err := tt.sealer.Open(t.Context(), tt.sealed, tt.aad); err == nil {
				t.Errorf("Open = %q, nil; want an error", got)
			}
		})
	}
}

func TestNewRejectsWrongKeyLength(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 16, 24, 31, 33, 64} {
		if _, err := aesgcm.New(make([]byte, n)); err == nil {
			t.Errorf("New(%d-byte key) = nil error, want an error", n)
		}
	}
	if _, err := aesgcm.New(make([]byte, aesgcm.KeySize)); err != nil {
		t.Errorf("New(%d-byte key): %v", aesgcm.KeySize, err)
	}
}

func TestSealIsRandomized(t *testing.T) {
	t.Parallel()
	s := newSealer(t)
	a, err := s.Seal(t.Context(), []byte("same"), []byte("aad"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	b, err := s.Seal(t.Context(), []byte("same"), []byte("aad"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Error("two Seals of the same plaintext are identical, want distinct nonces")
	}
}

func TestNewRandom(t *testing.T) {
	t.Parallel()
	a, err := aesgcm.NewRandom()
	if err != nil {
		t.Fatalf("NewRandom: %v", err)
	}
	b, err := aesgcm.NewRandom()
	if err != nil {
		t.Fatalf("NewRandom: %v", err)
	}
	sealed, err := a.Seal(t.Context(), []byte("x"), nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := a.Open(t.Context(), sealed, nil); err != nil {
		t.Errorf("Open with own key: %v", err)
	}
	if _, err := b.Open(t.Context(), sealed, nil); err == nil {
		t.Error("Open with another random key succeeded, want an error")
	}
}

func TestKeyID(t *testing.T) {
	t.Parallel()
	id := newSealer(t).KeyID()
	if id != aesgcm.InternalKeyID {
		t.Errorf("KeyID = %q, want %q", id, aesgcm.InternalKeyID)
	}
	parsed, err := nid.Parse(id)
	if err != nil {
		t.Fatalf("nid.Parse(%q): %v", id, err)
	}
	if err := parsed.Validate(); err != nil {
		t.Errorf("Validate(%q): %v", id, err)
	}
	if parsed.Type != "kmssymkey" {
		t.Errorf("type = %q, want kmssymkey", parsed.Type)
	}
	if parsed.String() != id {
		t.Errorf("round trip = %q, want %q", parsed.String(), id)
	}
}
