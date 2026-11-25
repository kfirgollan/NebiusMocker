package domain

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"

	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"
)

// Crypto parameters. They follow the Nebius KMS docs: AES-256-GCM, RSA-4096
// OAEP with SHA-256 (and MGF1-SHA-256), ECDSA signatures in ASN.1 DER.
const (
	// MaxPlaintextSize is the largest plaintext Encrypt accepts (64 KiB).
	MaxPlaintextSize = 64 << 10
	rsaBits          = 4096
	// ciphertextFormat is the first byte of every symmetric ciphertext.
	ciphertextFormat byte = 1
	// headerSize is format (1) + key version (4).
	headerSize = 5
)

// errBadCiphertext is returned for any ciphertext that does not authenticate:
// truncated, tampered, wrong AAD, wrong key or unknown version. One error for
// all of them, so a caller cannot probe which part was wrong.
var errBadCiphertext = errors.New("ciphertext is invalid or was not produced by this key with this AAD context")

// symVersion is one version of a symmetric key's material.
type symVersion struct {
	Version uint32 `json:"v"`
	Key     []byte `json:"k"`
}

// symMaterial is the secret state of a symmetric key: every version (old ones
// still decrypt) and the primary one used to encrypt.
type symMaterial struct {
	Versions []symVersion `json:"versions"`
	Primary  uint32       `json:"primary"`
	// Rotations counts manual rotations (limited to maxManualRotations).
	Rotations int `json:"rotations"`
}

// LogValue keeps key material out of logs even if a value is logged by mistake.
func (symMaterial) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// String keeps key material out of fmt output.
func (symMaterial) String() string { return "[REDACTED]" }

func symKeySize(alg kmspb.SymmetricAlgorithm) (int, error) {
	switch alg {
	case kmspb.SymmetricAlgorithm_AES_256:
		return 32, nil
	case kmspb.SymmetricAlgorithm_AES_128:
		return 16, nil
	case kmspb.SymmetricAlgorithm_SYMMETRIC_ALGORITHM_UNSPECIFIED:
		return 0, errors.New("symmetric algorithm is unspecified")
	default:
		return 0, fmt.Errorf("unsupported symmetric algorithm %s", alg)
	}
}

// newSymMaterial generates version 1 of a symmetric key.
func newSymMaterial(rnd io.Reader, alg kmspb.SymmetricAlgorithm) (symMaterial, error) {
	size, err := symKeySize(alg)
	if err != nil {
		return symMaterial{}, err
	}
	k, err := randomBytes(rnd, size)
	if err != nil {
		return symMaterial{}, err
	}
	return symMaterial{Versions: []symVersion{{Version: 1, Key: k}}, Primary: 1}, nil
}

// rotate adds a new version with fresh material and makes it primary.
func (m symMaterial) rotate(rnd io.Reader) (symMaterial, error) {
	size := len(m.Versions[len(m.Versions)-1].Key)
	k, err := randomBytes(rnd, size)
	if err != nil {
		return symMaterial{}, err
	}
	next := m.Primary
	for _, v := range m.Versions {
		next = max(next, v.Version)
	}
	next++
	out := symMaterial{Versions: append(append([]symVersion(nil), m.Versions...), symVersion{Version: next, Key: k}), Primary: next, Rotations: m.Rotations}
	return out, nil
}

func (m symMaterial) version(v uint32) ([]byte, bool) {
	for _, sv := range m.Versions {
		if sv.Version == v {
			return sv.Key, true
		}
	}
	return nil, false
}

// gcmAAD binds the header and the key ID to the caller's AAD context, so the
// version byte cannot be altered and a ciphertext cannot be replayed on
// another key.
func gcmAAD(header []byte, keyID string, aad []byte) []byte {
	out := make([]byte, 0, len(header)+len(keyID)+1+len(aad))
	out = append(out, header...)
	out = append(out, keyID...)
	out = append(out, 0)
	return append(out, aad...)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	return cipher.NewGCM(block)
}

// seal encrypts plaintext with the primary version. Format:
//
//	format(1) | key version (4, big endian) | nonce (12) | AES-GCM ciphertext+tag
func (m symMaterial) seal(rnd io.Reader, keyID string, plaintext, aad []byte) ([]byte, error) {
	key, ok := m.version(m.Primary)
	if !ok {
		return nil, fmt.Errorf("primary version %d missing", m.Primary)
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	header := make([]byte, headerSize, headerSize+aead.NonceSize()+len(plaintext)+aead.Overhead())
	header[0] = ciphertextFormat
	binary.BigEndian.PutUint32(header[1:], m.Primary)
	nonce, err := randomBytes(rnd, aead.NonceSize())
	if err != nil {
		return nil, err
	}
	header = append(header, nonce...)
	return aead.Seal(header, nonce, plaintext, gcmAAD(header[:headerSize], keyID, aad)), nil
}

// open decrypts a ciphertext produced by seal with any version of the key.
func (m symMaterial) open(keyID string, ciphertext, aad []byte) ([]byte, error) {
	if len(ciphertext) < headerSize || ciphertext[0] != ciphertextFormat {
		return nil, errBadCiphertext
	}
	key, ok := m.version(binary.BigEndian.Uint32(ciphertext[1:headerSize]))
	if !ok {
		return nil, errBadCiphertext
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	rest := ciphertext[headerSize:]
	if len(rest) < aead.NonceSize()+aead.Overhead() {
		return nil, errBadCiphertext
	}
	nonce, body := rest[:aead.NonceSize()], rest[aead.NonceSize():]
	pt, err := aead.Open(nil, nonce, body, gcmAAD(ciphertext[:headerSize], keyID, aad))
	if err != nil {
		return nil, errBadCiphertext
	}
	return pt, nil
}

func encodeSymMaterial(m symMaterial) ([]byte, error) { return json.Marshal(m) }

func decodeSymMaterial(b []byte) (symMaterial, error) {
	var m symMaterial
	if err := json.Unmarshal(b, &m); err != nil || len(m.Versions) == 0 {
		return symMaterial{}, errors.New("corrupt symmetric key material")
	}
	return m, nil
}

// asymMaterial is the PKCS#8 DER private key of an asymmetric key.
type asymMaterial struct{ der []byte }

// LogValue keeps key material out of logs.
func (asymMaterial) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// String keeps key material out of fmt output.
func (asymMaterial) String() string { return "[REDACTED]" }

// newAsymMaterial generates a private key for alg.
func newAsymMaterial(rnd io.Reader, alg kmspb.AsymmetricAlgorithm) (asymMaterial, error) {
	var (
		priv crypto.Signer
		err  error
	)
	switch alg {
	case kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256:
		priv, err = ecdsa.GenerateKey(elliptic.P256(), rnd)
	case kmspb.AsymmetricAlgorithm_ECDSA_NIST_P384_SHA_384:
		priv, err = ecdsa.GenerateKey(elliptic.P384(), rnd)
	case kmspb.AsymmetricAlgorithm_RSA_4096_ENC_OAEP_SHA_256:
		priv, err = rsa.GenerateKey(rnd, rsaBits)
	case kmspb.AsymmetricAlgorithm_ASYMMETRIC_ALGORITHM_UNSPECIFIED:
		return asymMaterial{}, errors.New("asymmetric algorithm is unspecified")
	default:
		return asymMaterial{}, fmt.Errorf("unsupported asymmetric algorithm %s", alg)
	}
	if err != nil {
		return asymMaterial{}, fmt.Errorf("generate %s key: %w", alg, err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return asymMaterial{}, fmt.Errorf("encode %s key: %w", alg, err)
	}
	return asymMaterial{der: der}, nil
}

func (m asymMaterial) signer() (crypto.Signer, error) {
	k, err := x509.ParsePKCS8PrivateKey(m.der)
	if err != nil {
		return nil, errors.New("corrupt asymmetric key material")
	}
	s, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("corrupt asymmetric key material")
	}
	return s, nil
}

// publicPEM returns the public key as a PKIX "PUBLIC KEY" PEM block, the
// format the docs feed to openssl.
func (m asymMaterial) publicPEM() (string, error) {
	s, err := m.signer()
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(s.Public())
	if err != nil {
		return "", fmt.Errorf("encode public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// signHash signs a precomputed digest with ECDSA, returning an ASN.1 DER
// signature (what `openssl dgst -verify` expects).
func (m asymMaterial) signHash(rnd io.Reader, hash []byte) ([]byte, error) {
	s, err := m.signer()
	if err != nil {
		return nil, err
	}
	k, ok := s.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an ECDSA key")
	}
	return ecdsa.SignASN1(rnd, k, hash)
}

// decrypt decrypts an RSA-OAEP (SHA-256, MGF1-SHA-256, empty label) ciphertext.
func (m asymMaterial) decrypt(rnd io.Reader, ciphertext []byte) ([]byte, error) {
	s, err := m.signer()
	if err != nil {
		return nil, err
	}
	k, ok := s.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	pt, err := rsa.DecryptOAEP(sha256.New(), rnd, k, ciphertext, nil)
	if err != nil {
		return nil, errBadCiphertext
	}
	return pt, nil
}

// hashSize is the digest length SignHash expects for alg (0 if alg cannot sign).
func hashSize(alg kmspb.AsymmetricAlgorithm) int {
	switch alg {
	case kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256:
		return sha256.Size
	case kmspb.AsymmetricAlgorithm_ECDSA_NIST_P384_SHA_384:
		return 48
	case kmspb.AsymmetricAlgorithm_RSA_4096_ENC_OAEP_SHA_256, kmspb.AsymmetricAlgorithm_ASYMMETRIC_ALGORITHM_UNSPECIFIED:
		return 0
	default:
		return 0
	}
}

func randomBytes(rnd io.Reader, n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rnd, b); err != nil {
		return nil, fmt.Errorf("random: %w", err)
	}
	return b, nil
}

// defaultRand is the entropy source of production services.
var defaultRand io.Reader = rand.Reader
