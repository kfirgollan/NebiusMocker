//go:build e2e

package computeinstance_test

import (
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/capacity"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

type reservedInstance struct {
	Spec struct {
		ReservationPolicy struct {
			Policy         string   `json:"policy"`
			ReservationIDs []string `json:"reservation_ids"`
		} `json:"reservation_policy"`
	} `json:"spec"`
	Status struct {
		State         string `json:"state"`
		ReservationID string `json:"reservation_id"`
	} `json:"status"`
}

type blockGroupUsage struct {
	Status struct {
		Usage string `json:"usage"`
	} `json:"status"`
}

// TestReservationPolicyCLI follows docs.nebius.com/compute/virtual-machines/reservations:
// create a GPU VM with --reservation-policy-policy strict and the block group
// ID, then change the policy to forbid while the VM is stopped.
func TestReservationPolicyCLI(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), compute.New(), vpc.New(), capacity.New()},
		testkit.WithObjStore(memory.New(clock.Real{})), testkit.WithOperationDelay(50*time.Millisecond),
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})

	var list struct {
		Items []struct {
			Metadata struct {
				ID string `json:"id"`
			} `json:"metadata"`
			Status struct {
				ResourceAffinity struct {
					ComputeV1 struct {
						Platform string `json:"platform"`
					} `json:"compute_v1"`
				} `json:"resource_affinity"`
			} `json:"status"`
		} `json:"items"`
	}
	cli.JSON(t, &list, "capacity", "capacity-block-group", "list", "--parent-id", inst.TenantID)
	var group string
	for _, g := range list.Items {
		if g.Status.ResourceAffinity.ComputeV1.Platform == "gpu-h100-sxm" {
			group = g.Metadata.ID
		}
	}
	if group == "" {
		t.Fatalf("no h100 block group in %+v", list)
	}
	usage := func() string {
		var g blockGroupUsage
		cli.JSON(t, &g, "capacity", "capacity-block-group", "get", "--id", group)
		if g.Status.Usage == "" {
			return "0" // the CLI omits a zero usage
		}
		return g.Status.Usage
	}
	get := func(id string) reservedInstance {
		var in reservedInstance
		cli.JSON(t, &in, "compute", "instance", "get", "--id", id)
		return in
	}

	subnet := subnetID(t, cli)
	id := idOf(t, cli, "compute", "instance", "create", "--name", "reserved-vm",
		"--resources-platform", "gpu-h100-sxm", "--resources-preset", "8gpu-128vcpu-1600gb",
		"--reservation-policy-policy", "strict", "--reservation-policy-reservation-ids", group,
		"--boot-disk-managed-disk-name", "reserved-boot", "--boot-disk-managed-disk-type", "network_ssd",
		"--boot-disk-managed-disk-size-gibibytes", "100",
		"--boot-disk-managed-disk-source-image-family-image-family", "ubuntu24.04-cuda12",
		"--boot-disk-attach-mode", "READ_WRITE", "--boot-disk-device-id", "boot",
		"--network-interfaces", nics(t, subnet))
	in := get(id)
	if in.Status.ReservationID != group || in.Spec.ReservationPolicy.Policy != "STRICT" ||
		len(in.Spec.ReservationPolicy.ReservationIDs) != 1 || in.Spec.ReservationPolicy.ReservationIDs[0] != group {
		t.Fatalf("strict instance = %+v, want reservation %s", in, group)
	}
	if got := usage(); got != "8" {
		t.Fatalf("block group usage = %q, want 8", got)
	}
	wantContains(t, "list-resources", run(t, cli, "capacity", "capacity-block-group", "list-resources", group), id)

	// The reservation is kept while stopped; updating the policy needs a stopped VM.
	run(t, cli, "compute", "instance", "stop", "--id", id)
	if got := get(id).Status.ReservationID; got != group {
		t.Fatalf("reservation_id while stopped = %q, want %s kept", got, group)
	}
	run(t, cli, "compute", "instance", "update", "--id", id, "--parent-id", inst.ProjectID,
		"--reservation-policy-policy", "forbid", "--reservation-policy-reservation-ids", "")
	in = get(id)
	if in.Spec.ReservationPolicy.Policy != "FORBID" || len(in.Spec.ReservationPolicy.ReservationIDs) != 0 {
		t.Fatalf("after update policy = %+v, want FORBID without ids", in.Spec.ReservationPolicy)
	}
	if got := usage(); in.Status.ReservationID != "" || got != "0" {
		t.Fatalf("after the policy change: reservation_id %q, usage %q; want the reservation dropped", in.Status.ReservationID, got)
	}
	run(t, cli, "compute", "instance", "start", "--id", id)
	in = get(id)
	if in.Status.State != "RUNNING" || in.Status.ReservationID != "" {
		t.Fatalf("after start = %+v, want RUNNING in the common pool", in.Status)
	}
	if got := usage(); got != "0" {
		t.Fatalf("block group usage after forbid = %q, want 0", got)
	}

	run(t, cli, "compute", "instance", "delete", "--id", id)
	if got := usage(); got != "0" {
		t.Fatalf("block group usage after delete = %q, want 0", got)
	}
}
