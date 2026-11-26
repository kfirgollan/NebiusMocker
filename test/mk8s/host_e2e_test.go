//go:build e2e_mk8s

package mk8s

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
)

// hostCheck is the host side of the user path: kubectl on this machine (not
// in a container) reaches the API server on its published 127.0.0.1 port,
// trusts the cluster CA from the cluster status, and authenticates with a
// Nebius IAM token (the editor dev token, which the webhook maps to
// nebius:editor). The exec plugin is not used here: on macOS the CLI only
// trusts the system keychain, which a test must not change; the user
// machine container covers it.
func hostCheck(t *testing.T, e *env, cluster *mk8spb.Cluster, nodes int) {
	t.Helper()
	ep := cluster.GetStatus().GetControlPlane().GetEndpoints().GetPublicEndpoint()
	port := ep[strings.LastIndex(ep, ":")+1:]
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, []byte(cluster.GetStatus().GetControlPlane().GetAuth().GetClusterCaCertificate()), 0o600); err != nil {
		t.Fatal(err)
	}
	kubectl := kubectlBinary(t, runtime.GOOS, runtime.GOARCH)
	empty := filepath.Join(t.TempDir(), "kubeconfig") // flags only: no context of the developer's ~/.kube
	if err := os.WriteFile(empty, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		base := []string{
			"--kubeconfig=" + empty, "--server=https://127.0.0.1:" + port,
			"--certificate-authority=" + caFile, "--token=" + config.DefaultEditorToken,
		}
		out, err := exec.CommandContext(t.Context(), kubectl, append(base, args...)...).CombinedOutput()
		return string(out), err
	}
	if out, err := run("get", "nodes", "-o", "name"); err != nil || strings.Count(out, "node/") != nodes {
		t.Fatalf("host kubectl get nodes (want %d): %v\n%s", nodes, err, out)
	}
	if out, _ := run("auth", "can-i", "create", "deployments"); lastLine(out) != "yes" {
		t.Fatalf("editor: can-i create deployments = %q, want yes", out)
	}
	if out, _ := run("auth", "can-i", "create", "clusterrolebindings"); lastLine(out) != "no" {
		t.Fatalf("editor: can-i create clusterrolebindings = %q, want no (only nebius:admin is cluster-admin)", out)
	}
	_ = e
}

// lastLine is kubectl's answer without the warnings printed before it.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
