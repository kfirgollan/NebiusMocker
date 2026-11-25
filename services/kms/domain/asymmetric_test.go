package domain

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func createAsym(t *testing.T, e *asymEnv, name string, alg kmspb.AsymmetricAlgorithm) string {
	t.Helper()
	op, err := e.svc.Create(t.Context(), CreateAsymmetricKeyCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: name},
		Spec:     &kmspb.AsymmetricKeySpec{Algorithm: alg},
	})
	if err != nil {
		t.Fatalf("Create(%q, %v): %v", name, alg, err)
	}
	return op.ResourceID
}

func parsePublicKey(t *testing.T, pemText string) any {
	t.Helper()
	block, rest := pem.Decode([]byte(pemText))
	if block == nil || block.Type != "PUBLIC KEY" || len(rest) != 0 {
		t.Fatalf("public key is not a single PUBLIC KEY PEM block: %q", pemText)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("ParsePKIXPublicKey: %v", err)
	}
	return pub
}

func TestAsymmetricCreateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		md       *commonpb.ResourceMetadata
		spec     *kmspb.AsymmetricKeySpec
		wantKind errs.Kind
	}{
		{
			name: "ok", md: &commonpb.ResourceMetadata{ParentId: testProject, Name: "k"},
			spec: &kmspb.AsymmetricKeySpec{Algorithm: kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256},
		},
		{
			name: "unspecified", md: &commonpb.ResourceMetadata{ParentId: testProject, Name: "k"},
			spec: &kmspb.AsymmetricKeySpec{}, wantKind: errs.KindBadRequest,
		},
		{name: "nil spec", md: &commonpb.ResourceMetadata{ParentId: testProject, Name: "k"}, wantKind: errs.KindBadRequest},
		{
			name: "bad parent", md: &commonpb.ResourceMetadata{ParentId: testTenant, Name: "k"},
			spec: &kmspb.AsymmetricKeySpec{Algorithm: kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256}, wantKind: errs.KindBadRequest,
		},
		{
			name: "unknown project", md: &commonpb.ResourceMetadata{ParentId: "project-nope", Name: "k"},
			spec: &kmspb.AsymmetricKeySpec{Algorithm: kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256}, wantKind: errs.KindNotFound,
		},
		{
			name: "id set", md: &commonpb.ResourceMetadata{Id: "kmsasymkey-x", ParentId: testProject, Name: "k"},
			spec: &kmspb.AsymmetricKeySpec{Algorithm: kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256}, wantKind: errs.KindBadRequest,
		},
		{
			name: "unknown algorithm", md: &commonpb.ResourceMetadata{ParentId: testProject, Name: "k"},
			spec: &kmspb.AsymmetricKeySpec{Algorithm: kmspb.AsymmetricAlgorithm(99)}, wantKind: errs.KindBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAsymEnv(t)
			_, err := e.svc.Create(t.Context(), CreateAsymmetricKeyCmd{Metadata: tt.md, Spec: tt.spec})
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Errorf("Create kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
		})
	}
}

func TestAsymmetricECDSASignHash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		alg  kmspb.AsymmetricAlgorithm
		hash func([]byte) []byte
		size int
	}{
		{"P-256", kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256, func(b []byte) []byte { h := sha256.Sum256(b); return h[:] }, 32},
		{"P-384", kmspb.AsymmetricAlgorithm_ECDSA_NIST_P384_SHA_384, func(b []byte) []byte { h := sha512.Sum384(b); return h[:] }, 48},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAsymEnv(t)
			id := createAsym(t, e, "k", tt.alg)
			pemText, err := e.svc.GetPublicKey(t.Context(), id)
			if err != nil {
				t.Fatalf("GetPublicKey: %v", err)
			}
			pub, ok := parsePublicKey(t, pemText).(*ecdsa.PublicKey)
			if !ok {
				t.Fatal("public key is not ECDSA")
			}
			digest := tt.hash([]byte("message"))
			sig, err := e.svc.SignHash(t.Context(), id, digest)
			if err != nil {
				t.Fatalf("SignHash: %v", err)
			}
			if !ecdsa.VerifyASN1(pub, digest, sig) {
				t.Error("signature does not verify against the PEM public key")
			}
			if ecdsa.VerifyASN1(pub, tt.hash([]byte("other")), sig) {
				t.Error("signature verified for a different digest")
			}

			for _, n := range []int{0, tt.size - 1, tt.size + 1, 64} {
				if n == tt.size {
					continue
				}
				if _, err := e.svc.SignHash(t.Context(), id, make([]byte, n)); !errs.IsKind(err, errs.KindBadRequest) {
					t.Errorf("SignHash(%d bytes) err = %v, want BadRequest", n, err)
				}
			}
			if _, err := e.svc.Decrypt(t.Context(), id, []byte("whatever")); !errs.IsKind(err, errs.KindBadRequest) {
				t.Errorf("Decrypt with ECDSA key err = %v, want BadRequest", err)
			}
		})
	}
}

func TestAsymmetricRSA(t *testing.T) {
	t.Parallel()
	e := newAsymEnv(t)
	id := createAsym(t, e, "rsa", kmspb.AsymmetricAlgorithm_RSA_4096_ENC_OAEP_SHA_256) // slow: one key for all subtests
	pemText, err := e.svc.GetPublicKey(t.Context(), id)
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	pub, ok := parsePublicKey(t, pemText).(*rsa.PublicKey)
	if !ok || pub.N.BitLen() != 4096 {
		t.Fatalf("public key = %T, want 4096-bit RSA", pub)
	}

	t.Run("decrypt", func(t *testing.T) {
		t.Parallel()
		ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, []byte("rsa secret"), nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := e.svc.Decrypt(t.Context(), id, ct)
		if err != nil || string(got) != "rsa secret" {
			t.Errorf("Decrypt = %q, %v, want rsa secret", got, err)
		}
	})
	t.Run("corrupted ciphertext", func(t *testing.T) {
		t.Parallel()
		ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, []byte("x"), nil)
		if err != nil {
			t.Fatal(err)
		}
		ct[len(ct)/2] ^= 0xff
		if _, err := e.svc.Decrypt(t.Context(), id, ct); !errs.IsKind(err, errs.KindBadRequest) {
			t.Errorf("Decrypt(corrupted) err = %v, want BadRequest", err)
		}
		if _, err := e.svc.Decrypt(t.Context(), id, []byte("short")); !errs.IsKind(err, errs.KindBadRequest) {
			t.Errorf("Decrypt(short) err = %v, want BadRequest", err)
		}
	})
	t.Run("wrong hash", func(t *testing.T) {
		t.Parallel()
		ct, err := rsa.EncryptOAEP(sha512.New(), rand.Reader, pub, []byte("x"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Decrypt(t.Context(), id, ct); !errs.IsKind(err, errs.KindBadRequest) {
			t.Errorf("Decrypt(OAEP-SHA512) err = %v, want BadRequest", err)
		}
	})
	t.Run("cannot sign", func(t *testing.T) {
		t.Parallel()
		if _, err := e.svc.SignHash(t.Context(), id, make([]byte, 32)); !errs.IsKind(err, errs.KindBadRequest) {
			t.Errorf("SignHash with RSA key err = %v, want BadRequest", err)
		}
	})
}

func TestAsymmetricUpdate(t *testing.T) {
	t.Parallel()
	e := newAsymEnv(t)
	id := createAsym(t, e, "k", kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256)
	md := func() *commonpb.ResourceMetadata {
		return &commonpb.ResourceMetadata{Id: id, ParentId: testProject, Name: "k"}
	}
	e.clk.Advance(time.Hour)

	if _, err := e.svc.Update(t.Context(), UpdateAsymmetricKeyCmd{Metadata: md()}); err != nil {
		t.Fatalf("no-op Update: %v", err)
	}
	k, _ := e.svc.Get(t.Context(), id, ActiveOnly)
	if k.GetMetadata().GetResourceVersion() != 1 {
		t.Errorf("no-op bumped version to %d", k.GetMetadata().GetResourceVersion())
	}
	if _, err := e.svc.Update(t.Context(), UpdateAsymmetricKeyCmd{Metadata: md(), Spec: &kmspb.AsymmetricKeySpec{Description: "d"}}); err != nil {
		t.Fatalf("Update description: %v", err)
	}
	k, _ = e.svc.Get(t.Context(), id, ActiveOnly)
	if k.GetSpec().GetDescription() != "d" || k.GetMetadata().GetResourceVersion() != 2 || !k.GetMetadata().GetUpdatedAt().AsTime().Equal(e.clk.Now()) {
		t.Errorf("after update: %v, want description d, version 2, updated_at now", k)
	}
	if _, err := e.svc.Update(t.Context(), UpdateAsymmetricKeyCmd{Metadata: md(), Spec: &kmspb.AsymmetricKeySpec{Algorithm: kmspb.AsymmetricAlgorithm_RSA_4096_ENC_OAEP_SHA_256}}); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("algorithm change err = %v, want BadRequest", err)
	}
	stale := md()
	stale.ResourceVersion = 1
	if _, err := e.svc.Update(t.Context(), UpdateAsymmetricKeyCmd{Metadata: stale, Spec: &kmspb.AsymmetricKeySpec{Description: "z"}}); !errs.IsKind(err, errs.KindConflict) {
		t.Errorf("stale version err = %v, want Conflict", err)
	}
}

func TestAsymmetricDeletion(t *testing.T) {
	t.Parallel()
	e := newAsymEnv(t)
	ctx := t.Context()
	id := createAsym(t, e, "k", kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256)
	digest := make([]byte, 32)
	if _, err := e.svc.SignHash(ctx, id, digest); err != nil {
		t.Fatal(err)
	}

	if _, err := e.svc.Undelete(ctx, id, "", nil); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("Undelete on active key err = %v, want BadState", err)
	}
	if _, err := e.svc.Delete(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Get(ctx, id, ActiveOnly); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after Delete err = %v, want NotFound", err)
	}
	k, err := e.svc.Get(ctx, id, IncludeScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if k.GetStatus().GetState() != kmspb.KeyState_SCHEDULED_FOR_DELETION ||
		!k.GetStatus().GetDeletedAt().AsTime().Equal(t0) || !k.GetStatus().GetPurgeAt().AsTime().Equal(t0.Add(DefaultDeletionDelay)) {
		t.Errorf("status = %v", k.GetStatus())
	}
	if _, err := e.svc.SignHash(ctx, id, digest); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("SignHash on scheduled key err = %v, want BadState", err)
	}
	if _, err := e.svc.GetPublicKey(ctx, id); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("GetPublicKey on scheduled key err = %v, want BadState", err)
	}
	if _, err := e.svc.Decrypt(ctx, id, nil); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("Decrypt on scheduled key err = %v, want BadState", err)
	}

	if _, err := e.svc.UpdateDeletionDelay(ctx, id, time.Hour, nil); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("short delay err = %v, want BadRequest", err)
	}
	if _, err := e.svc.UpdateDeletionDelay(ctx, id, 24*time.Hour, nil); err != nil {
		t.Fatalf("UpdateDeletionDelay: %v", err)
	}

	// Restore, then sign again.
	if _, err := e.svc.Undelete(ctx, id, "", nil); err != nil {
		t.Fatalf("Undelete: %v", err)
	}
	if _, err := e.svc.SignHash(ctx, id, digest); err != nil {
		t.Errorf("SignHash after Undelete: %v", err)
	}

	// Delete again and let the purge time pass.
	if _, err := e.svc.Delete(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(DefaultDeletionDelay)
	if _, err := e.svc.Get(ctx, id, IncludeScheduled); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after purge err = %v, want NotFound", err)
	}
	if _, err := e.repo.Material(ctx, id); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("material after purge err = %v, want NotFound", err)
	}
}

func TestNewAsymmetricKeyServiceRequiresDeps(t *testing.T) {
	t.Parallel()
	if _, err := NewAsymmetricKeyService(KeyDeps[*kmspb.AsymmetricKey]{}); err == nil {
		t.Error("NewAsymmetricKeyService with no deps succeeded, want error")
	}
}
