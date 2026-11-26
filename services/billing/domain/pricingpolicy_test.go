package domain_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/services/billing/domain"
)

func ppSpec(platform, maxPrice string) *billingpb.PricingPolicySpec {
	return &billingpb.PricingPolicySpec{
		ComputeInstanceSpec: &billingpb.ComputeInstanceSpec{Versions: &billingpb.ComputeInstanceSpec_V1{
			V1: &billingpb.ComputeInstanceSpecV1{Platform: platform},
		}},
		Pricing: &billingpb.PricingMethod{Versions: &billingpb.PricingMethod_MaxPriceV1{
			MaxPriceV1: &billingpb.MaxPriceV1{MaxPrice: maxPrice},
		}},
	}
}

func (e *ppEnv) create(ctx context.Context, parent, name string, spec *billingpb.PricingPolicySpec) (*operations.Operation, error) {
	return e.svc.Create(ctx, domain.CreatePricingPolicyCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name}, Spec: spec,
	})
}

func (e *ppEnv) mustCreate(t *testing.T, name, maxPrice string) *billingpb.PricingPolicy {
	t.Helper()
	op, err := e.create(t.Context(), ppProject, name, ppSpec("gpu-h100-sxm", maxPrice))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p, err := e.svc.Get(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return p
}

func (e *ppEnv) update(ctx context.Context, md *commonpb.ResourceMetadata, spec *billingpb.PricingPolicySpec, rm *mask.Mask) error {
	_, err := e.svc.Update(ctx, domain.UpdatePricingPolicyCmd{Metadata: md, Spec: spec, ResetMask: rm})
	return err
}

func wantKind(t *testing.T, err error, want errs.Kind) {
	t.Helper()
	if got := errs.KindOf(err); got != want {
		t.Fatalf("error kind = %v, want %v (err: %v)", got, want, err)
	}
}

func TestPricingPolicyCreateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		parent   string
		polName  string
		spec     *billingpb.PricingPolicySpec
		platform ppPlatforms
		wantKind errs.Kind
		wantErr  string // substring of the violating field
	}{
		{name: "ok", parent: ppProject, polName: "good1", spec: ppSpec("gpu-h100-sxm", "2.500"), platform: ppPlatforms{preemptOK: true}},
		{name: "compute enabled and allows preemptibles", parent: ppProject, polName: "good1", spec: ppSpec("gpu-h100-sxm", "2.500"), platform: ppPlatforms{enabled: true, preemptOK: true}},
		{name: "other known project", parent: ppOtherProject, polName: "good1", spec: ppSpec("gpu-h100-sxm", "2.500"), platform: ppPlatforms{preemptOK: true}},
		{name: "min price", parent: ppProject, polName: "good1", spec: ppSpec("gpu-h100-sxm", "0.500"), platform: ppPlatforms{preemptOK: true}},
		{name: "max price", parent: ppProject, polName: "good1", spec: ppSpec("gpu-h100-sxm", "50.000"), platform: ppPlatforms{preemptOK: true}},
		{name: "fewer than three decimals", parent: ppProject, polName: "good1", spec: ppSpec("gpu-h100-sxm", "3"), platform: ppPlatforms{preemptOK: true}},
		{name: "no parent", polName: "n", spec: ppSpec("gpu-h100-sxm", "2.500"), wantKind: errs.KindBadRequest, wantErr: "metadata.parent_id"},
		{name: "parent not a project", parent: "tenant-e00mock000000000default", polName: "n", spec: ppSpec("gpu-h100-sxm", "2.500"), wantKind: errs.KindBadRequest, wantErr: "metadata.parent_id"},
		{name: "unknown parent", parent: "project-e00mock000000000nope", polName: "n", spec: ppSpec("gpu-h100-sxm", "2.500"), wantKind: errs.KindNotFound},
		{name: "no name", parent: ppProject, spec: ppSpec("gpu-h100-sxm", "2.500"), wantKind: errs.KindBadRequest, wantErr: "metadata.name"},
		{name: "name with dash", parent: ppProject, polName: "my-policy", spec: ppSpec("gpu-h100-sxm", "2.500"), wantKind: errs.KindBadRequest, wantErr: "metadata.name"},
		{name: "name with underscore", parent: ppProject, polName: "my_policy", spec: ppSpec("gpu-h100-sxm", "2.500"), wantKind: errs.KindBadRequest, wantErr: "metadata.name"},
		{name: "no spec", parent: ppProject, polName: "n", wantKind: errs.KindBadRequest, wantErr: "spec"},
		{name: "no compute_instance_spec", parent: ppProject, polName: "n", spec: &billingpb.PricingPolicySpec{Pricing: ppSpec("x", "1").GetPricing()}, wantKind: errs.KindBadRequest, wantErr: "spec.compute_instance_spec"},
		{name: "no v1", parent: ppProject, polName: "n", spec: &billingpb.PricingPolicySpec{ComputeInstanceSpec: &billingpb.ComputeInstanceSpec{}, Pricing: ppSpec("x", "1").GetPricing()}, wantKind: errs.KindBadRequest, wantErr: "spec.compute_instance_spec.v1"},
		{name: "no platform", parent: ppProject, polName: "n", spec: ppSpec("", "2.500"), wantKind: errs.KindBadRequest, wantErr: "platform"},
		{name: "no pricing", parent: ppProject, polName: "n", spec: &billingpb.PricingPolicySpec{ComputeInstanceSpec: ppSpec("gpu-h100-sxm", "1").GetComputeInstanceSpec()}, wantKind: errs.KindBadRequest, wantErr: "spec.pricing"},
		{name: "no max_price_v1", parent: ppProject, polName: "n", spec: &billingpb.PricingPolicySpec{ComputeInstanceSpec: ppSpec("gpu-h100-sxm", "1").GetComputeInstanceSpec(), Pricing: &billingpb.PricingMethod{}}, wantKind: errs.KindBadRequest, wantErr: "spec.pricing.max_price_v1"},
		{name: "empty max_price", parent: ppProject, polName: "n", spec: ppSpec("gpu-h100-sxm", ""), wantKind: errs.KindBadRequest, wantErr: "max_price"},
		{name: "platform without spot market", parent: ppProject, polName: "n", spec: ppSpec("cpu-d3", "2.500"), wantKind: errs.KindBadRequest, wantErr: "platform"},
		{name: "platform not in the region", parent: ppWestProject, polName: "n", spec: ppSpec("gpu-h100-sxm", "2.500"), wantKind: errs.KindBadRequest, wantErr: "platform"},
		{name: "platform not offered by compute", parent: ppProject, polName: "n", spec: ppSpec("gpu-h100-sxm", "2.500"), platform: ppPlatforms{enabled: true, preemptOK: true, notOffered: map[string]bool{"gpu-h100-sxm": true}}, wantKind: errs.KindBadRequest, wantErr: "platform"},
		{name: "platform not preemptible", parent: ppProject, polName: "n", spec: ppSpec("gpu-h100-sxm", "2.500"), platform: ppPlatforms{enabled: true}, wantKind: errs.KindBadRequest, wantErr: "platform"},
		{name: "max_price not a number", parent: ppProject, polName: "n", spec: ppSpec("gpu-h100-sxm", "abc"), wantKind: errs.KindBadRequest, wantErr: "max_price"},
		{name: "max_price negative", parent: ppProject, polName: "n", spec: ppSpec("gpu-h100-sxm", "-1.000"), wantKind: errs.KindBadRequest, wantErr: "max_price"},
		{name: "max_price four decimals", parent: ppProject, polName: "n", spec: ppSpec("gpu-h100-sxm", "2.5001"), wantKind: errs.KindBadRequest, wantErr: "max_price"},
		{name: "max_price below range", parent: ppProject, polName: "n", spec: ppSpec("gpu-h100-sxm", "0.499"), wantKind: errs.KindOutOfRange, wantErr: "max_price"},
		{name: "max_price above range", parent: ppProject, polName: "n", spec: ppSpec("gpu-h100-sxm", "50.001"), wantKind: errs.KindOutOfRange, wantErr: "max_price"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newPPEnv(t, ppWithPlatforms(tt.platform))
			op, err := e.create(t.Context(), tt.parent, tt.polName, tt.spec)
			if tt.wantKind == errs.KindUnknown {
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				if op.Err != nil {
					t.Fatalf("operation: %v", op.Err)
				}
				return
			}
			wantKind(t, err, tt.wantKind)
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not name %q", err, tt.wantErr)
			}
			if len(e.repo.items) != 0 {
				t.Errorf("a rejected create stored %d policies", len(e.repo.items))
			}
		})
	}
}

func TestPricingPolicyOutOfRangeNamesTheRange(t *testing.T) {
	t.Parallel()
	e := newPPEnv(t)
	_, err := e.create(t.Context(), ppProject, "n", ppSpec("gpu-h100-sxm", "99.000"))
	wantKind(t, err, errs.KindOutOfRange)
	for _, want := range []string{"0.5", "50"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention the bound %s", err, want)
		}
	}
}

func TestPricingPolicyUnsupportedPlatformListsSupported(t *testing.T) {
	t.Parallel()
	e := newPPEnv(t)
	_, err := e.create(t.Context(), ppWestProject, "n", ppSpec("gpu-h100-sxm", "2.500"))
	wantKind(t, err, errs.KindBadRequest)
	if !strings.Contains(err.Error(), "gpu-h200-sxm") {
		t.Errorf("error %v does not list gpu-h200-sxm (a platform with a spot market in %s)", err, ppWest)
	}
}

func TestPricingPolicyCreateStoresServerOwnedStatus(t *testing.T) {
	t.Parallel()
	e := newPPEnv(t)
	p := e.mustCreate(t, "main", "2.500")
	prices := ppDefaultPrices(t)
	cp, _, _ := prices.SpotPrice("gpu-h100-sxm", ppEast)
	st := p.GetStatus()
	if st.GetSkuId() != cp.Spot.SKU || st.GetSkuId() == "" {
		t.Errorf("sku_id = %q, want %q", st.GetSkuId(), cp.Spot.SKU)
	}
	if st.GetCurrency() != "USD" {
		t.Errorf("currency = %q, want USD", st.GetCurrency())
	}
	if st.GetRunningVmCount() != 0 {
		t.Errorf("running_vm_count = %d, want 0", st.GetRunningVmCount())
	}
	if st.GetState() != billingpb.PricingPolicyStatus_STATE_ACTIVE {
		t.Errorf("state = %v, want ACTIVE", st.GetState())
	}
	if st.GetSchedulingState() != billingpb.PricingPolicyStatus_SCHEDULING_STATE_ALLOWED {
		t.Errorf("scheduling_state = %v, want ALLOWED", st.GetSchedulingState())
	}
	md := p.GetMetadata()
	if md.GetResourceVersion() != 1 || md.GetParentId() != ppProject || md.GetName() != "main" || md.GetCreatedAt() == nil {
		t.Errorf("metadata = %v", md)
	}
	if got := p.GetSpec().GetPricing().GetMaxPriceV1().GetMaxPrice(); got != "2.500" {
		t.Errorf("max_price = %q, want the string as given", got)
	}
}

func TestPricingPolicyCreateIsCreatingUntilTheOperationCompletes(t *testing.T) {
	t.Parallel()
	e := newPPEnv(t)
	e.ops.hold = true
	op, err := e.create(t.Context(), ppProject, "main", ppSpec("gpu-h100-sxm", "2.500"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p, err := e.svc.Get(t.Context(), op.ResourceID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p.GetStatus().GetState() != billingpb.PricingPolicyStatus_STATE_CREATING {
		t.Errorf("state while the operation runs = %v, want CREATING", p.GetStatus().GetState())
	}
	e.ops.finish(t)
	if p, _ = e.svc.Get(t.Context(), op.ResourceID); p.GetStatus().GetState() != billingpb.PricingPolicyStatus_STATE_ACTIVE {
		t.Errorf("state after the operation = %v, want ACTIVE", p.GetStatus().GetState())
	}
}

func TestPricingPolicyCreateRejectsServerOwnedMetadata(t *testing.T) {
	t.Parallel()
	e := newPPEnv(t)
	for name, md := range map[string]*commonpb.ResourceMetadata{
		"id":               {ParentId: ppProject, Name: "n", Id: "pricingpolicy-x"},
		"resource_version": {ParentId: ppProject, Name: "n", ResourceVersion: 3},
	} {
		_, err := e.svc.Create(t.Context(), domain.CreatePricingPolicyCmd{Metadata: md, Spec: ppSpec("gpu-h100-sxm", "2.500")})
		if errs.KindOf(err) != errs.KindBadRequest {
			t.Errorf("%s: kind = %v, want BadRequest (err: %v)", name, errs.KindOf(err), err)
		}
	}
}

func TestPricingPolicyNameUniquePerProject(t *testing.T) {
	t.Parallel()
	e := newPPEnv(t)
	e.mustCreate(t, "main", "2.500")
	_, err := e.create(t.Context(), ppProject, "main", ppSpec("gpu-h100-sxm", "3.000"))
	wantKind(t, err, errs.KindAlreadyExists)
	if _, err := e.create(t.Context(), ppOtherProject, "main", ppSpec("gpu-h100-sxm", "3.000")); err != nil {
		t.Errorf("same name in another project: %v", err)
	}
	other := e.mustCreate(t, "second", "3.000")
	err = e.update(t.Context(), &commonpb.ResourceMetadata{Id: other.GetMetadata().GetId(), Name: "main"}, nil, nil)
	wantKind(t, err, errs.KindAlreadyExists)
}

func TestPricingPolicySchedulingState(t *testing.T) {
	t.Parallel()
	custom, err := domain.ParsePriceList([]byte(`
currency: usd
compute:
  - platform: gpu-h100-sxm
    product_title: "H100"
    gpu_hour: "2.95"
    preemptible:
      product_title: "Preemptible H100"
      min_price: "0.500"
      max_price: "50.000"
      spot: { eu-north1: "5.000" }
`), []string{ppEast}, "e00")
	if err != nil {
		t.Fatalf("ParsePriceList: %v", err)
	}
	tests := []struct {
		name     string
		prices   *domain.PriceList
		maxPrice string
		want     billingpb.PricingPolicyStatus_SchedulingState
	}{
		{"default list, above spot", nil, "1.251", billingpb.PricingPolicyStatus_SCHEDULING_STATE_ALLOWED},
		{"default list, equal to spot", nil, "1.250", billingpb.PricingPolicyStatus_SCHEDULING_STATE_ALLOWED},
		{"default list, below spot", nil, "1.249", billingpb.PricingPolicyStatus_SCHEDULING_STATE_BLOCKED},
		{"custom list, below spot 5", custom, "4.999", billingpb.PricingPolicyStatus_SCHEDULING_STATE_BLOCKED},
		{"custom list, at spot 5", custom, "5", billingpb.PricingPolicyStatus_SCHEDULING_STATE_ALLOWED},
		{"custom list, above spot 5", custom, "7.500", billingpb.PricingPolicyStatus_SCHEDULING_STATE_ALLOWED},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var opts []ppOption
			if tt.prices != nil {
				opts = append(opts, ppWithPrices(tt.prices))
			}
			e := newPPEnv(t, opts...)
			p := e.mustCreate(t, "pol", tt.maxPrice)
			if got := p.GetStatus().GetSchedulingState(); got != tt.want {
				t.Errorf("Get scheduling_state = %v, want %v", got, tt.want)
			}
			byName, err := e.svc.GetByName(t.Context(), ppProject, "pol")
			if err != nil || byName.GetStatus().GetSchedulingState() != tt.want {
				t.Errorf("GetByName scheduling_state = %v, %v, want %v", byName.GetStatus().GetSchedulingState(), err, tt.want)
			}
			page, err := e.svc.List(t.Context(), ppProject, 0, "")
			if err != nil || len(page.Items) != 1 || page.Items[0].GetStatus().GetSchedulingState() != tt.want {
				t.Errorf("List = %v, %v, want scheduling_state %v", page, err, tt.want)
			}
		})
	}
}

func TestPricingPolicyGetMissing(t *testing.T) {
	t.Parallel()
	e := newPPEnv(t)
	_, err := e.svc.Get(t.Context(), "pricingpolicy-nope")
	wantKind(t, err, errs.KindNotFound)
	_, err = e.svc.GetByName(t.Context(), ppProject, "nope")
	wantKind(t, err, errs.KindNotFound)
}

func TestPricingPolicyList(t *testing.T) {
	t.Parallel()
	e := newPPEnv(t)
	for _, n := range []string{"a", "b", "c"} {
		e.mustCreate(t, n, "2.000")
	}
	page, err := e.svc.List(t.Context(), ppProject, 0, "")
	if err != nil || len(page.Items) != 3 || page.Items[0].GetMetadata().GetName() != "a" {
		t.Errorf("List = %v, %v, want a,b,c", page, err)
	}
	if _, err := e.svc.List(t.Context(), "", 0, ""); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("List without parent kind = %v, want BadRequest", errs.KindOf(err))
	}
	if _, err := e.svc.List(t.Context(), ppProject, 1001, ""); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("List page_size 1001 kind = %v, want BadRequest", errs.KindOf(err))
	}
	if _, err := e.svc.List(t.Context(), ppProject, -1, ""); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("List page_size -1 kind = %v, want BadRequest", errs.KindOf(err))
	}
}

func TestPricingPolicyUpdate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		md       func(id string) *commonpb.ResourceMetadata
		spec     *billingpb.PricingPolicySpec
		mask     *mask.Mask
		wantKind errs.Kind
		check    func(t *testing.T, p *billingpb.PricingPolicy)
	}{
		{
			name: "rename keeps ACTIVE and bumps version",
			md:   func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id, Name: "renamed"} },
			check: func(t *testing.T, p *billingpb.PricingPolicy) {
				t.Helper()
				if p.GetMetadata().GetName() != "renamed" || p.GetMetadata().GetResourceVersion() != 2 {
					t.Errorf("metadata = %v", p.GetMetadata())
				}
				if p.GetSpec().GetPricing().GetMaxPriceV1().GetMaxPrice() != "2.500" {
					t.Errorf("a partial update lost max_price: %v", p.GetSpec())
				}
			},
		},
		{
			name: "labels only",
			md: func(id string) *commonpb.ResourceMetadata {
				return &commonpb.ResourceMetadata{Id: id, Labels: map[string]string{"team": "ml"}}
			},
			check: func(t *testing.T, p *billingpb.PricingPolicy) {
				t.Helper()
				if p.GetMetadata().GetLabels()["team"] != "ml" || p.GetMetadata().GetName() != "main" {
					t.Errorf("metadata = %v", p.GetMetadata())
				}
			},
		},
		{
			name: "change max_price with the full spec",
			md:   func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id, Name: "main"} },
			spec: ppSpec("gpu-h100-sxm", "4.000"),
			check: func(t *testing.T, p *billingpb.PricingPolicy) {
				t.Helper()
				if got := p.GetSpec().GetPricing().GetMaxPriceV1().GetMaxPrice(); got != "4.000" {
					t.Errorf("max_price = %q", got)
				}
				if p.GetStatus().GetState() != billingpb.PricingPolicyStatus_STATE_ACTIVE {
					t.Errorf("state after the operation = %v, want ACTIVE", p.GetStatus().GetState())
				}
			},
		},
		{
			name: "change max_price in patch mode (price only)",
			md:   func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id} },
			spec: &billingpb.PricingPolicySpec{Pricing: ppSpec("", "9.000").GetPricing()},
			check: func(t *testing.T, p *billingpb.PricingPolicy) {
				t.Helper()
				if got := p.GetSpec().GetPricing().GetMaxPriceV1().GetMaxPrice(); got != "9.000" {
					t.Errorf("max_price = %q", got)
				}
				if p.GetSpec().GetComputeInstanceSpec().GetV1().GetPlatform() != "gpu-h100-sxm" {
					t.Errorf("patch lost the platform: %v", p.GetSpec())
				}
			},
		},
		{
			name: "unchanged full spec is a no-op",
			md:   func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id, Name: "main"} },
			spec: ppSpec("gpu-h100-sxm", "2.500"),
			check: func(t *testing.T, p *billingpb.PricingPolicy) {
				t.Helper()
				if p.GetMetadata().GetResourceVersion() != 1 {
					t.Errorf("version = %d, want 1 (nothing changed)", p.GetMetadata().GetResourceVersion())
				}
			},
		},
		{
			name:     "platform is immutable",
			md:       func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id} },
			spec:     ppSpec("gpu-h200-sxm", "2.500"),
			wantKind: errs.KindBadRequest,
		},
		{
			name:     "max_price out of range",
			md:       func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id} },
			spec:     ppSpec("gpu-h100-sxm", "0.100"),
			wantKind: errs.KindOutOfRange,
		},
		{
			name:     "bad name",
			md:       func(id string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: id, Name: "a-b"} },
			wantKind: errs.KindBadRequest,
		},
		{
			name: "parent is immutable",
			md: func(id string) *commonpb.ResourceMetadata {
				return &commonpb.ResourceMetadata{Id: id, ParentId: ppOtherProject}
			},
			wantKind: errs.KindBadRequest,
		},
		{
			name: "stale resource_version",
			md: func(id string) *commonpb.ResourceMetadata {
				return &commonpb.ResourceMetadata{Id: id, ResourceVersion: 7}
			},
			wantKind: errs.KindBadState,
		},
		{
			name:     "missing policy",
			md:       func(string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{Id: "pricingpolicy-nope"} },
			wantKind: errs.KindNotFound,
		},
		{
			name:     "no id",
			md:       func(string) *commonpb.ResourceMetadata { return &commonpb.ResourceMetadata{} },
			wantKind: errs.KindBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newPPEnv(t)
			p := e.mustCreate(t, "main", "2.500")
			id := p.GetMetadata().GetId()
			e.clk.Advance(time.Hour)
			err := e.update(t.Context(), tt.md(id), tt.spec, tt.mask)
			if tt.wantKind != errs.KindUnknown {
				wantKind(t, err, tt.wantKind)
				return
			}
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			got, err := e.svc.Get(t.Context(), id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			tt.check(t, got)
		})
	}
}

func TestPricingPolicyUpdateStatesAndImmutableStatus(t *testing.T) {
	t.Parallel()
	e := newPPEnv(t)
	p := e.mustCreate(t, "main", "2.500")
	id := p.GetMetadata().GetId()

	e.ops.hold = true
	if err := e.update(t.Context(), &commonpb.ResourceMetadata{Id: id}, &billingpb.PricingPolicySpec{Pricing: ppSpec("", "3.000").GetPricing()}, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := e.svc.Get(t.Context(), id)
	if got.GetStatus().GetState() != billingpb.PricingPolicyStatus_STATE_UPDATING {
		t.Errorf("state while updating = %v, want UPDATING", got.GetStatus().GetState())
	}
	e.ops.finish(t)
	got, _ = e.svc.Get(t.Context(), id)
	if got.GetStatus().GetState() != billingpb.PricingPolicyStatus_STATE_ACTIVE {
		t.Errorf("state after the update = %v, want ACTIVE", got.GetStatus().GetState())
	}

	// A rename never leaves ACTIVE.
	if err := e.update(t.Context(), &commonpb.ResourceMetadata{Id: id, Name: "again"}, nil, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = e.svc.Get(t.Context(), id)
	if got.GetStatus().GetState() != billingpb.PricingPolicyStatus_STATE_ACTIVE {
		t.Errorf("rename: state = %v, want ACTIVE", got.GetStatus().GetState())
	}
}

func TestPricingPolicyUpdateBlockedByRunningVMs(t *testing.T) {
	t.Parallel()
	e := newPPEnv(t)
	p := e.mustCreate(t, "main", "2.500")
	id := p.GetMetadata().GetId()
	e.vms.set(id, 2)

	err := e.update(t.Context(), &commonpb.ResourceMetadata{Id: id}, &billingpb.PricingPolicySpec{Pricing: ppSpec("", "3.000").GetPricing()}, nil)
	wantKind(t, err, errs.KindBadState)
	// The name may change any time.
	if err := e.update(t.Context(), &commonpb.ResourceMetadata{Id: id, Name: "renamed"}, nil, nil); err != nil {
		t.Errorf("rename with running VMs: %v", err)
	}
	// running_vm_count is what compute counts, on every read.
	got, _ := e.svc.Get(t.Context(), id)
	if got.GetStatus().GetRunningVmCount() != 2 {
		t.Errorf("running_vm_count = %d, want 2", got.GetStatus().GetRunningVmCount())
	}
	// Deleting is blocked too.
	_, err = e.svc.Delete(t.Context(), id, nil)
	wantKind(t, err, errs.KindBadState)
	// Once the VMs stop, max_price may change and the policy may go.
	e.vms.set(id, 0)
	if err := e.update(t.Context(), &commonpb.ResourceMetadata{Id: id}, &billingpb.PricingPolicySpec{Pricing: ppSpec("", "3.000").GetPricing()}, nil); err != nil {
		t.Errorf("max_price change with no running VM: %v", err)
	}
	if _, err := e.svc.Delete(t.Context(), id, nil); err != nil {
		t.Errorf("Delete with no running VM: %v", err)
	}
}

func TestPricingPolicyLookupForCompute(t *testing.T) {
	t.Parallel()
	e := newPPEnv(t)
	p := e.mustCreate(t, "main", "1.000") // below the spot price 1.250: BLOCKED
	id := p.GetMetadata().GetId()
	e.vms.set(id, 3)
	got, err := e.svc.Lookup(t.Context(), id)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if st := got.GetStatus(); st.GetSchedulingState() != billingpb.PricingPolicyStatus_SCHEDULING_STATE_BLOCKED || st.GetRunningVmCount() != 0 {
		t.Errorf("Lookup status = %v, want BLOCKED without a VM count", st)
	}
	_, err = e.svc.Lookup(t.Context(), "pricingpolicy-e00nope")
	wantKind(t, err, errs.KindNotFound)
}

func TestPricingPolicyResetMaskClearsRequiredField(t *testing.T) {
	t.Parallel()
	e := newPPEnv(t)
	p := e.mustCreate(t, "main", "2.500")
	rm, err := mask.Parse("spec.pricing")
	if err != nil {
		t.Fatalf("mask.Parse: %v", err)
	}
	err = e.update(t.Context(), &commonpb.ResourceMetadata{Id: p.GetMetadata().GetId()}, nil, rm)
	wantKind(t, err, errs.KindBadRequest)
}

func TestPricingPolicyDelete(t *testing.T) {
	t.Parallel()
	e := newPPEnv(t)
	p := e.mustCreate(t, "main", "2.500")
	id := p.GetMetadata().GetId()

	e.ops.hold = true
	op, err := e.svc.Delete(t.Context(), id, nil)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if op.ResourceID != id {
		t.Errorf("operation resource = %q, want %q", op.ResourceID, id)
	}
	got, err := e.svc.Get(t.Context(), id)
	if err != nil || got.GetStatus().GetState() != billingpb.PricingPolicyStatus_STATE_DELETING {
		t.Fatalf("state while deleting = %v, %v, want DELETING", got.GetStatus().GetState(), err)
	}
	e.ops.finish(t)
	_, err = e.svc.Get(t.Context(), id)
	wantKind(t, err, errs.KindNotFound)

	_, err = e.svc.Delete(t.Context(), id, nil)
	wantKind(t, err, errs.KindNotFound)
	// The name is free again.
	if _, err := e.create(t.Context(), ppProject, "main", ppSpec("gpu-h100-sxm", "2.500")); err != nil {
		t.Errorf("re-create after delete: %v", err)
	}
}

func TestNewPricingPolicyServiceRequiresDeps(t *testing.T) {
	t.Parallel()
	if _, err := domain.NewPricingPolicyService(domain.PricingPolicyDeps{}); err == nil {
		t.Fatal("NewPricingPolicyService with no dependencies succeeded")
	}
}
