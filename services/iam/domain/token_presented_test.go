package domain_test

import (
	"testing"

	"github.com/golang-jwt/jwt/v4"
	"github.com/google/go-cmp/cmp"

	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

func presentedJWT(t *testing.T, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString([]byte("not-a-real-key"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPresentedBy(t *testing.T) {
	t.Parallel()
	saJWT := presentedJWT(t, "publickey-e00k", jwt.MapClaims{"iss": "serviceaccount-e00a", "sub": "serviceaccount-e00a"})
	otherKidJWT := presentedJWT(t, "some-other-kid", jwt.MapClaims{"iss": "serviceaccount-e00a", "sub": "serviceaccount-e00a"})
	noKidJWT := presentedJWT(t, "", jwt.MapClaims{"iss": "https://idp.example", "sub": "serviceaccount-e00a"})
	extJWT := presentedJWT(t, "ext-kid", jwt.MapClaims{"iss": "https://idp.example", "sub": "ext-user"})

	tests := []struct {
		name string
		req  domain.ExchangeRequest
		want domain.Presented
	}{
		{
			name: "service account jwt",
			req:  domain.ExchangeRequest{SubjectToken: saJWT, SubjectTokenType: domain.TokenTypeJWT},
			want: domain.Presented{
				KeyID: "publickey-e00k", Algorithm: "HS256", Issuer: "serviceaccount-e00a", Subject: "serviceaccount-e00a",
				ServiceAccountID: "serviceaccount-e00a", CredentialID: "publickey-e00k", Token: saJWT,
			},
		},
		{
			name: "jwt with a kid that is not an auth public key",
			req:  domain.ExchangeRequest{SubjectToken: otherKidJWT, SubjectTokenType: domain.TokenTypeJWT},
			want: domain.Presented{
				KeyID: "some-other-kid", Algorithm: "HS256", Issuer: "serviceaccount-e00a", Subject: "serviceaccount-e00a",
				ServiceAccountID: "serviceaccount-e00a", CredentialID: "serviceaccount-e00a", Token: otherKidJWT,
			},
		},
		{
			name: "jwt without kid names the account from sub",
			req:  domain.ExchangeRequest{SubjectToken: noKidJWT, SubjectTokenType: domain.TokenTypeJWT},
			want: domain.Presented{
				Algorithm: "HS256", Issuer: "https://idp.example", Subject: "serviceaccount-e00a",
				ServiceAccountID: "serviceaccount-e00a", CredentialID: "serviceaccount-e00a", Token: noKidJWT,
			},
		},
		{
			name: "federated exchange reads the actor jwt",
			req: domain.ExchangeRequest{
				SubjectToken: "serviceaccount-e00a", SubjectTokenType: domain.TokenTypeSubjectIdentifier,
				ActorToken: extJWT, ActorTokenType: domain.TokenTypeJWT,
			},
			want: domain.Presented{
				KeyID: "ext-kid", Algorithm: "HS256", Issuer: "https://idp.example", Subject: "ext-user",
				ServiceAccountID: "serviceaccount-e00a", CredentialID: "serviceaccount-e00a", Token: extJWT,
			},
		},
		{
			name: "impersonation presents the actor access token",
			req: domain.ExchangeRequest{
				SubjectToken: "serviceaccount-e00a", SubjectTokenType: domain.TokenTypeSubjectIdentifier,
				ActorToken: "nmock.v1.claims.mac", ActorTokenType: domain.TokenTypeAccessToken,
			},
			want: domain.Presented{
				ServiceAccountID: "serviceaccount-e00a", CredentialID: "serviceaccount-e00a", Token: "nmock.v1.claims.mac",
			},
		},
		{
			name: "impersonation ignores a jwt-shaped actor token of another type",
			req: domain.ExchangeRequest{
				SubjectToken: "serviceaccount-e00a", SubjectTokenType: domain.TokenTypeSubjectIdentifier,
				ActorToken: extJWT, ActorTokenType: domain.TokenTypeAccessToken,
			},
			want: domain.Presented{
				ServiceAccountID: "serviceaccount-e00a", CredentialID: "serviceaccount-e00a", Token: extJWT,
			},
		},
		{
			name: "subject identifier that is not a service account",
			req: domain.ExchangeRequest{
				SubjectToken: "useraccount-1", SubjectTokenType: domain.TokenTypeSubjectIdentifier,
				ActorToken: "x", ActorTokenType: domain.TokenTypeAccessToken,
			},
			want: domain.Presented{Token: "x"},
		},
		{
			name: "garbage subject token",
			req:  domain.ExchangeRequest{SubjectToken: "garbage", SubjectTokenType: domain.TokenTypeJWT},
			want: domain.Presented{Token: "garbage"},
		},
		{
			name: "unknown subject token type presents nothing",
			req:  domain.ExchangeRequest{SubjectToken: saJWT, SubjectTokenType: "urn:other"},
			want: domain.Presented{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tt.want, domain.PresentedBy(tt.req)); diff != "" {
				t.Errorf("PresentedBy() (-want +got):\n%s", diff)
			}
		})
	}
}
