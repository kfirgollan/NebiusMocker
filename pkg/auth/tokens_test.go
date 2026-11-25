package auth_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

func TestNewIssuerValidation(t *testing.T) {
	t.Parallel()
	clk := clock.NewManual(t0)
	tests := []struct {
		name   string
		secret []byte
		ttl    time.Duration
		clk    clock.Clock
	}{
		{name: "short secret", secret: make([]byte, auth.MinSecretBytes-1), ttl: time.Hour, clk: clk},
		{name: "zero ttl", secret: make([]byte, auth.MinSecretBytes), ttl: 0, clk: clk},
		{name: "negative ttl", secret: make([]byte, auth.MinSecretBytes), ttl: -time.Second, clk: clk},
		{name: "nil clock", secret: make([]byte, auth.MinSecretBytes), ttl: time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if i, err := auth.NewIssuer(tt.secret, tt.ttl, tt.clk); err == nil {
				t.Fatalf("NewIssuer(%s) = %v, nil error, want error", tt.name, i)
			}
		})
	}
}

func mint(t *testing.T, iss *auth.Issuer, g auth.Grant) auth.Token {
	t.Helper()
	tok, err := iss.Mint(g)
	if err != nil {
		t.Fatalf("Mint(%+v): %v", g, err)
	}
	return tok
}

func verifyErr(iss *auth.Issuer, tok auth.Token) error {
	_, err := iss.Verify(tok.AccessToken)
	return err
}

func TestMintVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	iss := newIssuer(t, clock.NewManual(t0))
	tok := mint(t, iss, auth.Grant{Principal: "serviceaccount-1", Tenant: "tenant-1", KeyID: "publickey-1", Actor: "user-1"})
	if !strings.HasPrefix(tok.AccessToken, auth.TokenPrefix) || !auth.IsIssued(tok.AccessToken) {
		t.Errorf("token %q lacks prefix %q", tok.AccessToken, auth.TokenPrefix)
	}
	if auth.IsIssued("nmock-dev-admin") {
		t.Errorf("IsIssued(static token) = true")
	}
	if tok.ExpiresIn != time.Hour {
		t.Errorf("ExpiresIn = %v, want 1h", tok.ExpiresIn)
	}
	c, err := iss.Verify(tok.AccessToken)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Principal != "serviceaccount-1" || c.Tenant != "tenant-1" || c.KeyID != "publickey-1" || c.Actor != "user-1" {
		t.Errorf("claims = %+v, want principal/tenant/kid/actor of the grant", c)
	}
	if c.IssuedAt != t0.Unix() || c.ExpiresAt != t0.Add(time.Hour).Unix() {
		t.Errorf("iat, exp = %d, %d, want %d, %d", c.IssuedAt, c.ExpiresAt, t0.Unix(), t0.Add(time.Hour).Unix())
	}
	if c != tok.Claims {
		t.Errorf("Verify claims %+v != minted claims %+v", c, tok.Claims)
	}
	if iss.TTL() != time.Hour {
		t.Errorf("TTL() = %v, want 1h", iss.TTL())
	}
	if _, err := iss.Mint(auth.Grant{}); err == nil {
		t.Errorf("Mint without principal succeeded")
	}
}

func TestMintTTL(t *testing.T) {
	t.Parallel()
	iss := newIssuer(t, clock.NewManual(t0))
	tests := []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{name: "shorter honored", ttl: 10 * time.Minute, want: 10 * time.Minute},
		{name: "longer ignored", ttl: 5 * time.Hour, want: time.Hour},
		{name: "zero uses default", ttl: 0, want: time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tok := mint(t, iss, auth.Grant{Principal: "serviceaccount-1", TTL: tt.ttl})
			if tok.ExpiresIn != tt.want {
				t.Errorf("ExpiresIn = %v, want %v", tok.ExpiresIn, tt.want)
			}
		})
	}
}

func TestVerifyInvalid(t *testing.T) {
	t.Parallel()
	clk := clock.NewManual(t0)
	iss := newIssuer(t, clk)
	other := newIssuer(t, clk)
	tok := mint(t, iss, auth.Grant{Principal: "serviceaccount-1"})
	foreign := mint(t, other, auth.Grant{Principal: "serviceaccount-1"})
	// "nmock.v1.<payload>.<sig>" splits into [nmock v1 payload sig].
	parts := strings.Split(tok.AccessToken, ".")
	if len(parts) != 4 {
		t.Fatalf("token %q has %d parts, want 4", tok.AccessToken, len(parts))
	}
	flip := func(s string) string {
		b := []byte(s)
		if b[0] == 'A' {
			b[0] = 'B'
		} else {
			b[0] = 'A'
		}
		return string(b)
	}
	tests := []struct {
		name  string
		token string
	}{
		{name: "tampered payload", token: strings.Join([]string{parts[0], parts[1], flip(parts[2]), parts[3]}, ".")},
		{name: "tampered signature", token: strings.Join([]string{parts[0], parts[1], parts[2], flip(parts[3])}, ".")},
		{name: "truncated signature", token: tok.AccessToken[:len(tok.AccessToken)-4]},
		{name: "other issuer", token: foreign.AccessToken},
		{name: "garbage", token: "garbage"},
		{name: "empty", token: ""},
		{name: "no prefix", token: strings.TrimPrefix(tok.AccessToken, auth.TokenPrefix)},
		{name: "no signature", token: auth.TokenPrefix + parts[2]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := iss.Verify(tt.token); !errors.Is(err, auth.ErrInvalidToken) {
				t.Fatalf("Verify(%s) = %v, want ErrInvalidToken", tt.name, err)
			}
		})
	}
}

func TestVerifyExpiry(t *testing.T) {
	t.Parallel()
	clk := clock.NewManual(t0)
	iss := newIssuer(t, clk)
	tok := mint(t, iss, auth.Grant{Principal: "serviceaccount-1"})
	clk.Advance(time.Hour - time.Second)
	if err := verifyErr(iss, tok); err != nil {
		t.Fatalf("Verify 1s before exp = %v, want nil", err)
	}
	clk.Advance(time.Second)
	if err := verifyErr(iss, tok); !errors.Is(err, auth.ErrExpiredToken) {
		t.Fatalf("Verify at exp = %v, want ErrExpiredToken", err)
	}
}

func TestRevokeToken(t *testing.T) {
	t.Parallel()
	iss := newIssuer(t, clock.NewManual(t0))
	a := mint(t, iss, auth.Grant{Principal: "serviceaccount-1"})
	b := mint(t, iss, auth.Grant{Principal: "serviceaccount-1"})
	iss.RevokeToken(a.Claims.TokenID)
	if err := verifyErr(iss, a); !errors.Is(err, auth.ErrRevokedToken) {
		t.Errorf("revoked token Verify = %v, want ErrRevokedToken", err)
	}
	if err := verifyErr(iss, b); err != nil {
		t.Errorf("other token Verify = %v, want nil", err)
	}
}

func TestRevokeKey(t *testing.T) {
	t.Parallel()
	iss := newIssuer(t, clock.NewManual(t0))
	k1a := mint(t, iss, auth.Grant{Principal: "serviceaccount-1", KeyID: "publickey-1"})
	k1b := mint(t, iss, auth.Grant{Principal: "serviceaccount-1", KeyID: "publickey-1"})
	k2 := mint(t, iss, auth.Grant{Principal: "serviceaccount-1", KeyID: "publickey-2"})
	noKey := mint(t, iss, auth.Grant{Principal: "serviceaccount-1"})
	iss.RevokeKey("publickey-1")
	for name, tok := range map[string]auth.Token{"k1a": k1a, "k1b": k1b} {
		if err := verifyErr(iss, tok); !errors.Is(err, auth.ErrRevokedToken) {
			t.Errorf("%s Verify = %v, want ErrRevokedToken", name, err)
		}
	}
	for name, tok := range map[string]auth.Token{"k2": k2, "noKey": noKey} {
		if err := verifyErr(iss, tok); err != nil {
			t.Errorf("%s Verify = %v, want nil", name, err)
		}
	}
	after := mint(t, iss, auth.Grant{Principal: "serviceaccount-1", KeyID: "publickey-1"})
	if err := verifyErr(iss, after); err != nil {
		t.Errorf("token minted after RevokeKey Verify = %v, want nil", err)
	}
}

func TestRevokePrincipal(t *testing.T) {
	t.Parallel()
	iss := newIssuer(t, clock.NewManual(t0))
	own := mint(t, iss, auth.Grant{Principal: "serviceaccount-1"})
	impersonated := mint(t, iss, auth.Grant{Principal: "serviceaccount-2", Actor: "serviceaccount-1"})
	unrelated := mint(t, iss, auth.Grant{Principal: "serviceaccount-3", Actor: "serviceaccount-9"})
	iss.RevokePrincipal("serviceaccount-1")
	for name, tok := range map[string]auth.Token{"own": own, "impersonated by it": impersonated} {
		if err := verifyErr(iss, tok); !errors.Is(err, auth.ErrRevokedToken) {
			t.Errorf("%s Verify = %v, want ErrRevokedToken", name, err)
		}
	}
	if err := verifyErr(iss, unrelated); err != nil {
		t.Errorf("unrelated Verify = %v, want nil", err)
	}
}

func TestIssuerConcurrent(t *testing.T) {
	t.Parallel()
	clk := clock.NewManual(t0)
	iss := newIssuer(t, clk)
	var wg sync.WaitGroup
	for n := range 8 {
		wg.Go(func() {
			for range 50 {
				tok, err := iss.Mint(auth.Grant{Principal: "serviceaccount-1", KeyID: "publickey-1"})
				if err != nil {
					t.Errorf("Mint: %v", err)
					return
				}
				if _, err := iss.Verify(tok.AccessToken); err != nil && !errors.Is(err, auth.ErrRevokedToken) {
					t.Errorf("Verify: %v", err)
				}
				switch n % 4 {
				case 0:
					iss.RevokeToken(tok.Claims.TokenID)
				case 1:
					iss.RevokeKey("publickey-1")
				case 2:
					iss.RevokePrincipal("serviceaccount-1")
				default:
					clk.Advance(time.Millisecond)
				}
			}
		})
	}
	wg.Wait()
}
