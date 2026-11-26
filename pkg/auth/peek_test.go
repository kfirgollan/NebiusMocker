package auth_test

import (
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v4"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

func TestPeekJWT(t *testing.T) {
	t.Parallel()
	rsaKey := genRSA(t, 2048)
	claims := jwt.MapClaims{"iss": testSA, "sub": "serviceaccount-other"}
	noneTok, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		token  string
		want   auth.JWTHeaderClaims
		wantOK bool
	}{
		{
			name:   "rs256 with kid",
			token:  signJWT(t, jwt.SigningMethodRS256, testKID, claims, rsaKey),
			want:   auth.JWTHeaderClaims{KeyID: testKID, Algorithm: "RS256", Issuer: testSA, Subject: "serviceaccount-other"},
			wantOK: true,
		},
		{
			name:   "rs256 without kid",
			token:  signJWT(t, jwt.SigningMethodRS256, "", claims, rsaKey),
			want:   auth.JWTHeaderClaims{Algorithm: "RS256", Issuer: testSA, Subject: "serviceaccount-other"},
			wantOK: true,
		},
		{
			name:   "hs256",
			token:  signJWT(t, jwt.SigningMethodHS256, "k1", claims, []byte("secret")),
			want:   auth.JWTHeaderClaims{KeyID: "k1", Algorithm: "HS256", Issuer: testSA, Subject: "serviceaccount-other"},
			wantOK: true,
		},
		{
			name:   "alg none",
			token:  noneTok,
			want:   auth.JWTHeaderClaims{Algorithm: "none", Issuer: testSA, Subject: "serviceaccount-other"},
			wantOK: true,
		},
		{
			name:   "surrounding whitespace",
			token:  " " + signJWT(t, jwt.SigningMethodHS256, "", claims, []byte("secret")) + "\n",
			want:   auth.JWTHeaderClaims{Algorithm: "HS256", Issuer: testSA, Subject: "serviceaccount-other"},
			wantOK: true,
		},
		{name: "static token", token: "nmock-dev-admin"},
		{name: "empty", token: ""},
		{name: "garbage with dots", token: "a.b.c"},
		{name: "two parts", token: "aGVhZGVy.cGF5bG9hZA"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := auth.PeekJWT(tt.token)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("PeekJWT() = (%+v, %v), want (%+v, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestUnverifiedClaims(t *testing.T) {
	t.Parallel()
	iss := newIssuer(t, clock.NewManual(t0))
	tok := mint(t, iss, auth.Grant{Principal: "serviceaccount-1", Tenant: "tenant-1", KeyID: "publickey-1", Actor: "user-1"})
	lastDot := strings.LastIndex(tok.AccessToken, ".")
	forged := tok.AccessToken[:lastDot+1] + strings.Repeat("A", 10) // the MAC is never consulted

	tests := []struct {
		name   string
		token  string
		wantOK bool
	}{
		{name: "valid", token: tok.AccessToken, wantOK: true},
		{name: "tampered mac", token: forged, wantOK: true},
		{name: "missing mac", token: tok.AccessToken[:lastDot], wantOK: false},
		{name: "static token", token: "nmock-dev-admin"},
		{name: "prefix only", token: auth.TokenPrefix},
		{name: "undecodable claims", token: auth.TokenPrefix + "!!!.mac"},
		{name: "garbage", token: "garbage"},
		{name: "empty", token: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, ok := auth.UnverifiedClaims(tt.token)
			if ok != tt.wantOK {
				t.Fatalf("UnverifiedClaims() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				if c != (auth.Claims{}) {
					t.Errorf("claims = %+v, want zero when !ok", c)
				}
				return
			}
			if c != tok.Claims {
				t.Errorf("claims = %+v, want %+v", c, tok.Claims)
			}
		})
	}
	if _, err := iss.Verify(forged); err == nil {
		t.Error("Verify(forged) = nil error; the tampered token must still be rejected")
	}
}
