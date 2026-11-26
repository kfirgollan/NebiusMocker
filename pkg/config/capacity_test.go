package config

import "testing"

func TestFromLookupCapacityBlockGroupsFile(t *testing.T) {
	t.Parallel()
	if f := Default().Capacity.BlockGroupsFile; f != "" {
		t.Fatalf("default block groups file = %q, want the built-in catalog", f)
	}
	env := map[string]string{"NEBIUS_MOCKER_CAPACITY_BLOCK_GROUPS_FILE": " /etc/cbg.yaml "}
	c, err := FromLookup(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if c.Capacity.BlockGroupsFile != "/etc/cbg.yaml" {
		t.Fatalf("Capacity.BlockGroupsFile = %q", c.Capacity.BlockGroupsFile)
	}
}
