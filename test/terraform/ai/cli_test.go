//go:build e2e

package ai

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

// endpointDoc is the subset of the CLI's JSON output for an endpoint.
type endpointDoc struct {
	Metadata struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		State           string   `json:"state"`
		PublicEndpoints []string `json:"public_endpoints"`
	} `json:"status"`
}

// TestEndpointQuickstartCLI runs the Nebius docs quickstart for serverless AI
// endpoints with the official CLI, then calls the endpoint like curl would.
func TestEndpointQuickstartCLI(t *testing.T) {
	inst := startMockWithVPC(t)
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, TenantID: inst.TenantID})
	const (
		name      = "qs-endpoint-nginx"
		authToken = "qs-secret-token"
	)
	// The docs point users at the subnet VPC provides by default.
	var subnet endpointDoc
	cli.JSON(t, &subnet, "vpc", "subnet", "get-by-name", "--name", "default-subnet", "--parent-id", inst.ProjectID)
	subnetID := subnet.Metadata.ID
	if subnetID == "" {
		t.Fatal("nebius vpc subnet get-by-name default-subnet returned no ID")
	}
	run := func(args ...string) string {
		t.Helper()
		res, err := cli.Run(t.Context(), args...)
		if err != nil {
			t.Fatalf("%v", err)
		}
		return res.Stdout
	}
	get := func(id string) endpointDoc {
		t.Helper()
		var e endpointDoc
		cli.JSON(t, &e, "ai", "endpoint", "get", id)
		return e
	}

	run("ai", "endpoint", "create", "--name", name, "--image", "nginx:alpine",
		"--platform", "cpu-d3", "--preset", "4vcpu-16gb", "--public", "--container-port", "80",
		"--auth", "token", "--token", authToken, "--subnet-id", subnetID, "--parent-id", inst.ProjectID)

	id := strings.TrimSpace(run("ai", "endpoint", "get-by-name", "--name", name,
		"--parent-id", inst.ProjectID, "--format", "jsonpath={.metadata.id}"))
	if id == "" {
		t.Fatal("get-by-name returned no ID")
	}

	e := get(id)
	if e.Status.State != "RUNNING" {
		t.Fatalf("state after create = %q, want RUNNING", e.Status.State)
	}
	// The docs pick the URL with: select(startswith("https://")) | first.
	i := slices.IndexFunc(e.Status.PublicEndpoints, func(u string) bool { return strings.HasPrefix(u, "https://") })
	if i < 0 {
		t.Fatalf("public_endpoints = %v, want an https:// URL", e.Status.PublicEndpoints)
	}
	url := e.Status.PublicEndpoints[i]

	var list struct {
		Items []endpointDoc `json:"items"`
	}
	cli.JSON(t, &list, "ai", "endpoint", "list", "--parent-id", inst.ProjectID)
	if !slices.ContainsFunc(list.Items, func(d endpointDoc) bool { return d.Metadata.ID == id }) {
		t.Fatalf("endpoint list = %+v, want %s", list.Items, id)
	}

	curl(t, inst.CAFile, url, authToken, 200)
	curl(t, inst.CAFile, url, "", 401)
	curl(t, inst.CAFile, url, "wrong-token", 401)

	run("ai", "endpoint", "stop", "--id", id)
	if s := get(id).Status.State; s != "STOPPED" {
		t.Fatalf("state after stop = %q, want STOPPED", s)
	}
	curl(t, inst.CAFile, url, authToken, 503)

	run("ai", "endpoint", "start", "--id", id)
	if s := get(id).Status.State; s != "RUNNING" {
		t.Fatalf("state after start = %q, want RUNNING", s)
	}
	curl(t, inst.CAFile, url, authToken, 200)

	run("ai", "endpoint", "restart", "--id", id)
	if s := get(id).Status.State; s != "RUNNING" {
		t.Fatalf("state after restart = %q, want RUNNING", s)
	}
	curl(t, inst.CAFile, url, authToken, 200)

	run("ai", "endpoint", "delete", "--id", id)
	if _, err := cli.Run(t.Context(), "ai", "endpoint", "get", id); err == nil {
		t.Fatal("get after delete succeeded, want a not-found error")
	}
	curl(t, inst.CAFile, url, authToken, 404)
}

// curlCall calls url with the real curl binary, as the docs do, trusting the
// mock's CA through --cacert. It returns the HTTP status and the body.
func curlCall(t *testing.T, caFile, url, token string) (status int, body string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "body")
	args := []string{"-sS", "-o", out, "-w", "%{http_code}", "--cacert", caFile}
	if token != "" {
		args = append(args, "-H", "Authorization: Bearer "+token)
	}
	args = append(args, url)
	res, err := exec.CommandContext(t.Context(), "curl", args...).Output()
	if err != nil {
		t.Fatalf("curl %s: %v (curl is part of the documented flow and must be installed)", url, err)
	}
	status, err = strconv.Atoi(strings.TrimSpace(string(res)))
	if err != nil {
		t.Fatalf("curl %s printed status %q: %v", url, res, err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read curl body: %v", err)
	}
	return status, string(raw)
}

func curl(t *testing.T, caFile, url, token string, want int) {
	t.Helper()
	code, body := curlCall(t, caFile, url, token)
	if code != want {
		t.Fatalf("curl %s (token %q) = %d %s, want %d", url, token, code, body, want)
	}
	if want == 200 && !strings.Contains(body, "endpoint was called") {
		t.Fatalf("curl %s body = %s, want it to contain \"endpoint was called\"", url, body)
	}
}
