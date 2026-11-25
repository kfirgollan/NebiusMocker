//go:build e2e

package nebiuscli_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

// TestCLIReachesMocker installs the official CLI into the sandbox and lists the
// seeded IAM groups through a profile pointing at an in-process mocker.
func TestCLIReachesMocker(t *testing.T) {
	inst := testkit.StartT(t, []mocker.Module{iam.New()}, testkit.WithAuthnRequired())
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, ParentID: inst.ProjectID, TenantID: inst.TenantID})

	res, err := cli.Run(t.Context(), "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(res.Stdout, nebiuscli.Version) {
		t.Fatalf("nebius version = %q, want %s", res.Stdout, nebiuscli.Version)
	}

	var groups struct {
		Items []group `json:"items"`
	}
	cli.JSON(t, &groups, "iam", "group", "list", "--parent-id", inst.TenantID)
	if !slices.ContainsFunc(groups.Items, func(g group) bool { return g.Metadata.Name == "admins" }) {
		t.Fatalf("iam group list = %+v, want the seeded admins group", groups.Items)
	}
}

type group struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
}
