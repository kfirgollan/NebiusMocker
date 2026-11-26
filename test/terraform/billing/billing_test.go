//go:build e2e

// Package billing runs the official Terraform binary with the official
// nebius/nebius provider from the Terraform Registry against NebiusMocker's
// billing service (preemptible pricing policies). Nothing is patched or forked.
//
// Run with: make e2e   (needs terraform on PATH and registry access on first run)
package billing

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	billingpb "github.com/nebius/gosdk/proto/nebius/billing/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	billingsvc "github.com/nebius/gosdk/services/nebius/billing/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	billingmod "github.com/kfirgollan/NebiusMocker/services/billing"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
)

type env struct {
	inst     *testkit.Instance
	policies billingsvc.PricingPolicyService
}

func startBilling(t *testing.T) *env {
	t.Helper()
	// A non-zero delay makes operations asynchronous, so the provider's
	// OperationService polling path is exercised too.
	inst := testkit.StartT(t, []mocker.Module{iam.New(), compute.New(), billingmod.New()},
		mockerOptions(testkit.WithOperationDelay(200*time.Millisecond), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))...)
	return &env{inst: inst, policies: billingsvc.NewPricingPolicyService(inst.SDK(t))}
}

type cfg struct {
	Proj, Name, Platform, MaxPrice string
	Labels                         map[string]string
	DataSources                    bool
}

func (c cfg) render() string {
	var labels []string
	for k, v := range c.Labels {
		labels = append(labels, fmt.Sprintf("%s = %q", k, v))
	}
	s := fmt.Sprintf(`
resource "nebius_billing_v1_pricing_policy" "p" {
  parent_id             = %q
  name                  = %q
  labels                = { %s }
  compute_instance_spec = { v1 = { platform = %q } }
  pricing               = { max_price_v1 = { max_price = %q } }
}
`, c.Proj, c.Name, strings.Join(labels, ", "), c.Platform, c.MaxPrice)
	if c.DataSources {
		s += fmt.Sprintf(`
data "nebius_billing_v1_pricing_policy" "by_id" {
  id = nebius_billing_v1_pricing_policy.p.id
}
data "nebius_billing_v1_pricing_policy" "by_name" {
  parent_id = %q
  name      = nebius_billing_v1_pricing_policy.p.name
}
`, c.Proj)
	}
	return s
}

// check verifies through the Go SDK that the mock holds what the config says.
func (e *env) check(t *testing.T, id string, c cfg, wantScheduling billingpb.PricingPolicyStatus_SchedulingState) {
	t.Helper()
	p, err := e.policies.Get(t.Context(), &billingpb.GetPricingPolicyRequest{Id: id})
	if err != nil {
		t.Fatalf("Get pricing policy %s: %v", id, err)
	}
	m, spec, st := p.GetMetadata(), p.GetSpec(), p.GetStatus()
	if m.GetParentId() != c.Proj || m.GetName() != c.Name ||
		spec.GetComputeInstanceSpec().GetV1().GetPlatform() != c.Platform ||
		spec.GetPricing().GetMaxPriceV1().GetMaxPrice() != c.MaxPrice {
		t.Fatalf("mock policy = %v, want %+v", p, c)
	}
	if len(m.GetLabels()) != len(c.Labels) {
		t.Errorf("labels = %v, want %v", m.GetLabels(), c.Labels)
	}
	for k, v := range c.Labels {
		if m.GetLabels()[k] != v {
			t.Errorf("label %s = %q, want %q", k, m.GetLabels()[k], v)
		}
	}
	if st.GetState() != billingpb.PricingPolicyStatus_STATE_ACTIVE || st.GetSchedulingState() != wantScheduling ||
		st.GetCurrency() != "USD" || st.GetSkuId() == "" {
		t.Errorf("status = %v, want ACTIVE/%v/USD/sku set", st, wantScheduling)
	}
}

func (e *env) checkGone(t *testing.T, id string) {
	t.Helper()
	if _, err := e.policies.Get(t.Context(), &billingpb.GetPricingPolicyRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Fatalf("Get pricing policy %s: %v, want NotFound", id, err)
	}
}

func TestPricingPolicyLifecycle(t *testing.T) {
	e := startBilling(t)
	tf := newTerraform(t, e.inst)
	c := cfg{Proj: e.inst.ProjectID, Name: "policyone", Labels: map[string]string{"env": "dev"}, Platform: "gpu-h100-sxm", MaxPrice: "2.500"}
	allowed := billingpb.PricingPolicyStatus_SCHEDULING_STATE_ALLOWED
	blocked := billingpb.PricingPolicyStatus_SCHEDULING_STATE_BLOCKED

	// Create.
	tf.writeConfig(t, c.render())
	tf.init(t)
	tf.apply(t)
	tf.expectNoChanges(t)
	id := tf.stateIDs(t)["p"]
	e.check(t, id, c, allowed)

	// In place update: name, labels, max_price below the spot price (1.250).
	c.Name, c.Labels, c.MaxPrice = "policytwo", map[string]string{"env": "prod", "team": "ml"}, "1.000"
	tf.writeConfig(t, c.render())
	tf.expectPlan(t, map[string]string{"p": "update"})
	tf.apply(t)
	tf.expectNoChanges(t)
	if got := tf.stateIDs(t)["p"]; got != id {
		t.Fatalf("policy recreated on update: %s -> %s", id, got)
	}
	e.check(t, id, c, blocked)

	// Raising it above the spot price unblocks scheduling again.
	c.MaxPrice = "3.000"
	tf.writeConfig(t, c.render())
	tf.apply(t)
	tf.expectNoChanges(t)
	e.check(t, id, c, allowed)

	// Replace: compute_instance_spec is RequiresReplace.
	c.Platform = "gpu-l40s-a"
	tf.writeConfig(t, c.render())
	tf.expectPlan(t, map[string]string{"p": "replace"})
	tf.apply(t)
	tf.expectNoChanges(t)
	newID := tf.stateIDs(t)["p"]
	if newID == id {
		t.Fatalf("policy id unchanged after platform change: %s", id)
	}
	e.checkGone(t, id)
	e.check(t, newID, c, allowed)

	// Destroy.
	tf.destroy(t)
	e.checkGone(t, newID)
	if len(tf.stateIDs(t)) != 0 {
		t.Fatalf("state not empty after destroy: %v", tf.stateIDs(t))
	}
}

func TestPricingPolicyDataSources(t *testing.T) {
	e := startBilling(t)
	tf := newTerraform(t, e.inst)
	c := cfg{Proj: e.inst.ProjectID, Name: "policyds", Labels: map[string]string{"env": "dev"}, Platform: "gpu-h200-sxm", MaxPrice: "5.000", DataSources: true}
	tf.writeConfig(t, c.render())
	tf.init(t)
	tf.apply(t)
	tf.expectNoChanges(t)
	e.check(t, tf.stateIDs(t)["p"], c, billingpb.PricingPolicyStatus_SCHEDULING_STATE_ALLOWED)

	attrs := tf.stateAttrs(t)
	str := func(m map[string]any, path ...string) string {
		var cur any = m
		for _, p := range path {
			mm, _ := cur.(map[string]any)
			cur = mm[p]
		}
		return fmt.Sprint(cur)
	}
	res := attrs["nebius_billing_v1_pricing_policy.p"]
	for _, d := range []string{"data.nebius_billing_v1_pricing_policy.by_id", "data.nebius_billing_v1_pricing_policy.by_name"} {
		got := attrs[d]
		if got == nil {
			t.Fatalf("%s missing from state", d)
		}
		for _, p := range [][]string{
			{"id"},
			{"name"},
			{"parent_id"},
			{"labels", "env"},
			{"compute_instance_spec", "v1", "platform"},
			{"pricing", "max_price_v1", "max_price"},
			{"status", "state"},
			{"status", "scheduling_state"},
			{"status", "currency"},
			{"status", "sku_id"},
		} {
			if str(got, p...) != str(res, p...) || str(got, p...) == "<nil>" || str(got, p...) == "" {
				t.Errorf("%s %v = %q, resource %q", d, p, str(got, p...), str(res, p...))
			}
		}
	}
	if str(res, "pricing", "max_price_v1", "max_price") != "5.000" {
		t.Errorf("max_price = %s, want 5.000", str(res, "pricing", "max_price_v1", "max_price"))
	}
}

func TestPricingPolicyOutOfRange(t *testing.T) {
	e := startBilling(t)
	tf := newTerraform(t, e.inst)
	c := cfg{Proj: e.inst.ProjectID, Name: "toohigh", Labels: map[string]string{"env": "dev"}, Platform: "gpu-h100-sxm", MaxPrice: "99.000"}
	tf.writeConfig(t, c.render())
	tf.init(t)
	before := tf.out.Len()
	err := tf.tf.Apply(t.Context())
	if err == nil {
		t.Fatalf("apply with max_price 99.000 succeeded")
	}
	out := strings.ToLower(tf.out.String()[before:] + err.Error())
	// The provider prints the API's message, not the status code name.
	if !strings.Contains(out, "99.000 is outside the allowed range") {
		t.Fatalf("apply failed without the out-of-range message:\n%s", out)
	}
	// The status code behind it, seen by the Go SDK, is OUT_OF_RANGE.
	_, err = e.policies.Create(t.Context(), &billingpb.CreatePricingPolicyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: c.Proj, Name: c.Name},
		Spec: &billingpb.PricingPolicySpec{
			ComputeInstanceSpec: &billingpb.ComputeInstanceSpec{Versions: &billingpb.ComputeInstanceSpec_V1{V1: &billingpb.ComputeInstanceSpecV1{Platform: c.Platform}}},
			Pricing:             &billingpb.PricingMethod{Versions: &billingpb.PricingMethod_MaxPriceV1{MaxPriceV1: &billingpb.MaxPriceV1{MaxPrice: c.MaxPrice}}},
		},
	})
	if status.Code(err) != codes.OutOfRange {
		t.Fatalf("SDK Create with max_price 99.000: %v, want OutOfRange", err)
	}
	if len(tf.stateIDs(t)) != 0 {
		t.Fatalf("a policy was recorded despite the failure: %v", tf.stateIDs(t))
	}
}
