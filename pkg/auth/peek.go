package auth

import (
	"strings"

	"github.com/golang-jwt/jwt/v4"
)

// JWTHeaderClaims are the header and registered claims of a JWT, read
// without verifying it.
type JWTHeaderClaims struct {
	KeyID     string // header "kid"
	Algorithm string // header "alg"
	Issuer    string
	Subject   string
}

// PeekJWT reads the kid and alg headers and the iss and sub claims of a JWS
// in compact form without verifying anything, whatever its algorithm. It is
// for describing a refused credential (the audit event of a failed
// authentication names the key it presented); never trust what it returns.
// ok is false when token is not a decodable JWT.
func PeekJWT(token string) (h JWTHeaderClaims, ok bool) {
	var c jwt.MapClaims
	t, _, err := jwt.NewParser().ParseUnverified(strings.TrimSpace(token), &c)
	if err != nil {
		return JWTHeaderClaims{}, false
	}
	h.KeyID, _ = t.Header["kid"].(string)
	h.Algorithm, _ = t.Header["alg"].(string)
	h.Issuer, _ = c["iss"].(string)
	h.Subject, _ = c["sub"].(string)
	return h, true
}

// UnverifiedClaims decodes the claims of a token shaped like an issued one
// without checking its signature, expiry or revocation. It is for describing
// a refused token (the audit event of a failed authentication names its
// claimed principal); never authenticate with it. ok is false when token is
// not an issued token or its claims cannot be decoded.
func UnverifiedClaims(token string) (c Claims, ok bool) {
	signed, _, found := cutLast(token, ".")
	if !found || !IsIssued(signed) {
		return Claims{}, false
	}
	if err := decodeClaims(signed, &c); err != nil {
		return Claims{}, false
	}
	return c, true
}
