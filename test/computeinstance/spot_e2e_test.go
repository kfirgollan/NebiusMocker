//go:build e2e

package computeinstance_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/billing"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

type pricingPolicy struct {
	Spec struct {
		Pricing struct {
			MaxPriceV1 struct {
				MaxPrice string `json:"max_price"`
			} `json:"max_price_v1"`
		} `json:"pricing"`
	} `json:"spec"`
	Status struct {
		RunningVMCount json.Number `json:"running_vm_count"`
	} `json:"status"`
}

// TestSpotPricingPolicyCLI follows docs.nebius.com/compute/virtual-machines/preemptible
// and docs.nebius.com/signup-billing/manage-pricing-policy: a preemptible VM
// under a pricing policy pins the policy while it runs; a policy priced below
// the spot price blocks the VM from starting.
func TestSpotPricingPolicyCLI(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), compute.New(), vpc.New(), billing.New()},
		testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(50*time.Millisecond),
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})

	policyID := idOf(t, cli, "billing", "pricing-policy", "create", "--parent-id", inst.ProjectID, "--name", "spoth100",
		"--compute-instance-spec-v1-platform", "gpu-h100-sxm", "--pricing-max-price-v1-max-price", "2.000")
	getPolicy := func() pricingPolicy {
		var p pricingPolicy
		cli.JSON(t, &p, "billing", "pricing-policy", "get", "--id", policyID)
		return p
	}
	wantCount := func(want string) {
		t.Helper()
		got := getPolicy().Status.RunningVMCount.String()
		if got == "" {
			got = "0" // the CLI omits a zero count
		}
		if got != want {
			t.Fatalf("running_vm_count = %q, want %s", got, want)
		}
	}
	setPrice := func(price string) {
		t.Helper()
		run(t, cli, "billing", "pricing-policy", "update", policyID, "--pricing-max-price-v1-max-price", price)
		if got := getPolicy().Spec.Pricing.MaxPriceV1.MaxPrice; got != price {
			t.Fatalf("max_price = %q, want %s", got, price)
		}
	}
	wantCount("0")

	subnet := subnetID(t, cli)
	id := idOf(t, cli, "compute", "instance", "create", "--name", "spot-vm",
		"--resources-platform", "gpu-h100-sxm", "--resources-preset", "8gpu-128vcpu-1600gb",
		"--preemptible-on-preemption", "STOP", "--recovery-policy", "FAIL",
		"--spot-pricing-policy-id", policyID,
		"--boot-disk-managed-disk-name", "spot-boot", "--boot-disk-managed-disk-type", "network_ssd",
		"--boot-disk-managed-disk-size-gibibytes", "100",
		"--boot-disk-managed-disk-source-image-family-image-family", "ubuntu24.04-cuda12",
		"--boot-disk-attach-mode", "READ_WRITE", "--boot-disk-device-id", "boot",
		"--network-interfaces", nics(t, subnet))
	if got := getInstance(t, cli, id).Status.State; got != "RUNNING" {
		t.Fatalf("spot instance state = %q, want RUNNING", got)
	}
	wantCount("1")

	// A policy in use by a running VM cannot be repriced or deleted.
	out := fails(t, cli, "billing", "pricing-policy", "update", policyID, "--pricing-max-price-v1-max-price", "3.000")
	wantContains(t, "update while running", out, "FailedPrecondition", "1 VMs run under the policy")
	wantContains(t, "delete while running", fails(t, cli, "billing", "pricing-policy", "delete", "--id", policyID), "FailedPrecondition")
	if got := getPolicy().Spec.Pricing.MaxPriceV1.MaxPrice; got != "2.000" {
		t.Fatalf("max_price after refused update = %q, want 2.000", got)
	}

	run(t, cli, "compute", "instance", "stop", "--id", id)
	wantCount("0")

	// 1.000 is below the 1.250 spot price: the policy blocks the VM.
	setPrice("1.000")
	fails(t, cli, "compute", "instance", "start", "--id", id)
	if got := getInstance(t, cli, id).Status.State; got != "STOPPED" {
		t.Fatalf("instance state after refused start = %q, want STOPPED", got)
	}
	wantCount("0")

	setPrice("2.000")
	run(t, cli, "compute", "instance", "start", "--id", id)
	if got := getInstance(t, cli, id).Status.State; got != "RUNNING" {
		t.Fatalf("instance state after start = %q, want RUNNING", got)
	}
	wantCount("1")

	run(t, cli, "compute", "instance", "stop", "--id", id)
	run(t, cli, "compute", "instance", "delete", "--id", id)
	run(t, cli, "billing", "pricing-policy", "delete", "--id", policyID)
}
