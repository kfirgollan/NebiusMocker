package dal_test

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/billing/dal"
)

const (
	project  = "project-e00mock000000000default"
	project2 = "project-e00mock000000000other"
)

func policy(id, parent, name, maxPrice string) *billingpb.PricingPolicy {
	return &billingpb.PricingPolicy{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: parent, Name: name, ResourceVersion: 1},
		Spec: &billingpb.PricingPolicySpec{
			ComputeInstanceSpec: &billingpb.ComputeInstanceSpec{Versions: &billingpb.ComputeInstanceSpec_V1{
				V1: &billingpb.ComputeInstanceSpecV1{Platform: "gpu-h100-sxm"},
			}},
			Pricing: &billingpb.PricingMethod{Versions: &billingpb.PricingMethod_MaxPriceV1{
				MaxPriceV1: &billingpb.MaxPriceV1{MaxPrice: maxPrice},
			}},
		},
		Status: &billingpb.PricingPolicyStatus{State: billingpb.PricingPolicyStatus_STATE_ACTIVE, Currency: "USD"},
	}
}

func TestPricingPolicyRepoCRUD(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewPricingPolicyRepo(store.NewMemory())
	p := policy("pricingpolicy-1", project, "main", "2.500")
	if err := r.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := r.Get(ctx, "pricingpolicy-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(p, got, protocmp.Transform()); diff != "" {
		t.Errorf("Get mismatch (-want +got):\n%s", diff)
	}
	byName, err := r.GetByName(ctx, project, "main")
	if err != nil || byName.GetMetadata().GetId() != "pricingpolicy-1" {
		t.Errorf("GetByName = %v, %v, want pricingpolicy-1", byName, err)
	}
	if _, err := r.GetByName(ctx, project2, "main"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetByName(other project) kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Create(ctx, p); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("Create(same id) kind = %v, want AlreadyExists (err: %v)", errs.KindOf(err), err)
	}

	p.Spec.Pricing.GetMaxPriceV1().MaxPrice = "3.000"
	if err := r.Update(ctx, p); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, _ = r.Get(ctx, "pricingpolicy-1"); got.GetSpec().GetPricing().GetMaxPriceV1().GetMaxPrice() != "3.000" {
		t.Errorf("max_price after update = %q, want 3.000", got.GetSpec().GetPricing().GetMaxPriceV1().GetMaxPrice())
	}
	if err := r.Update(ctx, policy("pricingpolicy-x", project, "x", "1.000")); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Update(missing) kind = %v, want NotFound (err: %v)", errs.KindOf(err), err)
	}

	if err := r.Delete(ctx, "pricingpolicy-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, "pricingpolicy-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Get(deleted) kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := r.Delete(ctx, "pricingpolicy-1"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("Delete(missing) kind = %v, want NotFound", errs.KindOf(err))
	}
}

func TestPricingPolicyNameUniquePerProject(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewPricingPolicyRepo(store.NewMemory())
	if err := r.Create(ctx, policy("pricingpolicy-1", project, "main", "1.000")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Create(ctx, policy("pricingpolicy-2", project, "main", "1.000")); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Fatalf("Create(same name) kind = %v, want AlreadyExists (err: %v)", errs.KindOf(err), err)
	}
	if err := r.Create(ctx, policy("pricingpolicy-3", project2, "main", "1.000")); err != nil {
		t.Errorf("Create(same name, other project): %v", err)
	}
	if err := r.Create(ctx, policy("pricingpolicy-4", project, "other", "1.000")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Update(ctx, policy("pricingpolicy-4", project, "main", "1.000")); errs.KindOf(err) != errs.KindAlreadyExists {
		t.Errorf("Update(to a taken name) kind = %v, want AlreadyExists (err: %v)", errs.KindOf(err), err)
	}
	if err := r.Update(ctx, policy("pricingpolicy-4", project, "free", "1.000")); err != nil {
		t.Errorf("Update(to a free name): %v", err)
	}
}

func TestPricingPolicyListPaging(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := dal.NewPricingPolicyRepo(store.NewMemory())
	for i := range 5 {
		if err := r.Create(ctx, policy(fmt.Sprintf("pricingpolicy-%d", i), project, fmt.Sprintf("p%d", i), "1.000")); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	if err := r.Create(ctx, policy("pricingpolicy-o", project2, "o", "1.000")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var names []string
	token := ""
	pages := 0
	for {
		page, err := r.List(ctx, project, 2, token)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		pages++
		for _, p := range page.Items {
			names = append(names, p.GetMetadata().GetName())
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	want := []string{"p0", "p1", "p2", "p3", "p4"}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Errorf("listed names (-want +got):\n%s", diff)
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3", pages)
	}
	empty, err := r.List(ctx, "project-e00mock000000000none", 10, "")
	if err != nil || len(empty.Items) != 0 {
		t.Errorf("List(empty project) = %v, %v", empty, err)
	}
	if _, err := r.List(ctx, project, 2, "!!"); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("List(bad token) kind = %v, want BadRequest", errs.KindOf(err))
	}
}
