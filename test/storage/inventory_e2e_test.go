//go:build e2e_storage

package storage

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/parquet-go/parquet-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

// --- helpers (prefix inv) ----------------------------------------------

const invTimeLayout = "2006-01-02T15:04:05.000Z"

// invEnv is a mocker on a manual clock with an editor key and S3 client.
type invEnv struct {
	*env
	clk *clock.Manual
	s3  *s3.Client
	inv storagesvc.InventoryService
}

func invStart(t *testing.T) *invEnv {
	t.Helper()
	clk := clock.NewManual(clock.Real{}.Now())
	e := startWithClock(t, clk)
	keyID, secret, _ := e.grantedKey(t, "editor")
	return &invEnv{env: e, clk: clk, s3: e.s3Client(keyID, secret), inv: storagesvc.NewInventoryService(e.sdk)}
}

// create creates an inventory through gosdk and returns its id.
func (e *invEnv) create(t *testing.T, sourceID, name string, spec *storagepb.InventorySpec) string {
	t.Helper()
	return wait(t)(e.inv.Create(t.Context(), &storagepb.CreateInventoryRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: sourceID, Name: name}, Spec: spec,
	})).ResourceID()
}

func (e *invEnv) get(t *testing.T, id string) *storagepb.Inventory {
	t.Helper()
	got, err := e.inv.Get(t.Context(), &storagepb.GetInventoryRequest{Id: id})
	if err != nil {
		t.Fatalf("Get inventory %s: %v", id, err)
	}
	return got
}

// reported waits (advancing the mocker clock a minute per try) until the
// inventory has a successful report with NO_ERROR.
func (e *invEnv) reported(t *testing.T, id string) *storagepb.InventoryStatus {
	t.Helper()
	var st *storagepb.InventoryStatus
	poll(t, "inventory "+id+" to report", func() { e.clk.Advance(time.Minute) }, func() bool {
		st = e.get(t, id).GetStatus()
		return st.GetLastSuccess() != nil
	})
	if st.GetErrorCode() != storagepb.InventoryStatus_NO_ERROR {
		t.Fatalf("inventory %s error_code = %v, want NO_ERROR", id, st.GetErrorCode())
	}
	return st
}

// setEnabled sets spec.enabled through gosdk with an X-ResetMask (false is a zero value).
func (e *invEnv) setEnabled(t *testing.T, id string, enabled bool) {
	t.Helper()
	cur := e.get(t, id)
	spec := cur.GetSpec()
	spec.Enabled = enabled
	ctx, err := grpcheader.AddResetMaskToOutgoingContext(t.Context(), mask.ParseMust("spec.enabled"))
	if err != nil {
		t.Fatal(err)
	}
	wait(t)(e.inv.Update(ctx, &storagepb.UpdateInventoryRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: cur.GetMetadata().GetParentId()}, Spec: spec,
	}))
}

func invList(t *testing.T, c *s3.Client, bucket, prefix string) []string {
	t.Helper()
	var keys []string
	p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatalf("ListObjectsV2 %s/%s: %v", bucket, prefix, err)
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	return keys
}

func invGet(t *testing.T, c *s3.Client, bucket, key string) []byte {
	t.Helper()
	out, err := c.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("GetObject %s/%s: %v", bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }() // read fully below
	b, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("read %s/%s: %v", bucket, key, err)
	}
	return b
}

func invMD5(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

// invManifest is manifest.json as the AWS inventory defines it.
type invManifest struct {
	SourceBucket      string `json:"sourceBucket"`
	DestinationBucket string `json:"destinationBucket"`
	Version           string `json:"version"`
	CreationTimestamp string `json:"creationTimestamp"`
	FileFormat        string `json:"fileFormat"`
	FileSchema        string `json:"fileSchema"`
	Files             []struct {
		Key         string `json:"key"`
		Size        int    `json:"size"`
		MD5Checksum string `json:"MD5checksum"`
	} `json:"files"`
}

// invReport is one report read back through the S3 front.
type invReport struct {
	man     invManifest
	data    []byte
	dataKey string
}

var (
	invManifestKeyRE = regexp.MustCompile(`/\d{4}-\d{2}-\d{2}T\d{2}-\d{2}Z/manifest\.json$`)
	invSymlinkKeyRE  = regexp.MustCompile(`/hive/dt=\d{4}-\d{2}-\d{2}-\d{2}-\d{2}/symlink\.txt$`)
)

// invReadReport reads the single report under <prefix><src>/<name>/ in dest
// (S3 front only) and checks the documented layout, the manifest and the
// checksums.
func invReadReport(t *testing.T, c *s3.Client, dest, prefix, src, name, wantFormat string) invReport {
	t.Helper()
	base := prefix + src + "/" + name + "/"
	keys := invList(t, c, dest, base)
	var manKeys, sumKeys, dataKeys, linkKeys []string
	for _, k := range keys {
		switch {
		case invManifestKeyRE.MatchString(k):
			manKeys = append(manKeys, k)
		case strings.HasSuffix(k, "/manifest.checksum"):
			sumKeys = append(sumKeys, k)
		case strings.HasPrefix(k, base+"data/"):
			dataKeys = append(dataKeys, k)
		case invSymlinkKeyRE.MatchString(k):
			linkKeys = append(linkKeys, k)
		default:
			t.Errorf("unexpected key under %s: %s", base, k)
		}
	}
	if len(manKeys) != 1 || len(sumKeys) != 1 || len(dataKeys) != 1 || len(linkKeys) != 1 {
		t.Fatalf("keys under %s = %v; want one manifest.json, manifest.checksum, data file and hive symlink.txt", base, keys)
	}
	if want := strings.TrimSuffix(manKeys[0], "manifest.json") + "manifest.checksum"; sumKeys[0] != want {
		t.Errorf("manifest.checksum key = %s, want %s", sumKeys[0], want)
	}
	rep := invReport{dataKey: dataKeys[0]}

	manBytes := invGet(t, c, dest, manKeys[0])
	if err := json.Unmarshal(manBytes, &rep.man); err != nil {
		t.Fatalf("decode manifest.json: %v\n%s", err, manBytes)
	}
	m := rep.man
	if m.SourceBucket != src || m.DestinationBucket != dest || m.Version != "2016-11-30" || m.FileFormat != wantFormat || m.FileSchema == "" {
		t.Errorf("manifest = %+v; want source %s, destination %s, version 2016-11-30, format %s, a schema", m, src, dest, wantFormat)
	}
	if ms, err := strconv.ParseInt(m.CreationTimestamp, 10, 64); err != nil || ms <= 0 {
		t.Errorf("manifest creationTimestamp = %q, want epoch milliseconds", m.CreationTimestamp)
	}
	if got := strings.TrimSpace(string(invGet(t, c, dest, sumKeys[0]))); got != invMD5(manBytes) {
		t.Errorf("manifest.checksum = %q, want md5(manifest.json) = %s", got, invMD5(manBytes))
	}
	if len(m.Files) != 1 || m.Files[0].Key != dataKeys[0] {
		t.Fatalf("manifest files = %+v, want exactly %s", m.Files, dataKeys[0])
	}
	rep.data = invGet(t, c, dest, m.Files[0].Key)
	if m.Files[0].Size != len(rep.data) || m.Files[0].MD5Checksum != invMD5(rep.data) {
		t.Errorf("manifest file %+v does not match the downloaded data (size %d, md5 %s)", m.Files[0], len(rep.data), invMD5(rep.data))
	}
	if got, want := string(invGet(t, c, dest, linkKeys[0])), "s3://"+dest+"/"+m.Files[0].Key; strings.TrimSpace(got) != want {
		t.Errorf("symlink.txt = %q, want %q", got, want)
	}
	return rep
}

// invRow is a decoded inventory row keyed by column name, all values as
// strings ("" for null). CSV keys are URL-decoded.
type invRow map[string]string

// invCSVRows decodes a gzip CSV report with the columns of its manifest schema.
func invCSVRows(t *testing.T, rep invReport) []invRow {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(rep.data))
	if err != nil {
		t.Fatalf("gunzip report: %v", err)
	}
	cols := strings.Split(rep.man.FileSchema, ", ")
	r := csv.NewReader(zr)
	r.FieldsPerRecord = len(cols)
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatalf("decode CSV report: %v", err)
	}
	var rows []invRow
	for _, rec := range recs {
		row := invRow{}
		for i, c := range cols {
			row[c] = rec[i]
		}
		raw := row["Key"]
		if strings.ContainsAny(raw, " \u00ef\u043a") {
			t.Errorf("CSV key %q is not URL-encoded", raw)
		}
		k, err := url.QueryUnescape(raw)
		if err != nil {
			t.Fatalf("CSV key %q: %v", raw, err)
		}
		row["Key"] = k
		rows = append(rows, row)
	}
	return rows
}

// invParquetRows decodes a Parquet report; columns are named as in the file.
func invParquetRows(t *testing.T, rep invReport) []invRow {
	t.Helper()
	f, err := parquet.OpenFile(bytes.NewReader(rep.data), int64(len(rep.data)))
	if err != nil {
		t.Fatalf("open parquet report: %v", err)
	}
	var rows []invRow
	reader := parquet.NewReader(f)
	for {
		rec := map[string]any{}
		if err := reader.Read(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("read parquet row: %v", err)
		}
		row := invRow{}
		for k, v := range rec {
			switch x := v.(type) {
			case nil:
				row[k] = ""
			case string:
				row[k] = x
			case time.Time:
				row[k] = x.UTC().Format(invTimeLayout)
			default:
				row[k] = fmt.Sprint(x)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// invVersion is a version as listed through the S3 front.
type invVersion struct {
	key, id, etag, class string
	size                 int64
	latest, marker       bool
}

func invVersions(t *testing.T, c *s3.Client, bucket string) []invVersion {
	t.Helper()
	var out []invVersion
	p := s3.NewListObjectVersionsPaginator(c, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	for p.HasMorePages() {
		page, err := p.NextPage(t.Context())
		if err != nil {
			t.Fatalf("ListObjectVersions: %v", err)
		}
		for i := range page.Versions {
			v := &page.Versions[i]
			class := string(v.StorageClass)
			if class == "" {
				class = "STANDARD"
			}
			out = append(out, invVersion{
				key: aws.ToString(v.Key), id: aws.ToString(v.VersionId), etag: strings.Trim(aws.ToString(v.ETag), `"`), class: class,
				size: aws.ToInt64(v.Size), latest: aws.ToBool(v.IsLatest),
			})
		}
		for _, m := range page.DeleteMarkers {
			out = append(out, invVersion{key: aws.ToString(m.Key), id: aws.ToString(m.VersionId), latest: aws.ToBool(m.IsLatest), marker: true})
		}
	}
	return out
}

// invChecksumNames is the set of checksum algorithms HeadObject reports for a version.
func invChecksumNames(t *testing.T, c *s3.Client, bucket string, v invVersion) []string {
	t.Helper()
	h, err := c.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(v.key), VersionId: aws.String(v.id), ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		t.Fatalf("HeadObject %s: %v", v.key, err)
	}
	var names []string
	for name, val := range map[string]*string{
		"CRC32": h.ChecksumCRC32, "CRC32C": h.ChecksumCRC32C, "CRC64NVME": h.ChecksumCRC64NVME,
		"SHA1": h.ChecksumSHA1, "SHA256": h.ChecksumSHA256,
	} {
		if val != nil {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func invSplit(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	slices.Sort(parts)
	return parts
}

// invIDs are the sorted "key|version" identities of the versions kept.
func invIDs(vs []invVersion, keep func(invVersion) bool) []string {
	var out []string
	for _, v := range vs {
		if keep(v) {
			out = append(out, v.key+"|"+v.id)
		}
	}
	slices.Sort(out)
	return out
}

// invRowIDs are the sorted identities of rows; versioned reports have a VersionId column.
func invRowIDs(rows []invRow, keyCol, versionCol string) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r[keyCol]+"|"+r[versionCol])
	}
	slices.Sort(out)
	return out
}

// invRowOf finds the row of version v.
func invRowOf(t *testing.T, rows []invRow, keyCol, versionCol string, v invVersion) invRow {
	t.Helper()
	for _, r := range rows {
		if r[keyCol] == v.key && r[versionCol] == v.id {
			return r
		}
	}
	t.Fatalf("no row for %s (version %s)", v.key, v.id)
	return nil
}

// invSeed uploads the source objects through the S3 front, with the AWS SDK.
func invSeed(t *testing.T, c *s3.Client, bucket string) {
	t.Helper()
	ctx := t.Context()
	putObj(t, c, bucket, "logs/a b.txt", []byte("hello inventory"))
	putObj(t, c, bucket, "logs/\u00fcn\u00ef/\u043a\u043b\u044e\u0447.txt", []byte("unicode key"))
	putObj(t, c, bucket, "logs/over.txt", []byte("first"))
	putObj(t, c, bucket, "logs/over.txt", []byte("second version"))
	putObj(t, c, bucket, "logs/deleted.txt", []byte("soon gone"))
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("logs/deleted.txt")}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("logs/tagged.txt"), Body: strings.NewReader("tagged"), Tagging: aws.String("env=prod&team=core"),
	}); err != nil {
		t.Fatalf("PutObject tagged: %v", err)
	}
	// An upload with a client checksum (what the AWS CLI sends by default).
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("logs/crc.txt"), Body: strings.NewReader("checksummed"),
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
	}); err != nil {
		t.Fatalf("PutObject with CRC32: %v", err)
	}
	if err := tryPut(t, c, bucket, "logs/fast.bin", bytes.Repeat([]byte("f"), 300), "ENHANCED_THROUGHPUT"); err != nil {
		t.Fatalf("PutObject ENHANCED_THROUGHPUT: %v", err)
	}
	mp, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("logs/mp.bin")})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	var parts []types.CompletedPart
	for i, n := range []int{5 << 20, 1000} {
		up, err := c.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(bucket), Key: aws.String("logs/mp.bin"), UploadId: mp.UploadId, PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(make([]byte, n)),
		})
		if err != nil {
			t.Fatalf("UploadPart %d: %v", i+1, err)
		}
		parts = append(parts, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(int32(i + 1))})
	}
	if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String("logs/mp.bin"), UploadId: mp.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	putObj(t, c, bucket, "other/outside.txt", []byte("outside the prefix"))
}

// --- tests -------------------------------------------------------------

func TestInventoryReportsEndToEnd(t *testing.T) {
	e := invStart(t)
	srcName, dstA, dstB := unique("inv-src"), unique("inv-dst-a"), unique("inv-dst-b")
	// A lifecycle rule so the LIFECYCLE_EXPIRATION column has something to report.
	srcID := e.createBucket(t, srcName, &storagepb.BucketSpec{
		VersioningPolicy: storagepb.VersioningPolicy_ENABLED,
		LifecycleConfiguration: &storagepb.LifecycleConfiguration{Rules: []*storagepb.LifecycleRule{{
			Id: "expire-logs", Status: storagepb.LifecycleRule_ENABLED, Filter: &storagepb.LifecycleFilter{Prefix: "logs/"},
			Expiration: &storagepb.LifecycleExpiration{ExpiredWith: &storagepb.LifecycleExpiration_Days{Days: 30}},
		}}},
	})
	dstAID := e.createBucket(t, dstA, &storagepb.BucketSpec{})
	dstBID := e.createBucket(t, dstB, &storagepb.BucketSpec{})
	invSeed(t, e.s3, srcName)

	idA := e.create(t, srcID, "full-csv", &storagepb.InventorySpec{
		Enabled: true, SourcePrefix: "logs/", DestinationBucketId: dstAID, DestinationPrefix: "reports/",
		OutputFormat: storagepb.InventorySpec_CSV_GZIP, Schedule: storagepb.InventorySpec_DAILY, NoncurrentVersions: true,
		OptionalFields: []storagepb.InventorySpec_OptionalField{
			storagepb.InventorySpec_LAST_ACCESS, storagepb.InventorySpec_CHECKSUM_ALGORITHMS, storagepb.InventorySpec_INTELLIGENT_TIER,
			storagepb.InventorySpec_IS_MULTIPART_UPLOAD, storagepb.InventorySpec_LIFECYCLE_EXPIRATION, storagepb.InventorySpec_TAGS,
		},
	})
	idB := e.create(t, srcID, "current-parquet", &storagepb.InventorySpec{
		Enabled: true, DestinationBucketId: dstBID,
		OutputFormat: storagepb.InventorySpec_PARQUET_SNAPPY, Schedule: storagepb.InventorySpec_WEEKLY,
	})
	idC := e.create(t, srcID, "to-self", &storagepb.InventorySpec{
		Enabled: true, SourcePrefix: "logs/", DestinationBucketId: srcID, DestinationPrefix: "self-reports/",
		OutputFormat: storagepb.InventorySpec_CSV_GZIP, Schedule: storagepb.InventorySpec_DAILY,
	})

	// Read one object after the LAST_ACCESS configuration exists.
	if got := invGet(t, e.s3, srcName, "logs/a b.txt"); string(got) != "hello inventory" {
		t.Fatalf("GetObject = %q", got)
	}
	// Listing the versions (and HEAD below) are not reads.
	versions := invVersions(t, e.s3, srcName)
	inLogs := func(v invVersion) bool { return strings.HasPrefix(v.key, "logs/") }

	for _, id := range []string{idA, idB, idC} {
		e.reported(t, id)
	}

	t.Run("CSV with all optional fields and noncurrent versions", func(t *testing.T) {
		rep := invReadReport(t, e.s3, dstA, "reports/", srcName, "full-csv", "CSV")
		if !strings.HasSuffix(rep.dataKey, ".csv.gz") {
			t.Errorf("data key = %s", rep.dataKey)
		}
		const schema = "Bucket, Key, VersionId, IsLatest, IsDeleteMarker, Size, LastModifiedDate, ETag, StorageClass, " +
			"IsMultipartUploaded, IntelligentTieringAccessTier, ChecksumAlgorithm, LastAccessDate, LifecycleExpirationDate, Tags"
		if rep.man.FileSchema != schema {
			t.Errorf("fileSchema = %q, want %q", rep.man.FileSchema, schema)
		}
		rows := invCSVRows(t, rep)
		if got, want := invRowIDs(rows, "Key", "VersionId"), invIDs(versions, inLogs); !slices.Equal(got, want) {
			t.Fatalf("report rows = %v, want %v", got, want)
		}
		observed := map[string]string{}
		var noncurrent, markers int
		for _, v := range versions {
			if !inLogs(v) {
				continue
			}
			row := invRowOf(t, rows, "Key", "VersionId", v)
			if row["Bucket"] != srcName || row["IsLatest"] != strconv.FormatBool(v.latest) || row["IsDeleteMarker"] != strconv.FormatBool(v.marker) {
				t.Errorf("%s/%s: row %v disagrees with the listing %+v", v.key, v.id, row, v)
			}
			if !v.latest {
				noncurrent++
			}
			if v.marker {
				markers++
				for _, c := range []string{"Size", "ETag", "ChecksumAlgorithm", "Tags", "LastAccessDate", "LifecycleExpirationDate"} {
					if row[c] != "" {
						t.Errorf("delete marker %s: %s = %q, want empty", v.key, c, row[c])
					}
				}
				continue
			}
			if row["Size"] != strconv.FormatInt(v.size, 10) || strings.Trim(row["ETag"], `"`) != v.etag || row["StorageClass"] != v.class {
				t.Errorf("%s: size/etag/class = %s/%s/%s, listing says %d/%s/%s", v.key, row["Size"], row["ETag"], row["StorageClass"], v.size, v.etag, v.class)
			}
			if v.key == "logs/fast.bin" && row["StorageClass"] != "ENHANCED_THROUGHPUT" {
				t.Errorf("logs/fast.bin StorageClass = %q, want ENHANCED_THROUGHPUT", row["StorageClass"])
			}
			if got, want := row["IsMultipartUploaded"], strconv.FormatBool(v.key == "logs/mp.bin"); got != want {
				t.Errorf("%s: IsMultipartUploaded = %s, want %s", v.key, got, want)
			}
			wantTags := ""
			if v.key == "logs/tagged.txt" {
				wantTags = "env=prod&team=core"
			}
			if row["Tags"] != wantTags {
				t.Errorf("%s: Tags = %q, want %q", v.key, row["Tags"], wantTags)
			}
			// LastAccessDate is set only for the object that was read.
			if got := row["LastAccessDate"] != ""; got != (v.key == "logs/a b.txt") {
				t.Errorf("%s: LastAccessDate = %q; must be set only for the read object", v.key, row["LastAccessDate"])
			}
			// The rule expires current logs/ versions 30 days after modification, rounded up to midnight UTC.
			wantExpiry := ""
			if v.latest {
				mod, err := time.Parse(invTimeLayout, row["LastModifiedDate"])
				if err != nil {
					t.Fatalf("%s: LastModifiedDate %q: %v", v.key, row["LastModifiedDate"], err)
				}
				due := mod.Add(30 * 24 * time.Hour)
				if mid := due.Truncate(24 * time.Hour); !mid.Equal(due) {
					due = mid.Add(24 * time.Hour)
				}
				wantExpiry = due.Format(invTimeLayout)
			}
			if row["LifecycleExpirationDate"] != wantExpiry {
				t.Errorf("%s: LifecycleExpirationDate = %q, want %q", v.key, row["LifecycleExpirationDate"], wantExpiry)
			}
			// ChecksumAlgorithm must agree with what HeadObject reports through the front.
			if got, want := invSplit(row["ChecksumAlgorithm"]), invChecksumNames(t, e.s3, srcName, v); !slices.Equal(got, want) {
				t.Errorf("%s: report ChecksumAlgorithm %v, HeadObject (ChecksumMode ENABLED) reports %v", v.key, got, want)
			}
			observed[v.key] = row["ChecksumAlgorithm"]
		}
		t.Logf("observed ChecksumAlgorithm per key (report column): %q", observed)
		if observed["logs/crc.txt"] != "CRC32" {
			t.Errorf("logs/crc.txt ChecksumAlgorithm = %q, want CRC32 (uploaded with a CRC32 checksum)", observed["logs/crc.txt"])
		}
		if noncurrent != 2 || markers != 1 { // the first over.txt version and the version under the delete marker
			t.Errorf("noncurrent versions %d, delete markers %d; want 2 and 1", noncurrent, markers)
		}
	})

	t.Run("Parquet with current versions only", func(t *testing.T) {
		rep := invReadReport(t, e.s3, dstB, "", srcName, "current-parquet", "Parquet")
		if !strings.HasSuffix(rep.dataKey, ".parquet") {
			t.Errorf("data key = %s", rep.dataKey)
		}
		for _, col := range []string{"bucket", "key", "size", "last_modified_date", "e_tag", "storage_class"} {
			if !strings.Contains(rep.man.FileSchema, col) {
				t.Errorf("fileSchema lacks column %s:\n%s", col, rep.man.FileSchema)
			}
		}
		for _, col := range []string{"version_id", "is_latest", "tags", "last_access_date"} {
			if strings.Contains(rep.man.FileSchema, col) {
				t.Errorf("fileSchema has column %s without noncurrent_versions or optional fields:\n%s", col, rep.man.FileSchema)
			}
		}
		var rows []invRow
		for _, r := range invParquetRows(t, rep) {
			if !strings.HasPrefix(r["key"], "self-reports/") { // the to-self inventory may have run first
				rows = append(rows, r)
			}
		}
		current := func(v invVersion) bool { return v.latest && !v.marker }
		var want []string
		for _, v := range versions {
			if current(v) {
				want = append(want, v.key+"|")
			}
		}
		slices.Sort(want)
		if got := invRowIDs(rows, "key", "none"); !slices.Equal(got, want) {
			t.Fatalf("parquet rows = %v, want %v", got, want)
		}
		for _, v := range versions {
			if !current(v) {
				continue
			}
			row := invRowOf(t, rows, "key", "none", invVersion{key: v.key})
			if row["bucket"] != srcName || row["size"] != strconv.FormatInt(v.size, 10) || strings.Trim(row["e_tag"], `"`) != v.etag || row["storage_class"] != v.class {
				t.Errorf("%s: row %v disagrees with listing %+v", v.key, row, v)
			}
			if row["last_modified_date"] == "" {
				t.Errorf("%s: last_modified_date is null", v.key)
			}
		}
	})

	t.Run("destination is the source bucket", func(t *testing.T) {
		rep := invReadReport(t, e.s3, srcName, "self-reports/", srcName, "to-self", "CSV")
		rows := invCSVRows(t, rep)
		var want []string
		for _, v := range versions {
			if inLogs(v) && v.latest && !v.marker {
				want = append(want, v.key+"|")
			}
		}
		slices.Sort(want)
		if got := invRowIDs(rows, "Key", "none"); !slices.Equal(got, want) {
			t.Errorf("rows = %v, want %v", got, want)
		}
		if rep.man.SourceBucket != srcName || rep.man.DestinationBucket != srcName {
			t.Errorf("manifest buckets = %s -> %s", rep.man.SourceBucket, rep.man.DestinationBucket)
		}
	})
}

func TestInventoryErrorCodes(t *testing.T) {
	e := invStart(t)
	ctx := t.Context()
	srcID := e.createBucket(t, unique("inv-err-src"), &storagepb.BucketSpec{})
	goneID := e.createBucket(t, unique("inv-gone"), &storagepb.BucketSpec{})
	tinyID := e.createBucket(t, unique("inv-tiny"), &storagepb.BucketSpec{MaxSizeBytes: 64})
	spec := func(dst string) *storagepb.InventorySpec {
		return &storagepb.InventorySpec{
			Enabled: true, DestinationBucketId: dst, OutputFormat: storagepb.InventorySpec_CSV_GZIP, Schedule: storagepb.InventorySpec_DAILY,
		}
	}
	goneInv := e.create(t, srcID, "gone", spec(goneID))
	tinyInv := e.create(t, srcID, "tiny", spec(tinyID))

	buckets := storagesvc.NewBucketService(e.sdk)
	wait(t)(buckets.Delete(ctx, &storagepb.DeleteBucketRequest{Id: goneID}))
	wait(t)(buckets.Purge(ctx, &storagepb.PurgeBucketRequest{Id: goneID}))
	if _, err := buckets.Get(ctx, &storagepb.GetBucketRequest{Id: goneID}); status.Code(err) != codes.NotFound {
		t.Fatalf("destination bucket after purge: %v, want NotFound", err)
	}

	for id, want := range map[string]storagepb.InventoryStatus_Error{
		goneInv: storagepb.InventoryStatus_DESTINATION_BUCKET_NOT_FOUND,
		tinyInv: storagepb.InventoryStatus_QUOTA_EXCEEDED,
	} {
		poll(t, "inventory "+id+" to fail with "+want.String(), func() { e.clk.Advance(time.Minute) }, func() bool {
			return e.get(t, id).GetStatus().GetErrorCode() == want
		})
		if e.get(t, id).GetStatus().GetLastSuccess() != nil {
			t.Errorf("inventory %s has last_success despite %v", id, want)
		}
	}
}

func TestInventoryDisabledMakesNoReport(t *testing.T) {
	e := invStart(t)
	srcID := e.createBucket(t, unique("inv-dis-src"), &storagepb.BucketSpec{})
	dstID := e.createBucket(t, unique("inv-dis-dst"), &storagepb.BucketSpec{})
	spec := func() *storagepb.InventorySpec {
		return &storagepb.InventorySpec{
			Enabled: true, DestinationBucketId: dstID, DestinationPrefix: "r/",
			OutputFormat: storagepb.InventorySpec_CSV_GZIP, Schedule: storagepb.InventorySpec_DAILY,
		}
	}
	paused := e.create(t, srcID, "paused", spec())
	control := e.create(t, srcID, "control", spec())
	first := e.reported(t, paused).GetLastSuccess().AsTime()
	firstControl := e.reported(t, control).GetLastSuccess().AsTime()

	e.setEnabled(t, paused, false)
	if e.get(t, paused).GetSpec().GetEnabled() {
		t.Fatal("inventory still enabled after the update")
	}

	// Two daily periods pass (the S3 front would reject requests signed this
	// far from the mocker clock, so only gosdk status is read from here on):
	// the enabled control reports again, the disabled one does not.
	e.clk.Advance(48 * time.Hour)
	poll(t, "the enabled control inventory to report again", func() { e.clk.Advance(time.Minute) }, func() bool {
		return e.get(t, control).GetStatus().GetLastSuccess().AsTime().After(firstControl)
	})
	e.clk.Advance(time.Minute) // one more sweep after the control ran; give it real time to finish
	<-clock.Real{}.After(time.Second)
	if got := e.get(t, paused).GetStatus().GetLastSuccess().AsTime(); !got.Equal(first) {
		t.Errorf("disabled inventory reported again: last_success %v, was %v", got, first)
	}
}
