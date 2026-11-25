package config

import "testing"

func TestFromLookupComputeFabricsFile(t *testing.T) {
	t.Parallel()
	if f := Default().ComputeFabricsFile; f != "" {
		t.Fatalf("default fabrics file = %q, want the built-in catalog", f)
	}
	env := map[string]string{"NEBIUS_MOCKER_COMPUTE_FABRICS_FILE": " /etc/fabrics.yaml "}
	c, err := FromLookup(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if c.ComputeFabricsFile != "/etc/fabrics.yaml" {
		t.Fatalf("ComputeFabricsFile = %q", c.ComputeFabricsFile)
	}
}
