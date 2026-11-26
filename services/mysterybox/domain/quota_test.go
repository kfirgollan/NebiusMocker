package domain_test

import (
	"context"
	"fmt"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/domain"
)

// recordingGate records claims; with refuse set it rejects them without apply.
type recordingGate struct {
	refuse bool
	claims []peer.QuotaClaim
}

func (g *recordingGate) Admit(ctx context.Context, c peer.QuotaClaim, apply func(ctx context.Context) error) error {
	g.claims = append(g.claims, c)
	if g.refuse {
		return errs.QuotaExceeded("quota exhausted")
	}
	return apply(ctx)
}

func TestCreateSecretAdmitsQuota(t *testing.T) {
	t.Parallel()
	payload := []*mysteryboxpb.Payload{strEntry("k", "v")}
	creators := map[string]func(t *testing.T, e *env) error{
		"CreateSecret": func(t *testing.T, e *env) error {
			t.Helper()
			_, err := e.svc.CreateSecret(t.Context(), domain.CreateSecretCmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "s"},
				Spec:     &mysteryboxpb.SecretSpec{},
			})
			return err
		},
		"CreateSystemSecret": func(t *testing.T, e *env) error {
			t.Helper()
			_, err := e.svc.CreateSystemSecret(t.Context(), domain.SystemSecretCmd{ParentID: projectID, Name: "s", Payload: payload})
			return err
		},
	}
	for name, create := range creators {
		for _, refuse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s refuse=%v", name, refuse), func(t *testing.T) {
				t.Parallel()
				gate := &recordingGate{refuse: refuse}
				e := newEnvGate(t, domain.Limits{}, gate)
				err := create(t, e)
				if len(gate.claims) != 1 {
					t.Fatalf("claims = %v, want exactly one", gate.claims)
				}
				c := gate.claims[0]
				if c.ContainerID != projectID || c.Region != "" || len(c.Amounts) != 1 || c.Amounts[quota.MysteryBoxSecretCount] != 1 {
					t.Fatalf("claim = %+v, want %s=1 on %s", c, quota.MysteryBoxSecretCount, projectID)
				}
				if refuse {
					if !errs.IsKind(err, errs.KindQuotaExceeded) {
						t.Fatalf("err = %v, want QuotaExceeded", err)
					}
					if n := len(e.secrets.items); n != 0 {
						t.Fatalf("%d secrets stored after a refused claim", n)
					}
					return
				}
				if err != nil || len(e.secrets.items) != 1 {
					t.Fatalf("err = %v, stored = %d, want nil and 1", err, len(e.secrets.items))
				}
			})
		}
	}
}

func TestDeleteAndUndeleteDoNotAdmit(t *testing.T) {
	t.Parallel()
	gate := &recordingGate{}
	e := newEnvGate(t, domain.Limits{}, gate)
	op, err := e.svc.CreateSecret(t.Context(), domain.CreateSecretCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "s"}, Spec: &mysteryboxpb.SecretSpec{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.DeleteSecret(t.Context(), op.ResourceID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UndeleteSecret(t.Context(), op.ResourceID, "", nil); err != nil {
		t.Fatal(err)
	}
	if len(gate.claims) != 1 {
		t.Fatalf("claims = %v, want only the create claim", gate.claims)
	}
}
