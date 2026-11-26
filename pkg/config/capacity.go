package config

// CapacityConfig configures the capacity module (NEBIUS_MOCKER_CAPACITY_*).
type CapacityConfig struct {
	// BlockGroupsFile, when set, replaces the built-in catalog of capacity
	// block groups, their intervals and the GPU presets resource advice is
	// computed for (YAML or JSON; see docs/services/capacity.md).
	BlockGroupsFile string
}

// capacityParsers reads NEBIUS_MOCKER_CAPACITY_BLOCK_GROUPS_FILE.
func capacityParsers(c *CapacityConfig) []struct {
	name  string
	apply func(string) error
} {
	return []struct {
		name  string
		apply func(string) error
	}{
		{"CAPACITY_BLOCK_GROUPS_FILE", func(v string) error { c.BlockGroupsFile = v; return nil }},
	}
}
