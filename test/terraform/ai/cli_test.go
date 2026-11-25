//go:build e2e

package ai

import (
	"slices"
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
	inst := startMock(t)
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, TenantID: inst.TenantID})
	const (
		name      = "qs-endpoint-nginx"
		authToken = "qs-secret-token"
		subnetID  = "vpcsubnet-e00mock0000000000test"
	)
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
	i := slices.IndexFunc(e.Status.PublicEndpoints, func(u string) bool { return strings.HasPrefix(u, "http") })
	if i < 0 {
		t.Fatalf("public_endpoints = %v, want an http URL", e.Status.PublicEndpoints)
	}
	url := e.Status.PublicEndpoints[i]

	var list struct {
		Items []endpointDoc `json:"items"`
	}
	cli.JSON(t, &list, "ai", "endpoint", "list", "--parent-id", inst.ProjectID)
	if !slices.ContainsFunc(list.Items, func(d endpointDoc) bool { return d.Metadata.ID == id }) {
		t.Fatalf("endpoint list = %+v, want %s", list.Items, id)
	}

	assertCall(t, url, authToken, 200)
	assertCall(t, url, "", 401)
	assertCall(t, url, "wrong-token", 401)

	run("ai", "endpoint", "stop", "--id", id)
	if s := get(id).Status.State; s != "STOPPED" {
		t.Fatalf("state after stop = %q, want STOPPED", s)
	}
	assertCall(t, url, authToken, 503)

	run("ai", "endpoint", "start", "--id", id)
	if s := get(id).Status.State; s != "RUNNING" {
		t.Fatalf("state after start = %q, want RUNNING", s)
	}
	assertCall(t, url, authToken, 200)

	run("ai", "endpoint", "restart", "--id", id)
	if s := get(id).Status.State; s != "RUNNING" {
		t.Fatalf("state after restart = %q, want RUNNING", s)
	}
	assertCall(t, url, authToken, 200)

	run("ai", "endpoint", "delete", "--id", id)
	if _, err := cli.Run(t.Context(), "ai", "endpoint", "get", id); err == nil {
		t.Fatal("get after delete succeeded, want a not-found error")
	}
	assertCall(t, url, authToken, 404)
}
