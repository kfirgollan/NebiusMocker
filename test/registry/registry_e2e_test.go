//go:build e2e_registry

// Package registry tests the Container Registry data plane end to end:
// NebiusMocker in front of a real Harbor (official goharbor images), the
// official nebius CLI, the docker CLI and the official Go SDK.
//
// Run with: make e2e-registry (starts and removes Harbor itself). Running the
// package directly needs NEBIUS_MOCKER_E2E_HARBOR_URL (and _USERNAME,
// _PASSWORD) pointing at a Harbor; see test/registry/harbor/harbor.sh.
package registry

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"
	registrysvc "github.com/nebius/gosdk/services/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/registry"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

const forwarderConf = "harbor/forwarder.nginx.conf"

var pushedDigest = regexp.MustCompile(`digest: (sha256:[0-9a-f]{64})`)

type harborEnv struct {
	url, username, password, version string
}

func harborFromEnv(t *testing.T) harborEnv {
	t.Helper()
	get := func(k, def string) string {
		if v, ok := os.LookupEnv("NEBIUS_MOCKER_E2E_HARBOR_" + k); ok && v != "" { //nolint:forbidigo // e2e wiring set by make e2e-registry
			return v
		}
		return def
	}
	h := harborEnv{url: get("URL", ""), username: get("USERNAME", "admin"), password: get("PASSWORD", "Harbor12345"), version: get("VERSION", "v2.16.0")}
	if h.url == "" {
		t.Skip("NEBIUS_MOCKER_E2E_HARBOR_URL not set; run make e2e-registry")
	}
	return h
}

// TestDockerWorkflow follows the documented Nebius workflow against the mock:
// create a registry, log in with `nebius iam get-access-token | docker login
// <fqdn> --username iam --password-stdin`, docker push, docker pull after
// removing the local copy, and list/get the pushed image through the Go SDK's
// ArtifactService. Authorization is enforced: the viewer may pull but not push.
func TestDockerWorkflow(t *testing.T) {
	h := harborFromEnv(t)
	docker := newDockerEnv(t)
	inst := testkit.StartT(t, []mocker.Module{iam.New(), registry.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithConfig(func(c *config.Config) {
			c.IDMode = nid.Random // registries map to Harbor projects, which outlive one mocker
			c.Registry.Backend = config.RegistryHarbor
			c.Registry.HarborURL, c.Registry.HarborUsername, c.Registry.HarborPassword = h.url, h.username, h.password
		}))
	editorSDK := inst.SDKAs(t, config.DefaultEditorToken)
	registries := registrysvc.NewRegistryService(editorSDK)
	artifacts := registrysvc.NewArtifactService(editorSDK)

	reg := createRegistry(t, registries, inst.ProjectID)
	regID := reg.GetMetadata().GetId()
	fqdn := reg.GetStatus().GetRegistryFqdn()
	if !strings.HasPrefix(fqdn, "127.0.0.1:") {
		t.Fatalf("registry_fqdn = %q, want 127.0.0.1:<port>", fqdn)
	}
	docker.forwardLoopback(t, fqdn, "goharbor/nginx-photon:"+h.version, forwarderConf)

	// Image names follow Nebius: <fqdn>/<registry ID without "registry-">/<image>:<tag>.
	image := fmt.Sprintf("%s/%s/hello", fqdn, strings.TrimPrefix(regID, "registry-"))
	tag := "v" + randomSuffix(t)
	ref := image + ":" + tag
	t.Cleanup(func() { _, _ = docker.try(context.WithoutCancel(t.Context()), "rmi", "-f", ref) }) // best-effort

	// 1. Log in exactly as documented, with the official CLI.
	editorCLI := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: config.DefaultEditorToken, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	login(t, docker, editorCLI, fqdn)

	// 2. docker push.
	docker.buildImage(t, ref)
	out := docker.run(t, "push", ref)
	m := pushedDigest.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("docker push printed no digest:\n%s", out)
	}
	digest := m[1]

	// 3. docker pull after removing the local copy.
	docker.run(t, "rmi", ref)
	if _, err := docker.try(t.Context(), "image", "inspect", ref); err == nil {
		t.Fatal("image still present locally after rmi")
	}
	docker.run(t, "pull", ref)
	if got := strings.TrimSpace(docker.run(t, "image", "inspect", "--format", "{{index .RepoDigests 0}}", ref)); got != image+"@"+digest {
		t.Errorf("pulled image digest = %s, want %s@%s", got, image, digest)
	}

	// 4. The Go SDK lists the pushed image and its tag, and Get agrees.
	art := findArtifact(t, artifacts, regID, "hello", digest)
	if !slices.Contains(art.GetTags(), tag) || art.GetStatus() != registrypb.Artifact_ACTIVE || art.GetSize() <= 0 {
		t.Errorf("listed artifact = %v, want tag %s", art, tag)
	}
	got, err := artifacts.Get(t.Context(), &registrypb.GetArtifactRequest{Id: art.GetId()})
	if err != nil {
		t.Fatalf("ArtifactService.Get: %v", err)
	}
	if got.GetDigest() != digest || !slices.Contains(got.GetTags(), tag) || got.GetName() != "hello" {
		t.Errorf("Get = %v", got)
	}
	if r, _ := registries.Get(t.Context(), &registrypb.GetRegistryRequest{Id: regID}); r.GetStatus().GetImagesCount() != 1 {
		t.Errorf("images_count = %d, want 1", r.GetStatus().GetImagesCount())
	}

	// 5. Enforced authorization on the data plane: a viewer pulls but cannot push.
	viewerCLI := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: config.DefaultViewerToken, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	login(t, docker, viewerCLI, fqdn)
	docker.run(t, "pull", ref)
	denied := image + ":denied"
	docker.run(t, "tag", ref, denied)
	t.Cleanup(func() { _, _ = docker.try(context.WithoutCancel(t.Context()), "rmi", "-f", denied) }) // best-effort
	if out, err := docker.try(t.Context(), "push", denied); err == nil || !strings.Contains(strings.ToLower(out), "denied") {
		t.Errorf("viewer push: err=%v output:\n%s\nwant denied", err, out)
	}

	// 6. Delete the artifact through the SDK, then the (now empty) registry.
	op, err := artifacts.Delete(t.Context(), &registrypb.DeleteArtifactRequest{Id: art.GetId()})
	if err != nil {
		t.Fatalf("ArtifactService.Delete: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("delete artifact: %v", err)
	}
	if _, err := artifacts.Get(t.Context(), &registrypb.GetArtifactRequest{Id: art.GetId()}); status.Code(err) != codes.NotFound {
		t.Errorf("Get deleted artifact: %v, want NotFound", err)
	}
	deleteRegistry(t, registries, regID)
}

func createRegistry(t *testing.T, svc registrysvc.RegistryService, projectID string) *registrypb.Registry {
	t.Helper()
	op, err := svc.Create(t.Context(), &registrypb.CreateRegistryRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "e2e-" + randomSuffix(t)},
		Spec:     &registrypb.RegistrySpec{Description: "docker e2e"},
	})
	if err != nil {
		t.Fatalf("Create registry: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create registry wait: %v", err)
	}
	id := op.ResourceID()
	t.Cleanup(func() { // force-delete leftovers so the Harbor project goes away too
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
		defer cancel()
		if op, err := svc.Delete(ctx, &registrypb.DeleteRegistryRequest{Id: id, Force: true}); err == nil {
			_, _ = op.Wait(ctx) // best-effort cleanup
		}
	})
	r, err := svc.Get(t.Context(), &registrypb.GetRegistryRequest{Id: id})
	if err != nil {
		t.Fatalf("Get registry: %v", err)
	}
	if r.GetStatus().GetState() != registrypb.RegistryStatus_ACTIVE {
		t.Fatalf("registry state = %v, want ACTIVE", r.GetStatus().GetState())
	}
	return r
}

func deleteRegistry(t *testing.T, svc registrysvc.RegistryService, id string) {
	t.Helper()
	op, err := svc.Delete(t.Context(), &registrypb.DeleteRegistryRequest{Id: id})
	if err != nil {
		t.Fatalf("Delete registry: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Delete registry wait: %v", err)
	}
}

// login runs `nebius iam get-access-token | docker login <fqdn> --username iam
// --password-stdin`, the documented short-lived token flow, as a real pipe. It
// retries briefly: Docker Desktop's loopback forwarder may still be starting.
func login(t *testing.T, d *dockerEnv, cli *nebiuscli.CLI, fqdn string) {
	t.Helper()
	var last string
	for range 10 {
		out, err := loginOnce(t, d, cli, fqdn)
		if err == nil && strings.Contains(out, "Login Succeeded") {
			return
		}
		last = fmt.Sprintf("%v\n%s", err, out)
		<-clock.Real{}.After(time.Second)
	}
	t.Fatalf("docker login %s: %s", fqdn, last)
}

func loginOnce(t *testing.T, d *dockerEnv, cli *nebiuscli.CLI, fqdn string) (string, error) {
	t.Helper()
	ctx := t.Context()
	token := cli.Command(ctx, "iam", "get-access-token")
	var tokenErr bytes.Buffer
	token.Stderr = &tokenErr
	pipe, err := token.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	dl := d.cmd(ctx, "login", fqdn, "--username", "iam", "--password-stdin")
	dl.Stdin = pipe
	var out bytes.Buffer
	dl.Stdout, dl.Stderr = &out, &out
	if err := token.Start(); err != nil {
		t.Fatalf("start nebius iam get-access-token: %v", err)
	}
	loginErr := dl.Run()
	if err := token.Wait(); err != nil {
		t.Fatalf("nebius iam get-access-token: %v\n%s", err, tokenErr.String())
	}
	return out.String(), loginErr
}

func findArtifact(t *testing.T, svc registrysvc.ArtifactService, registryID, name, digest string) *registrypb.Artifact {
	t.Helper()
	var seen []string
	for a, err := range svc.Filter(t.Context(), &registrypb.ListArtifactsRequest{ParentId: registryID}) {
		if err != nil {
			t.Fatalf("ArtifactService.List: %v", err)
		}
		if a.GetName() == name && a.GetDigest() == digest {
			return a
		}
		seen = append(seen, a.GetName()+"@"+a.GetDigest())
	}
	t.Fatalf("pushed image %s@%s not listed; got %v", name, digest, seen)
	return nil
}
