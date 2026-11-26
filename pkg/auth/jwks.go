package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// ErrInvalidJWKSet wraps every reason a JSON Web Key Set is refused.
var ErrInvalidJWKSet = errors.New("invalid JWK set")

// minRSABits is the smallest RSA modulus a JWK may have.
const minRSABits = 2048

// JWK is one public signing key of a JSON Web Key Set (RFC 7517): an RSA key
// (RS256) or an EC P-256 key (ES256).
type JWK struct {
	// KeyID is the "kid"; empty when the set does not name its keys.
	KeyID string
	// Alg is the "alg" the key is restricted to; empty means any algorithm
	// matching its type.
	Alg string
	// Key is an *rsa.PublicKey or an *ecdsa.PublicKey on P-256.
	Key crypto.PublicKey
}

// JWKSet is the supported signing keys of a JSON Web Key Set.
type JWKSet struct {
	Keys []JWK
}

// jwkJSON is the wire form of a JWK (RFC 7517 §4, RFC 7518 §6).
type jwkJSON struct {
	Kty string `json:"kty"`
	Kid string `json:"kid,omitempty"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

type jwkSetJSON struct {
	Keys []jwkJSON `json:"keys"`
}

// ParseJWKSet reads a JSON Web Key Set. Keys of other types or curves, and
// encryption keys ("use": "enc"), are skipped, since an issuer's set may hold
// keys the mocker cannot verify with; the set must still contain at least one
// RSA or EC P-256 signing key. A malformed supported key fails the whole set.
func ParseJWKSet(data []byte) (JWKSet, error) {
	var raw jwkSetJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return JWKSet{}, fmt.Errorf("%w: not a JSON object with a \"keys\" array: %w", ErrInvalidJWKSet, err)
	}
	var set JWKSet
	for i := range raw.Keys {
		k := &raw.Keys[i]
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		key, ok, err := parseJWK(k)
		if err != nil {
			return JWKSet{}, fmt.Errorf("%w: keys[%d]: %w", ErrInvalidJWKSet, i, err)
		}
		if ok {
			set.Keys = append(set.Keys, key)
		}
	}
	if len(set.Keys) == 0 {
		return JWKSet{}, fmt.Errorf("%w: no supported signing key (kty RSA, or kty EC with crv P-256)", ErrInvalidJWKSet)
	}
	return set, nil
}

// parseJWK decodes one key; ok is false for an unsupported key type or curve.
func parseJWK(k *jwkJSON) (key JWK, ok bool, err error) {
	switch {
	case k.Kty == "RSA":
		if k.Alg != "" && k.Alg != "RS256" {
			return JWK{}, false, nil
		}
		pub, err := rsaFromJWK(k)
		return JWK{KeyID: k.Kid, Alg: k.Alg, Key: pub}, err == nil, err
	case k.Kty == "EC" && k.Crv == "P-256":
		if k.Alg != "" && k.Alg != "ES256" {
			return JWK{}, false, nil
		}
		pub, err := ecFromJWK(k)
		return JWK{KeyID: k.Kid, Alg: k.Alg, Key: pub}, err == nil, err
	default:
		return JWK{}, false, nil
	}
}

func rsaFromJWK(k *jwkJSON) (*rsa.PublicKey, error) {
	n, err := b64Int(k.N, "n")
	if err != nil {
		return nil, err
	}
	e, err := b64Int(k.E, "e")
	if err != nil {
		return nil, err
	}
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() >= 1<<31 || e.Bit(0) == 0 {
		return nil, errors.New("RSA exponent e is invalid")
	}
	if n.BitLen() < minRSABits {
		return nil, fmt.Errorf("RSA modulus is %d bits, at least %d are required", n.BitLen(), minRSABits)
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

func ecFromJWK(k *jwkJSON) (*ecdsa.PublicKey, error) {
	x, err := b64Bytes(k.X, "x")
	if err != nil {
		return nil, err
	}
	y, err := b64Bytes(k.Y, "y")
	if err != nil {
		return nil, err
	}
	const size = 32
	if len(x) != size || len(y) != size {
		return nil, fmt.Errorf("EC P-256 coordinates must be %d bytes each", size)
	}
	point := append(append([]byte{4}, x...), y...)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
	if err != nil {
		return nil, fmt.Errorf("EC P-256 point: %w", err)
	}
	return pub, nil
}

func b64Bytes(s, member string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("member %q is required", member)
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil, fmt.Errorf("member %q is not base64url: %w", member, err)
	}
	return b, nil
}

func b64Int(s, member string) (*big.Int, error) {
	b, err := b64Bytes(s, member)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}

// MarshalJSON encodes the set as a JSON Web Key Set ("use": "sig").
func (s JWKSet) MarshalJSON() ([]byte, error) {
	out := jwkSetJSON{Keys: make([]jwkJSON, 0, len(s.Keys))}
	for _, k := range s.Keys {
		j := jwkJSON{Kid: k.KeyID, Use: "sig", Alg: k.Alg}
		switch pub := k.Key.(type) {
		case *rsa.PublicKey:
			j.Kty = "RSA"
			j.N = base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
			j.E = base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
		case *ecdsa.PublicKey:
			if pub.Curve != elliptic.P256() {
				return nil, errors.New("auth: only P-256 EC keys can be encoded")
			}
			point, err := pub.Bytes()
			if err != nil {
				return nil, fmt.Errorf("auth: encode EC key: %w", err)
			}
			const size = 32
			j.Kty, j.Crv = "EC", "P-256"
			j.X = base64.RawURLEncoding.EncodeToString(point[1 : 1+size])
			j.Y = base64.RawURLEncoding.EncodeToString(point[1+size:])
		default:
			return nil, fmt.Errorf("auth: cannot encode %T as a JWK", k.Key)
		}
		out.Keys = append(out.Keys, j)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("auth: encode JWK set: %w", err)
	}
	return b, nil
}
