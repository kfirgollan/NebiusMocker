package domain_test

import (
	"errors"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/ai/domain"
)

func portsSpec(ports ...*aipb.EndpointSpec_Port) *aipb.EndpointSpec {
	s := baseSpec()
	s.AuthToken = ""
	s.Ports = ports
	return s
}

func port(p int32, proto aipb.EndpointSpec_Port_Protocol) *aipb.EndpointSpec_Port {
	return &aipb.EndpointSpec_Port{ContainerPort: p, Protocol: proto}
}

func TestCreateRejectsUDP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		ports     []*aipb.EndpointSpec_Port
		wantField string
	}{
		{"only port", []*aipb.EndpointSpec_Port{port(53, aipb.EndpointSpec_Port_UDP)}, "spec.ports[0].protocol"},
		{
			"second port",
			[]*aipb.EndpointSpec_Port{port(80, aipb.EndpointSpec_Port_HTTP), port(53, aipb.EndpointSpec_Port_UDP)},
			"spec.ports[1].protocol",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newService(t, opts{})
			_, err := h.svc.Create(t.Context(), createCmd("ep", portsSpec(tt.ports...)))
			if got := errs.KindOf(err); got != errs.KindBadRequest {
				t.Fatalf("Create kind = %v, want BadRequest (err: %v)", got, err)
			}
			e, ok := errors.AsType[*errs.Error](err)
			if !ok {
				t.Fatalf("error %v is not an *errs.Error", err)
			}
			var got []string
			for _, v := range e.Violations {
				got = append(got, v.Field)
			}
			if d := cmp.Diff([]string{tt.wantField}, got); d != "" {
				t.Errorf("violation fields (-want +got):\n%s", d)
			}
			if n := h.repo.count(); n != 0 {
				t.Errorf("stored %d endpoints after a rejected create, want 0", n)
			}
		})
	}
}

func TestPublicEndpointsWithTCP(t *testing.T) {
	t.Parallel()
	tcp := func(id string, p int) string { return "tls://" + id + "-p" + strconv.Itoa(p) + ".dp.test:4601" }
	tests := []struct {
		name  string
		ports []*aipb.EndpointSpec_Port
		want  func(id string) []string
	}{
		{
			name:  "tcp only",
			ports: []*aipb.EndpointSpec_Port{port(22, aipb.EndpointSpec_Port_TCP)},
			want:  func(id string) []string { return []string{tcp(id, 22)} },
		},
		{
			name: "one http and tcp keep spec order",
			ports: []*aipb.EndpointSpec_Port{
				port(5432, aipb.EndpointSpec_Port_TCP), port(80, aipb.EndpointSpec_Port_HTTP), port(22, aipb.EndpointSpec_Port_TCP),
			},
			want: func(id string) []string { return []string{tcp(id, 5432), testBaseURL + "/" + id, tcp(id, 22)} },
		},
		{
			name: "two http and a tcp",
			ports: []*aipb.EndpointSpec_Port{
				port(80, aipb.EndpointSpec_Port_HTTP), port(5432, aipb.EndpointSpec_Port_TCP), port(8080, aipb.EndpointSpec_Port_HTTP),
			},
			want: func(id string) []string {
				return []string{testBaseURL + "/" + id + ":80", tcp(id, 5432), testBaseURL + "/" + id + ":8080"}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newService(t, opts{})
			id := mustCreate(t, h, "ep", portsSpec(tt.ports...)).GetMetadata().GetId()
			got := mustGet(t, h, id).GetStatus().GetPublicEndpoints()
			if d := cmp.Diff(tt.want(id), got); d != "" {
				t.Errorf("public_endpoints (-want +got):\n%s", d)
			}
		})
	}
}

func TestAuthorizeTCP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		target   string // "" means the created endpoint
		port     int32
		stop     bool
		wantKind errs.Kind
	}{
		{name: "unknown endpoint", target: "aiendpoint-404", port: 5432, wantKind: errs.KindNotFound},
		{name: "http port is not tcp", port: 80, wantKind: errs.KindNotFound},
		{name: "unknown port", port: 9999, wantKind: errs.KindNotFound},
		{name: "stopped", port: 5432, stop: true, wantKind: errs.KindBadState},
		{name: "running", port: 5432},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newService(t, opts{})
			spec := portsSpec(port(80, aipb.EndpointSpec_Port_HTTP), port(5432, aipb.EndpointSpec_Port_TCP))
			id := mustCreate(t, h, "ep", spec).GetMetadata().GetId()
			if tt.stop {
				if _, err := h.svc.Stop(t.Context(), id, nil); err != nil {
					t.Fatalf("Stop: %v", err)
				}
			}
			target := tt.target
			if target == "" {
				target = id
			}
			call, err := h.svc.AuthorizeTCP(t.Context(), target, tt.port)
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("AuthorizeTCP kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if err != nil {
				return
			}
			if d := cmp.Diff(domain.Call{EndpointID: id, Port: tt.port}, call); d != "" {
				t.Errorf("Call mismatch (-want +got):\n%s", d)
			}
		})
	}
}

func TestCreateRequiresPayloadRead(t *testing.T) {
	t.Parallel()
	payloads := func() *fakePayloads { return &fakePayloads{secrets: []peer.SecretPayload{payloadWithToken}} }
	envSpec := func() *aipb.EndpointSpec {
		s := baseSpec()
		s.EnvironmentVariables = []*aipb.EndpointSpec_EnvironmentVariable{
			{Name: "TOKEN", MysteryboxSecret: &aipb.EndpointSpec_MysteryBoxSecretRef{SecretId: "mbsec-1"}},
		}
		return s
	}
	tests := []struct {
		name      string
		kinds     []string
		deny      []string
		spec      *aipb.EndpointSpec
		wantKind  errs.Kind
		wantReads bool // the payload is read
		wantPerms []permCall
	}{
		{
			name: "auth token denied", kinds: []string{"mbsec"}, deny: []string{"mbsec-1"}, spec: secretSpec("mbsec-1"),
			wantKind:  errs.KindPermissionDenied,
			wantPerms: []permCall{{Perm: "mysterybox.payload.read", Resource: "mbsec-1"}},
		},
		{
			name: "env secret denied", kinds: []string{"mbsec"}, deny: []string{"mbsec-1"}, spec: envSpec(),
			wantKind:  errs.KindPermissionDenied,
			wantPerms: []permCall{{Perm: "mysterybox.payload.read", Resource: "mbsec-1"}},
		},
		{
			name: "allowed", kinds: []string{"mbsec"}, spec: secretSpec("mbsec-1"), wantReads: true,
			wantPerms: []permCall{{Perm: "mysterybox.payload.read", Resource: "mbsec-1"}},
		},
		{name: "kind not registered", deny: []string{"mbsec-1"}, spec: secretSpec("mbsec-1")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fp := payloads()
			h := newService(t, opts{kinds: tt.kinds, secrets: fp, deny: tt.deny})
			_, err := h.svc.Create(t.Context(), createCmd("ep", tt.spec))
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Create kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if d := cmp.Diff(tt.wantPerms, h.perms.recorded()); d != "" {
				t.Errorf("permission checks (-want +got):\n%s", d)
			}
			if tt.wantKind == errs.KindPermissionDenied {
				if n := fp.readCount(); n != 0 {
					t.Errorf("payload read %d times for a denied caller, want 0", n)
				}
				if n := h.repo.count(); n != 0 {
					t.Errorf("stored %d endpoints, want 0", n)
				}
			}
			if tt.wantReads && fp.readCount() == 0 {
				t.Error("payload was never read for an allowed caller")
			}
		})
	}
}
