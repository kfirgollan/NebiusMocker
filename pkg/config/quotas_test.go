package config

import (
	"maps"
	"testing"
)

func TestQuotasLimits(t *testing.T) {
	t.Parallel()
	c, err := FromLookup(lookup(map[string]string{"NEBIUS_MOCKER_QUOTAS_LIMITS": "a=10,b=4Ti"}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	want := map[string]uint64{"a": 10, "b": 4 << 40}
	if !maps.Equal(c.Quotas.Limits, want) {
		t.Fatalf("limits = %v, want %v", c.Quotas.Limits, want)
	}
}

func TestQuotasLimitsSuffixes(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]uint64{
		"a=1Ki": 1 << 10, "a=2Mi": 2 << 20, "a=3Gi": 3 << 30, "a=1Pi": 1 << 50, "a = 5": 5, "a=0": 0,
	} {
		c, err := FromLookup(lookup(map[string]string{"NEBIUS_MOCKER_QUOTAS_LIMITS": in}))
		if err != nil || c.Quotas.Limits["a"] != want {
			t.Errorf("%q = %v, %v; want %d", in, c.Quotas.Limits, err, want)
		}
	}
}

func TestQuotasLimitsErrors(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"a", "a=", "a=-1", "a=1Xi", "a=20000000Pi", "=5"} {
		if _, err := FromLookup(lookup(map[string]string{"NEBIUS_MOCKER_QUOTAS_LIMITS": v})); err == nil {
			t.Errorf("limits %q accepted, want error", v)
		}
	}
}
