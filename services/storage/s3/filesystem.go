package s3

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// Filesystem buckets (domain.IsFilesystemBucket) keep their objects in the
// filesystem's own object-store bucket (target.store). The front relays to
// that bucket, rewrites the copy source of copies from a filesystem bucket,
// and maps the store's bucket name back to the Nebius name in response
// bodies; objects report the FILESYSTEM storage class.

// fsUnsupported are the S3 calls the Nebius S3 compatibility page lists as
// not supported for filesystem buckets.
var fsUnsupported = map[domain.S3Action]bool{
	domain.ActPutBucketVersioning: true,
	domain.ActPutBucketLifecycle:  true,
	domain.ActGetObjectTagging:    true,
	domain.ActPutObjectTagging:    true,
	domain.ActDeleteObjectTagging: true,
}

// checkFilesystemAction refuses the calls filesystem buckets do not support.
func checkFilesystemAction(b *storagepb.Bucket, act domain.S3Action) error {
	if domain.IsFilesystemBucket(b) && fsUnsupported[act] {
		return notImplemented(string(act) + " is not supported for filesystem buckets")
	}
	return nil
}

// namedResponses are the responses whose XML bodies name the bucket (Name,
// Bucket or a Location URL).
var namedResponses = []domain.S3Action{
	domain.ActListObjects, domain.ActListObjectVersions, domain.ActListMultipartUploads, domain.ActListParts,
	domain.ActCreateMultipartUpload, domain.ActCompleteMultipartUpload,
}

// resolveCopySource points the x-amz-copy-source of a copy at the source's
// object-store bucket when that is not the bucket's own name.
func (s *Server) resolveCopySource(ctx context.Context, r *http.Request, cs *copySource, src *storagepb.Bucket) error {
	if cs == nil || src == nil {
		return nil
	}
	store, err := s.buckets.Backend(ctx, src)
	if err != nil {
		return err
	}
	if store == cs.Bucket {
		return nil
	}
	cs.store = store
	v := ""
	if cs.VersionID != "" {
		v = "?versionId=" + url.QueryEscape(cs.VersionID)
	}
	r.Header.Set("X-Amz-Copy-Source", "/"+store+"/"+escapeKey(cs.Key)+v)
	return nil
}

// escapeKey percent-encodes each segment of an object key.
func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// filesystem configures the response rules of a filesystem bucket.
func (w *responseWriter) filesystem(b *storagepb.Bucket, t target) {
	if !domain.IsFilesystemBucket(b) {
		return
	}
	w.fsClass = true
	w.names = strings.NewReplacer(
		"<Name>"+t.store+"</Name>", "<Name>"+t.Bucket+"</Name>",
		"<Bucket>"+t.store+"</Bucket>", "<Bucket>"+t.Bucket+"</Bucket>",
		"/"+t.store+"/", "/"+t.Bucket+"/",
		"<StorageClass>"+domain.BackendClass(storagepb.StorageClass_FILESYSTEM)+"</StorageClass>",
		"<StorageClass>"+domain.S3ClassName(storagepb.StorageClass_FILESYSTEM)+"</StorageClass>",
	)
}
