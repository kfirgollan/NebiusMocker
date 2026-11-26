//go:build e2e_storage

package storage

import (
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

// TestInventoryCLIFlow runs the documented "Configure inventory" flow
// (docs.nebius.com/object-storage/buckets/configure-inventory) with the
// nebius CLI and the AWS CLI against the mocker.
func TestInventoryCLIFlow(t *testing.T) {
	clk := clock.NewManual(clock.Real{}.Now())
	e := startWithClock(t, clk)
	cli := newNebiusCLI(t, e)
	keyID, secret, _ := e.grantedKey(t, "editor")
	aws := e.awsCLI(t, keyID, secret)
	invSvc := storagesvc.NewInventoryService(e.sdk)

	srcName, dstName := unique("inv-cli-src"), unique("inv-cli-dst")
	nb(t, cli, "storage", "bucket", "create", "--name", srcName, "--versioning-policy", "enabled")
	nb(t, cli, "storage", "bucket", "create", "--name", dstName)

	dir := t.TempDir()
	obj := filepath.Join(dir, "o.txt")
	writeFile(t, obj, "inventory cli object\n")
	for _, k := range []string{"logs/one.txt", "logs/two.txt", "other/three.txt"} {
		aw(t, aws, "s3", "cp", obj, "s3://"+srcName+"/"+k)
	}

	// Documented flow, verbatim.
	sourceID := nb(t, cli, "storage", "bucket", "get-by-name", "--name", srcName, "--format", "jsonpath={.metadata.id}")
	destID := nb(t, cli, "storage", "bucket", "get-by-name", "--name", dstName, "--format", "jsonpath={.metadata.id}")
	const cfgName = "nightly"
	nb(t, cli, "storage", "inventory", "create",
		"--parent-id", sourceID, "--name", cfgName, "--destination-bucket-id", destID,
		"--destination-prefix", "reports/", "--source-prefix", "logs/", "--schedule", "daily",
		"--output-format", "csv_gzip", "--enabled=true", "--noncurrent-versions=true",
		"--optional-fields", "last_access,checksum_algorithms")
	listed := nbJSON(t, cli, "storage", "inventory", "list", "--parent-id", sourceID)
	if items, _ := listed["items"].([]any); len(items) != 1 {
		t.Fatalf("inventory list = %v, want one item", listed)
	}
	got := nbJSON(t, cli, "storage", "inventory", "get-by-name", "--parent-id", sourceID, "--name", cfgName)
	invID := jq(t, got, "metadata.id")
	if id := nb(t, cli, "storage", "inventory", "get-by-name", "--parent-id", sourceID, "--name", cfgName, "--format", "jsonpath={.metadata.id}"); id != invID {
		t.Errorf("get-by-name jsonpath id = %q, want %q", id, invID)
	}
	spec, _ := got["spec"].(map[string]any)
	if spec["destination_bucket_id"] != destID || spec["destination_prefix"] != "reports/" || spec["source_prefix"] != "logs/" ||
		spec["schedule"] != "DAILY" || spec["output_format"] != "CSV_GZIP" || spec["noncurrent_versions"] != true || spec["enabled"] != true {
		t.Errorf("inventory spec = %v", spec)
	}
	mustContain(t, "optional_fields", jsonText(t, spec["optional_fields"]), "LAST_ACCESS", "CHECKSUM_ALGORITHMS")
	if jq(t, got, "metadata.parent_id") != sourceID {
		t.Errorf("parent_id != %s", sourceID)
	}

	// Wait for the first report, then read it with the AWS CLI.
	poll(t, "the first report", func() { clk.Advance(time.Minute) }, func() bool {
		inv, err := invSvc.Get(t.Context(), &storagepb.GetInventoryRequest{Id: invID})
		return err == nil && inv.GetStatus().GetLastSuccess() != nil
	})
	ls := aw(t, aws, "s3", "ls", "s3://"+dstName+"/reports/", "--recursive")
	var manifestKey, dataKey string
	for _, line := range strings.Split(strings.TrimSpace(ls), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		switch k := f[len(f)-1]; {
		case strings.HasSuffix(k, "/manifest.json"):
			manifestKey = k
		case strings.HasSuffix(k, ".csv.gz"):
			dataKey = k
		}
	}
	if manifestKey == "" || dataKey == "" || !strings.HasPrefix(manifestKey, "reports/"+srcName+"/"+cfgName+"/") {
		t.Fatalf("aws s3 ls --recursive s3://%s/reports/ =\n%s\nwant a manifest.json and a .csv.gz under reports/%s/%s/", dstName, ls, srcName, cfgName)
	}
	mustContain(t, "ls", ls, "/manifest.checksum", "/hive/dt=", "/symlink.txt")

	var man invManifest
	if err := json.Unmarshal([]byte(aw(t, aws, "s3", "cp", "s3://"+dstName+"/"+manifestKey, "-")), &man); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	if man.SourceBucket != srcName || man.DestinationBucket != dstName || man.FileFormat != "CSV" || len(man.Files) != 1 || man.Files[0].Key != dataKey {
		t.Errorf("manifest = %+v", man)
	}
	local := filepath.Join(dir, "data.csv.gz")
	aw(t, aws, "s3", "cp", "s3://"+dstName+"/"+dataKey, local)
	raw, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != man.Files[0].Size || invMD5(raw) != man.Files[0].MD5Checksum {
		t.Errorf("downloaded data: size %d md5 %s, manifest says %+v", len(raw), invMD5(raw), man.Files[0])
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	cols := strings.Split(man.FileSchema, ", ")
	if !strings.Contains(man.FileSchema, "LastAccessDate") || !strings.Contains(man.FileSchema, "ChecksumAlgorithm") {
		t.Errorf("fileSchema = %q, want the optional columns LastAccessDate and ChecksumAlgorithm", man.FileSchema)
	}
	r := csv.NewReader(bytes.NewReader(plain))
	r.FieldsPerRecord = len(cols)
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, rec := range recs {
		for i, c := range cols {
			if c == "Key" {
				keys = append(keys, rec[i])
			}
		}
	}
	if joined := strings.Join(keys, ","); len(keys) != 2 || !strings.Contains(joined, "logs%2Fone.txt") || !strings.Contains(joined, "logs%2Ftwo.txt") {
		t.Errorf("report keys = %v, want logs/one.txt and logs/two.txt only (source prefix)", keys)
	}

	// Update: new prefix, weekly, parquet.
	nb(t, cli, "storage", "inventory", "update", "--id", invID,
		"--destination-prefix", "moved/", "--schedule", "weekly", "--output-format", "parquet_snappy")
	inv, err := invSvc.Get(t.Context(), &storagepb.GetInventoryRequest{Id: invID})
	if err != nil {
		t.Fatal(err)
	}
	if s := inv.GetSpec(); s.GetDestinationPrefix() != "moved/" || s.GetSchedule() != storagepb.InventorySpec_WEEKLY ||
		s.GetOutputFormat() != storagepb.InventorySpec_PARQUET_SNAPPY || s.GetSourcePrefix() != "logs/" || !s.GetEnabled() || !s.GetNoncurrentVersions() {
		t.Errorf("spec after update = %v; the update must change only the three flags", s)
	}

	// Disable and re-enable.
	nb(t, cli, "storage", "inventory", "update", "--id", invID, "--enabled=false")
	if inv, err = invSvc.Get(t.Context(), &storagepb.GetInventoryRequest{Id: invID}); err != nil || inv.GetSpec().GetEnabled() {
		t.Errorf("after --enabled=false: enabled = %v, %v (gosdk)", inv.GetSpec().GetEnabled(), err)
	}
	// The CLI omits false booleans from its output.
	if sp, _ := nbJSON(t, cli, "storage", "inventory", "get", "--id", invID)["spec"].(map[string]any); sp["enabled"] == true {
		t.Errorf("nebius get after --enabled=false: spec = %v", sp)
	}
	nb(t, cli, "storage", "inventory", "update", "--id", invID, "--enabled=true")
	if inv, err = invSvc.Get(t.Context(), &storagepb.GetInventoryRequest{Id: invID}); err != nil || !inv.GetSpec().GetEnabled() {
		t.Errorf("after --enabled=true: enabled = %v, %v (gosdk)", inv.GetSpec().GetEnabled(), err)
	}

	// Delete: the configuration goes, the delivered report stays readable.
	nb(t, cli, "storage", "inventory", "delete", "--id", invID)
	if out := nbFails(t, cli, "storage", "inventory", "get", "--id", invID); !strings.Contains(strings.ToLower(out), "not") {
		t.Errorf("get after delete: %s, want a not-found error", out)
	}
	mustContain(t, "ls after delete", aw(t, aws, "s3", "ls", "s3://"+dstName+"/reports/", "--recursive"), manifestKey, dataKey)
}

func jsonText(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
