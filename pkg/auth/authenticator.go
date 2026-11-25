package auth

import "fmt"

// Authenticator resolves bearer tokens to principals: the static dev tokens
// from configuration and the tokens an Issuer minted. It is what every
// front (the gRPC edge, the registry's docker front) authenticates with.
type Authenticator struct {
	static map[string]string
	issuer *Issuer
}

// NewAuthenticator accepts the static token → principal map and the tokens of
// issuer. Either may be nil.
func NewAuthenticator(static map[string]string, issuer *Issuer) *Authenticator {
	return &Authenticator{static: static, issuer: issuer}
}

// Authenticate returns the principal of token. Issued tokens fail with an
// error wrapping ErrInvalidToken, ErrExpiredToken or ErrRevokedToken; unknown
// static tokens with ErrInvalidToken.
func (a *Authenticator) Authenticate(token string) (string, error) {
	if principal, ok := a.static[token]; ok {
		return principal, nil
	}
	if a.issuer == nil || !IsIssued(token) {
		return "", ErrInvalidToken
	}
	c, err := a.issuer.Verify(token)
	if err != nil {
		return "", fmt.Errorf("verify issued token: %w", err)
	}
	return c.Principal, nil
}

// Principal is Authenticate as a lookup (for fronts that only need a yes/no).
func (a *Authenticator) Principal(token string) (string, bool) {
	p, err := a.Authenticate(token)
	return p, err == nil
}
