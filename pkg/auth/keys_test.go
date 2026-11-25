package auth_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
)

func genRSA(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("rsa.GenerateKey(%d): %v", bits, err)
	}
	return k
}

func pemOf(typ string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

func pkixPEM(t *testing.T, pub any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	return pemOf("PUBLIC KEY", der)
}

func pkcs1PEM(k *rsa.PublicKey) string {
	return pemOf("RSA PUBLIC KEY", x509.MarshalPKCS1PublicKey(k))
}

func TestParsePublicKeyFingerprint(t *testing.T) {
	t.Parallel()
	key := genRSA(t, 2048)
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	want := hex.EncodeToString(sum[:])

	for name, data := range map[string]string{
		"PKIX":  pkixPEM(t, &key.PublicKey),
		"PKCS1": pkcs1PEM(&key.PublicKey),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := auth.ParsePublicKey(data)
			if err != nil {
				t.Fatalf("ParsePublicKey(%s) = %v", name, err)
			}
			if got.Fingerprint != want {
				t.Errorf("Fingerprint = %s, want %s", got.Fingerprint, want)
			}
			if got.Algorithm != "RSA" || got.Bits != 2048 {
				t.Errorf("Algorithm, Bits = %s, %d, want RSA, 2048", got.Algorithm, got.Bits)
			}
			if !got.Key.Equal(&key.PublicKey) {
				t.Errorf("parsed key differs from the original")
			}
		})
	}
}

func TestParsePublicKey4096(t *testing.T) {
	t.Parallel()
	key := genRSA(t, 4096)
	got, err := auth.ParsePublicKey(pkixPEM(t, &key.PublicKey))
	if err != nil {
		t.Fatalf("ParsePublicKey(4096) = %v", err)
	}
	if got.Bits != 4096 {
		t.Errorf("Bits = %d, want 4096", got.Bits)
	}
}

func TestParsePublicKeyRejects(t *testing.T) {
	t.Parallel()
	good := genRSA(t, 2048)
	goodPEM := pkixPEM(t, &good.PublicKey)
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(good)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&good.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data string
	}{
		{name: "empty", data: ""},
		{name: "not PEM", data: "ssh-rsa AAAAB3NzaC1yc2E user@host"},
		{name: "two blocks", data: goodPEM + goodPEM},
		{name: "certificate block", data: pemOf("CERTIFICATE", der)},
		{name: "private key block", data: pemOf("PRIVATE KEY", privDER)},
		{name: "bad DER", data: pemOf("PUBLIC KEY", []byte("junk"))},
		{name: "EC P-256", data: pkixPEM(t, &ec.PublicKey)},
		{name: "Ed25519", data: pkixPEM(t, edPub)},
		{name: "RSA 1024", data: pkixPEM(t, &genRSA(t, 1024).PublicKey)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := auth.ParsePublicKey(tt.data)
			if !errors.Is(err, auth.ErrUnsupportedKey) {
				t.Fatalf("ParsePublicKey(%s) = %v, want ErrUnsupportedKey", tt.name, err)
			}
		})
	}
}
