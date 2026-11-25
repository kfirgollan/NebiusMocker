// Package rustfs is the objstore.Store backed by a RustFS server
// (https://github.com/rustfs/rustfs), spoken to over the S3 API with the AWS
// SDK for Go v2. NebiusMocker stores no object data itself.
//
// The package also provides Forwarder, which relays already-authenticated S3
// requests to RustFS re-signed with the RustFS credentials (the S3 data plane
// of the storage module).
package rustfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
)

// DefaultRegion is the region RustFS signs requests for unless configured.
const DefaultRegion = "us-east-1"

// Config locates a RustFS server.
type Config struct {
	// Endpoint is the S3 endpoint, e.g. "http://127.0.0.1:9000".
	Endpoint  string
	AccessKey string
	SecretKey string
	// Region defaults to DefaultRegion.
	Region string
	// HTTPClient defaults to a client with sane timeouts.
	HTTPClient *http.Client
}

// Client is an objstore.Store over RustFS.
type Client struct {
	s3       *s3.Client
	endpoint *url.URL
	cfg      Config
}

var _ objstore.Store = (*Client)(nil)

// New returns a client for cfg. It does not contact the server.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("rustfs: endpoint, access key and secret key are required")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("rustfs: endpoint %q is not an absolute URL", cfg.Endpoint)
	}
	if cfg.Region == "" {
		cfg.Region = DefaultRegion
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Transport: http.DefaultTransport}
	}
	client := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(strings.TrimRight(cfg.Endpoint, "/")),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		HTTPClient:   cfg.HTTPClient,
		// Plain PUTs with a Content-Length: no aws-chunked trailers needed.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &Client{s3: client, endpoint: u, cfg: cfg}, nil
}

// Ping checks that the server answers authenticated S3 requests.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.s3.ListBuckets(ctx, &s3.ListBucketsInput{}); err != nil {
		return fmt.Errorf("rustfs: list buckets: %w", err)
	}
	return nil
}

// EnsureBucket implements objstore.Store.
func (c *Client) EnsureBucket(ctx context.Context, bucket string) error {
	_, err := c.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	if err == nil || hasCode(err, "BucketAlreadyOwnedByYou", "BucketAlreadyExists") {
		return nil
	}
	return fmt.Errorf("create bucket %s: %w", bucket, err)
}

// DeleteBucket implements objstore.Store: it removes every object version
// and delete marker, then the bucket.
func (c *Client) DeleteBucket(ctx context.Context, bucket string) error {
	if err := c.emptyBucket(ctx, bucket); err != nil {
		if errors.Is(err, objstore.ErrBucketNotFound) {
			return nil
		}
		return err
	}
	_, err := c.s3.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	if err != nil && !hasCode(err, "NoSuchBucket") {
		return fmt.Errorf("delete bucket %s: %w", bucket, err)
	}
	return nil
}

func (c *Client) emptyBucket(ctx context.Context, bucket string) error {
	p := s3.NewListObjectVersionsPaginator(c.s3, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return mapErr(err, bucket, "")
		}
		var ids []types.ObjectIdentifier
		for i := range page.Versions {
			ids = append(ids, types.ObjectIdentifier{Key: page.Versions[i].Key, VersionId: page.Versions[i].VersionId})
		}
		for _, m := range page.DeleteMarkers {
			ids = append(ids, types.ObjectIdentifier{Key: m.Key, VersionId: m.VersionId})
		}
		if len(ids) == 0 {
			continue
		}
		out, err := c.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket), Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("delete objects in %s: %w", bucket, err)
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("delete object %s/%s: %s: %s", bucket, aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message))
		}
	}
	return nil
}

// BucketEmpty implements objstore.Store.
func (c *Client) BucketEmpty(ctx context.Context, bucket string) (bool, error) {
	out, err := c.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), MaxKeys: aws.Int32(1)})
	if err != nil {
		return false, mapErr(err, bucket, "")
	}
	return len(out.Contents) == 0, nil
}

// SetVersioning implements objstore.Store.
func (c *Client) SetVersioning(ctx context.Context, bucket string, v objstore.Versioning) error {
	var status types.BucketVersioningStatus
	switch v {
	case objstore.VersioningUnset:
		return nil
	case objstore.VersioningEnabled:
		status = types.BucketVersioningStatusEnabled
	case objstore.VersioningSuspended:
		status = types.BucketVersioningStatusSuspended
	default:
		return fmt.Errorf("rustfs: unknown versioning state %d", v)
	}
	_, err := c.s3.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: status},
	})
	if err != nil {
		return mapErr(err, bucket, "")
	}
	return nil
}

// Versioning returns the bucket's versioning state as RustFS reports it.
func (c *Client) Versioning(ctx context.Context, bucket string) (objstore.Versioning, error) {
	out, err := c.s3.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)})
	if err != nil {
		return objstore.VersioningUnset, mapErr(err, bucket, "")
	}
	switch out.Status {
	case types.BucketVersioningStatusEnabled:
		return objstore.VersioningEnabled, nil
	case types.BucketVersioningStatusSuspended:
		return objstore.VersioningSuspended, nil
	default:
		return objstore.VersioningUnset, nil
	}
}

// Put implements objstore.Store. An unknown size (negative) is spooled to a
// temporary file first, because S3 PUT needs a Content-Length.
func (c *Client) Put(ctx context.Context, bucket, key string, r io.Reader, size int64, opts objstore.PutOptions) (objstore.ObjectInfo, error) {
	if size < 0 {
		f, n, err := spool(r)
		if err != nil {
			return objstore.ObjectInfo{}, err
		}
		defer func() { _ = os.Remove(f.Name()) }() // best-effort temp cleanup
		defer func() { _ = f.Close() }()           // read-only use after spooling
		r, size = f, n
	}
	in := &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          r,
		ContentLength: aws.Int64(size),
		Metadata:      opts.Metadata,
	}
	if opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	if _, err := c.s3.PutObject(ctx, in, s3.WithAPIOptions(v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)); err != nil {
		return objstore.ObjectInfo{}, mapErr(err, bucket, key)
	}
	return c.Stat(ctx, bucket, key)
}

func spool(r io.Reader) (*os.File, int64, error) {
	f, err := os.CreateTemp("", "nebius-mocker-upload-*")
	if err != nil {
		return nil, 0, fmt.Errorf("spool upload: %w", err)
	}
	n, err := io.Copy(f, r)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = f.Close()           // best-effort: already failing
		_ = os.Remove(f.Name()) // best-effort: already failing
		return nil, 0, fmt.Errorf("spool upload: %w", err)
	}
	return f, n, nil
}

// Get implements objstore.Store.
func (c *Client) Get(ctx context.Context, bucket, key string) (io.ReadCloser, objstore.ObjectInfo, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, objstore.ObjectInfo{}, mapErr(err, bucket, key)
	}
	return out.Body, objstore.ObjectInfo{
		Bucket: bucket, Key: key, Size: aws.ToInt64(out.ContentLength), ETag: trimETag(out.ETag),
		ContentType: aws.ToString(out.ContentType), Metadata: out.Metadata, LastModified: aws.ToTime(out.LastModified),
	}, nil
}

// Stat implements objstore.Store.
func (c *Client) Stat(ctx context.Context, bucket, key string) (objstore.ObjectInfo, error) {
	out, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return objstore.ObjectInfo{}, mapErr(err, bucket, key)
	}
	return objstore.ObjectInfo{
		Bucket: bucket, Key: key, Size: aws.ToInt64(out.ContentLength), ETag: trimETag(out.ETag),
		ContentType: aws.ToString(out.ContentType), Metadata: out.Metadata, LastModified: aws.ToTime(out.LastModified),
	}, nil
}

// Delete implements objstore.Store.
func (c *Client) Delete(ctx context.Context, bucket, key string) error {
	if _, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err != nil {
		return mapErr(err, bucket, key)
	}
	return nil
}

// Copy implements objstore.Store.
func (c *Client) Copy(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) (objstore.ObjectInfo, error) {
	_, err := c.s3.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(dstBucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(srcBucket + "/" + escapePath(srcKey)),
	})
	if err != nil {
		return objstore.ObjectInfo{}, mapErr(err, srcBucket, srcKey)
	}
	return c.Stat(ctx, dstBucket, dstKey)
}

// List implements objstore.Store.
func (c *Client) List(ctx context.Context, bucket, prefix string) iter.Seq2[objstore.ObjectInfo, error] {
	return func(yield func(objstore.ObjectInfo, error) bool) {
		p := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				yield(objstore.ObjectInfo{}, mapErr(err, bucket, ""))
				return
			}
			for _, o := range page.Contents {
				info := objstore.ObjectInfo{
					Bucket: bucket, Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size),
					ETag: trimETag(o.ETag), LastModified: aws.ToTime(o.LastModified),
				}
				if !yield(info, nil) {
					return
				}
			}
		}
	}
}

// escapePath URL-encodes each segment of an object key (CopySource format).
func escapePath(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func trimETag(s *string) string { return strings.Trim(aws.ToString(s), `"`) }

// mapErr turns S3 "not found" errors into the objstore sentinels.
func mapErr(err error, bucket, key string) error {
	switch {
	case hasCode(err, "NoSuchBucket"):
		return fmt.Errorf("bucket %s: %w", bucket, objstore.ErrBucketNotFound)
	case hasCode(err, "NoSuchKey", "NotFound", "NoSuchVersion"):
		if key == "" {
			return fmt.Errorf("bucket %s: %w", bucket, objstore.ErrBucketNotFound)
		}
		return fmt.Errorf("object %s/%s: %w", bucket, key, objstore.ErrNotFound)
	default:
		return fmt.Errorf("s3 %s/%s: %w", bucket, key, err)
	}
}

func hasCode(err error, codes ...string) bool {
	if ae, ok := errors.AsType[smithy.APIError](err); ok {
		for _, c := range codes {
			if ae.ErrorCode() == c {
				return true
			}
		}
	}
	return false
}
