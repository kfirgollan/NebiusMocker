// Package s3endpoint adapts any S3 server (the mocker's own S3 front or an
// external S3-compatible endpoint) to the domain.Endpoint port used by data
// transfers, with the AWS SDK for Go v2.
package s3endpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// Regions used when a side names none.
const (
	defaultNebiusRegion = "eu-north1"
	defaultS3Region     = "us-east-1"
)

// Config configures an Opener.
type Config struct {
	// NebiusURL returns the base URL of the mocker's own S3 front (for example
	// "http://127.0.0.1:4580"), or "" when the front is disabled.
	NebiusURL func() string
	// Clock signs requests to the mocker's own S3 front, which verifies the
	// signature time against the same clock.
	Clock clock.Clock
	// HTTPClient is used for all requests; nil means a default client.
	HTTPClient *http.Client
}

// Opener opens S3 transfer endpoints (the Nebius and S3-compatible providers).
type Opener struct {
	cfg Config
}

// New returns an Opener.
func New(cfg Config) *Opener { return &Opener{cfg: cfg} }

// Open returns the endpoint of a Nebius or S3-compatible side.
func (o *Opener) Open(_ context.Context, side domain.Side) (domain.Endpoint, error) {
	var base string
	var signer s3.HTTPSignerV4
	region := side.Region
	switch side.Kind { //nolint:exhaustive // every other provider is unsupported here
	case domain.ProviderNebius:
		if region == "" {
			region = defaultNebiusRegion
		}
		if o.cfg.NebiusURL != nil {
			base = o.cfg.NebiusURL()
		}
		if base == "" {
			return nil, &domain.EndpointError{Code: "ServiceUnavailable", Message: "the S3 front is disabled"}
		}
		if o.cfg.Clock != nil {
			signer = clockSigner{Signer: newSigner(), clk: o.cfg.Clock}
		}
	case domain.ProviderS3Compatible:
		if region == "" {
			region = defaultS3Region // SigV4 needs one; many S3-compatible servers ignore it
		}
		base = side.Endpoint
	default:
		return nil, fmt.Errorf("s3endpoint: unsupported provider %s", side.Kind)
	}
	opts := s3.Options{
		BaseEndpoint:               aws.String(base),
		Region:                     region,
		UsePathStyle:               true,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		HTTPSignerV4:               signer,
	}
	if o.cfg.HTTPClient != nil {
		opts.HTTPClient = o.cfg.HTTPClient
	}
	if side.Creds.Anonymous {
		opts.Credentials = aws.AnonymousCredentials{}
	} else {
		opts.Credentials = credentials.NewStaticCredentialsProvider(side.Creds.AccessKeyID, side.Creds.Secret, "")
	}
	return &endpoint{client: s3.New(opts), bucket: side.Bucket}, nil
}

// newSigner matches the SDK's default S3 signer.
func newSigner() *v4.Signer {
	return v4.NewSigner(func(so *v4.SignerOptions) { so.DisableURIPathEscaping = true })
}

// clockSigner signs with the mocker's clock instead of the wall clock.
type clockSigner struct {
	*v4.Signer
	clk clock.Clock
}

func (c clockSigner) SignHTTP(ctx context.Context, creds aws.Credentials, r *http.Request, payloadHash, service, region string, _ time.Time, optFns ...func(*v4.SignerOptions)) error {
	return c.Signer.SignHTTP(ctx, creds, r, payloadHash, service, region, c.clk.Now(), optFns...)
}

type endpoint struct {
	client *s3.Client
	bucket string
}

func (e *endpoint) List(ctx context.Context, prefix string) iter.Seq2[domain.ObjectMeta, error] {
	return func(yield func(domain.ObjectMeta, error) bool) {
		p := s3.NewListObjectsV2Paginator(e.client, &s3.ListObjectsV2Input{
			Bucket:  aws.String(e.bucket),
			Prefix:  aws.String(prefix),
			MaxKeys: aws.Int32(domain.TransferListPage),
		})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				yield(domain.ObjectMeta{}, mapErr(ctx, err))
				return
			}
			for _, obj := range page.Contents {
				m := domain.ObjectMeta{
					Key:          aws.ToString(obj.Key),
					Size:         aws.ToInt64(obj.Size),
					ETag:         trimETag(obj.ETag),
					LastModified: aws.ToTime(obj.LastModified),
				}
				if !yield(m, nil) {
					return
				}
			}
		}
	}
}

func (e *endpoint) Head(ctx context.Context, key string) (domain.ObjectMeta, error) {
	out, err := e.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(e.bucket), Key: aws.String(key)})
	if err != nil {
		return domain.ObjectMeta{}, mapErr(ctx, err)
	}
	return domain.ObjectMeta{
		Key:                key,
		Size:               aws.ToInt64(out.ContentLength),
		ETag:               trimETag(out.ETag),
		LastModified:       aws.ToTime(out.LastModified),
		ContentType:        aws.ToString(out.ContentType),
		CacheControl:       aws.ToString(out.CacheControl),
		ContentDisposition: aws.ToString(out.ContentDisposition),
		ContentEncoding:    aws.ToString(out.ContentEncoding),
		ContentLanguage:    aws.ToString(out.ContentLanguage),
		Metadata:           lowerKeys(out.Metadata),
	}, nil
}

func (e *endpoint) Get(ctx context.Context, key string) (io.ReadCloser, domain.ObjectMeta, error) {
	out, err := e.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(e.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, domain.ObjectMeta{}, mapErr(ctx, err)
	}
	return out.Body, domain.ObjectMeta{
		Key:                key,
		Size:               aws.ToInt64(out.ContentLength),
		ETag:               trimETag(out.ETag),
		LastModified:       aws.ToTime(out.LastModified),
		ContentType:        aws.ToString(out.ContentType),
		CacheControl:       aws.ToString(out.CacheControl),
		ContentDisposition: aws.ToString(out.ContentDisposition),
		ContentEncoding:    aws.ToString(out.ContentEncoding),
		ContentLanguage:    aws.ToString(out.ContentLanguage),
		Metadata:           lowerKeys(out.Metadata),
	}, nil
}

// Put buffers the body (the request needs a payload hash over plain HTTP and
// the body is not seekable): in one PutObject below the multipart threshold,
// otherwise as a multipart upload buffering one part at a time.
func (e *endpoint) Put(ctx context.Context, key string, body io.Reader, meta domain.ObjectMeta) (string, error) {
	if meta.Size >= domain.TransferMultipartThreshold {
		return e.putMultipart(ctx, key, body, meta)
	}
	data, err := io.ReadAll(io.LimitReader(body, meta.Size))
	if err != nil {
		return "", mapErr(ctx, err)
	}
	out, err := e.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:             aws.String(e.bucket),
		Key:                aws.String(key),
		Body:               bytes.NewReader(data),
		ContentType:        optional(meta.ContentType),
		CacheControl:       optional(meta.CacheControl),
		ContentDisposition: optional(meta.ContentDisposition),
		ContentEncoding:    optional(meta.ContentEncoding),
		ContentLanguage:    optional(meta.ContentLanguage),
		Metadata:           meta.Metadata,
	})
	if err != nil {
		return "", mapErr(ctx, err)
	}
	return trimETag(out.ETag), nil
}

func (e *endpoint) putMultipart(ctx context.Context, key string, body io.Reader, meta domain.ObjectMeta) (string, error) {
	create, err := e.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:             aws.String(e.bucket),
		Key:                aws.String(key),
		ContentType:        optional(meta.ContentType),
		CacheControl:       optional(meta.CacheControl),
		ContentDisposition: optional(meta.ContentDisposition),
		ContentEncoding:    optional(meta.ContentEncoding),
		ContentLanguage:    optional(meta.ContentLanguage),
		Metadata:           meta.Metadata,
	})
	if err != nil {
		return "", mapErr(ctx, err)
	}
	etag, err := e.uploadParts(ctx, key, create.UploadId, body, meta.Size)
	if err != nil {
		// The cancelled ctx may be the reason for the failure: abort regardless.
		_, _ = e.client.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{
			Bucket: aws.String(e.bucket), Key: aws.String(key), UploadId: create.UploadId,
		}) // best-effort cleanup; the upload error is what matters
		return "", err
	}
	return etag, nil
}

func (e *endpoint) uploadParts(ctx context.Context, key string, uploadID *string, body io.Reader, size int64) (string, error) {
	var parts []types.CompletedPart
	buf := make([]byte, domain.TransferPartSize)
	for remaining, n := size, int32(1); remaining > 0; n++ {
		chunk := buf[:min(remaining, int64(len(buf)))]
		if _, err := io.ReadFull(body, chunk); err != nil {
			return "", mapErr(ctx, err)
		}
		up, err := e.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(e.bucket), Key: aws.String(key), UploadId: uploadID,
			PartNumber: aws.Int32(n), Body: bytes.NewReader(chunk),
		})
		if err != nil {
			return "", mapErr(ctx, err)
		}
		parts = append(parts, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(n)})
		remaining -= int64(len(chunk))
	}
	done, err := e.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(e.bucket), Key: aws.String(key), UploadId: uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return "", mapErr(ctx, err)
	}
	return trimETag(done.ETag), nil
}

func (e *endpoint) ReplaceMetadata(ctx context.Context, key string, meta domain.ObjectMeta) (string, error) {
	out, err := e.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:             aws.String(e.bucket),
		Key:                aws.String(key),
		CopySource:         aws.String(e.bucket + "/" + escapeKey(key)),
		MetadataDirective:  types.MetadataDirectiveReplace,
		ContentType:        optional(meta.ContentType),
		CacheControl:       optional(meta.CacheControl),
		ContentDisposition: optional(meta.ContentDisposition),
		ContentEncoding:    optional(meta.ContentEncoding),
		ContentLanguage:    optional(meta.ContentLanguage),
		Metadata:           meta.Metadata,
	})
	if err != nil {
		return "", mapErr(ctx, err)
	}
	if out.CopyObjectResult == nil {
		return "", nil
	}
	return trimETag(out.CopyObjectResult.ETag), nil
}

func (e *endpoint) Delete(ctx context.Context, key string) error {
	_, err := e.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(e.bucket), Key: aws.String(key)})
	return mapErr(ctx, err)
}

// mapErr turns an SDK error into the endpoint contract: nil stays nil, a
// cancelled ctx stays the ctx error, a missing object is errs.NotFound, an S3
// error response is an EndpointError with its code, anything else a
// transport "RequestError".
func mapErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if ae, ok := errors.AsType[smithy.APIError](err); ok {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return errs.NotFound(ae.ErrorMessage())
		}
		return &domain.EndpointError{Code: ae.ErrorCode(), Message: ae.ErrorMessage()}
	}
	return &domain.EndpointError{Code: "RequestError", Message: err.Error()}
}

func trimETag(s *string) string { return strings.Trim(aws.ToString(s), `"`) }

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

func lowerKeys(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

// escapeKey URL-escapes an object key for x-amz-copy-source, keeping "/".
func escapeKey(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
