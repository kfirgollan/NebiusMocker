package domain

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // the AWS inventory manifest carries MD5 checksums by definition
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
)

// Inventory report layout (AWS S3 Inventory compatible, docs.nebius.com/
// object-storage/buckets/inventory).
const (
	manifestVersion = "2016-11-30"
	// manifestTimeLayout names the report folder; hiveTimeLayout the Hive partition.
	manifestTimeLayout = "2006-01-02T15-04Z"
	hiveTimeLayout     = "2006-01-02-15-04"
)

// ReportStore is the object store as the report writer uses it: the
// content introspection of the source bucket and uploads to the destination.
type ReportStore interface {
	ContentBackend
	// ChecksumAlgorithms returns the algorithms of the checksums stored with
	// one object version (e.g. CRC32), for the CHECKSUM_ALGORITHMS field.
	ChecksumAlgorithms(ctx context.Context, bucket, key, versionID string) ([]string, error)
	Put(ctx context.Context, bucket, key string, r io.Reader, size int64, opts objstore.PutOptions) (objstore.ObjectInfo, error)
}

// ReportWriterDeps are the dependencies of an ObjectReportWriter.
type ReportWriterDeps struct {
	Store ReportStore
	// Access is the read log behind the LAST_ACCESS field.
	Access AccessLog
}

// ObjectReportWriter writes inventory reports into the destination bucket.
type ObjectReportWriter struct {
	store  ReportStore
	access AccessLog
}

var _ ReportWriter = (*ObjectReportWriter)(nil)

// NewReportWriter returns a report writer.
func NewReportWriter(d ReportWriterDeps) *ObjectReportWriter {
	return &ObjectReportWriter{store: d.Store, access: d.Access}
}

// reportObject is one object of a report, ready to upload.
type reportObject struct {
	key, contentType string
	data             []byte
}

// manifestFile and manifest are the manifest.json documents of AWS inventory.
type manifestFile struct {
	Key         string `json:"key"`
	Size        int    `json:"size"`
	MD5Checksum string `json:"MD5checksum"`
}

type manifest struct {
	SourceBucket      string         `json:"sourceBucket"`
	DestinationBucket string         `json:"destinationBucket"`
	Version           string         `json:"version"`
	CreationTimestamp string         `json:"creationTimestamp"`
	FileFormat        string         `json:"fileFormat"`
	FileSchema        string         `json:"fileSchema"`
	Files             []manifestFile `json:"files"`
}

// Write generates one report of src.Inventory: the data file, then the
// manifest, its checksum and the Hive symlink, all under a new key set.
func (w *ObjectReportWriter) Write(ctx context.Context, src ReportSource) (InventoryReport, error) {
	spec := src.Inventory.GetSpec()
	cols := reportColumns(spec)
	rows, err := w.collect(ctx, src, cols)
	if err != nil {
		return InventoryReport{}, err
	}
	now := src.Now.UTC()
	base := reportBase(src)

	var (
		data                        []byte
		ext, fileFmt, schema, ctype string
	)
	if spec.GetOutputFormat() == storagepb.InventorySpec_PARQUET_SNAPPY {
		data, schema, err = encodeParquet(cols, rows)
		ext, fileFmt, ctype = ".parquet", "Parquet", "application/octet-stream"
	} else {
		data, schema, err = encodeCSVGzip(cols, rows)
		ext, fileFmt, ctype = ".csv.gz", "CSV", "application/gzip"
	}
	if err != nil {
		return InventoryReport{}, err
	}
	dataKey := base + "/data/" + uuid.NewString() + ext
	sum := md5.Sum(data) //nolint:gosec // AWS manifest checksum
	man, err := json.Marshal(manifest{
		SourceBucket: src.Source.GetMetadata().GetName(), DestinationBucket: src.Destination.GetMetadata().GetName(),
		Version: manifestVersion, CreationTimestamp: strconv.FormatInt(now.UnixMilli(), 10),
		FileFormat: fileFmt, FileSchema: schema,
		Files: []manifestFile{{Key: dataKey, Size: len(data), MD5Checksum: hex.EncodeToString(sum[:])}},
	})
	if err != nil {
		return InventoryReport{}, fmt.Errorf("encode inventory manifest: %w", err)
	}
	manSum := md5.Sum(man) //nolint:gosec // AWS manifest checksum
	folder := base + "/" + now.Format(manifestTimeLayout)
	manifestKey := folder + "/manifest.json"
	symlink := "s3://" + src.Destination.GetMetadata().GetName() + "/" + dataKey + "\n"
	objs := []reportObject{
		{dataKey, ctype, data},
		{manifestKey, "application/json", man},
		{folder + "/manifest.checksum", "text/plain", []byte(hex.EncodeToString(manSum[:]))},
		{base + "/hive/dt=" + now.Format(hiveTimeLayout) + "/symlink.txt", "text/plain", []byte(symlink)},
	}
	if err := w.upload(ctx, src.Destination, objs); err != nil {
		return InventoryReport{}, err
	}
	return InventoryReport{ManifestKey: manifestKey, DataKeys: []string{dataKey}, Objects: int64(len(rows))}, nil
}

// reportBase is <destination_prefix>/<source bucket>/<inventory name>.
func reportBase(src ReportSource) string {
	base := src.Source.GetMetadata().GetName() + "/" + src.Inventory.GetMetadata().GetName()
	if p := strings.TrimRight(src.Inventory.GetSpec().GetDestinationPrefix(), "/"); p != "" {
		return p + "/" + base
	}
	return base
}

// upload refuses a report that would exceed the destination's size limit,
// then writes the objects in order.
func (w *ObjectReportWriter) upload(ctx context.Context, dst *storagepb.Bucket, objs []reportObject) error {
	name := dst.GetMetadata().GetName()
	if limit := dst.GetSpec().GetMaxSizeBytes(); limit > 0 {
		counters, err := ComputeCounters(ctx, w.store, name)
		if err != nil {
			return storeErr(name, err)
		}
		total := UsedBytes(counters)
		for _, o := range objs {
			total += int64(len(o.data))
		}
		if total > limit {
			return &errs.Error{Kind: errs.KindQuotaExceeded, Message: fmt.Sprintf("the report does not fit into destination bucket %q (max_size_bytes %d)", name, limit)}
		}
	}
	for _, o := range objs {
		if _, err := w.store.Put(ctx, name, o.key, bytes.NewReader(o.data), int64(len(o.data)), objstore.PutOptions{ContentType: o.contentType}); err != nil {
			return storeErr(name, err)
		}
	}
	return nil
}

func storeErr(bucket string, err error) error {
	if errors.Is(err, objstore.ErrBucketNotFound) {
		return errs.NotFound(bucket)
	}
	return fmt.Errorf("inventory bucket %q: %w", bucket, err)
}

// reportRow is one inventory row.
type reportRow struct {
	bucket     string
	v          objstore.Version
	tags       map[string]string
	lastAccess time.Time // zero: never read since tracking started
	expiresAt  time.Time // zero: no lifecycle rule applies
}

// reportRun is the state of listing one source bucket.
type reportRun struct {
	w             *ObjectReportWriter
	spec          *storagepb.InventorySpec
	srcName       string
	srcID         string
	rules         []*storagepb.LifecycleRule
	matcher       *lifecycleRun // the lifecycle filter matching, shared with ApplyLifecycle
	wantTags      bool
	wantAccess    bool
	wantChecksums bool
}

// collect lists the source bucket and builds the rows.
func (w *ObjectReportWriter) collect(ctx context.Context, src ReportSource, cols []reportColumn) ([]reportRow, error) {
	spec := src.Inventory.GetSpec()
	srcName := src.Source.GetMetadata().GetName()
	run := &reportRun{
		w: w, spec: spec, srcName: srcName, srcID: src.Source.GetMetadata().GetId(),
		rules:         enabledRules(src.Source.GetSpec().GetLifecycleConfiguration()),
		matcher:       &lifecycleRun{backend: w.store, bucket: srcName},
		wantTags:      slices.ContainsFunc(cols, func(c reportColumn) bool { return c.name == "Tags" }),
		wantAccess:    w.access != nil && slices.Contains(spec.GetOptionalFields(), storagepb.InventorySpec_LAST_ACCESS),
		wantChecksums: slices.Contains(spec.GetOptionalFields(), storagepb.InventorySpec_CHECKSUM_ALGORITHMS),
	}

	var (
		rows       []reportRow
		prev       objstore.Version // the next newer version of the current key
		noncurrent int              // noncurrent real versions seen for the key
	)
	for v, err := range w.store.ListVersions(ctx, srcName, spec.GetSourcePrefix()) {
		if err != nil {
			return nil, storeErr(srcName, err)
		}
		if v.Key != prev.Key {
			prev, noncurrent = objstore.Version{}, 0
		}
		if !v.IsLatest && !v.DeleteMarker {
			noncurrent++
		}
		if v.IsLatest && !v.DeleteMarker || spec.GetNoncurrentVersions() {
			row, err := run.row(ctx, v, prev, noncurrent)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
		prev = v
	}
	return rows, nil
}

// row builds the inventory row of one version.
func (x *reportRun) row(ctx context.Context, v, prev objstore.Version, noncurrent int) (reportRow, error) {
	row := reportRow{bucket: x.srcName, v: v}
	var err error
	if x.wantTags && !v.DeleteMarker {
		if row.tags, err = x.w.store.ObjectTags(ctx, x.srcName, v.Key, v.VersionID); err != nil {
			return row, storeErr(x.srcName, err)
		}
	}
	if x.wantChecksums && !v.DeleteMarker && len(v.ChecksumAlgorithms) == 0 {
		// Listings may omit the algorithm (RustFS does): ask the object.
		if row.v.ChecksumAlgorithms, err = x.w.store.ChecksumAlgorithms(ctx, x.srcName, v.Key, v.VersionID); err != nil {
			return row, storeErr(x.srcName, err)
		}
	}
	if row.expiresAt, err = lifecycleExpiry(ctx, x.matcher, x.rules, v, prev, noncurrent); err != nil {
		return row, err
	}
	if x.wantAccess && !v.DeleteMarker {
		t, ok, err := x.w.access.LastAccess(ctx, x.srcID, v.Key)
		if err != nil {
			return row, fmt.Errorf("last access of %q: %w", v.Key, err)
		}
		if ok {
			row.lastAccess = t.UTC()
		}
	}
	return row, nil
}

func enabledRules(cfg *storagepb.LifecycleConfiguration) []*storagepb.LifecycleRule {
	var out []*storagepb.LifecycleRule
	for _, r := range cfg.GetRules() {
		if r.GetStatus() == storagepb.LifecycleRule_ENABLED {
			out = append(out, r)
		}
	}
	return out
}

// lifecycleExpiry is the earliest time an ENABLED rule expires v, or the zero
// time. prev is the next newer version of the key (zero for the newest) and
// noncurrent the number of noncurrent real versions up to and including v.
func lifecycleExpiry(ctx context.Context, x *lifecycleRun, rules []*storagepb.LifecycleRule, v, prev objstore.Version, noncurrent int) (time.Time, error) {
	if v.DeleteMarker {
		return time.Time{}, nil
	}
	var best time.Time
	for _, r := range rules {
		var due time.Time
		switch {
		case v.IsLatest && r.GetExpiration() != nil && !r.GetExpiration().GetExpiredObjectDeleteMarker():
			e := r.GetExpiration()
			if _, ok := e.GetExpiredWith().(*storagepb.LifecycleExpiration_Date); ok {
				due = e.GetDate().AsTime().UTC()
			} else {
				due = CeilMidnightUTC(v.LastModified.Add(time.Duration(e.GetDays()) * day))
			}
		case !v.IsLatest && r.GetNoncurrentVersionExpiration() != nil && prev.Key != "":
			n := r.GetNoncurrentVersionExpiration()
			if noncurrent <= int(n.GetNewerNoncurrentVersions()) {
				continue
			}
			due = CeilMidnightUTC(prev.LastModified.Add(time.Duration(n.GetNoncurrentDays()) * day))
		default:
			continue
		}
		ok, err := x.matches(ctx, r.GetFilter(), v)
		if err != nil {
			return time.Time{}, fmt.Errorf("lifecycle rule %q: %w", r.GetId(), err)
		}
		if ok && (best.IsZero() || due.Before(best)) {
			best = due
		}
	}
	return best, nil
}
