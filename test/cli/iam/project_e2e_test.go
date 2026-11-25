//go:build e2e

// Package iam runs the official nebius CLI against NebiusMocker through the
// IAM project and tenant commands the Nebius docs show
// (docs.nebius.com/iam/manage-projects, /iam/get-tenants).
package iam

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

type container struct {
	Metadata struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		ParentID string `json:"parent_id"`
	} `json:"metadata"`
	Spec struct {
		Region string `json:"region"`
	} `json:"spec"`
}

type listOutput struct {
	Items         []container `json:"items"`
	NextPageToken string      `json:"next_page_token"`
}

func startWithProjects(t *testing.T) *testkit.Instance {
	t.Helper()
	return testkit.StartT(t, []mocker.Module{iam.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce), testkit.WithOperationDelay(200*time.Millisecond),
		testkit.WithProjects(
			config.ProjectSeed{Name: "dev"},
			config.ProjectSeed{Name: "ml", Region: "eu-west1"},
			config.ProjectSeed{Name: "ops"},
		))
}

func idsOf(items []container) []string {
	out := make([]string, 0, len(items))
	for _, c := range items {
		out = append(out, c.Metadata.ID)
	}
	return out
}

func seededIDs(inst *testkit.Instance) []string {
	out := make([]string, 0, len(inst.Projects))
	for _, p := range inst.Projects {
		out = append(out, p.ID)
	}
	return out
}

// listPages runs a list command page by page (--page-size 1), following
// next_page_token like a script would, and returns every item.
func listPages(t *testing.T, cli *nebiuscli.CLI, args ...string) []container {
	t.Helper()
	var all []container
	token := ""
	for range 100 {
		page := append(slices.Clone(args), "--page-size", "1")
		if token != "" {
			page = append(page, "--page-token", token)
		}
		var out listOutput
		cli.JSON(t, &out, page...)
		if len(out.Items) == 0 && out.NextPageToken != "" {
			t.Fatalf("%s: empty page with a next token", strings.Join(args, " "))
		}
		all = append(all, out.Items...)
		if token = out.NextPageToken; token == "" {
			return all
		}
	}
	t.Fatalf("%s: paging did not end", strings.Join(args, " "))
	return nil
}

// TestCLIViewerListsEveryProject is the done criterion through the CLI: a
// viewer-only principal lists every project of the seeded tenant, with the
// deprecated v1 command and the v2 one, page by page and with --all.
func TestCLIViewerListsEveryProject(t *testing.T) {
	inst := startWithProjects(t)
	want := seededIDs(inst)
	if len(want) < 4 {
		t.Fatalf("seeded projects = %v, want the default one and 3 more", want)
	}
	viewer := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: config.DefaultViewerToken, TenantID: inst.TenantID})

	for _, cmd := range [][]string{{"iam", "project", "list"}, {"iam", "v2", "project", "list"}} {
		args := append(slices.Clone(cmd), "--parent-id", inst.TenantID)
		if got := idsOf(listPages(t, viewer, args...)); !slices.Equal(got, want) {
			t.Errorf("%s page by page = %v, want %v", strings.Join(cmd, " "), got, want)
		}
		var all listOutput
		viewer.JSON(t, &all, append(slices.Clone(args), "--page-size", "1", "--all")...)
		if got := idsOf(all.Items); !slices.Equal(got, want) {
			t.Errorf("%s --all = %v, want %v", strings.Join(cmd, " "), got, want)
		}
		var def listOutput
		viewer.JSON(t, &def, args...)
		if got := idsOf(def.Items); !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", strings.Join(cmd, " "), got, want)
		}
	}

	// Regions and names round-trip.
	for _, p := range inst.Projects {
		var got container
		viewer.JSON(t, &got, "iam", "v2", "project", "get-by-name", "--parent-id", inst.TenantID, "--name", p.Name)
		if got.Metadata.ID != p.ID || got.Spec.Region != p.Region || got.Metadata.ParentID != inst.TenantID {
			t.Errorf("get-by-name %s = %+v, want %+v", p.Name, got, p)
		}
	}

	for _, cmd := range [][]string{{"iam", "tenant", "list"}, {"iam", "v2", "tenant", "list"}} {
		var out listOutput
		viewer.JSON(t, &out, cmd...)
		if got := idsOf(out.Items); !slices.Equal(got, []string{inst.TenantID}) {
			t.Errorf("%s = %v, want [%s]", strings.Join(cmd, " "), got, inst.TenantID)
		}
	}

	// A viewer cannot change projects.
	if _, err := viewer.Run(t.Context(), "iam", "v2", "project", "create",
		"--parent-id", inst.TenantID, "--name", "not-allowed", "--region", "eu-north1"); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "permission") {
		t.Errorf("viewer project create: err = %v, want a permission denial", err)
	}
	if _, err := viewer.Run(t.Context(), "iam", "v2", "project", "delete", "--id", want[1]); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "permission") {
		t.Errorf("viewer project delete: err = %v, want a permission denial", err)
	}
}

// TestCLIProjectCreateUpdateDelete follows docs.nebius.com/iam/manage-projects
// as an admin: create, rename, delete.
func TestCLIProjectCreateUpdateDelete(t *testing.T) {
	inst := startWithProjects(t)
	admin := nebiuscli.New(t, nebiuscli.Profile{Addr: inst.Addr, Token: inst.Token, TenantID: inst.TenantID})

	var created container
	admin.JSON(t, &created, "iam", "v2", "project", "create",
		"--parent-id", inst.TenantID, "--name", "cli-proj", "--region", "eu-west1")
	if created.Metadata.ID == "" || created.Spec.Region != "eu-west1" || created.Metadata.Name != "cli-proj" {
		t.Fatalf("create = %+v", created)
	}

	var renamed container
	admin.JSON(t, &renamed, "iam", "v2", "project", "update", "--id", created.Metadata.ID, "--name", "cli-proj-2")
	if renamed.Metadata.Name != "cli-proj-2" || renamed.Spec.Region != "eu-west1" {
		t.Fatalf("update = %+v", renamed)
	}

	var out listOutput
	admin.JSON(t, &out, "iam", "v2", "project", "list", "--parent-id", inst.TenantID, "--all")
	if !slices.Contains(idsOf(out.Items), created.Metadata.ID) {
		t.Fatalf("list after create = %v, missing %s", idsOf(out.Items), created.Metadata.ID)
	}

	if _, err := admin.Run(t.Context(), "iam", "v2", "project", "delete", "--id", created.Metadata.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	res, err := admin.Run(t.Context(), "iam", "v2", "project", "get", "--id", created.Metadata.ID)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "not found") {
		t.Fatalf("get after delete: err = %v, stdout %s; want not found", err, res.Stdout)
	}
}
