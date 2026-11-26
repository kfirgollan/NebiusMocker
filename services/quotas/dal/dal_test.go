package dal

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	quotaspb "github.com/nebius/gosdk/proto/nebius/quotas/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/quotas/domain"
)

const project = "project-e00test0000000000001"

func allowance(id, name, region string, limit uint64) domain.Allowance {
	return domain.Allowance{Region: region, Msg: &quotaspb.QuotaAllowance{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: project, Name: name, ResourceVersion: 1},
		Spec:     &quotaspb.QuotaAllowanceSpec{Region: region, Limit: proto.Uint64(limit)},
	}}
}

func TestEnsureInsertsOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewRepo(store.NewMemory())
	first := allowance("quotaallowance-e00a", "q", "eu-north1", 10)
	if err := r.Ensure(ctx, []domain.Allowance{first}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	changed := allowance("quotaallowance-e00a", "q", "eu-north1", 99)
	if err := r.Ensure(ctx, []domain.Allowance{changed}); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	got, err := r.Get(ctx, "quotaallowance-e00a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(first.Msg, got.Msg, protocmp.Transform()); diff != "" {
		t.Errorf("Ensure replaced an existing allowance (-want +got):\n%s", diff)
	}
	if got.Region != "eu-north1" {
		t.Errorf("region = %q, want eu-north1", got.Region)
	}
}

func TestByKeyDistinguishesRegions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewRepo(store.NewMemory())
	eu, us := allowance("quotaallowance-e00a", "q", "eu-north1", 1), allowance("quotaallowance-u00b", "q", "us-central1", 2)
	if err := r.Ensure(ctx, []domain.Allowance{eu, us}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, want := range []domain.Allowance{eu, us} {
		got, err := r.ByKey(ctx, project, "q", want.Region)
		if err != nil || got.Msg.GetMetadata().GetId() != want.Msg.GetMetadata().GetId() || got.Region != want.Region {
			t.Errorf("ByKey(%s) = %v, %v", want.Region, got, err)
		}
	}
	if _, err := r.ByKey(ctx, project, "q", "other"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("ByKey unknown region: %v, want NotFound", err)
	}
	if _, err := r.ByKey(ctx, project, "other", "eu-north1"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("ByKey unknown name: %v, want NotFound", err)
	}
}

func TestGetPutNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewRepo(store.NewMemory())
	if _, err := r.Get(ctx, "quotaallowance-e00missing"); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get: %v, want NotFound", err)
	}
	if err := r.Put(ctx, allowance("quotaallowance-e00missing", "q", "eu-north1", 1)); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Put: %v, want NotFound", err)
	}
}

func TestPutReplaces(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewRepo(store.NewMemory())
	if err := r.Ensure(ctx, []domain.Allowance{allowance("quotaallowance-e00a", "q", "eu-north1", 1)}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	next := allowance("quotaallowance-e00a", "q", "eu-north1", 5)
	if err := r.Put(ctx, next); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := r.ByKey(ctx, project, "q", "eu-north1")
	if err != nil {
		t.Fatalf("ByKey: %v", err)
	}
	if diff := cmp.Diff(next.Msg, got.Msg, protocmp.Transform()); diff != "" {
		t.Errorf("Put (-want +got):\n%s", diff)
	}
}

func TestListPaging(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := NewRepo(store.NewMemory())
	var all []domain.Allowance
	for i := range 5 {
		all = append(all, allowance(fmt.Sprintf("quotaallowance-e00%d", i), fmt.Sprintf("q%d", i), "eu-north1", uint64(i)))
	}
	if err := r.Ensure(ctx, all); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	var ids []string
	token := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("paging does not terminate")
		}
		p, err := r.List(ctx, project, 2, token)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(p.Items) > 2 {
			t.Fatalf("page of %d items, want at most 2", len(p.Items))
		}
		for _, a := range p.Items {
			ids = append(ids, a.Msg.GetMetadata().GetId())
		}
		if token = p.NextPageToken; token == "" {
			break
		}
	}
	if len(ids) != 5 {
		t.Fatalf("listed %v, want 5 ids", ids)
	}
	for i, id := range ids {
		if want := fmt.Sprintf("quotaallowance-e00%d", i); id != want {
			t.Errorf("ids[%d] = %s, want %s (creation order)", i, id, want)
		}
	}
	if p, err := r.List(ctx, "project-e00other", 10, ""); err != nil || len(p.Items) != 0 {
		t.Errorf("List other container = %v, %v; want empty", p, err)
	}
}

func TestListMalformedToken(t *testing.T) {
	t.Parallel()
	r := NewRepo(store.NewMemory())
	for _, tok := range []string{"!!!", "bm90LWEtbnVtYmVy"} { // not base64; base64 of "not-a-number"
		if _, err := r.List(t.Context(), project, 10, tok); !errs.IsKind(err, errs.KindBadRequest) {
			t.Errorf("List token %q: %v, want BadRequest", tok, err)
		}
	}
}
