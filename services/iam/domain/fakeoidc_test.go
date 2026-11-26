package domain_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

func TestFakeOIDCIssuersMintVerifies(t *testing.T) {
	t.Parallel()
	clk := clock.NewManual(txT0)
	f, err := domain.NewFakeOIDCIssuers(clk)
	if err != nil {
		t.Fatal(err)
	}
	const iss = "http://localhost:4566/_mocker/oidc/ci"
	tok, err := f.Mint("ci", domain.FakeOIDCMintCmd{Issuer: iss, Subject: "runner", Audience: "nebius"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	doc, err := f.JWKS("ci")
	if err != nil {
		t.Fatal(err)
	}
	set, err := auth.ParseJWKSet(doc)
	if err != nil || len(set.Keys) != 1 || set.Keys[0].KeyID == "" {
		t.Fatalf("JWKS = %s (%v), want one named key", doc, err)
	}
	j, err := auth.ParseExternalJWT(tok)
	if err != nil {
		t.Fatal(err)
	}
	if j.Issuer != iss || j.Subject != "runner" || j.Alg != "RS256" || j.KeyID != set.Keys[0].KeyID {
		t.Errorf("token = %+v, want iss %s sub runner RS256 kid %s", j, iss, set.Keys[0].KeyID)
	}
	if err := auth.VerifyExternalJWT(j, set, clk.Now()); err != nil {
		t.Errorf("minted token does not verify: %v", err)
	}
	// Default TTL is an hour on the mocker clock.
	if err := auth.VerifyExternalJWT(j, set, clk.Now().Add(domain.DefaultFakeOIDCTokenTTL)); err == nil {
		t.Error("token still valid after the default TTL")
	}
	// Issuers have their own keys; one issuer's key is stable.
	other, err := f.JWKS("other")
	if err != nil || bytes.Equal(other, doc) {
		t.Errorf("issuer other shares the key of ci (%v)", err)
	}
	if again, _ := f.JWKS("ci"); !bytes.Equal(again, doc) {
		t.Error("the key of issuer ci changed between calls")
	}
	short, err := f.Mint("ci", domain.FakeOIDCMintCmd{Issuer: iss, Subject: "runner", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	js, _ := auth.ParseExternalJWT(short)
	if err := auth.VerifyExternalJWT(js, set, clk.Now().Add(2*time.Minute)); err == nil {
		t.Error("1m token still valid after 2m")
	}
}

func TestFakeOIDCIssuersRefusals(t *testing.T) {
	t.Parallel()
	f, err := domain.NewFakeOIDCIssuers(clock.NewManual(txT0))
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		issuer string
		cmd    domain.FakeOIDCMintCmd
		field  string
	}{
		"no sub":       {"ci", domain.FakeOIDCMintCmd{Issuer: "http://x"}, "sub"},
		"negative ttl": {"ci", domain.FakeOIDCMintCmd{Issuer: "http://x", Subject: "s", TTL: -time.Second}, "ttl"},
		"bad name":     {"../etc", domain.FakeOIDCMintCmd{Issuer: "http://x", Subject: "s"}, "name"},
	} {
		_, err := f.Mint(tt.issuer, tt.cmd)
		if !errs.IsKind(err, errs.KindBadRequest) || !hasViolation(err, tt.field) {
			t.Errorf("%s: %v, want BadRequest naming %s", name, err, tt.field)
		}
	}
	if _, err := f.JWKS(""); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("JWKS of an empty name = %v, want BadRequest", err)
	}
	if _, err := domain.NewFakeOIDCIssuers(nil); err == nil {
		t.Error("NewFakeOIDCIssuers(nil) succeeded")
	}
}
