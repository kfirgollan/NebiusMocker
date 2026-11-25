// Package auth is NebiusMocker's transport-free authentication core: the
// opaque access tokens the mocker issues (mint, stateless verification,
// revocation), the RS256 JWT assertions service accounts exchange for them,
// and the uploaded public keys those assertions are verified against.
// See docs/architecture/06-iam-and-authorization.md.
package auth

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
)

// AlgorithmRSA is reported as an auth public key's status.algorithm. JWT
// assertions signed with RSA keys use RS256, the only algorithm Nebius token
// exchange accepts.
const AlgorithmRSA = "RSA"

// MinRSAKeyBits is the smallest RSA modulus accepted for an uploaded key.
const MinRSAKeyBits = 2048

// ErrUnsupportedKey reports public key data that is not a PEM-encoded RSA key
// of at least MinRSAKeyBits.
var ErrUnsupportedKey = errors.New("unsupported public key")

// PublicKey is a parsed, uploaded public key.
type PublicKey struct {
	Key *rsa.PublicKey
	// Algorithm is AlgorithmRSA.
	Algorithm string
	// Bits is the modulus size.
	Bits int
	// Fingerprint is the lower-case hex SHA-256 of the DER
	// SubjectPublicKeyInfo, so the same key has the same fingerprint whether it
	// was uploaded as PKIX ("PUBLIC KEY") or PKCS#1 ("RSA PUBLIC KEY") PEM.
	Fingerprint string
}

// ParsePublicKey parses PEM public key data: a PKIX "PUBLIC KEY" block (what
// `openssl rsa -pubout` writes) or a PKCS#1 "RSA PUBLIC KEY" block. Only RSA
// keys are supported; other types and keys shorter than MinRSAKeyBits fail
// with an error wrapping ErrUnsupportedKey.
func ParsePublicKey(data string) (PublicKey, error) {
	block, rest := pem.Decode([]byte(data))
	if block == nil {
		return PublicKey{}, fmt.Errorf("%w: not PEM-encoded", ErrUnsupportedKey)
	}
	if b, _ := pem.Decode(rest); b != nil {
		return PublicKey{}, fmt.Errorf("%w: more than one PEM block", ErrUnsupportedKey)
	}
	var pub any
	var err error
	switch block.Type {
	case "PUBLIC KEY":
		pub, err = x509.ParsePKIXPublicKey(block.Bytes)
	case "RSA PUBLIC KEY":
		pub, err = x509.ParsePKCS1PublicKey(block.Bytes)
	default:
		return PublicKey{}, fmt.Errorf("%w: PEM block %q, want \"PUBLIC KEY\" or \"RSA PUBLIC KEY\"", ErrUnsupportedKey, block.Type)
	}
	if err != nil {
		return PublicKey{}, fmt.Errorf("%w: %w", ErrUnsupportedKey, err)
	}
	rsaKey, ok := pub.(*rsa.PublicKey)
	if !ok {
		return PublicKey{}, fmt.Errorf("%w: %T keys are not supported, only RSA (RS256)", ErrUnsupportedKey, pub)
	}
	bits := rsaKey.N.BitLen()
	if bits < MinRSAKeyBits {
		return PublicKey{}, fmt.Errorf("%w: RSA key is %d bits, at least %d are required", ErrUnsupportedKey, bits, MinRSAKeyBits)
	}
	der, err := x509.MarshalPKIXPublicKey(rsaKey)
	if err != nil {
		return PublicKey{}, fmt.Errorf("%w: %w", ErrUnsupportedKey, err)
	}
	sum := sha256.Sum256(der)
	return PublicKey{Key: rsaKey, Algorithm: AlgorithmRSA, Bits: bits, Fingerprint: hex.EncodeToString(sum[:])}, nil
}
