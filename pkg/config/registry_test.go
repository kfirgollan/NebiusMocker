package config

import "testing"

func TestFromLookupRegistry(t *testing.T) {
	t.Parallel()
	if d := Default().Registry; d.Backend != RegistryMemory || d.Listen != DefaultRegistryListen {
		t.Fatalf("default Registry = %+v, want memory backend on %s", d, DefaultRegistryListen)
	}
	c, err := FromLookup(lookup(map[string]string{
		"NEBIUS_MOCKER_REGISTRY_BACKEND":         "harbor",
		"NEBIUS_MOCKER_REGISTRY_LISTEN":          "127.0.0.1:5000",
		"NEBIUS_MOCKER_REGISTRY_FQDN":            "cr.local:5000",
		"NEBIUS_MOCKER_REGISTRY_HARBOR_URL":      "http://harbor:8080",
		"NEBIUS_MOCKER_REGISTRY_HARBOR_PASSWORD": "secret",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	want := RegistryConfig{
		Backend: RegistryHarbor, Listen: "127.0.0.1:5000", FQDN: "cr.local:5000",
		HarborURL: "http://harbor:8080", HarborUsername: "admin", HarborPassword: "secret",
	}
	if c.Registry != want {
		t.Fatalf("Registry = %+v, want %+v", c.Registry, want)
	}
	if _, err := FromLookup(lookup(map[string]string{"NEBIUS_MOCKER_REGISTRY_BACKEND": "quay"})); err == nil {
		t.Fatal("FromLookup(REGISTRY_BACKEND=quay) succeeded, want error")
	}
}
