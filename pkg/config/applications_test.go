package config

import (
	"slices"
	"testing"
)

func TestFromLookupApplications(t *testing.T) {
	t.Parallel()
	env := func(kv map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := kv[k]; return v, ok }
	}
	c, err := FromLookup(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Applications.Products, "nebius/soperator") || len(c.Applications.Products) != 6 {
		t.Fatalf("default products = %v", c.Applications.Products)
	}
	c, err = FromLookup(env(map[string]string{"NEBIUS_MOCKER_APPLICATIONS_PRODUCTS": "acme/one, acme/two"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme/one", "acme/two"}; !slices.Equal(c.Applications.Products, want) {
		t.Fatalf("products = %v, want %v", c.Applications.Products, want)
	}
	for _, bad := range []string{"a,,b", "Acme/One", "a/b,a/b", "a b"} {
		if _, err := FromLookup(env(map[string]string{"NEBIUS_MOCKER_APPLICATIONS_PRODUCTS": bad})); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
