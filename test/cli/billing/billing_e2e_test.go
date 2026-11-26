//go:build e2e

// Package billing runs the official nebius CLI against NebiusMocker through
// the billing flows the Nebius docs show: managing preemptible pricing
// policies (https://docs.nebius.com/signup-billing/manage-pricing-policy),
// reading spot prices and estimating costs with the calculator.
package billing

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	billingmod "github.com/kfirgollan/NebiusMocker/services/billing"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

type policy struct {
	Metadata struct {
		ID       string            `json:"id"`
		ParentID string            `json:"parent_id"`
		Name     string            `json:"name"`
		Labels   map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		ComputeInstanceSpec struct {
			V1 struct {
				Platform string `json:"platform"`
			} `json:"v1"`
		} `json:"compute_instance_spec"`
		Pricing struct {
			MaxPriceV1 struct {
				MaxPrice string `json:"max_price"`
			} `json:"max_price_v1"`
		} `json:"pricing"`
	} `json:"spec"`
	Status struct {
		State           string `json:"state"`
		SchedulingState string `json:"scheduling_state"`
		Currency        string `json:"currency"`
		SKUID           string `json:"sku_id"`
	} `json:"status"`
}

type env struct {
	cli     *nebiuscli.CLI
	project string
}

func start(t *testing.T) *env {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), compute.New(), billingmod.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce), testkit.WithOperationDelay(100*time.Millisecond))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	return &env{cli: cli, project: inst.ProjectID}
}

// fails runs a command that must fail with an error mentioning want.
func (e *env) fails(t *testing.T, want string, args ...string) {
	t.Helper()
	_, err := e.cli.Run(t.Context(), args...)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("nebius %s: err = %v, want one mentioning %q", strings.Join(args, " "), err, want)
	}
}

// TestPricingPolicyFlows follows the docs: create a policy for a platform
// with a per-GPU max price, read it back, rename it, lower the price below
// the market and delete it.
func TestPricingPolicyFlows(t *testing.T) {
	e := start(t)

	var p policy
	e.cli.JSON(t, &p, "billing", "pricing-policy", "create", "--parent-id", e.project, "--name", "h100policy",
		"--labels", "team=ml", "--compute-instance-spec-v1-platform", "gpu-h100-sxm", "--pricing-max-price-v1-max-price", "2.5")
	id := p.Metadata.ID
	if id == "" || p.Metadata.Name != "h100policy" || p.Metadata.ParentID != e.project || p.Metadata.Labels["team"] != "ml" ||
		p.Spec.ComputeInstanceSpec.V1.Platform != "gpu-h100-sxm" || !samePrice(p.Spec.Pricing.MaxPriceV1.MaxPrice, 2.5) {
		t.Fatalf("create = %+v", p)
	}
	if p.Status.State != "STATE_ACTIVE" || p.Status.SchedulingState != "SCHEDULING_STATE_ALLOWED" || p.Status.Currency != "USD" || p.Status.SKUID == "" {
		t.Fatalf("create status = %+v, want ACTIVE/ALLOWED/USD with a sku", p.Status)
	}

	var got policy
	e.cli.JSON(t, &got, "billing", "pricing-policy", "get", "--id", id)
	if got.Metadata.ID != id || !samePrice(got.Spec.Pricing.MaxPriceV1.MaxPrice, 2.5) {
		t.Fatalf("get = %+v", got)
	}
	got = policy{}
	e.cli.JSON(t, &got, "billing", "pricing-policy", "get-by-name", "--parent-id", e.project, "--name", "h100policy")
	if got.Metadata.ID != id {
		t.Fatalf("get-by-name = %+v", got)
	}

	var list struct {
		Items []policy `json:"items"`
	}
	e.cli.JSON(t, &list, "billing", "pricing-policy", "list", "--parent-id", e.project)
	if len(list.Items) != 1 || list.Items[0].Metadata.ID != id {
		t.Fatalf("list = %+v", list)
	}

	e.fails(t, "range", "billing", "pricing-policy", "create", "--parent-id", e.project, "--name", "toohigh",
		"--compute-instance-spec-v1-platform", "gpu-h100-sxm", "--pricing-max-price-v1-max-price", "99")

	// Rename, then lower the price below the market (1.250): scheduling is blocked.
	got = policy{}
	e.cli.JSON(t, &got, "billing", "pricing-policy", "update", "--id", id, "--name", "renamed")
	if got.Metadata.ID != id || got.Metadata.Name != "renamed" || !samePrice(got.Spec.Pricing.MaxPriceV1.MaxPrice, 2.5) ||
		got.Spec.ComputeInstanceSpec.V1.Platform != "gpu-h100-sxm" {
		t.Fatalf("rename = %+v", got)
	}
	got = policy{}
	e.cli.JSON(t, &got, "billing", "pricing-policy", "update", "--id", id, "--pricing-max-price-v1-max-price", "1.0")
	if !samePrice(got.Spec.Pricing.MaxPriceV1.MaxPrice, 1) || got.Metadata.Name != "renamed" {
		t.Fatalf("lower price = %+v", got)
	}
	got = policy{}
	e.cli.JSON(t, &got, "billing", "pricing-policy", "get", "--id", id)
	if got.Status.SchedulingState != "SCHEDULING_STATE_BLOCKED" || got.Status.State != "STATE_ACTIVE" {
		t.Fatalf("status after lowering = %+v, want ACTIVE/BLOCKED", got.Status)
	}

	e.cli.JSON(t, &struct{}{}, "billing", "pricing-policy", "delete", "--id", id)
	e.fails(t, "NotFound", "billing", "pricing-policy", "get", "--id", id)
}

// samePrice compares a decimal string with want: the CLI user typed "2.5", the
// API may echo it with or without padding to three decimals.
func samePrice(got string, want float64) bool {
	f, err := strconv.ParseFloat(got, 64)
	return err == nil && f == want
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestSpotPrices lists the current spot prices and a price series, as the
// spot price docs show.
func TestSpotPrices(t *testing.T) {
	e := start(t)

	var list struct {
		Items []map[string]any `json:"items"`
	}
	e.cli.JSON(t, &list, "billing", "v1alpha1", "spot-price", "list", "--all")
	if len(list.Items) < 3 {
		t.Fatalf("spot-price list = %d items, want at least the 3 eu-north1 markets", len(list.Items))
	}
	if !strings.Contains(toJSON(t, list.Items), "gpu-h100-sxm") || !strings.Contains(toJSON(t, list.Items), "1.25") {
		t.Fatalf("spot-price list lacks gpu-h100-sxm at 1.25: %v", list.Items)
	}

	start := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339) //nolint:forbidigo // the mock runs on the wall clock; the CLI user passes "two hours ago"
	var series struct {
		Items []map[string]any `json:"items"`
	}
	e.cli.JSON(t, &series, "billing", "v1alpha1", "spot-price", "list-series", "--filters-start-time", start,
		"--aggregation-granularity", "granularity_hour_1", "--aggregation-method", "method_avg_price", "--all")
	if len(series.Items) < 3 {
		t.Fatalf("spot-price list-series = %d series, want one per SKU", len(series.Items))
	}
	for _, s := range series.Items {
		pts, _ := s["points"].([]any)
		if len(pts) < 1 || len(pts) > 3 {
			t.Fatalf("series %v has %d points over 2 hours of hourly steps", s, len(pts))
		}
	}
}

// TestCalculatorEstimate prices a preemptible and an on-demand 8-GPU H100
// instance with the calculator (8 x 1.25 spot; on-demand per the price list),
// with exactly the flags of the spot-prices docs: no boot disk or network
// interface is needed to price an instance.
func TestCalculatorEstimate(t *testing.T) {
	e := start(t)
	base := []string{
		"billing", "v1alpha1", "calculator", "estimate",
		"--resource-spec-compute-instance-spec-parent-id", e.project,
		"--resource-spec-compute-instance-spec-resources-platform", "gpu-h100-sxm",
		"--resource-spec-compute-instance-spec-resources-preset", "8gpu-128vcpu-1600gb",
	}

	var est struct {
		HourlyCost struct {
			General struct {
				Total struct {
					Cost string `json:"cost"`
				} `json:"total"`
			} `json:"general"`
		} `json:"hourly_cost"`
	}
	e.cli.JSON(t, &est, append(append([]string{}, base...), "--resource-spec-compute-instance-spec-preemptible-on-preemption", "STOP")...)
	if got := est.HourlyCost.General.Total.Cost; got != "10" {
		t.Fatalf("preemptible hourly cost = %q, want 10 (8 x 1.25)", got)
	}
	est.HourlyCost.General.Total.Cost = ""
	e.cli.JSON(t, &est, base...)
	if got := est.HourlyCost.General.Total.Cost; got != "23.6" {
		t.Fatalf("on-demand hourly cost = %q, want 23.6", got)
	}
}
