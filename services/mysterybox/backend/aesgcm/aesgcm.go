// Package aesgcm seals MysteryBox payloads with AES-256-GCM under one
// instance key. It stands in for KMS: a later sealer can call the KMS data
// plane instead, behind the same domain port.
package aesgcm

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/services/mysterybox/domain"
)

// InternalKeyID is the NID reported as SecretStatus.effective_kms_key_id for
// every secret sealed by this package. It names no real KMS key.
const InternalKeyID = "kmssymkey-e00mock000000mysterybox"

// KeySize is the instance key length in bytes (AES-256).
const KeySize = 32

// Sealer implements domain.PayloadSealer.
type Sealer struct {
	aead cipher.AEAD
}

var _ domain.PayloadSealer = (*Sealer)(nil)

// New returns a sealer over a 32-byte key.
func New(key []byte) (*Sealer, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("aesgcm: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// NewRandom returns a sealer over a fresh random key. Payloads sealed by it
// cannot be read by another process, which suits the in-memory store.
func NewRandom() (*Sealer, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("aesgcm: random key: %w", err)
	}
	return New(key)
}

// KeyID implements domain.PayloadSealer.
func (*Sealer) KeyID() string { return InternalKeyID }

// Seal implements domain.PayloadSealer: nonce || ciphertext+tag.
func (s *Sealer) Seal(_ context.Context, plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plaintext)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("aesgcm: nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open implements domain.PayloadSealer.
func (s *Sealer) Open(_ context.Context, sealed, aad []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n+s.aead.Overhead() {
		return nil, errors.New("aesgcm: sealed payload too short")
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], aad)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: %w", err)
	}
	return plain, nil
}
