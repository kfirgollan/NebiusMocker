package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/ai/domain"
)

const gib = int64(1) << 30

func TestCreateHappyPath(t *testing.T) {
	t.Parallel()
	httpPort := func(p int32) *aipb.EndpointSpec_Port {
		return &aipb.EndpointSpec_Port{ContainerPort: p, Protocol: aipb.EndpointSpec_Port_HTTP}
	}
	tests := []struct {
		name     string
		mutate   func(*aipb.EndpointSpec)
		wantURLs func(id string) []string
	}{
		{
			name:     "one http port",
			mutate:   func(*aipb.EndpointSpec) {},
			wantURLs: func(id string) []string { return []string{"http://dp.test/" + id} },
		},
		{
			name: "two http ports",
			mutate: func(s *aipb.EndpointSpec) {
				s.AuthToken = ""
				s.Ports = []*aipb.EndpointSpec_Port{httpPort(80), httpPort(8080)}
			},
			wantURLs: func(id string) []string {
				return []string{"http://dp.test/" + id + ":80", "http://dp.test/" + id + ":8080"}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newService(t, opts{})
			spec := baseSpec()
			tt.mutate(spec)
			e := mustCreate(t, h, "ep", spec)
			id := e.GetMetadata().GetId()
			if st := e.GetStatus().GetState(); st != aipb.EndpointStatus_RUNNING {
				t.Errorf("state = %v, want RUNNING", st)
			}
			if v := e.GetMetadata().GetResourceVersion(); v != 1 {
				t.Errorf("resource_version = %d, want 1", v)
			}
			ins := e.GetStatus().GetInstances()
			if len(ins) != 1 {
				t.Fatalf("instances = %d, want 1", len(ins))
			}
			if got := ins[0].GetComputeInstanceId(); !strings.HasPrefix(got, "computeinstance-") {
				t.Errorf("compute_instance_id = %q, want computeinstance- prefix", got)
			}
			if ins[0].GetPrivateIp() == "" {
				t.Error("private_ip is empty")
			}
			want := tt.wantURLs(id)
			if d := cmp.Diff(want, e.GetStatus().GetPublicEndpoints()); d != "" {
				t.Errorf("public_endpoints mismatch (-want +got):\n%s", d)
			}
			if d := cmp.Diff(want, e.GetStatus().GetPrivateEndpoints()); d != "" {
				t.Errorf("private_endpoints mismatch (-want +got):\n%s", d)
			}
		})
	}
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*domain.CreateEndpointCmd)
	}{
		{"auth_token with secret", func(c *domain.CreateEndpointCmd) {
			c.Spec.AuthTokenMysteryboxSecret = &aipb.EndpointSpec_MysteryBoxSecretRef{SecretId: "mbsec-1"}
		}},
		{"auth with no http port", func(c *domain.CreateEndpointCmd) { c.Spec.Ports = nil }},
		{"auth with two http ports", func(c *domain.CreateEndpointCmd) {
			c.Spec.Ports = append(c.Spec.Ports, &aipb.EndpointSpec_Port{ContainerPort: 81, Protocol: aipb.EndpointSpec_Port_HTTP})
		}},
		{"unknown platform", func(c *domain.CreateEndpointCmd) { c.Spec.Platform = "nope" }},
		{"preset not in platform", func(c *domain.CreateEndpointCmd) { c.Spec.Preset = "8gpu-128vcpu-1600gb" }},
		{"gpu count unavailable", func(c *domain.CreateEndpointCmd) {
			c.Spec.Platform, c.Spec.Preset, c.Spec.Gpu = "", "", []string{"H100:3"}
		}},
		{"cpu family unknown", func(c *domain.CreateEndpointCmd) {
			c.Spec.Platform, c.Spec.Preset, c.Spec.Cpu = "", "", []string{"nope:4"}
		}},
		{"disk too small on cpu", func(c *domain.CreateEndpointCmd) {
			c.Spec.Disk = &aipb.EndpointSpec_DiskSpec{SizeBytes: 19 * gib}
		}},
		{"disk too small on gpu", func(c *domain.CreateEndpointCmd) {
			c.Spec.Platform, c.Spec.Preset, c.Spec.Gpu = "", "", []string{"H100:8"}
			c.Spec.Disk = &aipb.EndpointSpec_DiskSpec{SizeBytes: 49 * gib}
		}},
		{"env value and secret", func(c *domain.CreateEndpointCmd) {
			c.Spec.EnvironmentVariables = []*aipb.EndpointSpec_EnvironmentVariable{
				{Name: "A", Value: "x", MysteryboxSecret: &aipb.EndpointSpec_MysteryBoxSecretRef{SecretId: "mbsec-1"}},
			}
		}},
		{"duplicate env names", func(c *domain.CreateEndpointCmd) {
			c.Spec.EnvironmentVariables = []*aipb.EndpointSpec_EnvironmentVariable{{Name: "A", Value: "1"}, {Name: "A", Value: "2"}}
		}},
		{"relative volume path", func(c *domain.CreateEndpointCmd) {
			c.Spec.Volumes = []*aipb.EndpointSpec_VolumeMount{{Source: "vol", ContainerPath: "data"}}
		}},
		{"s3 config with non-s3 source", func(c *domain.CreateEndpointCmd) {
			c.Spec.Volumes = []*aipb.EndpointSpec_VolumeMount{{
				Source: "bucket", ContainerPath: "/data",
				SourceConfig: &aipb.EndpointSpec_VolumeMount_S3Config_{S3Config: &aipb.EndpointSpec_VolumeMount_S3Config{Region: "eu"}},
			}}
		}},
		{"injected file without content", func(c *domain.CreateEndpointCmd) {
			c.Spec.InjectedFiles = []*aipb.EndpointSpec_FileInjection{{ContainerPath: "/etc/x"}}
		}},
		{"metadata id set", func(c *domain.CreateEndpointCmd) { c.Metadata.Id = "aiendpoint-x" }},
		{"parent not a project", func(c *domain.CreateEndpointCmd) { c.Metadata.ParentId = "tenant-x" }},
		{"missing parent", func(c *domain.CreateEndpointCmd) { c.Metadata.ParentId = "" }},
		{"subnet not a vpcsubnet", func(c *domain.CreateEndpointCmd) { c.Spec.SubnetId = "foo" }},
		{"missing image", func(c *domain.CreateEndpointCmd) { c.Spec.Image = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newService(t, opts{})
			cmd := createCmd("ep", baseSpec())
			tt.mutate(&cmd)
			_, err := h.svc.Create(t.Context(), cmd)
			if got := errs.KindOf(err); got != errs.KindBadRequest {
				t.Fatalf("Create kind = %v, want BadRequest (err: %v)", got, err)
			}
			if n := h.repo.count(); n != 0 {
				t.Errorf("stored %d endpoints after a rejected create, want 0", n)
			}
		})
	}
}

func TestCreateHardwareSelectors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		gpu, cpu     []string
		wantPlatform string
		wantPreset   string
	}{
		{name: "gpu H100:8", gpu: []string{"H100:8"}, wantPlatform: "gpu-h100-sxm", wantPreset: "8gpu-128vcpu-1600gb"},
		{name: "cpu amd:16", cpu: []string{"amd:16"}, wantPlatform: "cpu-d3", wantPreset: "16vcpu-64gb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newService(t, opts{})
			spec := baseSpec()
			spec.Platform, spec.Preset, spec.Gpu, spec.Cpu = "", "", tt.gpu, tt.cpu
			e := mustCreate(t, h, "ep", spec)
			if got := e.GetSpec().GetPlatform(); got != tt.wantPlatform {
				t.Errorf("spec.platform = %q, want %q", got, tt.wantPlatform)
			}
			if got := e.GetSpec().GetPreset(); got != tt.wantPreset {
				t.Errorf("spec.preset = %q, want %q", got, tt.wantPreset)
			}
		})
	}
}

func TestCreateReferences(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		opts     opts
		subnet   string
		parent   string
		wantKind errs.Kind // zero = no error
	}{
		{name: "subnet unknown", opts: opts{kinds: []string{"vpcsubnet"}}, subnet: "vpcsubnet-x", wantKind: errs.KindNotFound},
		{name: "subnet known", opts: opts{kinds: []string{"vpcsubnet"}, ids: []string{"vpcsubnet-x"}}, subnet: "vpcsubnet-x"},
		{name: "subnet kind not registered", opts: opts{}, subnet: "vpcsubnet-x"},
		{name: "project unknown", opts: opts{kinds: []string{"project"}}, parent: testProject, wantKind: errs.KindNotFound},
		{name: "project known", opts: opts{kinds: []string{"project"}, ids: []string{testProject}}, parent: testProject},
		{name: "project kind not registered", opts: opts{}, parent: testProject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newService(t, tt.opts)
			spec := baseSpec()
			spec.SubnetId = tt.subnet
			cmd := createCmd("ep", spec)
			if tt.parent != "" {
				cmd.Metadata.ParentId = tt.parent
			}
			_, err := h.svc.Create(t.Context(), cmd)
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Create kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
		})
	}
}

func secretSpec(id string) *aipb.EndpointSpec {
	s := baseSpec()
	s.AuthToken = ""
	s.AuthTokenMysteryboxSecret = &aipb.EndpointSpec_MysteryBoxSecretRef{SecretId: id}
	return s
}

var (
	payloadWithToken = peer.SecretPayload{
		SecretID: "mbsec-1", VersionID: "mbsecver-9",
		Entries: []peer.PayloadEntry{{Key: "AUTH_TOKEN", StringValue: "tok"}},
	}
	payloadWithoutToken = peer.SecretPayload{
		SecretID: "mbsec-2", VersionID: "mbsecver-8",
		Entries: []peer.PayloadEntry{{Key: "OTHER", StringValue: "x"}},
	}
)

func TestCreateSecrets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		opts     opts
		spec     *aipb.EndpointSpec
		wantKind errs.Kind
		wantRef  *aipb.EndpointSpec_MysteryBoxSecretRef
	}{
		{
			name:    "pinned from payload",
			opts:    opts{secrets: &fakePayloads{secrets: []peer.SecretPayload{payloadWithToken}}},
			spec:    secretSpec("mbsec-1"),
			wantRef: &aipb.EndpointSpec_MysteryBoxSecretRef{SecretId: "mbsec-1", VersionId: "mbsecver-9"},
		},
		{
			name:     "payload without AUTH_TOKEN",
			opts:     opts{secrets: &fakePayloads{secrets: []peer.SecretPayload{payloadWithoutToken}}},
			spec:     secretSpec("mbsec-2"),
			wantKind: errs.KindBadRequest,
		},
		{
			name:     "provider returns not found",
			opts:     opts{secrets: &fakePayloads{secrets: []peer.SecretPayload{payloadWithToken}}},
			spec:     secretSpec("mbsec-404"),
			wantKind: errs.KindNotFound,
		},
		{
			name:    "no provider and kind unregistered",
			opts:    opts{},
			spec:    secretSpec("mbsec-1"),
			wantRef: &aipb.EndpointSpec_MysteryBoxSecretRef{SecretId: "mbsec-1"},
		},
		{
			name:     "malformed secret id",
			opts:     opts{},
			spec:     secretSpec("foo"),
			wantKind: errs.KindBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newService(t, tt.opts)
			op, err := h.svc.Create(t.Context(), createCmd("ep", tt.spec))
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Create kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if err != nil {
				return
			}
			e, err := h.svc.GetSecret(t.Context(), op.ResourceID)
			if err != nil {
				t.Fatal(err)
			}
			if d := cmp.Diff(tt.wantRef, e.GetSpec().GetAuthTokenMysteryboxSecret(), protocmp.Transform()); d != "" {
				t.Errorf("auth_token_mysterybox_secret mismatch (-want +got):\n%s", d)
			}
		})
	}
}

func TestCreateProviderFailure(t *testing.T) {
	t.Parallel()
	h := newService(t, opts{secrets: &fakePayloads{err: errors.New("boom")}})
	if _, err := h.svc.Create(t.Context(), createCmd("ep", secretSpec("mbsec-1"))); err == nil {
		t.Fatal("Create with a failing secret provider succeeded, want an error")
	}
}

func TestNameUniquenessAndLookup(t *testing.T) {
	t.Parallel()
	h := newService(t, opts{})
	first := mustCreate(t, h, "same", baseSpec())
	if _, err := h.svc.Create(t.Context(), createCmd("same", baseSpec())); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate name err = %v, want AlreadyExists", err)
	}
	got, err := h.svc.GetByName(t.Context(), testProject, "same")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.GetMetadata().GetId() != first.GetMetadata().GetId() {
		t.Errorf("GetByName id = %s, want %s", got.GetMetadata().GetId(), first.GetMetadata().GetId())
	}
	if _, err := h.svc.GetByName(t.Context(), testProject, "missing"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("GetByName(missing) err = %v, want NotFound", err)
	}
}

func TestListPaging(t *testing.T) {
	t.Parallel()
	h := newService(t, opts{})
	for _, n := range []string{"a", "b", "c"} {
		mustCreate(t, h, n, baseSpec())
	}
	names := func(p domain.EndpointPage) []string {
		var out []string
		for _, e := range p.Items {
			out = append(out, e.GetMetadata().GetName())
		}
		return out
	}
	p1, err := h.svc.List(t.Context(), testProject, 2, "")
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if d := cmp.Diff([]string{"a", "b"}, names(p1)); d != "" {
		t.Errorf("page 1 (-want +got):\n%s", d)
	}
	if p1.NextPageToken == "" {
		t.Fatal("page 1 has no next page token")
	}
	p2, err := h.svc.List(t.Context(), testProject, 2, p1.NextPageToken)
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if d := cmp.Diff([]string{"c"}, names(p2)); d != "" {
		t.Errorf("page 2 (-want +got):\n%s", d)
	}
	if p2.NextPageToken != "" {
		t.Errorf("page 2 token = %q, want empty", p2.NextPageToken)
	}
	all, err := h.svc.List(t.Context(), testProject, 0, "")
	if err != nil || len(all.Items) != 3 {
		t.Errorf("List(page_size 0) = %d items, %v; want 3, nil", len(all.Items), err)
	}
	for _, size := range []int64{-1, 1001} {
		if _, err := h.svc.List(t.Context(), testProject, size, ""); !errs.IsKind(err, errs.KindBadRequest) {
			t.Errorf("List(page_size %d) err = %v, want BadRequest", size, err)
		}
	}
}

func TestRedaction(t *testing.T) {
	t.Parallel()
	h := newService(t, opts{})
	spec := baseSpec()
	spec.RegistryCredentials = &aipb.EndpointSpec_RegistryCredentials{Username: "u", Password: "p"}
	spec.InjectedFiles = []*aipb.EndpointSpec_FileInjection{{ContainerPath: "/etc/x", Content: []byte("secret")}}
	created := mustCreate(t, h, "ep", spec)
	id := created.GetMetadata().GetId()

	views := map[string]func() (*aipb.Endpoint, error){
		"Get":       func() (*aipb.Endpoint, error) { return h.svc.Get(t.Context(), id) },
		"GetByName": func() (*aipb.Endpoint, error) { return h.svc.GetByName(t.Context(), testProject, "ep") },
		"List": func() (*aipb.Endpoint, error) {
			p, err := h.svc.List(t.Context(), testProject, 0, "")
			if err != nil || len(p.Items) != 1 {
				return nil, errors.Join(err, errors.New("want one item"))
			}
			return p.Items[0], nil
		},
	}
	for name, view := range views {
		e, err := view()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := e.GetSpec().GetRegistryCredentials().GetPassword(); got != "" {
			t.Errorf("%s: registry password = %q, want redacted", name, got)
		}
		if got := e.GetSpec().GetRegistryCredentials().GetUsername(); got != "u" {
			t.Errorf("%s: registry username = %q, want kept", name, got)
		}
		if got := e.GetSpec().GetInjectedFiles()[0].GetContent(); len(got) != 0 {
			t.Errorf("%s: injected file content = %q, want redacted", name, got)
		}
	}
	if got := created.GetSpec().GetRegistryCredentials().GetPassword(); got != "p" {
		t.Errorf("GetSecret: registry password = %q, want p", got)
	}
	if got := string(created.GetSpec().GetInjectedFiles()[0].GetContent()); got != "secret" {
		t.Errorf("GetSecret: injected file content = %q, want secret", got)
	}
	// Redaction must not leak into the stored copy.
	again, err := h.svc.GetSecret(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.GetSpec().GetRegistryCredentials().GetPassword(); got != "p" {
		t.Errorf("stored registry password = %q after Get, want p", got)
	}
}

func TestDryRun(t *testing.T) {
	t.Parallel()
	h := newService(t, opts{})
	cmd := createCmd("ep", baseSpec())
	cmd.DryRun = true
	if _, err := h.svc.Create(t.Context(), cmd); err != nil {
		t.Fatalf("dry-run Create: %v", err)
	}
	if n := h.repo.count(); n != 0 {
		t.Errorf("dry run stored %d endpoints, want 0", n)
	}
	bad := createCmd("ep", baseSpec())
	bad.DryRun = true
	bad.Spec.Platform = "nope"
	if _, err := h.svc.Create(t.Context(), bad); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("invalid dry-run err = %v, want BadRequest", err)
	}
}
