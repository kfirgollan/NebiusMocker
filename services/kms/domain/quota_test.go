package domain

import (
	"context"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

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

func TestKeyCreateAdmitsQuota(t *testing.T) {
	t.Parallel()
	md := func() *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{ParentId: testProject, Name: "k"} }
	symCreate := func(t *testing.T, gate QuotaGate) (stored func() int, err error) {
		t.Helper()
		e := newSymEnv(t)
		e.svc.lc.quota = gate
		_, err = e.svc.Create(t.Context(), CreateSymmetricKeyCmd{Metadata: md(), Spec: symSpec()})
		return func() int { return len(e.repo.entries) }, err
	}
	asymCreate := func(t *testing.T, gate QuotaGate) (stored func() int, err error) {
		t.Helper()
		e := newAsymEnv(t)
		e.svc.lc.quota = gate
		_, err = e.svc.Create(t.Context(), CreateAsymmetricKeyCmd{
			Metadata: md(),
			Spec:     &kmspb.AsymmetricKeySpec{Algorithm: kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256},
		})
		return func() int { return len(e.repo.entries) }, err
	}
	tests := []struct {
		name   string
		quota  string
		refuse bool
		create func(t *testing.T, gate QuotaGate) (func() int, error)
	}{
		{name: "symmetric admitted", quota: quota.KMSSymmetricKeyCount, create: symCreate},
		{name: "symmetric refused", quota: quota.KMSSymmetricKeyCount, refuse: true, create: symCreate},
		{name: "asymmetric admitted", quota: quota.KMSAsymmetricKeyCount, create: asymCreate},
		{name: "asymmetric refused", quota: quota.KMSAsymmetricKeyCount, refuse: true, create: asymCreate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gate := &recordingGate{refuse: tt.refuse}
			stored, err := tt.create(t, gate)
			if len(gate.claims) != 1 {
				t.Fatalf("claims = %v, want exactly one", gate.claims)
			}
			c := gate.claims[0]
			if c.ContainerID != testProject || c.Region != "" || len(c.Amounts) != 1 || c.Amounts[tt.quota] != 1 {
				t.Fatalf("claim = %+v, want %s=1 on %s", c, tt.quota, testProject)
			}
			if tt.refuse {
				if !errs.IsKind(err, errs.KindQuotaExceeded) {
					t.Fatalf("err = %v, want QuotaExceeded", err)
				}
				if n := stored(); n != 0 {
					t.Fatalf("%d keys stored after a refused claim", n)
				}
				return
			}
			if err != nil || stored() != 1 {
				t.Fatalf("err = %v, stored = %d, want nil and 1", err, stored())
			}
		})
	}
}
