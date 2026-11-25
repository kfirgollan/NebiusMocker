package config

// computeFabricsParsers reads NEBIUS_MOCKER_COMPUTE_FABRICS_FILE: a YAML (or
// JSON) InfiniBand fabric catalog that replaces the built-in one used by
// GpuClusterService (see docs/services/compute.md).
func computeFabricsParsers(file *string) []struct {
	name  string
	apply func(string) error
} {
	return []struct {
		name  string
		apply func(string) error
	}{
		{"COMPUTE_FABRICS_FILE", func(v string) error { *file = v; return nil }},
	}
}
