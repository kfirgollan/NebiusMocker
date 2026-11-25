package localca_test

import (
	"bytes"
	"crypto/x509"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/localca"
)

var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestLoadOrCreatePersistsAndIssues(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clk := clock.NewManual(start)
	ca, err := localca.LoadOrCreate(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, localca.CertFile))
	if err != nil || !bytes.Equal(onDisk, ca.CertPEM()) {
		t.Fatalf("ca.pem = %q, %v; want the CA certificate", onDisk, err)
	}
	again, err := localca.LoadOrCreate(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.CertPEM(), ca.CertPEM()) {
		t.Fatal("second load created a new CA")
	}

	leaf, err := ca.Issue("127.0.0.1", "localhost", "*.ai.localhost")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(onDisk) {
		t.Fatal("ca.pem is not a PEM certificate")
	}
	for _, host := range []string{"127.0.0.1", "localhost", "aiendpoint-1-p22.ai.localhost"} {
		if _, err := cert.Verify(x509.VerifyOptions{DNSName: host, Roots: pool, CurrentTime: start}); err != nil {
			t.Errorf("verify %s: %v", host, err)
		}
	}
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "example.com", Roots: pool, CurrentTime: start}); err == nil {
		t.Error("leaf verified for a host it was not issued for")
	}
}

func TestLoadOrCreateConcurrentAgree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clk := clock.NewManual(start)
	const n = 8
	pems := make([][]byte, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			ca, err := localca.LoadOrCreate(dir, clk)
			if err != nil {
				t.Error(err)
				return
			}
			pems[i] = ca.CertPEM()
		})
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if !bytes.Equal(pems[i], pems[0]) {
			t.Fatalf("process %d loaded a different CA", i)
		}
	}
}

func TestLoadOrCreateRejectsCorruptFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca-key.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := localca.LoadOrCreate(dir, clock.NewManual(start)); err == nil {
		t.Fatal("corrupt CA file accepted")
	}
}
