package domain

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
)

// csvTimeLayout is the ISO 8601 UTC form AWS inventory CSVs use.
const csvTimeLayout = "2006-01-02T15:04:05.000Z"

// reportColumn is one inventory column in both output formats.
type reportColumn struct {
	name   string // CSV (AWS) column name
	pqName string // Parquet column name
	node   parquet.Node
	csv    func(*reportRow) string
	pq     func(*reportRow) any // nil for an absent optional value
}

// reportColumns lists the columns of an inventory configuration in the fixed
// AWS order: Bucket, Key, the version columns, the standard columns, then the
// selected optional fields.
func reportColumns(spec *storagepb.InventorySpec) []reportColumn {
	cols := []reportColumn{
		strCol("Bucket", "bucket", func(r *reportRow) string { return r.bucket }),
		{
			name: "Key", pqName: "key", node: parquet.String(),
			csv: func(r *reportRow) string { return url.QueryEscape(r.v.Key) },
			pq:  func(r *reportRow) any { return r.v.Key },
		},
	}
	if spec.GetNoncurrentVersions() {
		cols = append(cols,
			strCol("VersionId", "version_id", reportVersionID),
			boolCol("IsLatest", "is_latest", func(r *reportRow) bool { return r.v.IsLatest }),
			boolCol("IsDeleteMarker", "is_delete_marker", func(r *reportRow) bool { return r.v.DeleteMarker }),
		)
	}
	cols = append(cols, []reportColumn{
		{
			"Size", "size", parquet.Optional(parquet.Int(64)),
			func(r *reportRow) string { return notMarker(r, strconv.FormatInt(r.v.Size, 10)) },
			func(r *reportRow) any { return unlessMarker(r, r.v.Size) },
		},
		timeCol("LastModifiedDate", "last_modified_date", func(r *reportRow) time.Time { return r.v.LastModified }),
		{
			"ETag", "e_tag", parquet.Optional(parquet.String()),
			func(r *reportRow) string { return notMarker(r, r.v.ETag) },
			func(r *reportRow) any { return unlessMarker(r, r.v.ETag) },
		},
		strCol("StorageClass", "storage_class", func(r *reportRow) string { return NebiusClass(r.v.StorageClass).String() }),
	}...)
	for _, f := range optionalOrder {
		if slices.Contains(spec.GetOptionalFields(), f) {
			c, _ := optionalColumn(f)
			cols = append(cols, c)
		}
	}
	return cols
}

// optionalOrder is the fixed column order of the optional fields.
var optionalOrder = []storagepb.InventorySpec_OptionalField{
	storagepb.InventorySpec_IS_MULTIPART_UPLOAD,
	storagepb.InventorySpec_INTELLIGENT_TIER,
	storagepb.InventorySpec_CHECKSUM_ALGORITHMS,
	storagepb.InventorySpec_LAST_ACCESS,
	storagepb.InventorySpec_LIFECYCLE_EXPIRATION,
	storagepb.InventorySpec_TAGS,
}

func optionalColumn(f storagepb.InventorySpec_OptionalField) (reportColumn, bool) {
	switch f {
	case storagepb.InventorySpec_IS_MULTIPART_UPLOAD:
		return boolCol("IsMultipartUploaded", "is_multipart_uploaded", func(r *reportRow) bool { return r.v.Multipart() }), true
	case storagepb.InventorySpec_INTELLIGENT_TIER:
		// INTELLIGENT is rejected by this mocker: no object has a tier.
		return strCol("IntelligentTieringAccessTier", "intelligent_tiering_access_tier", func(*reportRow) string { return "" }), true
	case storagepb.InventorySpec_CHECKSUM_ALGORITHMS:
		return strCol("ChecksumAlgorithm", "checksum_algorithm", func(r *reportRow) string { return strings.Join(r.v.ChecksumAlgorithms, ",") }), true
	case storagepb.InventorySpec_LAST_ACCESS:
		return optTimeCol("LastAccessDate", "last_access_date", func(r *reportRow) time.Time { return r.lastAccess }), true
	case storagepb.InventorySpec_LIFECYCLE_EXPIRATION:
		return optTimeCol("LifecycleExpirationDate", "lifecycle_expiration_date", func(r *reportRow) time.Time { return r.expiresAt }), true
	case storagepb.InventorySpec_TAGS:
		return strCol("Tags", "tags", tagsString), true
	case storagepb.InventorySpec_OPTIONAL_FIELD_UNSPECIFIED:
	}
	return reportColumn{}, false
}

func strCol(name, pqName string, f func(*reportRow) string) reportColumn {
	return reportColumn{name: name, pqName: pqName, node: parquet.String(), csv: f, pq: func(r *reportRow) any { return f(r) }}
}

func boolCol(name, pqName string, f func(*reportRow) bool) reportColumn {
	return reportColumn{
		name: name, pqName: pqName, node: parquet.Leaf(parquet.BooleanType),
		csv: func(r *reportRow) string { return strconv.FormatBool(f(r)) },
		pq:  func(r *reportRow) any { return f(r) },
	}
}

// timeCol is a required timestamp column; a zero time is "" in CSV.
func timeCol(name, pqName string, f func(*reportRow) time.Time) reportColumn {
	return reportColumn{
		name: name, pqName: pqName, node: parquet.Timestamp(parquet.Millisecond),
		csv: func(r *reportRow) string { return formatTime(f(r)) },
		pq:  func(r *reportRow) any { return f(r).UTC().UnixMilli() },
	}
}

// optTimeCol is an optional timestamp column: "" in CSV and null in Parquet
// when the time is zero.
func optTimeCol(name, pqName string, f func(*reportRow) time.Time) reportColumn {
	return reportColumn{
		name: name, pqName: pqName, node: parquet.Optional(parquet.Timestamp(parquet.Millisecond)),
		csv: func(r *reportRow) string { return formatTime(f(r)) },
		pq: func(r *reportRow) any {
			if t := f(r); !t.IsZero() {
				return t.UTC().UnixMilli()
			}
			return nil
		},
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(csvTimeLayout)
}

func reportVersionID(r *reportRow) string {
	if r.v.VersionID == objstore.NullVersion {
		return ""
	}
	return r.v.VersionID
}

func notMarker(r *reportRow, s string) string {
	if r.v.DeleteMarker {
		return ""
	}
	return s
}

// unlessMarker is v, or nil (a Parquet null) for a delete marker.
func unlessMarker(r *reportRow, v any) any {
	if r.v.DeleteMarker {
		return nil
	}
	return v
}

// tagsString is the url-encoded, key-sorted tag set ("k1=v1&k2=v2").
func tagsString(r *reportRow) string {
	vs := url.Values{}
	for k, v := range r.tags {
		vs.Set(k, v)
	}
	return vs.Encode()
}

// quoteCSV double-quotes one field the way AWS inventory does.
func quoteCSV(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// encodeCSVGzip writes the rows as a gzip-compressed, headerless CSV with
// every field quoted. The schema is the column names joined by ", ".
func encodeCSVGzip(cols []reportColumn, rows []reportRow) (data []byte, schema string, err error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	var line strings.Builder
	for i := range rows {
		line.Reset()
		for j, c := range cols {
			if j > 0 {
				line.WriteByte(',')
			}
			line.WriteString(quoteCSV(c.csv(&rows[i])))
		}
		line.WriteByte('\n')
		if _, err := zw.Write([]byte(line.String())); err != nil {
			return nil, "", fmt.Errorf("compress inventory report: %w", err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, "", fmt.Errorf("compress inventory report: %w", err)
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.name
	}
	return buf.Bytes(), strings.Join(names, ", "), nil
}

// encodeParquet writes the rows as a Snappy-compressed Parquet file with the
// AWS-style snake_case columns. The schema is the Parquet schema string.
func encodeParquet(cols []reportColumn, rows []reportRow) (data []byte, schema string, err error) {
	group := parquet.Group{}
	for _, c := range cols {
		group[c.pqName] = c.node
	}
	sch := parquet.NewSchema("s3.inventory", group)
	var buf bytes.Buffer
	w := parquet.NewWriter(&buf, sch, parquet.Compression(&parquet.Snappy))
	for i := range rows {
		rec := make(map[string]any, len(cols))
		for _, c := range cols {
			if v := c.pq(&rows[i]); v != nil {
				rec[c.pqName] = v
			}
		}
		if err := w.Write(rec); err != nil {
			return nil, "", fmt.Errorf("write inventory parquet row: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("write inventory parquet: %w", err)
	}
	return buf.Bytes(), sch.String(), nil
}
