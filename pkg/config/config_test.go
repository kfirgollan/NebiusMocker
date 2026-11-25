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
		"NEBIUS_MOCKER_REGIONS":                "eu-north1=e00, us-central1=u00",
		"NEBIUS_MOCKER_COMPUTE_IMAGES_FILE":    "/tmp/images.yaml",
		"NEBIUS_MOCKER_COMPUTE_PLATFORMS_FILE": "/tmp/platforms.yaml",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	want := []Region{{"eu-north1", "e00"}, {"us-central1", "u00"}}
	if len(c.Regions) != 2 || c.Regions[0] != want[0] || c.Regions[1] != want[1] || c.ComputeImagesFile != "/tmp/images.yaml" || c.ComputePlatformsFile != "/tmp/platforms.yaml" {
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

func TestFromLookupMysteryBox(t *testing.T) {
	t.Parallel()
	if got := Default().MysteryBox; got != (MysteryBoxConfig{}) {
		t.Fatalf("default MysteryBox = %+v, want zero (module defaults)", got)
	}
	c, err := FromLookup(lookup(map[string]string{
		"NEBIUS_MOCKER_MYSTERYBOX_MAX_ENTRIES":     "10",
		"NEBIUS_MOCKER_MYSTERYBOX_MAX_VALUE_BYTES": "1024",
		"NEBIUS_MOCKER_MYSTERYBOX_MAX_VERSIONS":    "5",
		"NEBIUS_MOCKER_MYSTERYBOX_PURGE_DELAY":     "1h",
		"NEBIUS_MOCKER_MYSTERYBOX_KEY":             "a2V5",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	want := MysteryBoxConfig{MaxEntries: 10, MaxValueBytes: 1024, MaxVersions: 5, PurgeDelay: time.Hour, Key: "a2V5"}
	if c.MysteryBox != want {
		t.Fatalf("MysteryBox = %+v, want %+v", c.MysteryBox, want)
	}
	for _, bad := range []string{"0", "-3", "many"} {
		if _, err := FromLookup(lookup(map[string]string{"NEBIUS_MOCKER_MYSTERYBOX_MAX_ENTRIES": bad})); err == nil {
			t.Errorf("FromLookup(MAX_ENTRIES=%s) succeeded, want error", bad)
		}
	}
}

func TestFromLookupAudit(t *testing.T) {
	t.Parallel()
	if got := Default().Audit; got != (AuditConfig{MaxEvents: 50_000}) {
		t.Fatalf("default audit = %+v", got)
	}
	c, err := FromLookup(lookup(map[string]string{
		"NEBIUS_MOCKER_AUDIT_MAX_EVENTS":      "1000",
		"NEBIUS_MOCKER_AUDIT_READS":           "true",
		"NEBIUS_MOCKER_AUDIT_EXPORT_DURATION": "2s",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if want := (AuditConfig{MaxEvents: 1000, Reads: true, ExportDuration: 2 * time.Second}); c.Audit != want {
		t.Fatalf("Audit = %+v, want %+v", c.Audit, want)
	}
	for _, env := range []map[string]string{
		{"NEBIUS_MOCKER_AUDIT_MAX_EVENTS": "0"},
		{"NEBIUS_MOCKER_AUDIT_MAX_EVENTS": "many"},
		{"NEBIUS_MOCKER_AUDIT_READS": "sometimes"},
		{"NEBIUS_MOCKER_AUDIT_EXPORT_DURATION": "-1s"},
	} {
		if _, err := FromLookup(lookup(env)); err == nil {
			t.Errorf("FromLookup(%v) succeeded, want error", env)
		}
	}
}
