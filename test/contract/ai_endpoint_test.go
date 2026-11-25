package contract

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"
	aisvc "github.com/nebius/gosdk/services/nebius/ai/v1"
	mbsvc "github.com/nebius/gosdk/services/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/ai"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

const (
	aiToken = "s3cret"
	aiGiB   = int64(1) << 30
)

// aiEnv is a running mocker with the IAM and AI modules.
type aiEnv struct {
	inst *testkit.Instance
	mod  *ai.Module
	svc  aisvc.EndpointService
	// http trusts the data plane's local CA, as `curl --cacert ca.pem` would.
	http *http.Client
	ca   *x509.CertPool
}

func startAI(t *testing.T, extra []mocker.Module, opts ...testkit.Option) *aiEnv {
	t.Helper()
	mod := ai.New(ai.WithListen("127.0.0.1:0"), ai.WithTLSDir(t.TempDir()))
	mods := append([]mocker.Module{iam.New(), mod}, extra...)
	inst := testkit.StartT(t, mods, opts...)
	pem, err := os.ReadFile(mod.CAFile())
	if err != nil {
		t.Fatalf("read CA file: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("CA file %s holds no certificate", mod.CAFile())
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	t.Cleanup(client.CloseIdleConnections)
	return &aiEnv{inst: inst, mod: mod, svc: aisvc.NewEndpointService(inst.SDK(t)), http: client, ca: pool}
}

// validSpec is a spec the mocker accepts.
func validSpec() *aipb.EndpointSpec {
	return &aipb.EndpointSpec{
		Image:     "nginx:alpine",
		Platform:  "cpu-d3",
		Preset:    "4vcpu-16gb",
		Disk:      &aipb.EndpointSpec_DiskSpec{Type: computepb.DiskSpec_NETWORK_SSD},
		Ports:     []*aipb.EndpointSpec_Port{{ContainerPort: 80, Protocol: aipb.EndpointSpec_Port_HTTP}},
		AuthToken: aiToken,
	}
}

func aiCreateReq(parent, name string, spec *aipb.EndpointSpec) *aipb.CreateEndpointRequest {
	return &aipb.CreateEndpointRequest{Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name}, Spec: spec}
}

// aiCreate creates an endpoint from validSpec (after mutate), waits for the
// operation and returns the endpoint ID.
func aiCreate(t *testing.T, svc aisvc.EndpointService, parent, name string, mutate func(*aipb.EndpointSpec)) string {
	t.Helper()
	spec := validSpec()
	if mutate != nil {
		mutate(spec)
	}
	op, err := svc.Create(t.Context(), aiCreateReq(parent, name, spec))
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create(%q) wait: %v", name, err)
	}
	return op.ResourceID()
}

func aiGet(t *testing.T, svc aisvc.EndpointService, id string) *aipb.Endpoint {
	t.Helper()
	e, err := svc.Get(t.Context(), &aipb.GetEndpointRequest{Id: id})
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return e
}

// aiInstanceID returns the single instance ID of a running endpoint.
func aiInstanceID(t *testing.T, e *aipb.Endpoint) string {
	t.Helper()
	ins := e.GetStatus().GetInstances()
	if len(ins) != 1 {
		t.Fatalf("instances = %d, want 1", len(ins))
	}
	return ins[0].GetComputeInstanceId()
}

func TestAIEndpointCRUD(t *testing.T) {
	t.Parallel()
	env := startAI(t, nil)
	ctx := t.Context()
	parent := env.inst.ProjectID

	op, err := env.svc.Create(ctx, &aipb.CreateEndpointRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: "chat", Labels: map[string]string{"team": "ml"}},
		Spec:     validSpec(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Create wait: %v", err)
	}
	id := op.ResourceID()

	got := aiGet(t, env.svc, id)
	md := got.GetMetadata()
	if !strings.HasPrefix(id, "aiendpoint-") || md.GetId() != id {
		t.Errorf("id = %q (metadata %q), want aiendpoint- prefix", id, md.GetId())
	}
	if md.GetParentId() != parent || md.GetName() != "chat" || md.GetLabels()["team"] != "ml" {
		t.Errorf("metadata = %v", md)
	}
	if md.GetResourceVersion() != 1 {
		t.Errorf("resource_version = %d, want 1", md.GetResourceVersion())
	}
	if md.GetCreatedAt() == nil || md.GetCreatedAt().AsTime().IsZero() {
		t.Errorf("created_at = %v, want set", md.GetCreatedAt())
	}
	if diff := cmp.Diff(validSpec(), got.GetSpec(), protocmp.Transform()); diff != "" {
		t.Errorf("spec round trip (-want +got):\n%s", diff)
	}
	st := got.GetStatus()
	if st.GetState() != aipb.EndpointStatus_RUNNING {
		t.Errorf("state = %v, want RUNNING", st.GetState())
	}
	if len(st.GetInstances()) != 1 {
		t.Errorf("instances = %d, want 1", len(st.GetInstances()))
	}
	if !strings.HasPrefix(env.mod.DataPlaneURL(), "https://") {
		t.Errorf("DataPlaneURL = %q, want https://", env.mod.DataPlaneURL())
	}
	wantURLs := []string{env.mod.DataPlaneURL() + "/" + id}
	if len(st.GetPublicEndpoints()) != 1 || !strings.HasPrefix(st.GetPublicEndpoints()[0], "https://") {
		t.Errorf("public_endpoints = %v, want one https:// URL", st.GetPublicEndpoints())
	}
	if diff := cmp.Diff(wantURLs, st.GetPublicEndpoints()); diff != "" {
		t.Errorf("public_endpoints (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(wantURLs, st.GetPrivateEndpoints()); diff != "" {
		t.Errorf("private_endpoints (-want +got):\n%s", diff)
	}

	byName, err := env.svc.GetByName(ctx, &aipb.GetEndpointByNameRequest{ParentId: parent, Name: "chat"})
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if diff := cmp.Diff(got, byName, protocmp.Transform()); diff != "" {
		t.Errorf("GetByName differs from Get (-get +byName):\n%s", diff)
	}

	op, err = env.svc.Delete(ctx, &aipb.DeleteEndpointRequest{Id: id})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Delete wait: %v", err)
	}
	_, err = env.svc.Get(ctx, &aipb.GetEndpointRequest{Id: id})
	assertCode(t, err, codes.NotFound)
}

func TestAIEndpointListPaging(t *testing.T) {
	t.Parallel()
	env := startAI(t, nil)
	want := map[string]bool{}
	for _, n := range []string{"a", "b", "c"} {
		want[aiCreate(t, env.svc, env.inst.ProjectID, n, nil)] = true
	}

	got := map[string]bool{}
	token, pages := "", 0
	for {
		resp, err := env.svc.List(t.Context(), &aipb.ListEndpointsRequest{ParentId: env.inst.ProjectID, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		pages++
		if n := len(resp.GetItems()); n > 2 {
			t.Fatalf("page %d has %d items, want <= 2", pages, n)
		}
		for _, e := range resp.GetItems() {
			got[e.GetMetadata().GetId()] = true
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	if pages != 2 {
		t.Errorf("pages = %d, want 2", pages)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("listed endpoints (-want +got):\n%s", diff)
	}
}

func TestAIEndpointLifecycle(t *testing.T) {
	t.Parallel()
	env := startAI(t, nil, testkit.WithOperationDelay(500*time.Millisecond))
	ctx := t.Context()
	svc := env.svc
	state := func(id string) aipb.EndpointStatus_State { return aiGet(t, svc, id).GetStatus().GetState() }

	op, err := svc.Create(ctx, aiCreateReq(env.inst.ProjectID, "life", validSpec()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := op.ResourceID()
	if got := state(id); got != aipb.EndpointStatus_PROVISIONING {
		t.Fatalf("state right after Create = %v, want PROVISIONING", got)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Create wait: %v", err)
	}
	first := aiGet(t, svc, id)
	if first.GetStatus().GetState() != aipb.EndpointStatus_RUNNING {
		t.Fatalf("state after Create = %v, want RUNNING", first.GetStatus().GetState())
	}
	firstInstance := aiInstanceID(t, first)

	// Stop.
	op, err = svc.Stop(ctx, &aipb.StopEndpointRequest{Id: id})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := state(id); got != aipb.EndpointStatus_STOPPING {
		t.Errorf("state during Stop = %v, want STOPPING", got)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Stop wait: %v", err)
	}
	stopped := aiGet(t, svc, id)
	if stopped.GetStatus().GetState() != aipb.EndpointStatus_STOPPED || len(stopped.GetStatus().GetInstances()) != 0 {
		t.Errorf("after Stop: state %v, instances %d; want STOPPED, 0", stopped.GetStatus().GetState(), len(stopped.GetStatus().GetInstances()))
	}

	// Stopping a stopped endpoint is a bad-state error: errs.KindBadState maps
	// to FAILED_PRECONDITION with a BadResourceState detail.
	_, err = svc.Stop(ctx, &aipb.StopEndpointRequest{Id: id})
	assertCode(t, err, codes.FailedPrecondition)
	assertServiceDetail(t, err, "bad resource state", func(se *commonpb.ServiceError) bool { return se.GetBadResourceState() != nil })

	// Start.
	op, err = svc.Start(ctx, &aipb.StartEndpointRequest{Id: id})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := state(id); got != aipb.EndpointStatus_STARTING {
		t.Errorf("state during Start = %v, want STARTING", got)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Start wait: %v", err)
	}
	started := aiGet(t, svc, id)
	if started.GetStatus().GetState() != aipb.EndpointStatus_RUNNING {
		t.Fatalf("state after Start = %v, want RUNNING", started.GetStatus().GetState())
	}
	startedInstance := aiInstanceID(t, started)
	if startedInstance == firstInstance {
		t.Errorf("instance after Start = %q, want a new one", startedInstance)
	}

	// Restart.
	op, err = svc.Restart(ctx, &aipb.RestartEndpointRequest{Id: id})
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Restart wait: %v", err)
	}
	restarted := aiGet(t, svc, id)
	if restarted.GetStatus().GetState() != aipb.EndpointStatus_RUNNING {
		t.Fatalf("state after Restart = %v, want RUNNING", restarted.GetStatus().GetState())
	}
	if got := aiInstanceID(t, restarted); got == startedInstance {
		t.Errorf("instance after Restart = %q, want a new one", got)
	}
}

func TestAIEndpointOperationConflict(t *testing.T) {
	t.Parallel()
	env := startAI(t, nil, testkit.WithOperationDelay(time.Hour))
	op, err := env.svc.Create(t.Context(), aiCreateReq(env.inst.ProjectID, "slow", validSpec()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The Create operation still runs: any further operation conflicts.
	_, err = env.svc.Delete(t.Context(), &aipb.DeleteEndpointRequest{Id: op.ResourceID()})
	assertCode(t, err, codes.FailedPrecondition)
	assertServiceDetail(t, err, "operation conflict", func(se *commonpb.ServiceError) bool { return se.GetOperationConflict() != nil })
}

// assertServiceDetail checks that err carries a ServiceError detail accepted by ok.
func assertServiceDetail(t *testing.T, err error, what string, ok func(*commonpb.ServiceError) bool) {
	t.Helper()
	details := status.Convert(err).Details()
	for _, d := range details {
		if se, isSE := d.(*commonpb.ServiceError); isSE && ok(se) {
			return
		}
	}
	t.Errorf("error %v has no %s ServiceError detail (details: %v)", err, what, details)
}

func TestAIEndpointValidation(t *testing.T) {
	t.Parallel()
	env := startAI(t, nil)
	aiCreate(t, env.svc, env.inst.ProjectID, "taken", nil)

	tests := []struct {
		name   string
		mutate func(*aipb.EndpointSpec)
		want   codes.Code
	}{
		{"both auth tokens", func(s *aipb.EndpointSpec) {
			s.AuthTokenMysteryboxSecret = &aipb.EndpointSpec_MysteryBoxSecretRef{SecretId: "mbsec-1"}
		}, codes.InvalidArgument},
		{"auth with two http ports", func(s *aipb.EndpointSpec) {
			s.Ports = append(s.Ports, &aipb.EndpointSpec_Port{ContainerPort: 8080, Protocol: aipb.EndpointSpec_Port_HTTP})
		}, codes.InvalidArgument},
		{"unknown platform", func(s *aipb.EndpointSpec) { s.Platform = "cpu-nope" }, codes.InvalidArgument},
		{"bad preset", func(s *aipb.EndpointSpec) { s.Preset = "1000vcpu" }, codes.InvalidArgument},
		{"missing image", func(s *aipb.EndpointSpec) { s.Image = "" }, codes.InvalidArgument},
		{"udp port", func(s *aipb.EndpointSpec) {
			s.AuthToken = ""
			s.Ports = []*aipb.EndpointSpec_Port{{ContainerPort: 53, Protocol: aipb.EndpointSpec_Port_UDP}}
		}, codes.InvalidArgument},
		{"disk too small", func(s *aipb.EndpointSpec) { s.Disk.SizeBytes = aiGiB }, codes.InvalidArgument},
		{"taken", nil, codes.AlreadyExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec := validSpec()
			if tt.mutate != nil {
				tt.mutate(spec)
			}
			name := tt.name
			if tt.want != codes.AlreadyExists {
				name = "v-" + strings.ReplaceAll(tt.name, " ", "-")
			}
			_, err := env.svc.Create(t.Context(), aiCreateReq(env.inst.ProjectID, name, spec))
			assertCode(t, err, tt.want)
		})
	}
}

func TestAIEndpointSubnetReference(t *testing.T) {
	t.Parallel()
	const missing = "vpcsubnet-e00doesnotexist00"
	setSubnet := func(id string) func(*aipb.EndpointSpec) {
		return func(s *aipb.EndpointSpec) { s.SubnetId = id }
	}

	t.Run("vpc enabled checks existence", func(t *testing.T) {
		t.Parallel()
		env := startAI(t, []mocker.Module{vpc.New()})
		c := vpcClientsFor(t, env.inst, env.inst.Token)
		subnetID := newSubnet(t, c, "ai-sub", newNetwork(t, c, "ai-net"), "/24")

		spec := validSpec()
		setSubnet(missing)(spec)
		_, err := env.svc.Create(t.Context(), aiCreateReq(env.inst.ProjectID, "bad", spec))
		assertCode(t, err, codes.NotFound)
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("error %q does not name the subnet %s", err, missing)
		}
		id := aiCreate(t, env.svc, env.inst.ProjectID, "good", setSubnet(subnetID))
		got, err := env.svc.Get(t.Context(), &aipb.GetEndpointRequest{Id: id})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.GetSpec().GetSubnetId() != subnetID {
			t.Errorf("spec.subnet_id = %q, want %q", got.GetSpec().GetSubnetId(), subnetID)
		}
	})
	t.Run("vpc not enabled is not checked", func(t *testing.T) {
		t.Parallel()
		env := startAI(t, nil)
		aiCreate(t, env.svc, env.inst.ProjectID, "free", setSubnet(missing))
	})
}

func TestAIEndpointMysteryBox(t *testing.T) {
	t.Parallel()
	env := startAI(t, []mocker.Module{mysterybox.New()})
	parent := env.inst.ProjectID
	mb := mbsvc.NewSecretService(env.inst.SDK(t))
	secretID := mbCreateSecret(t, mb, parent, "ai-token", []*mysteryboxpb.Payload{mbStr("AUTH_TOKEN", "box-token")})
	versionID := mbPrimary(t, mb, secretID)

	id := aiCreate(t, env.svc, parent, "boxed", func(s *aipb.EndpointSpec) {
		s.AuthToken = ""
		s.AuthTokenMysteryboxSecret = &aipb.EndpointSpec_MysteryBoxSecretRef{SecretId: secretID}
	})
	ref := aiGet(t, env.svc, id).GetSpec().GetAuthTokenMysteryboxSecret()
	if ref.GetSecretId() != secretID || ref.GetVersionId() != versionID {
		t.Errorf("secret ref = %v, want %s pinned to %s", ref, secretID, versionID)
	}
	url := env.mod.DataPlaneURL() + "/" + id + "/x"
	if res := callDataPlane(t, env.http, http.MethodGet, url, "Bearer box-token"); res.status != http.StatusOK {
		t.Errorf("box token: status %d, want 200", res.status)
	}
	if res := callDataPlane(t, env.http, http.MethodGet, url, "Bearer "+aiToken); res.status != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d, want 401", res.status)
	}

	spec := validSpec()
	spec.AuthToken = ""
	spec.AuthTokenMysteryboxSecret = &aipb.EndpointSpec_MysteryBoxSecretRef{SecretId: "mbsec-e00doesnotexist00"}
	_, err := env.svc.Create(t.Context(), aiCreateReq(parent, "missing-secret", spec))
	assertCode(t, err, codes.NotFound)

	noToken := mbCreateSecret(t, mb, parent, "no-token", []*mysteryboxpb.Payload{mbStr("OTHER", "x")})
	spec.AuthTokenMysteryboxSecret = &aipb.EndpointSpec_MysteryBoxSecretRef{SecretId: noToken}
	_, err = env.svc.Create(t.Context(), aiCreateReq(parent, "no-token", spec))
	assertCode(t, err, codes.InvalidArgument)
}

func TestAIEndpointSecretView(t *testing.T) {
	t.Parallel()
	env := startAI(t, nil)
	id := aiCreate(t, env.svc, env.inst.ProjectID, "creds", func(s *aipb.EndpointSpec) {
		s.RegistryCredentials = &aipb.EndpointSpec_RegistryCredentials{Username: "bot", Password: "hunter2"}
	})

	if pw := aiGet(t, env.svc, id).GetSpec().GetRegistryCredentials().GetPassword(); pw != "" {
		t.Errorf("default view password = %q, want empty", pw)
	}
	e, err := env.svc.Get(t.Context(), &aipb.GetEndpointRequest{Id: id, View: aipb.GetEndpointRequest_SECRET})
	if err != nil {
		t.Fatalf("Get SECRET: %v", err)
	}
	if pw := e.GetSpec().GetRegistryCredentials().GetPassword(); pw != "hunter2" {
		t.Errorf("SECRET view password = %q, want hunter2", pw)
	}
}
