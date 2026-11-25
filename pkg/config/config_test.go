package config

import (
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
)

func lookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func TestFromLookup(t *testing.T) {
	t.Parallel()
	c, err := FromLookup(lookup(map[string]string{
		"NEBIUS_MOCKER_LISTEN":     "127.0.0.1:9000",
		"NEBIUS_MOCKER_SERVICES":   "iam, vpc",
		"NEBIUS_MOCKER_AUTHN":      "required",
		"NEBIUS_MOCKER_AUTHZ":      "enforce",
		"NEBIUS_MOCKER_DEV_TOKENS": "a=tenantuseraccount-e00x,b=serviceaccount-e00y",
		"NEBIUS_MOCKER_OP_DELAY":   "250ms",
		"NEBIUS_MOCKER_IDS":        "sequential",
		"NEBIUS_MOCKER_TENANT_ID":  "", // empty keeps the default
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if c.Listen != "127.0.0.1:9000" || len(c.Services) != 2 || c.Authn != AuthnRequired || c.Authz != authz.ModeEnforce ||
		c.Tokens["b"] != "serviceaccount-e00y" || c.OperationDelay != 250*time.Millisecond ||
		c.IDMode != nid.Sequential || c.TenantID != DefaultTenantID {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestRegions(t *testing.T) {
	t.Parallel()
	if d := Default().Regions; len(d) == 0 || d[0] != (Region{Name: "eu-north1", RoutingCode: "e00"}) {
		t.Fatalf("default regions = %v, want eu-north1=e00 first", d)
	}
	c, err := FromLookup(lookup(map[string]string{
		"NEBIUS_MOCKER_REGIONS":             "eu-north1=e00, us-central1=u00",
		"NEBIUS_MOCKER_COMPUTE_IMAGES_FILE": "/tmp/images.yaml",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	want := []Region{{"eu-north1", "e00"}, {"us-central1", "u00"}}
	if len(c.Regions) != 2 || c.Regions[0] != want[0] || c.Regions[1] != want[1] || c.ComputeImagesFile != "/tmp/images.yaml" {
		t.Fatalf("regions = %v, images file = %q", c.Regions, c.ComputeImagesFile)
	}
}

func TestFromLookupErrors(t *testing.T) {
	t.Parallel()
	for _, env := range []map[string]string{
		{"NEBIUS_MOCKER_AUTHN": "sometimes"},
		{"NEBIUS_MOCKER_DEV_TOKENS": "no-equals-sign"},
		{"NEBIUS_MOCKER_OP_DELAY": "-1s"},
		{"NEBIUS_MOCKER_IDS": "uuid"},
		{"NEBIUS_MOCKER_AUTHZ": "maybe"},
		{"NEBIUS_MOCKER_REGIONS": "eu-north1"},
		{"NEBIUS_MOCKER_REGIONS": "eu-north1=e00,eu-west1=e00"},
	} {
		if _, err := FromLookup(lookup(env)); err == nil {
			t.Errorf("FromLookup(%v) succeeded, want error", env)
		}
	}
}

func TestFromLookupObjStore(t *testing.T) {
	t.Parallel()
	if got := Default().ObjStore.Backend; got != ObjStoreMemory {
		t.Fatalf("default objstore backend = %v, want memory", got)
	}
	c, err := FromLookup(lookup(map[string]string{
		"NEBIUS_MOCKER_OBJSTORE":            "rustfs",
		"NEBIUS_MOCKER_OBJSTORE_ENDPOINT":   "http://rustfs:9000",
		"NEBIUS_MOCKER_OBJSTORE_ACCESS_KEY": "ak",
		"NEBIUS_MOCKER_OBJSTORE_SECRET_KEY": "sk",
		"NEBIUS_MOCKER_OBJSTORE_REGION":     "eu-north1",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	want := ObjStoreConfig{Backend: ObjStoreRustFS, Endpoint: "http://rustfs:9000", AccessKey: "ak", SecretKey: "sk", Region: "eu-north1"}
	if c.ObjStore != want {
		t.Fatalf("ObjStore = %+v, want %+v", c.ObjStore, want)
	}
	if _, err := FromLookup(lookup(map[string]string{"NEBIUS_MOCKER_OBJSTORE": "s3"})); err == nil {
		t.Error("FromLookup(OBJSTORE=s3) succeeded, want error")
	}
}

func TestFromLookupStorage(t *testing.T) {
	t.Parallel()
	if d := Default().Storage; d.S3Listen != ":4580" || d.Region != "eu-north1" || d.PurgeGrace != 168*time.Hour {
		t.Fatalf("default storage config = %+v", d)
	}
	c, err := FromLookup(lookup(map[string]string{
		"NEBIUS_MOCKER_STORAGE_S3_LISTEN":   "127.0.0.1:0",
		"NEBIUS_MOCKER_STORAGE_S3_DOMAIN":   "s3.local",
		"NEBIUS_MOCKER_STORAGE_REGION":      "eu-west1",
		"NEBIUS_MOCKER_STORAGE_PURGE_GRACE": "1h",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	want := StorageConfig{S3Listen: "127.0.0.1:0", S3Domain: "s3.local", Region: "eu-west1", PurgeGrace: time.Hour}
	if c.Storage != want {
		t.Fatalf("Storage = %+v, want %+v", c.Storage, want)
	}
	if _, err := FromLookup(lookup(map[string]string{"NEBIUS_MOCKER_STORAGE_PURGE_GRACE": "0s"})); err == nil {
		t.Error("FromLookup(PURGE_GRACE=0s) succeeded, want error")
	}
}
