package contract

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/nebius/gosdk/operations"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"
)

func kmsCreateAsym(t *testing.T, c kmsClients, project, name string, alg kmspb.AsymmetricAlgorithm) string {
	t.Helper()
	op, err := c.asym.Create(t.Context(), &kmspb.CreateAsymmetricKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: project, Name: name},
		Spec:     &kmspb.AsymmetricKeySpec{Algorithm: alg},
	})
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}
	kmsMustWait(t, op, nil)
	return op.ResourceID()
}

func kmsEncrypt(t *testing.T, c kmsClients, keyID string, aad, pt []byte) []byte {
	t.Helper()
	r, err := c.symCrypto.Encrypt(t.Context(), &kmspb.SymmetricEncryptRequest{KeyId: keyID, AadContext: aad, Plaintext: pt})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return r.GetCiphertext()
}

func kmsDecrypt(t *testing.T, c kmsClients, keyID string, aad, ct []byte) ([]byte, error) {
	t.Helper()
	r, err := c.symCrypto.Decrypt(t.Context(), &kmspb.SymmetricDecryptRequest{KeyId: keyID, AadContext: aad, Ciphertext: ct})
	return r.GetPlaintext(), err
}

func kmsPublicKey(t *testing.T, c kmsClients, keyID string) any {
	t.Helper()
	r, err := c.asymCrypto.GetPublicKey(t.Context(), &kmspb.AsymmetricGetPublicKeyRequest{KeyId: keyID})
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	block, _ := pem.Decode([]byte(r.GetPublicKey()))
	if block == nil {
		t.Fatalf("public key is not PEM: %q", r.GetPublicKey())
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse public key: %v", err)
	}
	return pub
}

func TestKMSSymmetricRoundTrip(t *testing.T) {
	t.Parallel()
	inst, c := startKMS(t)
	key := kmsSymKind(c).mustCreate(t, inst.ProjectID, "crypto", nil)
	msg, aad := []byte("attack at dawn"), []byte("ctx=orders")

	ct := kmsEncrypt(t, c, key.id, aad, msg)
	if bytes.Contains(ct, msg) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := kmsDecrypt(t, c, key.id, aad, ct)
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("Decrypt = %q, %v; want %q", got, err, msg)
	}

	// Empty AAD round trip too.
	got, err = kmsDecrypt(t, c, key.id, nil, kmsEncrypt(t, c, key.id, nil, msg))
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("Decrypt without aad = %q, %v", got, err)
	}

	_, err = kmsDecrypt(t, c, key.id, []byte("ctx=other"), ct)
	kmsWantCode(t, err, codes.InvalidArgument)
	_, err = kmsDecrypt(t, c, key.id, nil, ct)
	kmsWantCode(t, err, codes.InvalidArgument)

	for _, i := range []int{0, 1, 5, len(ct) - 1} {
		bad := bytes.Clone(ct)
		bad[i] ^= 0xff
		_, err = kmsDecrypt(t, c, key.id, aad, bad)
		if kmsCode(err) != codes.InvalidArgument {
			t.Errorf("tampered byte %d: code = %v, want InvalidArgument", i, kmsCode(err))
		}
	}
	_, err = kmsDecrypt(t, c, key.id, aad, ct[:3])
	kmsWantCode(t, err, codes.InvalidArgument)

	// A second key cannot open the first key's ciphertext.
	other := kmsSymKind(c).mustCreate(t, inst.ProjectID, "other", nil)
	_, err = kmsDecrypt(t, c, other.id, aad, ct)
	kmsWantCode(t, err, codes.InvalidArgument)

	_, err = c.symCrypto.Encrypt(t.Context(), &kmspb.SymmetricEncryptRequest{KeyId: "symkey-e00doesnotexist", Plaintext: msg})
	kmsWantCode(t, err, codes.NotFound)
	_, err = c.symCrypto.Encrypt(t.Context(), &kmspb.SymmetricEncryptRequest{Plaintext: msg})
	kmsWantCode(t, err, codes.InvalidArgument)
}

func TestKMSSymmetricRotateKeepsOldCiphertexts(t *testing.T) {
	t.Parallel()
	inst, c := startKMS(t)
	key := kmsSymKind(c).mustCreate(t, inst.ProjectID, "rotated", nil)
	aad := []byte("rot")

	old := kmsEncrypt(t, c, key.id, aad, []byte("old"))
	op, err := c.sym.Rotate(t.Context(), &kmspb.RotateSymmetricKeyRequest{Id: key.id})
	kmsMustWait(t, op, err)
	fresh := kmsEncrypt(t, c, key.id, aad, []byte("new"))

	if v := binary.BigEndian.Uint32(old[1:5]); v != 1 {
		t.Errorf("old ciphertext key version = %d, want 1", v)
	}
	if v := binary.BigEndian.Uint32(fresh[1:5]); v != 2 {
		t.Errorf("new ciphertext key version = %d, want 2", v)
	}
	for _, tc := range []struct {
		ct   []byte
		want string
	}{{old, "old"}, {fresh, "new"}} {
		got, err := kmsDecrypt(t, c, key.id, aad, tc.ct)
		if err != nil || string(got) != tc.want {
			t.Errorf("Decrypt = %q, %v; want %q", got, err, tc.want)
		}
	}
}

func TestKMSGenerateDataKey(t *testing.T) {
	t.Parallel()
	inst, c := startKMS(t)
	key := kmsSymKind(c).mustCreate(t, inst.ProjectID, "envelope", nil)
	aad := []byte("dek")

	r, err := c.symCrypto.GenerateDataKey(t.Context(), &kmspb.GenerateDataKeyRequest{
		KeyId: key.id, AadContext: aad, DataKeySpec: kmspb.SymmetricAlgorithm_AES_256,
	})
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	if len(r.GetDataKeyPlaintext()) != 32 {
		t.Fatalf("data key is %d bytes, want 32", len(r.GetDataKeyPlaintext()))
	}
	unwrapped, err := kmsDecrypt(t, c, key.id, aad, r.GetDataKeyCiphertext())
	if err != nil || !bytes.Equal(unwrapped, r.GetDataKeyPlaintext()) {
		t.Fatalf("unwrapped data key = %x, %v; want %x", unwrapped, err, r.GetDataKeyPlaintext())
	}

	// The data key is a working AES-256-GCM key.
	block, err := aes.NewCipher(unwrapped)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("NewGCM: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	sealed := gcm.Seal(nil, nonce, []byte("payload"), nil)
	if opened, err := gcm.Open(nil, nonce, sealed, nil); err != nil || string(opened) != "payload" {
		t.Fatalf("local AES-GCM round trip = %q, %v", opened, err)
	}

	// skip_plaintext returns only the wrapped key.
	r, err = c.symCrypto.GenerateDataKey(t.Context(), &kmspb.GenerateDataKeyRequest{
		KeyId: key.id, DataKeySpec: kmspb.SymmetricAlgorithm_AES_256, SkipPlaintext: true,
	})
	if err != nil {
		t.Fatalf("GenerateDataKey(skip_plaintext): %v", err)
	}
	if len(r.GetDataKeyPlaintext()) != 0 || len(r.GetDataKeyCiphertext()) == 0 {
		t.Errorf("skip_plaintext: plaintext=%d bytes, ciphertext=%d bytes", len(r.GetDataKeyPlaintext()), len(r.GetDataKeyCiphertext()))
	}
	if pt, err := kmsDecrypt(t, c, key.id, nil, r.GetDataKeyCiphertext()); err != nil || len(pt) != 32 {
		t.Errorf("unwrap skipped key = %d bytes, %v; want 32", len(pt), err)
	}

	_, err = c.symCrypto.GenerateDataKey(t.Context(), &kmspb.GenerateDataKeyRequest{
		KeyId: key.id, DataKeySpec: kmspb.SymmetricAlgorithm_AES_128,
	})
	kmsWantCode(t, err, codes.InvalidArgument)
}

func TestKMSECDSASignHash(t *testing.T) {
	t.Parallel()
	inst, c := startKMS(t)
	for _, tc := range []struct {
		name   string
		alg    kmspb.AsymmetricAlgorithm
		curve  elliptic.Curve
		digest func([]byte) []byte
	}{
		{"p256", kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256, elliptic.P256(), func(b []byte) []byte { h := sha256.Sum256(b); return h[:] }},
		{"p384", kmspb.AsymmetricAlgorithm_ECDSA_NIST_P384_SHA_384, elliptic.P384(), func(b []byte) []byte { h := sha512.Sum384(b); return h[:] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id := kmsCreateAsym(t, c, inst.ProjectID, tc.name, tc.alg)
			pub, ok := kmsPublicKey(t, c, id).(*ecdsa.PublicKey)
			if !ok || pub.Curve != tc.curve {
				t.Fatalf("public key = %T on %v, want ECDSA on %v", pub, pub, tc.curve)
			}
			digest := tc.digest([]byte("sign me"))
			r, err := c.asymCrypto.SignHash(t.Context(), &kmspb.AsymmetricSignHashRequest{KeyId: id, Hash: digest})
			if err != nil {
				t.Fatalf("SignHash: %v", err)
			}
			if !ecdsa.VerifyASN1(pub, digest, r.GetSignature()) {
				t.Error("signature does not verify against the public key")
			}
			if ecdsa.VerifyASN1(pub, tc.digest([]byte("other")), r.GetSignature()) {
				t.Error("signature verifies for a different digest")
			}

			_, err = c.asymCrypto.SignHash(t.Context(), &kmspb.AsymmetricSignHashRequest{KeyId: id, Hash: digest[:10]})
			kmsWantCode(t, err, codes.InvalidArgument)
			// ECDSA keys cannot decrypt.
			_, err = c.asymCrypto.Decrypt(t.Context(), &kmspb.AsymmetricDecryptRequest{KeyId: id, Ciphertext: make([]byte, 512)})
			kmsWantCode(t, err, codes.InvalidArgument)
		})
	}
}

func TestKMSRSADecrypt(t *testing.T) {
	t.Parallel()
	inst, c := startKMS(t)
	id := kmsCreateAsym(t, c, inst.ProjectID, "rsa", kmspb.AsymmetricAlgorithm_RSA_4096_ENC_OAEP_SHA_256)
	pub, ok := kmsPublicKey(t, c, id).(*rsa.PublicKey)
	if !ok || pub.N.BitLen() != 4096 {
		t.Fatalf("public key = %T, want 4096-bit RSA", pub)
	}
	msg := []byte("rsa secret")
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, msg, nil)
	if err != nil {
		t.Fatalf("EncryptOAEP: %v", err)
	}
	r, err := c.asymCrypto.Decrypt(t.Context(), &kmspb.AsymmetricDecryptRequest{KeyId: id, Ciphertext: ct})
	if err != nil || !bytes.Equal(r.GetPlaintext(), msg) {
		t.Fatalf("Decrypt = %q, %v; want %q", r.GetPlaintext(), err, msg)
	}

	bad := bytes.Clone(ct)
	bad[10] ^= 0xff
	_, err = c.asymCrypto.Decrypt(t.Context(), &kmspb.AsymmetricDecryptRequest{KeyId: id, Ciphertext: bad})
	kmsWantCode(t, err, codes.InvalidArgument)

	// RSA keys cannot sign.
	digest := sha256.Sum256(msg)
	_, err = c.asymCrypto.SignHash(t.Context(), &kmspb.AsymmetricSignHashRequest{KeyId: id, Hash: digest[:]})
	kmsWantCode(t, err, codes.InvalidArgument)
}

func TestKMSCryptoOnScheduledKey(t *testing.T) {
	t.Parallel()
	inst, c := startKMS(t)
	sym := kmsSymKind(c)
	key := sym.mustCreate(t, inst.ProjectID, "sched", nil)
	ec := kmsCreateAsym(t, c, inst.ProjectID, "sched-ec", kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256)
	digest := sha256.Sum256([]byte("x"))
	aad := []byte("a")
	ct := kmsEncrypt(t, c, key.id, aad, []byte("kept"))

	sym.mustDelete(t, key.id)
	delOp, err := c.asym.Delete(t.Context(), &kmspb.DeleteAsymmetricKeyRequest{Id: ec})
	kmsMustWait(t, delOp, err)

	_, err = c.symCrypto.Encrypt(t.Context(), &kmspb.SymmetricEncryptRequest{KeyId: key.id, Plaintext: []byte("x")})
	kmsWantCode(t, err, codes.FailedPrecondition)
	_, err = kmsDecrypt(t, c, key.id, aad, ct)
	kmsWantCode(t, err, codes.FailedPrecondition)
	_, err = c.symCrypto.GenerateDataKey(t.Context(), &kmspb.GenerateDataKeyRequest{KeyId: key.id, DataKeySpec: kmspb.SymmetricAlgorithm_AES_256})
	kmsWantCode(t, err, codes.FailedPrecondition)
	_, err = c.asymCrypto.SignHash(t.Context(), &kmspb.AsymmetricSignHashRequest{KeyId: ec, Hash: digest[:]})
	kmsWantCode(t, err, codes.FailedPrecondition)
	_, err = c.asymCrypto.GetPublicKey(t.Context(), &kmspb.AsymmetricGetPublicKeyRequest{KeyId: ec})
	kmsWantCode(t, err, codes.FailedPrecondition)

	var op operations.Operation
	op, err = c.sym.Undelete(t.Context(), &kmspb.UndeleteSymmetricKeyRequest{Id: key.id})
	kmsMustWait(t, op, err)
	op, err = c.asym.Undelete(t.Context(), &kmspb.UndeleteAsymmetricKeyRequest{Id: ec})
	kmsMustWait(t, op, err)

	got, err := kmsDecrypt(t, c, key.id, aad, ct)
	if err != nil || string(got) != "kept" {
		t.Fatalf("Decrypt after undelete = %q, %v; want kept", got, err)
	}
	if _, err := c.asymCrypto.SignHash(t.Context(), &kmspb.AsymmetricSignHashRequest{KeyId: ec, Hash: digest[:]}); err != nil {
		t.Errorf("SignHash after undelete: %v", err)
	}
}
