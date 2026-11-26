//go:build e2e_mk8s

package mk8s

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// KubectlVersion is the pinned kubectl release (the newest seeded
// control-plane version, 1.36).
const KubectlVersion = "v1.36.6"

// kubectlBinary returns the path of the pinned kubectl for goos/arch,
// downloading it once into the user cache dir
// (<UserCacheDir>/nebius-mocker/kubectl/<version>/<goos>-<arch>)
// and checking it against the published SHA-256. Nothing is installed
// system-wide; the user-machine container mounts the Linux one.
func kubectlBinary(t *testing.T, goos, arch string) string {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("user cache dir: %v", err)
	}
	dir := filepath.Join(cache, "nebius-mocker", "kubectl", KubectlVersion, goos+"-"+arch)
	bin := filepath.Join(dir, "kubectl")
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	base := fmt.Sprintf("https://dl.k8s.io/release/%s/bin/%s/%s/kubectl", KubectlVersion, goos, arch)
	sum := strings.Fields(string(download(t, base+".sha256")))
	if len(sum) == 0 {
		t.Fatalf("empty checksum from %s.sha256", base)
	}
	data := download(t, base)
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != sum[0] {
		t.Fatalf("kubectl %s checksum mismatch", base)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := bin + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, bin); err != nil {
		t.Fatal(err)
	}
	return bin
}

func download(t *testing.T, url string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("download %s (offline?): %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }() // best-effort: fully read below
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("download %s: %v", url, err)
	}
	return data
}
