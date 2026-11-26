//go:build e2e

// Package quotas runs the official nebius CLI against NebiusMocker through the
// quota flows the Nebius docs show (https://docs.nebius.com/overview/quotas:
// "nebius quotas quota-allowance list --parent-id …" and
// "… get-by-name --parent-id … --region … --name …"), plus the CLI's create,
// update and delete, and a create that a lowered allowance refuses.
package quotas

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/quotas"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

const region = "eu-north1"

type allowance struct {
	Metadata struct {
		ID              string            `json:"id"`
		ParentID        string            `json:"parent_id"`
		Name            string            `json:"name"`
		ResourceVersion string            `json:"resource_version"`
		Labels          map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Limit  string `json:"limit"`
		Region string `json:"region"`
	} `json:"spec"`
	Status struct {
		State      string `json:"state"`
		Usage      string `json:"usage"`
		UsageState string `json:"usage_state"`
		Service    string `json:"service"`
		Unit       string `json:"unit"`
	} `json:"status"`
}

func (a allowance) usage(t *testing.T) uint64 {
	t.Helper()
	if a.Status.Usage == "" {
		return 0 // proto3 JSON omits zero
	}
	u, err := strconv.ParseUint(a.Status.Usage, 10, 64)
	if err != nil {
		t.Fatalf("usage %q: %v", a.Status.Usage, err)
	}
	return u
}

func TestQuotaAllowanceFlows(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), quotas.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce), testkit.WithOperationDelay(100*time.Millisecond))
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})
	project := inst.ProjectID

	// nebius quotas quota-allowance list --parent-id <parent_ID>
	var list struct {
		Items []allowance `json:"items"`
	}
	cli.JSON(t, &list, "quotas", "quota-allowance", "list", "--parent-id", project, "--page-size", "1000")
	found := false
	for _, a := range list.Items {
		found = found || a.Metadata.Name == quota.VPCNetworkCount
		if a.Status.State != "STATE_ACTIVE" || a.Spec.Region != region {
			t.Fatalf("listed allowance = %+v", a)
		}
	}
	if !found {
		t.Fatalf("list has no %s: %+v", quota.VPCNetworkCount, list.Items)
	}

	// nebius quotas quota-allowance get-by-name --parent-id … --region … --name …
	getByName := func() allowance {
		var a allowance
		cli.JSON(t, &a, "quotas", "quota-allowance", "get-by-name", "--parent-id", project, "--region", region, "--name", quota.VPCNetworkCount)
		return a
	}
	a := getByName()
	if a.Spec.Limit != "1000" || a.Status.Service != "vpc" || a.Status.Unit != "count" {
		t.Fatalf("get-by-name = %+v", a)
	}
	usage := a.usage(t)

	// Lower the allowance to the current usage: a new network is refused.
	var created allowance
	cli.JSON(t, &created, "quotas", "quota-allowance", "create", "--parent-id", project, "--name", quota.VPCNetworkCount,
		"--limit", strconv.FormatUint(usage, 10), "--labels", "team=net")
	if created.Metadata.ID != a.Metadata.ID || created.Spec.Limit != strconv.FormatUint(usage, 10) && usage != 0 {
		t.Fatalf("create = %+v, want allowance %s with limit %d", created, a.Metadata.ID, usage)
	}
	_, err := cli.Run(t.Context(), "vpc", "network", "create", "--parent-id", project, "--name", "over-quota")
	if err == nil || !strings.Contains(err.Error(), quota.VPCNetworkCount) {
		t.Fatalf("network create over quota: err = %v, want a quota error naming %s", err, quota.VPCNetworkCount)
	}

	// nebius quotas quota-allowance update --id … --limit …: room for one more.
	var updated allowance
	cli.JSON(t, &updated, "quotas", "quota-allowance", "update", "--id", a.Metadata.ID, "--limit", strconv.FormatUint(usage+1, 10))
	if updated.Spec.Limit != strconv.FormatUint(usage+1, 10) {
		t.Fatalf("update = %+v", updated)
	}
	if _, err := cli.Run(t.Context(), "vpc", "network", "create", "--parent-id", project, "--name", "fits"); err != nil {
		t.Fatalf("network create after raising the quota: %v", err)
	}
	if got := getByName(); got.usage(t) != usage+1 || got.Status.UsageState != "USAGE_STATE_USED" {
		t.Fatalf("after create = %+v, want usage %d", got, usage+1)
	}

	// nebius quotas quota-allowance delete --id …: back to the default.
	if _, err := cli.Run(t.Context(), "quotas", "quota-allowance", "delete", "--id", a.Metadata.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var got allowance
	cli.JSON(t, &got, "quotas", "quota-allowance", "get", "--id", a.Metadata.ID)
	if got.Spec.Limit != "1000" {
		t.Fatalf("after delete = %+v, want the default limit", got)
	}
}
