package contract

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
)

func TestBillingPricingPolicyCRUD(t *testing.T) {
	t.Parallel()
	e := startBilling(t)
	ctx := t.Context()
	p := e.createPolicy(t, "main", "2.500")
	id := p.GetMetadata().GetId()

	if !strings.HasPrefix(id, "pricingpolicy-") || p.GetMetadata().GetParentId() != e.inst.ProjectID || p.GetMetadata().GetName() != "main" {
		t.Errorf("metadata = %v", p.GetMetadata())
	}
	if got := p.GetSpec().GetPricing().GetMaxPriceV1().GetMaxPrice(); got != "2.500" {
		t.Errorf("max_price = %q, want it exactly as sent", got)
	}
	if got := p.GetSpec().GetComputeInstanceSpec().GetV1().GetPlatform(); got != "gpu-h100-sxm" {
		t.Errorf("platform = %q", got)
	}
	st := p.GetStatus()
	if st.GetState() != billingpb.PricingPolicyStatus_STATE_ACTIVE ||
		st.GetSchedulingState() != billingpb.PricingPolicyStatus_SCHEDULING_STATE_ALLOWED ||
		st.GetCurrency() != "USD" || st.GetRunningVmCount() != 0 || !strings.HasPrefix(st.GetSkuId(), "sku-") {
		t.Errorf("status = %v", st)
	}

	byName, err := e.policies.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: e.inst.ProjectID, Name: "main"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Fatalf("GetByName = %v, %v", byName, err)
	}
	_, err = e.policies.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: e.inst.ProjectID, Name: "nope"})
	kmsWantCode(t, err, codes.NotFound)
	_, err = e.policies.Get(ctx, &billingpb.GetPricingPolicyRequest{Id: "pricingpolicy-doesnotexist"})
	kmsWantCode(t, err, codes.NotFound)

	op, err := e.policies.Delete(ctx, &billingpb.DeletePricingPolicyRequest{Id: id})
	kmsMustWait(t, op, err)
	_, err = e.policies.Get(ctx, &billingpb.GetPricingPolicyRequest{Id: id})
	kmsWantCode(t, err, codes.NotFound)
	op, err = e.policies.Delete(ctx, &billingpb.DeletePricingPolicyRequest{Id: id})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.NotFound)
}

func TestBillingPricingPolicyList(t *testing.T) {
	t.Parallel()
	e := startBilling(t)
	ctx := t.Context()
	want := map[string]bool{}
	for _, n := range []string{"alpha", "beta", "gamma"} {
		want[e.createPolicy(t, n, "2.000").GetMetadata().GetId()] = true
	}
	got := map[string]bool{}
	token, pages := "", 0
	for {
		page, err := e.policies.List(ctx, &billingpb.ListPricingPoliciesRequest{ParentId: e.inst.ProjectID, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.GetItems()) > 2 {
			t.Fatalf("page has %d items, want <= 2", len(page.GetItems()))
		}
		for _, p := range page.GetItems() {
			got[p.GetMetadata().GetId()] = true
		}
		pages++
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	if pages != 2 || len(got) != 3 {
		t.Errorf("pages = %d, policies = %d, want 2 and 3", pages, len(got))
	}
	for id := range want {
		if !got[id] {
			t.Errorf("policy %s missing from the listing", id)
		}
	}
	_, err := e.policies.List(ctx, &billingpb.ListPricingPoliciesRequest{})
	kmsWantCode(t, err, codes.InvalidArgument)
	_, err = e.policies.List(ctx, &billingpb.ListPricingPoliciesRequest{ParentId: e.inst.ProjectID, PageSize: 5000})
	kmsWantCode(t, err, codes.InvalidArgument)
}

func TestBillingPricingPolicyCreateErrors(t *testing.T) {
	t.Parallel()
	e := startBilling(t)
	e.createPolicy(t, "taken", "2.000")
	proj := e.inst.ProjectID
	withSpec := func(name string, spec *billingpb.PricingPolicySpec) *billingpb.CreatePricingPolicyRequest {
		return &billingpb.CreatePricingPolicyRequest{Metadata: &commonpb.ResourceMetadata{ParentId: proj, Name: name}, Spec: spec}
	}
	tests := []struct {
		name string
		req  *billingpb.CreatePricingPolicyRequest
		code codes.Code
		want string // substring of the error message
	}{
		{"duplicate name", createPolicyRequest(proj, "taken", "2.000"), codes.AlreadyExists, ""},
		{"name with a dash", createPolicyRequest(proj, "my-policy", "2.000"), codes.InvalidArgument, "name"},
		{"no name", createPolicyRequest(proj, "", "2.000"), codes.InvalidArgument, "name"},
		{"no parent", createPolicyRequest("", "x", "2.000"), codes.InvalidArgument, "parent_id"},
		{"unknown parent", createPolicyRequest("project-e00mock000000000nope", "x", "2.000"), codes.NotFound, ""},
		{"no spec", &billingpb.CreatePricingPolicyRequest{Metadata: &commonpb.ResourceMetadata{ParentId: proj, Name: "x"}}, codes.InvalidArgument, "spec"},
		{"no platform", withSpec("x", policySpec("", "2.000")), codes.InvalidArgument, "platform"},
		{"platform without a spot market", withSpec("x", policySpec("cpu-d3", "2.000")), codes.InvalidArgument, "gpu-h100-sxm"},
		{"no max_price", withSpec("x", policySpec("gpu-h100-sxm", "")), codes.InvalidArgument, "max_price"},
		{"malformed max_price", withSpec("x", policySpec("gpu-h100-sxm", "cheap")), codes.InvalidArgument, "max_price"},
		{"four decimals", withSpec("x", policySpec("gpu-h100-sxm", "2.0001")), codes.InvalidArgument, "max_price"},
		{"below the range", withSpec("x", policySpec("gpu-h100-sxm", "0.499")), codes.OutOfRange, "0.5"},
		{"above the range", withSpec("x", policySpec("gpu-h100-sxm", "50.001")), codes.OutOfRange, "50"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			op, err := e.policies.Create(t.Context(), tt.req)
			err = kmsWait(t.Context(), op, err)
			kmsWantCode(t, err, tt.code)
			if tt.want != "" && !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
	list, err := e.policies.List(t.Context(), &billingpb.ListPricingPoliciesRequest{ParentId: proj})
	if err != nil || len(list.GetItems()) != 1 {
		t.Errorf("List after rejected creates = %v, %v, want only \"taken\"", list, err)
	}
}

func TestBillingPricingPolicyBlockedUntilThePriceIsReached(t *testing.T) {
	t.Parallel()
	e := startBilling(t)
	// The spot price of gpu-h100-sxm in eu-north1 is 1.250.
	for name, tc := range map[string]struct {
		maxPrice string
		want     billingpb.PricingPolicyStatus_SchedulingState
	}{
		"below": {"1.249", billingpb.PricingPolicyStatus_SCHEDULING_STATE_BLOCKED},
		"equal": {"1.250", billingpb.PricingPolicyStatus_SCHEDULING_STATE_ALLOWED},
		"above": {"3.000", billingpb.PricingPolicyStatus_SCHEDULING_STATE_ALLOWED},
	} {
		p := e.createPolicy(t, name, tc.maxPrice)
		if got := p.GetStatus().GetSchedulingState(); got != tc.want {
			t.Errorf("%s: scheduling_state = %v, want %v", name, got, tc.want)
		}
		if p.GetStatus().GetState() != billingpb.PricingPolicyStatus_STATE_ACTIVE {
			t.Errorf("%s: state = %v, want ACTIVE (a blocked policy is accepted)", name, p.GetStatus().GetState())
		}
	}
	list, err := e.policies.List(t.Context(), &billingpb.ListPricingPoliciesRequest{ParentId: e.inst.ProjectID})
	if err != nil || len(list.GetItems()) != 3 {
		t.Fatalf("List = %v, %v", list, err)
	}
	for _, p := range list.GetItems() {
		blocked := p.GetStatus().GetSchedulingState() == billingpb.PricingPolicyStatus_SCHEDULING_STATE_BLOCKED
		if blocked != (p.GetMetadata().GetName() == "below") {
			t.Errorf("List: %s scheduling_state = %v", p.GetMetadata().GetName(), p.GetStatus().GetSchedulingState())
		}
	}
}

func TestBillingPricingPolicyUpdate(t *testing.T) {
	t.Parallel()
	e := startBilling(t)
	ctx := t.Context()
	p := e.createPolicy(t, "upd", "2.500")
	id := p.GetMetadata().GetId()
	proj := e.inst.ProjectID

	update := func(md *commonpb.ResourceMetadata, spec *billingpb.PricingPolicySpec) error {
		op, err := e.policies.Update(ctx, &billingpb.UpdatePricingPolicyRequest{Metadata: md, Spec: spec})
		return kmsWait(ctx, op, err)
	}
	md := func() *commonpb.ResourceMetadata {
		return &commonpb.ResourceMetadata{Id: id, ParentId: proj, Name: "upd"}
	}
	get := func() *billingpb.PricingPolicy {
		got, err := e.policies.Get(ctx, &billingpb.GetPricingPolicyRequest{Id: id})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return got
	}

	// An unchanged update keeps the resource version.
	if err := update(md(), policySpec("gpu-h100-sxm", "2.500")); err != nil {
		t.Fatalf("unchanged update: %v", err)
	}
	if got := get().GetMetadata().GetResourceVersion(); got != p.GetMetadata().GetResourceVersion() {
		t.Errorf("resource_version %d -> %d after an unchanged update", p.GetMetadata().GetResourceVersion(), got)
	}

	// Name and labels.
	m := md()
	m.Name, m.Labels = "renamed", map[string]string{"env": "dev"}
	if err := update(m, policySpec("gpu-h100-sxm", "2.500")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got := get()
	if got.GetMetadata().GetName() != "renamed" || got.GetMetadata().GetLabels()["env"] != "dev" || got.GetMetadata().GetResourceVersion() != 2 {
		t.Errorf("metadata = %v", got.GetMetadata())
	}

	// max_price, exactly as sent, with the scheduling state recomputed.
	m = md()
	m.Name = "renamed"
	if err := update(m, policySpec("gpu-h100-sxm", "1.000")); err != nil {
		t.Fatalf("max_price update: %v", err)
	}
	got = get()
	if got.GetSpec().GetPricing().GetMaxPriceV1().GetMaxPrice() != "1.000" ||
		got.GetStatus().GetSchedulingState() != billingpb.PricingPolicyStatus_SCHEDULING_STATE_BLOCKED ||
		got.GetStatus().GetState() != billingpb.PricingPolicyStatus_STATE_ACTIVE ||
		got.GetMetadata().GetResourceVersion() != 3 {
		t.Errorf("after max_price update: %v", got)
	}

	// Patch mode (what `nebius billing pricing-policy update --name x` sends):
	// an empty reset mask and only the changed fields.
	patchCtx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.New())
	if err != nil {
		t.Fatalf("reset mask: %v", err)
	}
	op, err := e.policies.Update(patchCtx, &billingpb.UpdatePricingPolicyRequest{Metadata: &commonpb.ResourceMetadata{Id: id, Name: "patched"}})
	kmsMustWait(t, op, err)
	got = get()
	if got.GetMetadata().GetName() != "patched" || got.GetSpec().GetPricing().GetMaxPriceV1().GetMaxPrice() != "1.000" ||
		got.GetSpec().GetComputeInstanceSpec().GetV1().GetPlatform() != "gpu-h100-sxm" {
		t.Errorf("after patch rename: %v", got)
	}
	op, err = e.policies.Update(patchCtx, &billingpb.UpdatePricingPolicyRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id},
		Spec:     &billingpb.PricingPolicySpec{Pricing: policySpec("", "7.250").GetPricing()},
	})
	kmsMustWait(t, op, err)
	if got = get(); got.GetSpec().GetPricing().GetMaxPriceV1().GetMaxPrice() != "7.250" || got.GetMetadata().GetName() != "patched" {
		t.Errorf("after patch max_price: %v", got)
	}

	// Rejections.
	m = md()
	m.Name = "patched"
	kmsWantCode(t, update(m, policySpec("gpu-h200-sxm", "7.250")), codes.InvalidArgument) // platform is immutable
	kmsWantCode(t, update(m, policySpec("gpu-h100-sxm", "0.100")), codes.OutOfRange)
	kmsWantCode(t, update(m, policySpec("gpu-h100-sxm", "1.23456")), codes.InvalidArgument)
	m.Name = "bad-name"
	kmsWantCode(t, update(m, policySpec("gpu-h100-sxm", "7.250")), codes.InvalidArgument)
	m = md()
	m.Name, m.ParentId = "patched", "project-e00mock000000000other"
	kmsWantCode(t, update(m, policySpec("gpu-h100-sxm", "7.250")), codes.InvalidArgument) // parent is immutable
	m = md()
	m.Name, m.ResourceVersion = "patched", 99
	kmsWantCode(t, update(m, policySpec("gpu-h100-sxm", "7.250")), codes.FailedPrecondition)
	kmsWantCode(t, update(&commonpb.ResourceMetadata{Id: "pricingpolicy-doesnotexist", Name: "x"}, policySpec("gpu-h100-sxm", "2.000")), codes.NotFound)

	// A name taken by another policy.
	e.createPolicy(t, "other", "2.000")
	m = md()
	m.Name = "other"
	kmsWantCode(t, update(m, policySpec("gpu-h100-sxm", "7.250")), codes.AlreadyExists)
	if got = get(); got.GetMetadata().GetName() != "patched" {
		t.Errorf("name after rejected updates = %q, want patched", got.GetMetadata().GetName())
	}
}

func TestBillingPricingPolicyStatusIsServerOwned(t *testing.T) {
	t.Parallel()
	e := startBilling(t)
	ctx := t.Context()
	// Nothing in a request can set the status; the create request has none.
	p := e.createPolicy(t, "owned", "2.500")
	id := p.GetMetadata().GetId()
	op, err := e.policies.Update(ctx, &billingpb.UpdatePricingPolicyRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: "owned"}, Spec: policySpec("gpu-h100-sxm", "2.500"),
	})
	kmsMustWait(t, op, err)
	got, err := e.policies.Get(ctx, &billingpb.GetPricingPolicyRequest{Id: id})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetStatus().GetSkuId() != p.GetStatus().GetSkuId() || got.GetStatus().GetCurrency() != "USD" || got.GetStatus().GetRunningVmCount() != 0 {
		t.Errorf("status changed by an update: %v -> %v", p.GetStatus(), got.GetStatus())
	}
}
