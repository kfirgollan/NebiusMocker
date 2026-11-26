package domain

import (
	"context"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
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

func TestCreateRegistryAdmitsQuota(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		refuse bool
	}{
		{name: "admitted"},
		{name: "refused", refuse: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			gate := &recordingGate{refuse: tt.refuse}
			e.registry.quota = gate
			_, err := e.registry.Create(t.Context(), CreateRegistryCmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: "x"},
			})
			if len(gate.claims) != 1 {
				t.Fatalf("claims = %v, want exactly one", gate.claims)
			}
			c := gate.claims[0]
			if c.ContainerID != testProject || c.Region != "" || len(c.Amounts) != 1 || c.Amounts[quota.RegistryCount] != 1 {
				t.Fatalf("claim = %+v, want %s=1 on %s", c, quota.RegistryCount, testProject)
			}
			if tt.refuse {
				if !errs.IsKind(err, errs.KindQuotaExceeded) {
					t.Fatalf("err = %v, want QuotaExceeded", err)
				}
				if n := len(e.regs.items); n != 0 {
					t.Fatalf("%d registries stored after a refused claim", n)
				}
				return
			}
			if err != nil || len(e.regs.items) != 1 {
				t.Fatalf("err = %v, stored = %d, want nil and 1", err, len(e.regs.items))
			}
		})
	}
}
