package config

import (
	"strings"
	"testing"
)

func lookupOf(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func TestSeededProjectsDefault(t *testing.T) {
	t.Parallel()
	got, err := Default().SeededProjects()
	if err != nil {
		t.Fatalf("SeededProjects: %v", err)
	}
	want := SeededProject{ID: DefaultProjectID, Name: DefaultProjectName, Region: "eu-north1"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("default seed = %+v, want only %+v", got, want)
	}
}

func TestFromLookupIAMProjects(t *testing.T) {
	t.Parallel()
	c, err := FromLookup(lookupOf(map[string]string{"NEBIUS_MOCKER_IAM_PROJECTS": " dev , ml:eu-west1,ops:eu-north1"}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	got, err := c.SeededProjects()
	if err != nil {
		t.Fatalf("SeededProjects: %v", err)
	}
	wantNames := []struct{ name, region, prefix string }{
		{"default", "eu-north1", DefaultProjectID},
		{"dev", "eu-north1", "project-e00"},
		{"ml", "eu-west1", "project-e01"}, // the region's routing code
		{"ops", "eu-north1", "project-e00"},
	}
	if len(got) != len(wantNames) {
		t.Fatalf("seed = %+v, want %d projects", got, len(wantNames))
	}
	ids := map[string]bool{}
	for i, w := range wantNames {
		p := got[i]
		if p.Name != w.name || p.Region != w.region || !strings.HasPrefix(p.ID, w.prefix) {
			t.Errorf("project %d = %+v, want %s in %s with ID prefix %s", i, p, w.name, w.region, w.prefix)
		}
		if ids[p.ID] {
			t.Errorf("duplicate ID %s", p.ID)
		}
		ids[p.ID] = true
	}

	// IDs are stable across restarts.
	again, err := c.SeededProjects()
	if err != nil || again[1].ID != got[1].ID {
		t.Fatalf("second SeededProjects = %+v, %v; want the same IDs as %+v", again, err, got)
	}
}

func TestFromLookupIAMProjectsInvalid(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{
		"default",         // the default project's name
		"dev,dev",         // duplicate
		"dev:mars-south1", // unknown region
		":eu-north1",      // empty name
		"a=b",             // malformed name
	} {
		if _, err := FromLookup(lookupOf(map[string]string{"NEBIUS_MOCKER_IAM_PROJECTS": bad})); err == nil {
			t.Errorf("NEBIUS_MOCKER_IAM_PROJECTS=%q accepted", bad)
		}
	}
	// Regions are validated against NEBIUS_MOCKER_REGIONS, whatever the env order.
	c, err := FromLookup(lookupOf(map[string]string{
		"NEBIUS_MOCKER_REGIONS":      "eu-north1=e00,xx-test1=x00",
		"NEBIUS_MOCKER_IAM_PROJECTS": "lab:xx-test1",
	}))
	if err != nil {
		t.Fatalf("custom region: %v", err)
	}
	if p, _ := c.SeededProjects(); len(p) != 2 || !strings.HasPrefix(p[1].ID, "project-x00") {
		t.Fatalf("custom region seed = %+v", p)
	}
}
