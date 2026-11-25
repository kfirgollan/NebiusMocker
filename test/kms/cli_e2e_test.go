//go:build e2e

// Package kms_test runs the KMS workflows from https://docs.nebius.com/kms with
// the official nebius CLI against an in-process mocker.
package kms_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/kms"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

func newCLI(t *testing.T) *nebiuscli.CLI {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), kms.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	return nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
}

// run runs a CLI command and returns its trimmed stdout.
func run(t *testing.T, cli *nebiuscli.CLI, args ...string) string {
	t.Helper()
	res, err := cli.Run(t.Context(), args...)
	if err != nil {
		t.Fatalf("nebius %s: %v\nstderr: %s", strings.Join(args, " "), err, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

// fails runs a CLI command that must fail and returns its stderr.
func fails(t *testing.T, cli *nebiuscli.CLI, args ...string) string {
	t.Helper()
	res, err := cli.Run(t.Context(), args...)
	if err == nil {
		t.Fatalf("nebius %s succeeded, want an error; stdout: %s", strings.Join(args, " "), res.Stdout)
	}
	return res.Stderr + res.Stdout
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode base64 %q: %v", s, err)
	}
	return b
}

// TestSymmetricQuickstart follows the symmetric quickstart, symmetric
// cryptography and key management pages: create, encrypt, decrypt (with AAD),
// rotate, envelope encryption with a data key, delete and undelete.
func TestSymmetricQuickstart(t *testing.T) {
	cli := newCLI(t)
	keyID := run(t, cli, "kms", "symmetric-key", "create",
		"--name", "my-symmetric-key", "--algorithm", "aes_256", "--format", "jsonpath={.metadata.id}")
	if !strings.HasPrefix(keyID, "kmssymkey-") {
		t.Fatalf("created key id = %q", keyID)
	}
	if got := run(t, cli, "kms", "symmetric-key", "get", "--id", keyID, "--format", "jsonpath={.spec.rotation_period}"); got == "" {
		t.Errorf("rotation_period is empty, want the 90-day default")
	}

	plaintext := b64([]byte("Hello world"))
	if plaintext != "SGVsbG8gd29ybGQ=" {
		t.Fatalf("base64 = %s", plaintext)
	}
	ct := run(t, cli, "kms", "symmetric-crypto", "encrypt", "--key-id", keyID, "--plaintext", plaintext, "--format", "jsonpath={.ciphertext}")
	got := run(t, cli, "kms", "symmetric-crypto", "decrypt", "--key-id", keyID, "--ciphertext", ct, "--format", "jsonpath={.plaintext}")
	if string(unb64(t, got)) != "Hello world" {
		t.Fatalf("decrypt = %q, want Hello world", got)
	}

	aad := b64([]byte("context-1"))
	ctAAD := run(t, cli, "kms", "symmetric-crypto", "encrypt", "--key-id", keyID, "--plaintext", plaintext, "--aad-context", aad, "--format", "jsonpath={.ciphertext}")
	fails(t, cli, "kms", "symmetric-crypto", "decrypt", "--key-id", keyID, "--ciphertext", ctAAD)
	got = run(t, cli, "kms", "symmetric-crypto", "decrypt", "--key-id", keyID, "--ciphertext", ctAAD, "--aad-context", aad, "--format", "jsonpath={.plaintext}")
	if got != plaintext {
		t.Fatalf("decrypt with aad = %q, want %q", got, plaintext)
	}

	run(t, cli, "kms", "symmetric-key", "rotate", "--id", keyID)
	if got := run(t, cli, "kms", "symmetric-crypto", "decrypt", "--key-id", keyID, "--ciphertext", ct, "--format", "jsonpath={.plaintext}"); got != plaintext {
		t.Fatalf("decrypt of pre-rotation ciphertext = %q", got)
	}

	// Envelope encryption: the wrapped data key unwraps to the plaintext one.
	var dk struct {
		Plaintext  string `json:"data_key_plaintext"`
		Ciphertext string `json:"data_key_ciphertext"`
	}
	cli.JSON(t, &dk, "kms", "symmetric-crypto", "generate-data-key", "--key-id", keyID, "--data-key-spec", "aes_256", "--aad-context", aad)
	if len(unb64(t, dk.Plaintext)) != 32 {
		t.Fatalf("data key is %d bytes, want 32", len(unb64(t, dk.Plaintext)))
	}
	if got := run(t, cli, "kms", "symmetric-crypto", "decrypt", "--key-id", keyID, "--ciphertext", dk.Ciphertext, "--aad-context", aad, "--format", "jsonpath={.plaintext}"); got != dk.Plaintext {
		t.Fatalf("unwrapped data key differs from the generated one")
	}
	var skipped struct {
		Plaintext  string `json:"data_key_plaintext"`
		Ciphertext string `json:"data_key_ciphertext"`
	}
	cli.JSON(t, &skipped, "kms", "symmetric-crypto", "generate-data-key", "--key-id", keyID, "--data-key-spec", "aes_256", "--skip-plaintext")
	if skipped.Plaintext != "" || skipped.Ciphertext == "" {
		t.Fatalf("skip-plaintext returned plaintext=%q ciphertext=%q", skipped.Plaintext, skipped.Ciphertext)
	}

	if !strings.Contains(run(t, cli, "kms", "symmetric-key", "list"), keyID) {
		t.Fatalf("list does not show %s", keyID)
	}

	run(t, cli, "kms", "symmetric-key", "delete", "--id", keyID)
	if msg := fails(t, cli, "kms", "symmetric-crypto", "encrypt", "--key-id", keyID, "--plaintext", plaintext); !strings.Contains(msg, "SCHEDULED_FOR_DELETION") {
		t.Errorf("encrypt with a deleted key: error %q does not say the key is scheduled for deletion", msg)
	}
	if strings.Contains(run(t, cli, "kms", "symmetric-key", "list"), keyID) {
		t.Fatalf("list shows the deleted key %s", keyID)
	}
	run(t, cli, "kms", "symmetric-key", "undelete", "--id", keyID)
	if got := run(t, cli, "kms", "symmetric-crypto", "decrypt", "--key-id", keyID, "--ciphertext", ct, "--format", "jsonpath={.plaintext}"); got != plaintext {
		t.Fatalf("decrypt after undelete = %q", got)
	}
}

// TestAsymmetricQuickstart follows the asymmetric quickstart: sign a SHA-256
// hash with a P-256 key and verify it with openssl against the public key.
func TestAsymmetricQuickstart(t *testing.T) {
	cli := newCLI(t)
	keyID := run(t, cli, "kms", "asymmetric-key", "create",
		"--name", "my-asymmetric-key", "--algorithm", "ecdsa_nist_p256_sha_256", "--format", "jsonpath={.metadata.id}")
	pubPEM := run(t, cli, "kms", "asymmetric-crypto", "get-public-key", "--key-id", keyID, "--format", "jsonpath={.public_key}")

	msg := []byte("Hello world\n")
	digest := sha256.Sum256(msg)
	sigB64 := run(t, cli, "kms", "asymmetric-crypto", "sign-hash", "--key-id", keyID, "--hash", b64(digest[:]), "--format", "jsonpath={.signature}")
	sig := unb64(t, sigB64)

	pub := parsePublicKey(t, pubPEM)
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("public key is %T, want ECDSA", pub)
	}
	if !ecdsa.VerifyASN1(ec, digest[:], sig) {
		t.Fatalf("signature does not verify")
	}

	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Log("openssl not installed: skipping the openssl verification step")
		return
	}
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cmd := exec.CommandContext(t.Context(), openssl, "dgst", "-sha256",
		"-verify", write("public_key.pem", []byte(pubPEM+"\n")),
		"-signature", write("signature.bin", sig), write("hello.txt", msg))
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("Verified OK")) {
		t.Fatalf("openssl verify: %v: %s", err, out)
	}
}

// TestAsymmetricDecrypt encrypts locally with the RSA public key (OAEP,
// SHA-256, MGF1-SHA-256, as the docs' openssl pkeyutl command) and decrypts
// with the CLI.
func TestAsymmetricDecrypt(t *testing.T) {
	cli := newCLI(t)
	keyID := run(t, cli, "kms", "asymmetric-key", "create",
		"--name", "rsa", "--algorithm", "rsa_4096_enc_oaep_sha_256", "--format", "jsonpath={.metadata.id}")
	pub, ok := parsePublicKey(t, run(t, cli, "kms", "asymmetric-crypto", "get-public-key", "--key-id", keyID, "--format", "jsonpath={.public_key}")).(*rsa.PublicKey)
	if !ok {
		t.Fatal("public key is not RSA")
	}
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, []byte("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := run(t, cli, "kms", "asymmetric-crypto", "decrypt", "--key-id", keyID, "--ciphertext", b64(ct), "--format", "jsonpath={.plaintext}")
	if string(unb64(t, got)) != "secret" {
		t.Fatalf("decrypt = %q", got)
	}
}

func parsePublicKey(t *testing.T, s string) any {
	t.Helper()
	block, _ := pem.Decode([]byte(s))
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatalf("public key is not a PUBLIC KEY PEM block: %q", s)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}
