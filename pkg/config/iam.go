package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/nid"
)

// DefaultProjectName is the name of the seeded default project.
const DefaultProjectName = "default"

// IAMConfig configures the IAM module (NEBIUS_MOCKER_IAM_*).
type IAMConfig struct {
	// Projects are extra projects seeded in the default tenant, after the
	// default project (NEBIUS_MOCKER_IAM_PROJECTS="name[:region],…").
	Projects []ProjectSeed
}

// ProjectSeed is one extra project to seed.
type ProjectSeed struct {
	Name string
	// Region is one of Config.Regions; empty means the default (first) region.
	Region string
}

// SeededProject is a project the IAM module seeds, with its stable ID.
type SeededProject struct {
	ID, Name, Region string
}

func iamParsers(c *IAMConfig) []struct {
	name  string
	apply func(string) error
} {
	return []struct {
		name  string
		apply func(string) error
	}{
		{"IAM_PROJECTS", func(v string) error { return parseProjectSeeds(v, &c.Projects) }},
	}
}

// parseProjectSeeds parses "name[:region],…" (e.g. "dev,ml:eu-west1").
func parseProjectSeeds(v string, out *[]ProjectSeed) error {
	var seeds []ProjectSeed
	for _, item := range splitList(v) {
		name, region, _ := strings.Cut(item, ":")
		seeds = append(seeds, ProjectSeed{Name: strings.TrimSpace(name), Region: strings.TrimSpace(region)})
	}
	*out = seeds
	return nil
}

// SeededProjects returns every project the IAM module seeds in TenantID: the
// default project (ProjectID, named "default", in the first region) followed
// by IAM.Projects. The IDs of extra projects derive from the tenant and the
// name, so they stay the same across restarts. It fails on an unknown region,
// an empty or malformed name, or a name used twice.
func (c Config) SeededProjects() ([]SeededProject, error) {
	if len(c.Regions) == 0 {
		return nil, errors.New("no regions configured")
	}
	codes := make(map[string]string, len(c.Regions))
	for _, r := range c.Regions {
		codes[r.Name] = r.RoutingCode
	}
	out := []SeededProject{{ID: c.ProjectID, Name: DefaultProjectName, Region: c.Regions[0].Name}}
	seen := map[string]bool{DefaultProjectName: true}
	for _, s := range c.IAM.Projects {
		if s.Name == "" || strings.ContainsAny(s.Name, ":,= \t") {
			return nil, fmt.Errorf("project name %q: want a non-empty name without ':', ',', '=' or spaces", s.Name)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("project name %q is used twice (%q is the default project)", s.Name, DefaultProjectName)
		}
		seen[s.Name] = true
		region := s.Region
		if region == "" {
			region = c.Regions[0].Name
		}
		code, ok := codes[region]
		if !ok {
			return nil, fmt.Errorf("project %q: unknown region %q (configured: NEBIUS_MOCKER_REGIONS)", s.Name, region)
		}
		id, err := nid.Deterministic("project", code, c.TenantID+"/"+s.Name)
		if err != nil {
			return nil, fmt.Errorf("project %q: %w", s.Name, err)
		}
		out = append(out, SeededProject{ID: id, Name: s.Name, Region: region})
	}
	return out, nil
}
