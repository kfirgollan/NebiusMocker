//go:build e2e_registry

// Package registry tests the Container Registry data plane end to end, the
// way docs/services/registry.md tells users to run it: NebiusMocker as a
// container (make docker-image) next to a real Harbor (deploy/harbor/harbor.sh),
// with the official nebius CLI, the docker CLI and the official Go SDK.
//
// Run with: make e2e-registry (builds the image, starts and removes Harbor).
// Running the package directly needs NEBIUS_MOCKER_E2E_IMAGE plus the
// HARBOR_* variables that harbor.sh writes to <workdir>/env.
package registry

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/nebius/gosdk"
	"github.com/nebius/gosdk/conn"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"
	registrysvc "github.com/nebius/gosdk/services/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
	"github.com/kfirgollan/NebiusMocker/test/internal/testca"
)

// dindImage is the official Docker-in-Docker image used as a clean Linux
// "user machine" for the credential-helper flow.
const dindImage = "docker:29.10.1-dind"

// crHost is a Nebius registry host that `nebius registry configure-helper`
// writes into ~/.docker/config.json.
const crHost = "cr.eu-north1.nebius.cloud"

var pushedDigest = regexp.MustCompile(`digest: (sha256:[0-9a-f]{64})`)

type e2eEnv struct {
	image                               string // the nebius-mocker image under test
	network, internalURL                string // Harbor's compose network and URL inside it
	harborUser, harborPass              string
	projectID, editorToken, viewerToken string
}

func envFromMake(t *testing.T) e2eEnv {
	t.Helper()
	get := func(k string) string {
		v, _ := os.LookupEnv(k) //nolint:forbidigo // e2e wiring exported by make e2e-registry
		return v
	}
	e := e2eEnv{
		image: get("NEBIUS_MOCKER_E2E_IMAGE"), network: get("HARBOR_NETWORK"), internalURL: get("HARBOR_INTERNAL_URL"),
		harborUser: get("HARBOR_USERNAME"), harborPass: get("HARBOR_PASSWORD"),
		projectID: config.DefaultProjectID, editorToken: config.DefaultEditorToken, viewerToken: config.DefaultViewerToken,
	}
	if e.image == "" || e.network == "" || e.internalURL == "" {
		t.Skip("NEBIUS_MOCKER_E2E_IMAGE / HARBOR_* not set; run make e2e-registry")
	}
	return e
}

// startMocker runs the nebius-mocker image on Harbor's network with the
// Harbor backend, enforced authz and required authn, plus extra docker run
// args and NEBIUS_MOCKER_* settings, and waits until it serves.
func startMocker(t *testing.T, d *dockerEnv, e e2eEnv, settings map[string]string, args ...string) {
	t.Helper()
	env := map[string]string{
		"AUTHN": "required", "AUTHZ": "enforce", "IDS": "random",
		"REGISTRY_BACKEND": "harbor", "REGISTRY_HARBOR_URL": e.internalURL,
		"REGISTRY_HARBOR_USERNAME": e.harborUser, "REGISTRY_HARBOR_PASSWORD": e.harborPass,
	}
	for k, v := range settings {
		env[k] = v
	}
	runArgs := []string{"--network", e.network}
	for k, v := range env {
		runArgs = append(runArgs, "-e", "NEBIUS_MOCKER_"+k+"="+v)
	}
	runArgs = append(append(runArgs, args...), e.image)
	name := d.runContainer(t, "nebius-mocker", runArgs...)
	d.waitLog(t, name, "nebius-mocker listening")
}

// TestStaticTokenLogin follows the documented short-lived-token workflow
// against the mocker container: create a registry, `nebius iam
// get-access-token | docker login <fqdn> --username iam --password-stdin`,
// docker push, docker pull after removing the local copy, and list/get the
// pushed image with the Go SDK. Authorization is enforced: a viewer pulls but
// cannot push. The host's own docker daemon is used, unmodified; this works on
// Docker Desktop because the mocker's port is a published container port.
func TestStaticTokenLogin(t *testing.T) {
	e := envFromMake(t)
	d := newDockerEnv(t)
	grpcPort, dockerPort := freePort(t), freePort(t)
	// As documented: publish 4566 and the docker endpoint; when the docker
	// endpoint port is remapped, NEBIUS_MOCKER_REGISTRY_LISTEN keeps
	// status.registry_fqdn (127.0.0.1:<port>) equal to the published port.
	startMocker(t, d, e, map[string]string{"REGISTRY_LISTEN": ":" + itoa(dockerPort)},
		"-p", fmt.Sprintf("127.0.0.1:%d:4566", grpcPort), "-p", fmt.Sprintf("127.0.0.1:%d:%d", dockerPort, dockerPort))
	inst := &testkit.Instance{Addr: fmt.Sprintf("127.0.0.1:%d", grpcPort), TenantID: config.DefaultTenantID, ProjectID: e.projectID}
	editorSDK := inst.SDKAs(t, e.editorToken)
	registries, artifacts := registrysvc.NewRegistryService(editorSDK), registrysvc.NewArtifactService(editorSDK)

	reg := createRegistry(t, registries, e.projectID)
	regID, fqdn := reg.GetMetadata().GetId(), reg.GetStatus().GetRegistryFqdn()
	if want := fmt.Sprintf("127.0.0.1:%d", dockerPort); fqdn != want {
		t.Fatalf("registry_fqdn = %q, want %q", fqdn, want)
	}
	// Image names follow Nebius: <fqdn>/<registry ID without "registry-">/<image>:<tag>.
	image := fmt.Sprintf("%s/%s/hello", fqdn, strings.TrimPrefix(regID, "registry-"))
	tag := "v" + randomSuffix(t)
	ref := image + ":" + tag
	t.Cleanup(func() { _, _ = d.try(context.WithoutCancel(t.Context()), "rmi", "-f", ref) }) // best-effort

	editorCLI := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: e.editorToken, ParentID: e.projectID, TenantID: inst.TenantID})
	login(t, d, editorCLI, fqdn)

	d.buildImage(t, ref)
	out := d.run(t, "push", ref)
	m := pushedDigest.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("docker push printed no digest:\n%s", out)
	}
	digest := m[1]

	d.run(t, "rmi", ref)
	if _, err := d.try(t.Context(), "image", "inspect", ref); err == nil {
		t.Fatal("image still present locally after rmi")
	}
	d.run(t, "pull", ref)
	if got := strings.TrimSpace(d.run(t, "image", "inspect", "--format", "{{index .RepoDigests 0}}", ref)); got != image+"@"+digest {
		t.Errorf("pulled image digest = %s, want %s@%s", got, image, digest)
	}

	art := assertPushed(t, artifacts, regID, digest, tag)
	if r, _ := registries.Get(t.Context(), &registrypb.GetRegistryRequest{Id: regID}); r.GetStatus().GetImagesCount() != 1 {
		t.Errorf("images_count = %d, want 1", r.GetStatus().GetImagesCount())
	}

	viewerCLI := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: e.viewerToken, ParentID: e.projectID, TenantID: inst.TenantID})
	login(t, d, viewerCLI, fqdn)
	d.run(t, "pull", ref)
	denied := image + ":denied"
	d.run(t, "tag", ref, denied)
	t.Cleanup(func() { _, _ = d.try(context.WithoutCancel(t.Context()), "rmi", "-f", denied) }) // best-effort
	if out, err := d.try(t.Context(), "push", denied); err == nil || !strings.Contains(strings.ToLower(out), "denied") {
		t.Errorf("viewer push: err=%v output:\n%s\nwant denied", err, out)
	}

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

// TestCredentialHelper follows the documented credential-helper workflow,
// unmodified: `nebius registry configure-helper`, then docker push and pull
// with no docker login. The helper only knows cr.<region>.nebius.cloud hosts
// and runs the CLI without --insecure, so it needs what any local stand-in for
// those hosts needs: the hosts resolve to the mocker, and the mocker's CA is
// trusted (docs/services/registry.md, "Credential helper"). A fresh official
// docker:dind container plays the user's Linux machine: DNS comes from network
// aliases on the mocker container, and the CA is installed the standard ways
// (system CA store for the CLI, /etc/docker/certs.d for dockerd).
func TestCredentialHelper(t *testing.T) {
	e := envFromMake(t)
	d := newDockerEnv(t)
	names := []string{crHost, "api.nebius.cloud", "cpl.iam.api.nebius.cloud", "registry.api.nebius.cloud"}
	ca := testca.New(t, append(slices.Clone(names), "localhost"), []net.IP{net.IPv4(127, 0, 0, 1)})
	grpcPort := freePort(t)
	args := []string{"-p", fmt.Sprintf("127.0.0.1:%d:4566", grpcPort), "-v", ca.Dir + ":/certs:ro"}
	for _, n := range names {
		args = append(args, "--network-alias", n)
	}
	startMocker(t, d, e, map[string]string{
		"TLS_CERT_FILE": "/certs/tls.crt", "TLS_KEY_FILE": "/certs/tls.key",
		"REGISTRY_LISTEN": ":443", "REGISTRY_FQDN": crHost,
	}, args...)
	sdk := tlsSDK(t, fmt.Sprintf("127.0.0.1:%d", grpcPort), ca, e.editorToken)
	registries, artifacts := registrysvc.NewRegistryService(sdk), registrysvc.NewArtifactService(sdk)
	reg := createRegistry(t, registries, e.projectID)
	regID := reg.GetMetadata().GetId()
	if fqdn := reg.GetStatus().GetRegistryFqdn(); fqdn != crHost {
		t.Fatalf("registry_fqdn = %q, want %q", fqdn, crHost)
	}
	tag := "v" + randomSuffix(t)
	ref := fmt.Sprintf("%s/%s/hello:%s", crHost, strings.TrimPrefix(regID, "registry-"), tag)

	client := d.runContainer(t, "nebius-mocker-client", "--privileged", "--network", e.network,
		"-e", "DOCKER_TLS_CERTDIR=", "-v", ca.CAFile+":/nebius-mocker-ca.crt:ro", dindImage)
	sh := func(script string) string {
		t.Helper()
		return d.run(t, "exec", client, "sh", "-ec", "export PATH=/root/.nebius/bin:$PATH\n"+script)
	}
	eventually(t, 2*time.Minute, func() error {
		_, err := d.try(t.Context(), "exec", client, "docker", "info")
		return err
	})

	// The user machine: tools, then trust the mocker's CA.
	// bash and GNU coreutils: the installer needs them (busybox mktemp rejects its template).
	sh("apk add --no-cache --quiet bash curl coreutils")
	sh(`cp /nebius-mocker-ca.crt /usr/local/share/ca-certificates/nebius-mocker.crt
update-ca-certificates >/dev/null 2>&1
mkdir -p /etc/docker/certs.d/` + crHost + `
cp /nebius-mocker-ca.crt /etc/docker/certs.d/` + crHost + `/ca.crt`)
	// Install the CLI with the documented installer (pinned to the tested version).
	sh("curl -sSL " + nebiuscli.InstallerURL + " | NEBIUS_CLI_VERSION=" + nebiuscli.Version + " bash")
	// A profile authenticated with a token file; no --insecure anywhere.
	sh(fmt.Sprintf("printf %%s %q > /root/token\nnebius profile create mocker --endpoint api.nebius.cloud:4566 --token-file /root/token --parent-id %s </dev/null",
		e.editorToken, e.projectID))
	// The documented step.
	sh("nebius registry configure-helper")
	cfg := sh("cat /root/.docker/config.json")
	if !strings.Contains(cfg, fmt.Sprintf("%q: %q", crHost, "nebius")) {
		t.Fatalf("configure-helper did not register the helper for %s:\n%s", crHost, cfg)
	}

	dockerfile, content := imageContext(t)
	out := sh(fmt.Sprintf("mkdir -p /work\nprintf '%%b' %q > /work/Dockerfile\nprintf '%%b' %q > /work/hello.txt\ndocker build --quiet --tag %s /work >/dev/null\ndocker push %s",
		dockerfile, content, ref, ref))
	m := pushedDigest.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("docker push printed no digest:\n%s", out)
	}
	digest := m[1]
	sh(fmt.Sprintf("docker rmi %s >/dev/null\n! docker image inspect %s >/dev/null 2>&1\ndocker pull %s", ref, ref, ref))
	if got := strings.TrimSpace(sh("docker image inspect --format '{{index .RepoDigests 0}}' " + ref)); !strings.HasSuffix(got, "@"+digest) {
		t.Errorf("pulled image digest = %s, want @%s", got, digest)
	}
	// No credentials were stored: they came from the helper on every push/pull.
	if cfg := sh("cat /root/.docker/config.json"); strings.Contains(cfg, `"auths"`) {
		t.Errorf("docker stored credentials although only the helper should be used:\n%s", cfg)
	}

	art := assertPushed(t, artifacts, regID, digest, tag)
	op, err := artifacts.Delete(t.Context(), &registrypb.DeleteArtifactRequest{Id: art.GetId()})
	if err != nil {
		t.Fatalf("ArtifactService.Delete: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("delete artifact: %v", err)
	}
	deleteRegistry(t, registries, regID)
}

// tlsSDK is the official Go SDK over TLS, trusting the test CA.
func tlsSDK(t *testing.T, addr string, ca *testca.CA, token string) *gosdk.SDK {
	t.Helper()
	sdk, err := gosdk.New(t.Context(),
		gosdk.WithResolvers(conn.NewBasicResolver("*", addr)),
		gosdk.WithDialOptions(grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: ca.Pool, MinVersion: tls.VersionTLS12}))),
		gosdk.WithCredentials(gosdk.IAMToken(token)),
	)
	if err != nil {
		t.Fatalf("gosdk.New: %v", err)
	}
	t.Cleanup(func() { _ = sdk.Close() })
	return sdk
}

func createRegistry(t *testing.T, svc registrysvc.RegistryService, projectID string) *registrypb.Registry {
	t.Helper()
	created, err := svc.Create(t.Context(), &registrypb.CreateRegistryRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "e2e-" + randomSuffix(t)},
		Spec:     &registrypb.RegistrySpec{Description: "docker e2e"},
	})
	if err != nil {
		t.Fatalf("Create registry: %v", err)
	}
	if _, err := created.Wait(t.Context()); err != nil {
		t.Fatalf("Create registry wait: %v", err)
	}
	id := created.ResourceID()
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

// assertPushed checks that the Go SDK lists the pushed image with its tag and
// that Get agrees; it returns the artifact.
func assertPushed(t *testing.T, svc registrysvc.ArtifactService, registryID, digest, tag string) *registrypb.Artifact {
	t.Helper()
	var art *registrypb.Artifact
	var seen []string
	for a, err := range svc.Filter(t.Context(), &registrypb.ListArtifactsRequest{ParentId: registryID}) {
		if err != nil {
			t.Fatalf("ArtifactService.List: %v", err)
		}
		seen = append(seen, a.GetName()+"@"+a.GetDigest())
		if a.GetName() == "hello" && a.GetDigest() == digest {
			art = a
		}
	}
	if art == nil {
		t.Fatalf("pushed image hello@%s not listed; got %v", digest, seen)
	}
	if !slices.Contains(art.GetTags(), tag) || art.GetStatus() != registrypb.Artifact_ACTIVE || art.GetSize() <= 0 {
		t.Errorf("listed artifact = %v, want tag %s", art, tag)
	}
	got, err := svc.Get(t.Context(), &registrypb.GetArtifactRequest{Id: art.GetId()})
	if err != nil {
		t.Fatalf("ArtifactService.Get: %v", err)
	}
	if got.GetDigest() != digest || !slices.Contains(got.GetTags(), tag) || got.GetName() != "hello" {
		t.Errorf("Get = %v", got)
	}
	return art
}

// login runs `nebius iam get-access-token | docker login <fqdn> --username iam
// --password-stdin`, the documented short-lived token flow, as a real pipe.
func login(t *testing.T, d *dockerEnv, cli *nebiuscli.CLI, fqdn string) {
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
	if loginErr != nil || !strings.Contains(out.String(), "Login Succeeded") {
		t.Fatalf("docker login %s: %v\n%s", fqdn, loginErr, out.String())
	}
}
