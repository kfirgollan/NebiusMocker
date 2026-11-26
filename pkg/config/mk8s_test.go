package config

import (
	"slices"
	"testing"
)

func TestFromLookupMK8s(t *testing.T) {
	t.Parallel()
	env := func(kv map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := kv[k]; return v, ok }
	}
	c, err := FromLookup(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.MK8s.Backend != MK8sBackendMock || !slices.Equal(c.MK8s.Versions, []string{"1.34", "1.35", "1.36"}) {
		t.Fatalf("default MK8s = %+v", c.MK8s)
	}
	c, err = FromLookup(env(map[string]string{
		"NEBIUS_MOCKER_MK8S_BACKEND":  "k3s",
		"NEBIUS_MOCKER_MK8S_VERSIONS": "1.36, 1.30 ,1.31",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.MK8s.Backend != MK8sBackendK3s || !slices.Equal(c.MK8s.Versions, []string{"1.30", "1.31", "1.36"}) {
		t.Fatalf("MK8s = %+v", c.MK8s)
	}
	for _, bad := range []map[string]string{
		{"NEBIUS_MOCKER_MK8S_BACKEND": "kind"},
		{"NEBIUS_MOCKER_MK8S_VERSIONS": "1.30,1.30"},
		{"NEBIUS_MOCKER_MK8S_VERSIONS": "1.30.1"},
		{"NEBIUS_MOCKER_MK8S_VERSIONS": "1.9"},
	} {
		if _, err := FromLookup(env(bad)); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
