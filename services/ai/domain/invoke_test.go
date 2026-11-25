package domain_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/ai/domain"
)

func TestAuthorizeCall(t *testing.T) {
	t.Parallel()
	twoPorts := func() *aipb.EndpointSpec {
		s := baseSpec()
		s.AuthToken = ""
		s.Ports = []*aipb.EndpointSpec_Port{
			{ContainerPort: 80, Protocol: aipb.EndpointSpec_Port_HTTP},
			{ContainerPort: 8080, Protocol: aipb.EndpointSpec_Port_HTTP},
		}
		return s
	}
	tests := []struct {
		name     string
		spec     *aipb.EndpointSpec
		stop     bool
		target   string // "" means the endpoint ID
		suffix   string // appended to the endpoint ID
		auth     string
		wantKind errs.Kind
		wantPort int32
	}{
		{name: "unknown id", spec: baseSpec(), target: "aiendpoint-404", wantKind: errs.KindNotFound},
		{name: "correct token", spec: baseSpec(), auth: "Bearer s3cret", wantPort: 80},
		{name: "scheme is case-insensitive", spec: baseSpec(), auth: "bearer s3cret", wantPort: 80},
		{name: "missing header", spec: baseSpec(), wantKind: errs.KindUnauthenticated},
		{name: "wrong token", spec: baseSpec(), auth: "Bearer nope", wantKind: errs.KindUnauthenticated},
		{name: "basic scheme", spec: baseSpec(), auth: "Basic s3cret", wantKind: errs.KindUnauthenticated},
		{name: "token without scheme", spec: baseSpec(), auth: "s3cret", wantKind: errs.KindUnauthenticated},
		{name: "stopped", spec: baseSpec(), stop: true, auth: "Bearer s3cret", wantKind: errs.KindBadState},
		{name: "open endpoint", spec: func() *aipb.EndpointSpec { s := baseSpec(); s.AuthToken = ""; return s }(), wantPort: 80},
		{name: "second port", spec: twoPorts(), suffix: ":8080", wantPort: 8080},
		{name: "first port by default", spec: twoPorts(), wantPort: 80},
		{name: "unknown port", spec: twoPorts(), suffix: ":9999", wantKind: errs.KindNotFound},
		{name: "non-numeric port", spec: twoPorts(), suffix: ":abc", wantKind: errs.KindNotFound},
		{name: "empty port", spec: twoPorts(), suffix: ":", wantKind: errs.KindNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newService(t, opts{})
			id := mustCreate(t, h, "ep", tt.spec).GetMetadata().GetId()
			if tt.stop {
				if _, err := h.svc.Stop(t.Context(), id, nil); err != nil {
					t.Fatalf("Stop: %v", err)
				}
			}
			target := tt.target
			if target == "" {
				target = id + tt.suffix
			}
			call, err := h.svc.AuthorizeCall(t.Context(), target, tt.auth)
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("AuthorizeCall kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if err != nil {
				return
			}
			if d := cmp.Diff(domain.Call{EndpointID: id, Port: tt.wantPort}, call); d != "" {
				t.Errorf("Call mismatch (-want +got):\n%s", d)
			}
		})
	}
}

func TestAuthorizeCallSecretToken(t *testing.T) {
	t.Parallel()
	payloads := &fakePayloads{secrets: []peer.SecretPayload{payloadWithToken}}

	t.Run("token read from the payload", func(t *testing.T) {
		t.Parallel()
		h := newService(t, opts{secrets: payloads})
		id := mustCreate(t, h, "ep", secretSpec("mbsec-1")).GetMetadata().GetId()
		if _, err := h.svc.AuthorizeCall(t.Context(), id, "Bearer tok"); err != nil {
			t.Fatalf("correct token: %v", err)
		}
		if _, err := h.svc.AuthorizeCall(t.Context(), id, "Bearer other"); !errs.IsKind(err, errs.KindUnauthenticated) {
			t.Errorf("wrong token err = %v, want Unauthenticated", err)
		}
	})

	t.Run("binary token", func(t *testing.T) {
		t.Parallel()
		bin := peer.SecretPayload{
			SecretID: "mbsec-1", VersionID: "mbsecver-9",
			Entries: []peer.PayloadEntry{{Key: "AUTH_TOKEN", Binary: true, BinaryValue: []byte("bin")}},
		}
		h := newService(t, opts{secrets: &fakePayloads{secrets: []peer.SecretPayload{bin}}})
		id := mustCreate(t, h, "ep", secretSpec("mbsec-1")).GetMetadata().GetId()
		if _, err := h.svc.AuthorizeCall(t.Context(), id, "Bearer bin"); err != nil {
			t.Fatalf("binary token: %v", err)
		}
	})

	t.Run("provider gone", func(t *testing.T) {
		t.Parallel()
		h := newService(t, opts{secrets: payloads})
		id := mustCreate(t, h, "ep", secretSpec("mbsec-1")).GetMetadata().GetId()
		h.secrets.set(nil)
		if _, err := h.svc.AuthorizeCall(t.Context(), id, "Bearer tok"); !errs.IsKind(err, errs.KindBadState) {
			t.Errorf("no provider err = %v, want BadState", err)
		}
	})

	t.Run("secret no longer readable", func(t *testing.T) {
		t.Parallel()
		h := newService(t, opts{secrets: payloads})
		id := mustCreate(t, h, "ep", secretSpec("mbsec-1")).GetMetadata().GetId()
		h.secrets.set(&fakePayloads{})
		if _, err := h.svc.AuthorizeCall(t.Context(), id, "Bearer tok"); !errs.IsKind(err, errs.KindBadState) {
			t.Errorf("unreadable secret err = %v, want BadState", err)
		}
	})
}
