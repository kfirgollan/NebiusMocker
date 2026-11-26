package domain_test

import (
	"context"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
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

func wantClaim(t *testing.T, gate *recordingGate, name string) {
	t.Helper()
	if len(gate.claims) != 1 {
		t.Fatalf("claims = %v, want exactly one", gate.claims)
	}
	c := gate.claims[0]
	if c.ContainerID != projectID || c.Region != "" || len(c.Amounts) != 1 || c.Amounts[name] != 1 {
		t.Fatalf("claim = %+v, want %s=1 on %s", c, name, projectID)
	}
}

// opErr is the ID of a finished operation and its failure: the synchronous
// error, else the operation's own.
func opErr(op *operations.Operation, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return op.ResourceID, op.Err
}

func TestBucketCreateAdmitsQuota(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		refuse bool
		fs     bool // a filesystem bucket
	}{
		{name: "object bucket admitted"},
		{name: "object bucket refused", refuse: true},
		{name: "filesystem bucket admitted", fs: true},
		{name: "filesystem bucket refused", fs: true, refuse: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gate := &recordingGate{refuse: tt.refuse}
			f := newFSFixture(t, fsOptions{gate: gate})
			spec := &storagepb.BucketSpec{}
			if tt.fs {
				spec = fsSpec()
			}
			_, err := f.svc.Create(t.Context(), domain.CreateBucketCmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "bkt"}, Spec: spec,
			})
			wantClaim(t, gate, quota.StorageBucketCount)
			if tt.refuse {
				if !errs.IsKind(err, errs.KindQuotaExceeded) {
					t.Fatalf("err = %v, want QuotaExceeded", err)
				}
				if n := len(f.buckets.items); n != 0 {
					t.Fatalf("%d buckets stored after a refused claim", n)
				}
				return
			}
			if err != nil || len(f.buckets.items) != 1 {
				t.Fatalf("err = %v, stored = %d, want nil and 1", err, len(f.buckets.items))
			}
		})
	}
}

func TestTransferCreateAdmitsQuota(t *testing.T) {
	t.Parallel()
	create := map[string]func(f *trFixture) (string, error){
		"v1": func(f *trFixture) (string, error) {
			op, err := f.svc.CreateV1(t.Context(), domain.CreateTransferV1Cmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "x"}, Spec: trV1Spec(),
			})
			return opErr(op, err)
		},
		"v1alpha1": func(f *trFixture) (string, error) {
			op, err := f.svc.CreateV1Alpha1(t.Context(), domain.CreateTransferV1Alpha1Cmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "x"}, Spec: trAlphaSpec(),
			})
			return opErr(op, err)
		},
	}
	for api, do := range create {
		for _, refuse := range []bool{false, true} {
			name := api + " admitted"
			if refuse {
				name = api + " refused"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				gate := &recordingGate{refuse: refuse}
				f := newTrFixtureGate(t, 1, gate)
				id, err := do(f)
				wantClaim(t, gate, quota.StorageTransferCount)
				if refuse {
					if !errs.IsKind(err, errs.KindQuotaExceeded) {
						t.Fatalf("err = %v, want QuotaExceeded", err)
					}
					if n := len(f.repo.recs); n != 0 {
						t.Fatalf("%d transfers stored after a refused claim", n)
					}
					return
				}
				if err != nil || id == "" || len(f.repo.recs) != 1 {
					t.Fatalf("err = %v, id = %q, stored = %d, want nil, an id and 1", err, id, len(f.repo.recs))
				}
			})
		}
	}
}
