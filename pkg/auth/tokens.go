package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

// TokenPrefix starts every access token the mocker issues:
// "nmock.v1.<base64url(claims JSON)>.<base64url(HMAC-SHA256)>".
const TokenPrefix = "nmock.v1."

// MinSecretBytes is the shortest accepted token secret.
const MinSecretBytes = 32

// Token verification failures. Verify wraps exactly one of them.
var (
	ErrInvalidToken = errors.New("invalid access token")
	ErrExpiredToken = errors.New("access token expired")
	ErrRevokedToken = errors.New("access token revoked")
)

// Grant is what a token is issued for.
type Grant struct {
	// Principal is the NID the token authenticates as (a service account or
	// user account).
	Principal string
	// Tenant is the principal's tenant.
	Tenant string
	// KeyID is the auth public key the token was exchanged with, if any.
	// Deactivating or deleting that key revokes the token.
	KeyID string
	// Actor is the impersonating principal, if the token was issued through
	// impersonation. Revoking the actor revokes the token.
	Actor string
	// TTL overrides the issuer's TTL when positive and shorter.
	TTL time.Duration
}

// Claims are carried inside an issued token.
type Claims struct {
	TokenID   string `json:"jti"`
	Principal string `json:"sub"`
	Tenant    string `json:"tid,omitempty"`
	KeyID     string `json:"kid,omitempty"`
	Actor     string `json:"act,omitempty"`
	// IssuedAt and ExpiresAt are Unix seconds on the mocker's clock.
	IssuedAt  int64 `json:"iat"`
	ExpiresAt int64 `json:"exp"`
}

// Token is a newly issued access token.
type Token struct {
	AccessToken string
	Claims      Claims
	// ExpiresIn is the remaining lifetime at issue (the OAuth expires_in).
	ExpiresIn time.Duration
}

// Issuer mints and verifies access tokens. Verification is stateless (the
// HMAC under the secret), except for the in-memory deny-list of revoked
// tokens, which is per process. It is safe for concurrent use.
type Issuer struct {
	secret []byte
	ttl    time.Duration
	clk    clock.Clock

	mu sync.Mutex
	// live indexes unexpired issued tokens by ID, so revoking a key or a
	// principal can deny the tokens issued for it.
	live    map[string]Claims
	revoked map[string]int64 // token ID → its expiry (Unix s), pruned after
}

// NewIssuer returns an issuer signing with secret and issuing tokens valid for ttl.
func NewIssuer(secret []byte, ttl time.Duration, clk clock.Clock) (*Issuer, error) {
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("auth: token secret must be at least %d bytes", MinSecretBytes)
	}
	if ttl <= 0 {
		return nil, errors.New("auth: token TTL must be positive")
	}
	if clk == nil {
		return nil, errors.New("auth: clock must not be nil")
	}
	return &Issuer{
		secret: append([]byte(nil), secret...), ttl: ttl, clk: clk,
		live: map[string]Claims{}, revoked: map[string]int64{},
	}, nil
}

// NewSecret returns a random token secret (the per-boot default).
func NewSecret() ([]byte, error) {
	b := make([]byte, MinSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("auth: token secret: %w", err)
	}
	return b, nil
}

// TTL is the lifetime of issued tokens.
func (i *Issuer) TTL() time.Duration { return i.ttl }

// IsIssued reports whether token has the shape of a mocker-issued token (it
// may still be invalid); other bearer tokens are static dev tokens.
func IsIssued(token string) bool { return strings.HasPrefix(token, TokenPrefix) }

// Mint issues a token for g.
func (i *Issuer) Mint(g Grant) (Token, error) {
	if g.Principal == "" {
		return Token{}, errors.New("auth: grant has no principal")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Token{}, fmt.Errorf("auth: token id: %w", err)
	}
	ttl := i.ttl
	if g.TTL > 0 && g.TTL < ttl {
		ttl = g.TTL
	}
	now := i.clk.Now()
	c := Claims{
		TokenID: hex.EncodeToString(id[:]), Principal: g.Principal, Tenant: g.Tenant, KeyID: g.KeyID, Actor: g.Actor,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(ttl).Unix(),
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return Token{}, fmt.Errorf("auth: encode claims: %w", err)
	}
	signed := TokenPrefix + base64.RawURLEncoding.EncodeToString(payload)
	tok := signed + "." + base64.RawURLEncoding.EncodeToString(i.mac(signed))

	i.mu.Lock()
	defer i.mu.Unlock()
	i.pruneLocked(now.Unix())
	i.live[c.TokenID] = c
	return Token{AccessToken: tok, Claims: c, ExpiresIn: time.Duration(c.ExpiresAt-c.IssuedAt) * time.Second}, nil
}

// Verify checks the token's signature, expiry (on the mocker's clock) and
// revocation, and returns its claims.
func (i *Issuer) Verify(token string) (Claims, error) {
	signed, sig, ok := cutLast(token, ".")
	if !ok || !IsIssued(signed) {
		return Claims{}, ErrInvalidToken
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(gotMAC, i.mac(signed)) {
		return Claims{}, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(signed, TokenPrefix))
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil || c.Principal == "" || c.TokenID == "" {
		return Claims{}, ErrInvalidToken
	}
	if i.clk.Now().Unix() >= c.ExpiresAt {
		return Claims{}, ErrExpiredToken
	}
	i.mu.Lock()
	_, revoked := i.revoked[c.TokenID]
	i.mu.Unlock()
	if revoked {
		return Claims{}, ErrRevokedToken
	}
	return c, nil
}

// RevokeToken denies one token by ID.
func (i *Issuer) RevokeToken(tokenID string) {
	i.revokeWhere(func(c Claims) bool { return c.TokenID == tokenID })
}

// RevokeKey denies every token exchanged with the auth public key keyID.
// Tokens issued after the call are not affected (a reactivated key works again).
func (i *Issuer) RevokeKey(keyID string) {
	i.revokeWhere(func(c Claims) bool { return c.KeyID == keyID })
}

// RevokePrincipal denies every token issued for, or by impersonation by,
// principalID (e.g. when a service account is deleted).
func (i *Issuer) RevokePrincipal(principalID string) {
	i.revokeWhere(func(c Claims) bool { return c.Principal == principalID || c.Actor == principalID })
}

func (i *Issuer) revokeWhere(match func(Claims) bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.pruneLocked(i.clk.Now().Unix())
	for id, c := range i.live {
		if match(c) {
			i.revoked[id] = c.ExpiresAt
			delete(i.live, id)
		}
	}
}

// pruneLocked forgets tokens that have expired: they fail Verify anyway.
func (i *Issuer) pruneLocked(now int64) {
	for id, c := range i.live {
		if now >= c.ExpiresAt {
			delete(i.live, id)
		}
	}
	for id, exp := range i.revoked {
		if now >= exp {
			delete(i.revoked, id)
		}
	}
}

func (i *Issuer) mac(signed string) []byte {
	m := hmac.New(sha256.New, i.secret)
	m.Write([]byte(signed))
	return m.Sum(nil)
}

func cutLast(s, sep string) (before, after string, found bool) {
	if j := strings.LastIndex(s, sep); j >= 0 {
		return s[:j], s[j+len(sep):], true
	}
	return s, "", false
}
