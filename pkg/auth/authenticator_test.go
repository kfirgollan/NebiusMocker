package auth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func newIssuer(t *testing.T, clk clock.Clock) *auth.Issuer {
	t.Helper()
	secret, err := auth.NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	i, err := auth.NewIssuer(secret, time.Hour, clk)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return i
}

func TestAuthenticator(t *testing.T) {
	t.Parallel()
	clk := clock.NewManual(t0)
	iss := newIssuer(t, clk)
	live, err := iss.Mint(auth.Grant{Principal: "serviceaccount-1"})
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := iss.Mint(auth.Grant{Principal: "serviceaccount-2"})
	if err != nil {
		t.Fatal(err)
	}
	iss.RevokeToken(revoked.Claims.TokenID)
	short, err := iss.Mint(auth.Grant{Principal: "serviceaccount-3", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute)

	a := auth.NewAuthenticator(map[string]string{"nmock-dev-admin": "user-admin"}, iss)
	tests := []struct {
		name  string
		token string
		want  string
		err   error
	}{
		{name: "static", token: "nmock-dev-admin", want: "user-admin"},
		{name: "issued", token: live.AccessToken, want: "serviceaccount-1"},
		{name: "revoked", token: revoked.AccessToken, err: auth.ErrRevokedToken},
		{name: "expired", token: short.AccessToken, err: auth.ErrExpiredToken},
		{name: "unknown", token: "nope", err: auth.ErrInvalidToken},
		{name: "empty", token: "", err: auth.ErrInvalidToken},
		{name: "forged issued", token: auth.TokenPrefix + "e30.AAAA", err: auth.ErrInvalidToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := a.Authenticate(tt.token)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("Authenticate(%s) error = %v, want %v", tt.name, err, tt.err)
				}
				if p, ok := a.Principal(tt.token); ok || p != "" {
					t.Errorf("Principal(%s) = %q, %v, want \"\", false", tt.name, p, ok)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Authenticate(%s) = %q, %v, want %q", tt.name, got, err, tt.want)
			}
			if p, ok := a.Principal(tt.token); !ok || p != tt.want {
				t.Errorf("Principal(%s) = %q, %v, want %q, true", tt.name, p, ok, tt.want)
			}
		})
	}
}

func TestAuthenticatorNilIssuer(t *testing.T) {
	t.Parallel()
	iss := newIssuer(t, clock.NewManual(t0))
	tok, err := iss.Mint(auth.Grant{Principal: "serviceaccount-1"})
	if err != nil {
		t.Fatal(err)
	}
	a := auth.NewAuthenticator(map[string]string{"static": "user-1"}, nil)
	if got, err := a.Authenticate("static"); err != nil || got != "user-1" {
		t.Errorf("Authenticate(static) = %q, %v, want user-1", got, err)
	}
	if _, err := a.Authenticate(tok.AccessToken); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("Authenticate(issued) without issuer = %v, want ErrInvalidToken", err)
	}
	empty := auth.NewAuthenticator(nil, nil)
	if _, err := empty.Authenticate("x"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("Authenticate with no tokens = %v, want ErrInvalidToken", err)
	}
}
