package domain_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/parquet-go/parquet-go"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

const (
	repSrc = "src-bucket"
	repDst = "dst-bucket"
	repInv = "daily"
)

var repNow = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// repStore is a domain.ReportStore: the source bucket is scripted
// (versions, tags), the destination is a real in-memory object store.
type repStore struct {
	src *contentFake
	mem *memory.Store
}

func newRepStore(t *testing.T) *repStore {
	t.Helper()
	s := &repStore{src: newContentFake(true), mem: memory.New(clock.NewManual(repNow))}
	if err := s.mem.EnsureBucket(t.Context(), repDst); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *repStore) ListVersions(ctx context.Context, b, p string) iter.Seq2[objstore.Version, error] {
	if b == repSrc {
		return s.src.ListVersions(ctx, b, p)
	}
	return s.mem.ListVersions(ctx, b, p)
}

func (s *repStore) ListUploads(ctx context.Context, b string) iter.Seq2[objstore.Upload, error] {
	if b == repSrc {
		return s.src.ListUploads(ctx, b)
	}
	return s.mem.ListUploads(ctx, b)
}

func (s *repStore) DeleteVersion(ctx context.Context, b, k, v string) error {
	return s.mem.DeleteVersion(ctx, b, k, v)
}

func (s *repStore) AbortUpload(ctx context.Context, b, k, u string) error {
	return s.mem.AbortUpload(ctx, b, k, u)
}

func (s *repStore) ChecksumAlgorithms(context.Context, string, string, string) ([]string, error) {
	return nil, nil
}

func (s *repStore) ObjectTags(ctx context.Context, b, k, v string) (map[string]string, error) {
	if b == repSrc {
		return s.src.ObjectTags(ctx, b, k, v)
	}
	return s.mem.ObjectTags(ctx, b, k, v)
}

func (s *repStore) Delete(ctx context.Context, b, k string) error { return s.mem.Delete(ctx, b, k) }

func (s *repStore) Put(ctx context.Context, b, k string, r io.Reader, size int64, o objstore.PutOptions) (objstore.ObjectInfo, error) {
	return s.mem.Put(ctx, b, k, r, size, o)
}

// repAccess is a domain.AccessLog over a map.
type repAccess map[string]time.Time

func (a repAccess) Touch(context.Context, string, string, time.Time) error { return nil }
func (a repAccess) Forget(context.Context, string) error                   { return nil }
func (a repAccess) LastAccess(_ context.Context, _, key string) (time.Time, bool, error) {
	t, ok := a[key]
	return t, ok, nil
}

func repSpec(mut func(*storagepb.InventorySpec)) *storagepb.InventorySpec {
	s := &storagepb.InventorySpec{
		Enabled: true, DestinationBucketId: "storagebucket-dst",
		OutputFormat: storagepb.InventorySpec_CSV_GZIP, Schedule: storagepb.InventorySpec_DAILY,
	}
	if mut != nil {
		mut(s)
	}
	return s
}

func repSource(spec *storagepb.InventorySpec, lc *storagepb.LifecycleConfiguration, dstMax int64) domain.ReportSource {
	return domain.ReportSource{
		Inventory: &storagepb.Inventory{
			Metadata: &commonpb.ResourceMetadata{Id: "inv-1", Name: repInv, ParentId: "storagebucket-src"},
			Spec:     spec,
		},
		Source: &storagepb.Bucket{
			Metadata: &commonpb.ResourceMetadata{Id: "storagebucket-src", Name: repSrc},
			Spec:     &storagepb.BucketSpec{LifecycleConfiguration: lc},
		},
		Destination: &storagepb.Bucket{
			Metadata: &commonpb.ResourceMetadata{Id: "storagebucket-dst", Name: repDst},
			Spec:     &storagepb.BucketSpec{MaxSizeBytes: dstMax},
		},
		Now: repNow,
	}
}

func repRead(t *testing.T, s *repStore, key string) []byte {
	t.Helper()
	rc, _, err := s.mem.Get(t.Context(), repDst, key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func repMD5(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// repCSV decodes a report's gzip CSV data object into its lines.
func repCSV(t *testing.T, s *repStore, rep domain.InventoryReport) []string {
	t.Helper()
	if len(rep.DataKeys) != 1 {
		t.Fatalf("data keys = %v, want 1", rep.DataKeys)
	}
	zr, err := gzip.NewReader(bytes.NewReader(repRead(t, s, rep.DataKeys[0])))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func repWrite(t *testing.T, s *repStore, src domain.ReportSource, access domain.AccessLog) domain.InventoryReport {
	t.Helper()
	rep, err := domain.NewReportWriter(domain.ReportWriterDeps{Store: s, Access: access}).Write(t.Context(), src)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	return rep
}

func TestReportCSVDefaultFields(t *testing.T) {
	t.Parallel()
	s := newRepStore(t)
	at := time.Date(2026, 1, 2, 3, 4, 5, 6_000_000, time.UTC)
	s.src.put("a.txt", objstore.NullVersion, at, 11, "etag1", "STANDARD")
	s.src.put("dir/a b ü\"q.txt", objstore.NullVersion, at, 0, "d41d8cd9", "REDUCED_REDUNDANCY")
	s.src.putMarker("gone", "v9", at)
	rep := repWrite(t, s, repSource(repSpec(nil), nil, 0), nil)

	want := []string{
		`"src-bucket","a.txt","11","2026-01-02T03:04:05.006Z","etag1","STANDARD"`,
		`"src-bucket","dir%2Fa+b+%C3%BC%22q.txt","0","2026-01-02T03:04:05.006Z","d41d8cd9","ENHANCED_THROUGHPUT"`,
	}
	if diff := cmp.Diff(want, repCSV(t, s, rep)); diff != "" {
		t.Fatalf("rows (-want +got):\n%s", diff)
	}
	if rep.Objects != 2 {
		t.Fatalf("Objects = %d, want 2", rep.Objects)
	}
}

func TestReportCSVQuotesDoubled(t *testing.T) {
	t.Parallel()
	s := newRepStore(t)
	s.src.put("k", objstore.NullVersion, repNow, 1, `e"t`, "STANDARD")
	rep := repWrite(t, s, repSource(repSpec(nil), nil, 0), nil)
	if got := repCSV(t, s, rep)[0]; !strings.Contains(got, `"e""t"`) {
		t.Fatalf("row %s does not double the inner quote", got)
	}
}

func TestReportNoncurrentVersionsAndMarkers(t *testing.T) {
	t.Parallel()
	s := newRepStore(t)
	t1, t2, t3 := repNow.Add(-3*time.Hour), repNow.Add(-2*time.Hour), repNow.Add(-time.Hour)
	s.src.put("k", "v1", t1, 5, "e1", "STANDARD")
	s.src.put("k", "v2", t2, 6, "e2", "STANDARD")
	s.src.putMarker("k", "v3", t3)
	s.src.put("n", objstore.NullVersion, t1, 1, "en", "STANDARD")
	spec := repSpec(func(sp *storagepb.InventorySpec) { sp.NoncurrentVersions = true })
	rep := repWrite(t, s, repSource(spec, nil, 0), nil)

	f := func(ts time.Time) string { return ts.Format("2006-01-02T15:04:05.000Z") }
	want := []string{
		`"src-bucket","k","v3","true","true","","` + f(t3) + `","","STANDARD"`,
		`"src-bucket","k","v2","false","false","6","` + f(t2) + `","e2","STANDARD"`,
		`"src-bucket","k","v1","false","false","5","` + f(t1) + `","e1","STANDARD"`,
		`"src-bucket","n","","true","false","1","` + f(t1) + `","en","STANDARD"`,
	}
	if diff := cmp.Diff(want, repCSV(t, s, rep)); diff != "" {
		t.Fatalf("rows (-want +got):\n%s", diff)
	}
	var m map[string]any
	if err := json.Unmarshal(repRead(t, s, rep.ManifestKey), &m); err != nil {
		t.Fatal(err)
	}
	if got := m["fileSchema"]; got != "Bucket, Key, VersionId, IsLatest, IsDeleteMarker, Size, LastModifiedDate, ETag, StorageClass" {
		t.Fatalf("fileSchema = %v", got)
	}
}

func TestReportAllOptionalFieldsCSV(t *testing.T) {
	t.Parallel()
	s := newRepStore(t)
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	s.src.put("tagged", objstore.NullVersion, at, 3, "abc-3", "STANDARD")
	s.src.versions["tagged"][0].ChecksumAlgorithms = []string{"CRC64NVME", "SHA256"}
	s.src.tag("tagged", objstore.NullVersion, map[string]string{"b": "2 2", "a": "1"})
	s.src.put("plain", objstore.NullVersion, at, 1, "e", "STANDARD")
	spec := repSpec(func(sp *storagepb.InventorySpec) {
		// Deliberately out of the fixed column order.
		sp.OptionalFields = []storagepb.InventorySpec_OptionalField{
			storagepb.InventorySpec_TAGS, storagepb.InventorySpec_LIFECYCLE_EXPIRATION, storagepb.InventorySpec_LAST_ACCESS,
			storagepb.InventorySpec_CHECKSUM_ALGORITHMS, storagepb.InventorySpec_INTELLIGENT_TIER, storagepb.InventorySpec_IS_MULTIPART_UPLOAD,
		}
	})
	lc := &storagepb.LifecycleConfiguration{Rules: []*storagepb.LifecycleRule{{
		Id: "r", Status: storagepb.LifecycleRule_ENABLED,
		Filter:     &storagepb.LifecycleFilter{Tags: []*storagepb.LifecycleFilter_Tag{{Key: "a", Value: "1"}}},
		Expiration: &storagepb.LifecycleExpiration{ExpiredWith: &storagepb.LifecycleExpiration_Days{Days: 30}},
	}}}
	access := repAccess{"tagged": time.Date(2026, 3, 3, 9, 8, 7, 654_000_000, time.UTC)}
	rep := repWrite(t, s, repSource(spec, lc, 0), access)

	want := []string{
		`"src-bucket","tagged","3","2026-03-01T10:00:00.000Z","abc-3","STANDARD","true","","CRC64NVME,SHA256","2026-03-03T09:08:07.654Z","2026-04-01T00:00:00.000Z","a=1&b=2+2"`,
		`"src-bucket","plain","1","2026-03-01T10:00:00.000Z","e","STANDARD","false","","","","",""`,
	}
	if diff := cmp.Diff(want, repCSV(t, s, rep)); diff != "" {
		t.Fatalf("rows (-want +got):\n%s", diff)
	}
	var m struct{ FileSchema string }
	if err := json.Unmarshal(repRead(t, s, rep.ManifestKey), &m); err != nil {
		t.Fatal(err)
	}
	wantSchema := "Bucket, Key, Size, LastModifiedDate, ETag, StorageClass, IsMultipartUploaded, IntelligentTieringAccessTier, " +
		"ChecksumAlgorithm, LastAccessDate, LifecycleExpirationDate, Tags"
	if m.FileSchema != wantSchema {
		t.Fatalf("fileSchema = %q, want %q", m.FileSchema, wantSchema)
	}
}

func TestReportLifecycleExpirationDates(t *testing.T) {
	t.Parallel()
	s := newRepStore(t)
	t1 := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 2, 10, 10, 0, 0, 0, time.UTC)
	s.src.put("k", "v1", t1, 1, "e1", "STANDARD")
	s.src.put("k", "v2", t2, 1, "e2", "STANDARD")
	s.src.put("logs/x", "n", t1, 1, "ex", "STANDARD")
	s.src.putMarker("m", "vm", t2)
	exp := func(id, prefix string, e *storagepb.LifecycleExpiration, n *storagepb.LifecycleNoncurrentVersionExpiration, st storagepb.LifecycleRule_Status) *storagepb.LifecycleRule {
		return &storagepb.LifecycleRule{Id: id, Status: st, Filter: &storagepb.LifecycleFilter{Prefix: prefix}, Expiration: e, NoncurrentVersionExpiration: n}
	}
	days := func(d int32) *storagepb.LifecycleExpiration {
		return &storagepb.LifecycleExpiration{ExpiredWith: &storagepb.LifecycleExpiration_Days{Days: d}}
	}
	lc := &storagepb.LifecycleConfiguration{Rules: []*storagepb.LifecycleRule{
		exp("all", "", days(30), &storagepb.LifecycleNoncurrentVersionExpiration{NoncurrentDays: 5}, storagepb.LifecycleRule_ENABLED),
		exp("logs", "logs/", &storagepb.LifecycleExpiration{ExpiredWith: &storagepb.LifecycleExpiration_Date{Date: timestamppb.New(time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC))}}, nil, storagepb.LifecycleRule_ENABLED),
		exp("off", "", days(1), nil, storagepb.LifecycleRule_DISABLED),
	}}
	spec := repSpec(func(sp *storagepb.InventorySpec) {
		sp.NoncurrentVersions = true
		sp.OptionalFields = []storagepb.InventorySpec_OptionalField{storagepb.InventorySpec_LIFECYCLE_EXPIRATION}
	})
	rep := repWrite(t, s, repSource(spec, lc, 0), nil)
	lines := repCSV(t, s, rep)
	// Columns: Bucket,Key,VersionId,IsLatest,IsDeleteMarker,Size,LastModified,ETag,Class,Expiration.
	last := func(line string) string {
		f := strings.Split(line, ",")
		return f[len(f)-1]
	}
	got := map[string]string{}
	for _, l := range lines {
		f := strings.Split(l, ",")
		got[strings.Trim(f[1], `"`)+"@"+strings.Trim(f[2], `"`)] = last(l)
	}
	// A delete marker never expires; logs/x matches both rules and the
	// earlier date (Feb 5) wins over t1+30d.
	want := map[string]string{
		"k@v2":       `"2026-03-13T00:00:00.000Z"`, // Feb 10 10:00 + 30d, up to midnight
		"k@v1":       `"2026-02-16T00:00:00.000Z"`, // v2 written Feb 10 10:00 + 5d, up to midnight
		"logs%2Fx@n": `"2026-02-05T00:00:00.000Z"`,
		"m@vm":       `""`,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("expiration dates (-want +got):\n%s", diff)
	}
}

func TestReportKeysManifestChecksumSymlink(t *testing.T) {
	t.Parallel()
	s := newRepStore(t)
	s.src.put("a", objstore.NullVersion, repNow, 1, "e", "STANDARD")
	spec := repSpec(func(sp *storagepb.InventorySpec) { sp.DestinationPrefix = "reports/inv/" })
	rep := repWrite(t, s, repSource(spec, nil, 0), nil)

	base := "reports/inv/src-bucket/daily"
	if rep.ManifestKey != base+"/2026-03-04T05-06Z/manifest.json" {
		t.Fatalf("ManifestKey = %q", rep.ManifestKey)
	}
	dataKey := rep.DataKeys[0]
	if !strings.HasPrefix(dataKey, base+"/data/") || !strings.HasSuffix(dataKey, ".csv.gz") {
		t.Fatalf("data key %q", dataKey)
	}
	data := repRead(t, s, dataKey)
	var m struct {
		SourceBucket, DestinationBucket, Version, CreationTimestamp, FileFormat string
		Files                                                                   []struct {
			Key         string `json:"key"`
			Size        int    `json:"size"`
			MD5Checksum string `json:"MD5checksum"`
		}
	}
	man := repRead(t, s, rep.ManifestKey)
	if err := json.Unmarshal(man, &m); err != nil {
		t.Fatal(err)
	}
	if m.SourceBucket != repSrc || m.DestinationBucket != repDst || m.Version != "2016-11-30" || m.FileFormat != "CSV" ||
		m.CreationTimestamp != "1772600767000" {
		t.Fatalf("manifest = %+v", m)
	}
	if len(m.Files) != 1 || m.Files[0].Key != dataKey || m.Files[0].Size != len(data) || m.Files[0].MD5Checksum != repMD5(data) {
		t.Fatalf("manifest files = %+v, want %s size %d md5 %s", m.Files, dataKey, len(data), repMD5(data))
	}
	if got := string(repRead(t, s, base+"/2026-03-04T05-06Z/manifest.checksum")); got != repMD5(man) {
		t.Fatalf("manifest.checksum = %q, want %q", got, repMD5(man))
	}
	if got := string(repRead(t, s, base+"/hive/dt=2026-03-04-05-06/symlink.txt")); got != "s3://dst-bucket/"+dataKey+"\n" {
		t.Fatalf("symlink = %q", got)
	}
	// A second report never overwrites the first.
	src2 := repSource(spec, nil, 0)
	src2.Now = repNow.Add(24 * time.Hour)
	rep2 := repWrite(t, s, src2, nil)
	if rep2.ManifestKey == rep.ManifestKey || rep2.DataKeys[0] == dataKey {
		t.Fatalf("second report reuses keys: %v / %v", rep2, rep)
	}
	if !bytes.Equal(repRead(t, s, dataKey), data) {
		t.Fatal("first report's data was overwritten")
	}
}

func TestReportEmptyPrefixHasNoLeadingSlash(t *testing.T) {
	t.Parallel()
	s := newRepStore(t)
	rep := repWrite(t, s, repSource(repSpec(nil), nil, 0), nil)
	if !strings.HasPrefix(rep.ManifestKey, "src-bucket/daily/") {
		t.Fatalf("ManifestKey = %q, want no leading slash", rep.ManifestKey)
	}
	if rep.Objects != 0 {
		t.Fatalf("Objects = %d, want 0", rep.Objects)
	}
}

func TestReportSourcePrefixFilter(t *testing.T) {
	t.Parallel()
	s := newRepStore(t)
	for _, k := range []string{"logs/a", "logs/b", "other/c"} {
		s.src.put(k, objstore.NullVersion, repNow, 1, "e", "STANDARD")
	}
	spec := repSpec(func(sp *storagepb.InventorySpec) { sp.SourcePrefix = "logs/" })
	rep := repWrite(t, s, repSource(spec, nil, 0), nil)
	lines := repCSV(t, s, rep)
	if len(lines) != 2 || !strings.Contains(lines[0], `"logs%2Fa"`) || !strings.Contains(lines[1], `"logs%2Fb"`) {
		t.Fatalf("rows = %v", lines)
	}
}

func TestReportParquet(t *testing.T) {
	t.Parallel()
	s := newRepStore(t)
	at := time.Date(2026, 3, 1, 10, 0, 0, 123_000_000, time.UTC)
	s.src.put("dir/a b.txt", "v1", at, 7, "abc-2", "STANDARD")
	s.src.versions["dir/a b.txt"][0].ChecksumAlgorithms = []string{"CRC64NVME"}
	s.src.tag("dir/a b.txt", "v1", map[string]string{"t": "1"})
	s.src.putMarker("dir/a b.txt", "v2", at.Add(time.Hour))
	spec := repSpec(func(sp *storagepb.InventorySpec) {
		sp.OutputFormat = storagepb.InventorySpec_PARQUET_SNAPPY
		sp.NoncurrentVersions = true
		sp.OptionalFields = []storagepb.InventorySpec_OptionalField{
			storagepb.InventorySpec_TAGS, storagepb.InventorySpec_LAST_ACCESS, storagepb.InventorySpec_LIFECYCLE_EXPIRATION,
			storagepb.InventorySpec_IS_MULTIPART_UPLOAD, storagepb.InventorySpec_CHECKSUM_ALGORITHMS,
		}
	})
	access := repAccess{"dir/a b.txt": time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)}
	rep := repWrite(t, s, repSource(spec, nil, 0), access)
	if !strings.HasSuffix(rep.DataKeys[0], ".parquet") {
		t.Fatalf("data key %q, want .parquet", rep.DataKeys[0])
	}
	data := repRead(t, s, rep.DataKeys[0])
	pf, err := parquet.OpenFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if got := pf.Metadata().RowGroups[0].Columns[0].MetaData.Codec.String(); got != "SNAPPY" {
		t.Fatalf("compression = %s, want SNAPPY", got)
	}
	var names []string
	for _, f := range pf.Schema().Fields() {
		names = append(names, f.Name())
	}
	wantNames := []string{
		"bucket", "key", "version_id", "is_latest", "is_delete_marker", "size", "last_modified_date", "e_tag", "storage_class",
		"is_multipart_uploaded", "checksum_algorithm", "last_access_date", "lifecycle_expiration_date", "tags",
	}
	// Parquet groups order fields by name; compare as sets in the listed order.
	if diff := cmp.Diff(sortedCopy(wantNames), sortedCopy(names)); diff != "" {
		t.Fatalf("columns (-want +got):\n%s", diff)
	}
	rows := repParquetRows(t, data)
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want 2", rows)
	}
	marker, version := rows[0], rows[1]
	wantMarker := map[string]any{
		"bucket": repSrc, "key": "dir/a b.txt", "version_id": "v2", "is_latest": true, "is_delete_marker": true,
		"last_modified_date": at.Add(time.Hour).UnixMilli(), "storage_class": "STANDARD",
		"is_multipart_uploaded": false, "checksum_algorithm": "", "tags": "",
	}
	if diff := cmp.Diff(wantMarker, marker); diff != "" {
		t.Fatalf("marker row (-want +got):\n%s", diff)
	}
	wantVersion := map[string]any{
		"bucket": repSrc, "key": "dir/a b.txt", "version_id": "v1", "is_latest": false, "is_delete_marker": false,
		"size": int64(7), "last_modified_date": at.UnixMilli(), "e_tag": "abc-2", "storage_class": "STANDARD",
		"is_multipart_uploaded": true, "checksum_algorithm": "CRC64NVME",
		"last_access_date": time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC).UnixMilli(), "tags": "t=1",
	}
	if diff := cmp.Diff(wantVersion, version); diff != "" {
		t.Fatalf("version row (-want +got):\n%s", diff)
	}
	var m struct{ FileFormat, FileSchema string }
	if err := json.Unmarshal(repRead(t, s, rep.ManifestKey), &m); err != nil {
		t.Fatal(err)
	}
	if m.FileFormat != "Parquet" || !strings.HasPrefix(m.FileSchema, "message s3.inventory {") {
		t.Fatalf("manifest = %+v", m)
	}
}

func TestReportQuota(t *testing.T) {
	t.Parallel()
	s := newRepStore(t)
	s.src.put("a", objstore.NullVersion, repNow, 1, "e", "STANDARD")
	// The destination already holds 90 of its 300 bytes.
	if _, err := s.mem.Put(t.Context(), repDst, "existing", bytes.NewReader(make([]byte, 90)), 90, objstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := domain.NewReportWriter(domain.ReportWriterDeps{Store: s}).Write(t.Context(), repSource(repSpec(nil), nil, 300))
	if !errs.IsKind(err, errs.KindQuotaExceeded) {
		t.Fatalf("Write over quota: %v, want QuotaExceeded", err)
	}
	if n := repCount(t, s); n != 1 {
		t.Fatalf("destination holds %d objects after a refused report, want 1", n)
	}
	// A roomy limit succeeds.
	repWrite(t, s, repSource(repSpec(nil), nil, 1<<20), nil)
}

func TestReportMissingDestination(t *testing.T) {
	t.Parallel()
	s := newRepStore(t)
	if err := s.mem.DeleteBucket(t.Context(), repDst); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int64{0, 1 << 20} {
		_, err := domain.NewReportWriter(domain.ReportWriterDeps{Store: s}).Write(t.Context(), repSource(repSpec(nil), nil, limit))
		if !errs.IsKind(err, errs.KindNotFound) {
			t.Fatalf("Write to a missing destination (limit %d): %v, want NotFound", limit, err)
		}
	}
}

func repCount(t *testing.T, s *repStore) int {
	t.Helper()
	n := 0
	for _, err := range s.mem.List(t.Context(), repDst, "") {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	return n
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// repParquetRows decodes every row of a Parquet file into column -> value;
// null columns are absent.
func repParquetRows(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	r := parquet.NewReader(bytes.NewReader(data))
	defer r.Close()
	var out []map[string]any
	for {
		row := map[string]any{}
		if err := r.Read(&row); err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			t.Fatal(err)
		}
		for k, v := range row {
			if v == nil {
				delete(row, k)
			}
		}
		out = append(out, row)
	}
}
