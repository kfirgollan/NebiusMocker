package rustfs

import (
	"cmp"
	"context"
	"iter"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
)

var _ objstore.Inventory = (*Client)(nil)

const defaultStorageClass = "STANDARD"

// ListVersions implements objstore.Inventory.
func (c *Client) ListVersions(ctx context.Context, bucket, prefix string) iter.Seq2[objstore.Version, error] {
	return func(yield func(objstore.Version, error) bool) {
		p := s3.NewListObjectVersionsPaginator(c.s3, &s3.ListObjectVersionsInput{
			Bucket: aws.String(bucket), Prefix: aws.String(prefix),
		})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				yield(objstore.Version{}, mapErr(err, bucket, ""))
				return
			}
			vs := pageVersions(page)
			slices.SortStableFunc(vs, compareVersions)
			for i := range vs {
				if !yield(vs[i], nil) {
					return
				}
			}
		}
	}
}

// pageVersions merges one page's versions and delete markers.
func pageVersions(page *s3.ListObjectVersionsOutput) []objstore.Version {
	vs := make([]objstore.Version, 0, len(page.Versions)+len(page.DeleteMarkers))
	for i := range page.Versions {
		v := &page.Versions[i]
		vs = append(vs, objstore.Version{
			Key: aws.ToString(v.Key), VersionID: aws.ToString(v.VersionId), IsLatest: aws.ToBool(v.IsLatest),
			Size: aws.ToInt64(v.Size), ETag: trimETag(v.ETag), StorageClass: storageClass(string(v.StorageClass)),
			LastModified: aws.ToTime(v.LastModified), ChecksumAlgorithms: checksumAlgorithms(v.ChecksumAlgorithm),
		})
	}
	for i := range page.DeleteMarkers {
		m := &page.DeleteMarkers[i]
		vs = append(vs, objstore.Version{
			Key: aws.ToString(m.Key), VersionID: aws.ToString(m.VersionId), IsLatest: aws.ToBool(m.IsLatest),
			DeleteMarker: true, StorageClass: defaultStorageClass, LastModified: aws.ToTime(m.LastModified),
		})
	}
	return vs
}

// checksumAlgorithms names the checksum algorithms of one listed version.
func checksumAlgorithms(algs []types.ChecksumAlgorithm) []string {
	if len(algs) == 0 {
		return nil
	}
	out := make([]string, len(algs))
	for i, a := range algs {
		out[i] = string(a)
	}
	return out
}

func storageClass(s string) string {
	if s == "" {
		return defaultStorageClass
	}
	return s
}

// compareVersions orders by key, then newest first (the latest wins ties).
func compareVersions(a, b objstore.Version) int {
	if c := cmp.Compare(a.Key, b.Key); c != 0 {
		return c
	}
	if c := b.LastModified.Compare(a.LastModified); c != 0 {
		return c
	}
	switch {
	case a.IsLatest && !b.IsLatest:
		return -1
	case b.IsLatest && !a.IsLatest:
		return 1
	}
	return 0
}

// ListUploads implements objstore.Inventory.
func (c *Client) ListUploads(ctx context.Context, bucket string) iter.Seq2[objstore.Upload, error] {
	return func(yield func(objstore.Upload, error) bool) {
		p := s3.NewListMultipartUploadsPaginator(c.s3, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				yield(objstore.Upload{}, mapErr(err, bucket, ""))
				return
			}
			for i := range page.Uploads {
				u := &page.Uploads[i]
				up := objstore.Upload{
					Key: aws.ToString(u.Key), UploadID: aws.ToString(u.UploadId),
					StorageClass: storageClass(string(u.StorageClass)), Initiated: aws.ToTime(u.Initiated),
				}
				if err := c.sumParts(ctx, bucket, &up); err != nil {
					yield(objstore.Upload{}, err)
					return
				}
				if !yield(up, nil) {
					return
				}
			}
		}
	}
}

// sumParts fills up.Parts and up.PartsSize from the uploaded parts.
func (c *Client) sumParts(ctx context.Context, bucket string, up *objstore.Upload) error {
	p := s3.NewListPartsPaginator(c.s3, &s3.ListPartsInput{
		Bucket: aws.String(bucket), Key: aws.String(up.Key), UploadId: aws.String(up.UploadID),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			if hasCode(err, "NoSuchUpload") { // aborted or completed meanwhile
				return nil
			}
			return mapErr(err, bucket, up.Key)
		}
		for i := range page.Parts {
			up.Parts++
			up.PartsSize += aws.ToInt64(page.Parts[i].Size)
		}
	}
	return nil
}

// DeleteVersion implements objstore.Inventory.
func (c *Client) DeleteVersion(ctx context.Context, bucket, key, versionID string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: aws.String(versionID),
	})
	if err != nil && !hasCode(err, "NoSuchKey", "NoSuchVersion", "NotFound") {
		return mapErr(err, bucket, key)
	}
	return nil
}

// AbortUpload implements objstore.Inventory.
func (c *Client) AbortUpload(ctx context.Context, bucket, key, uploadID string) error {
	_, err := c.s3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	if err != nil && !hasCode(err, "NoSuchUpload") {
		return mapErr(err, bucket, key)
	}
	return nil
}

// ChecksumAlgorithms returns the algorithms of the checksums stored with an
// object version ("" for the current one), e.g. [CRC32]. RustFS keeps the
// checksum a client sent but leaves ChecksumAlgorithm out of its listings,
// so this asks HeadObject with checksum mode.
func (c *Client) ChecksumAlgorithms(ctx context.Context, bucket, key, versionID string) ([]string, error) {
	in := &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled}
	if versionID != "" {
		in.VersionId = aws.String(versionID)
	}
	out, err := c.s3.HeadObject(ctx, in)
	if err != nil {
		return nil, mapErr(err, bucket, key)
	}
	var algs []string
	for _, s := range []struct {
		alg types.ChecksumAlgorithm
		v   *string
	}{
		{types.ChecksumAlgorithmCrc32, out.ChecksumCRC32},
		{types.ChecksumAlgorithmCrc32c, out.ChecksumCRC32C},
		{types.ChecksumAlgorithmCrc64nvme, out.ChecksumCRC64NVME},
		{types.ChecksumAlgorithmSha1, out.ChecksumSHA1},
		{types.ChecksumAlgorithmSha256, out.ChecksumSHA256},
	} {
		if aws.ToString(s.v) != "" {
			algs = append(algs, string(s.alg))
		}
	}
	return algs, nil
}

// ObjectTags implements objstore.Inventory.
func (c *Client) ObjectTags(ctx context.Context, bucket, key, versionID string) (map[string]string, error) {
	in := &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if versionID != "" {
		in.VersionId = aws.String(versionID)
	}
	out, err := c.s3.GetObjectTagging(ctx, in)
	if err != nil {
		return nil, mapErr(err, bucket, key)
	}
	tags := make(map[string]string, len(out.TagSet))
	for _, t := range out.TagSet {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return tags, nil
}
