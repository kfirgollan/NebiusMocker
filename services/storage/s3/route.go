package s3

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// target is the bucket and key a request addresses.
type target struct {
	Bucket string
	Key    string
}

// parseTarget reads the bucket from a virtual-hosted host name
// ("<bucket>.<domain>") or else from the first path segment (path style).
func parseTarget(r *http.Request, domainSuffix string) target {
	host := r.Host
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if b, ok := strings.CutSuffix(host, "."+domainSuffix); ok && domainSuffix != "" && b != "" {
		return target{Bucket: b, Key: path}
	}
	bucket, key, _ := strings.Cut(path, "/")
	return target{Bucket: bucket, Key: key}
}

// operation is a classified S3 request.
type operation struct {
	action domain.S3Action
	// target is the key (object actions) or the listing prefix.
	target string
	// copy is the source of CopyObject / UploadPartCopy.
	copy *copySource
}

// copySource is an x-amz-copy-source.
type copySource struct {
	Bucket, Key, VersionID string
	// Length is the x-amz-copy-source-range length, or -1 for the whole object.
	Length int64
}

// otherBucketReads are bucket sub-resources relayed to the backend as reads.
var otherBucketReads = []string{
	"acl", "policy", "policyStatus", "tagging", "encryption", "website", "logging", "notification", "replication",
	"requestPayment", "ownershipControls", "publicAccessBlock", "object-lock", "accelerate", "analytics",
	"inventory", "metrics", "intelligent-tiering",
}

const bucketConfigOverS3 = "this bucket setting is managed with the Nebius API (nebius.storage.v1.BucketService)"

// classify maps an S3 request to its action.
func classify(r *http.Request, t target) (operation, error) {
	q := r.URL.Query()
	switch {
	case t.Bucket == "":
		if r.Method == http.MethodGet {
			return operation{action: domain.ActListBuckets}, nil
		}
		return operation{}, notImplemented("unsupported service-level operation")
	case t.Key == "":
		return classifyBucket(r.Method, q)
	default:
		return classifyObject(r, t.Key, q)
	}
}

func classifyBucket(method string, q url.Values) (operation, error) {
	var act domain.S3Action
	switch method {
	case http.MethodGet:
		act = bucketGet(q)
	case http.MethodHead:
		act = domain.ActHeadBucket
	case http.MethodPut:
		act = pick(q, map[string]domain.S3Action{"versioning": domain.ActPutBucketVersioning, "lifecycle": domain.ActPutBucketLifecycle, "cors": domain.ActPutBucketCors}, domain.ActCreateBucket)
	case http.MethodDelete:
		act = pick(q, map[string]domain.S3Action{"lifecycle": domain.ActDeleteBucketLifecycle, "cors": domain.ActDeleteBucketCors}, domain.ActDeleteBucket)
	case http.MethodPost:
		if q.Has("delete") {
			act = domain.ActDeleteObjects
		}
	}
	if act == "" || (act == domain.ActCreateBucket || act == domain.ActDeleteBucket) && hasSubresource(q) {
		return operation{}, notImplemented(bucketConfigOverS3)
	}
	return operation{action: act, target: q.Get("prefix")}, nil
}

func bucketGet(q url.Values) domain.S3Action {
	switch {
	case q.Has("location"):
		return domain.ActGetBucketLocation
	case q.Has("versioning"):
		return domain.ActGetBucketVersioning
	case q.Has("lifecycle"):
		return domain.ActGetBucketLifecycle
	case q.Has("cors"):
		return domain.ActGetBucketCors
	case q.Has("versions"):
		return domain.ActListObjectVersions
	case q.Has("uploads"):
		return domain.ActListMultipartUploads
	}
	for _, k := range otherBucketReads {
		if q.Has(k) {
			return domain.ActGetBucketOther
		}
	}
	return domain.ActListObjects
}

// pick returns the action of the first sub-resource present, else def.
func pick(q url.Values, subs map[string]domain.S3Action, def domain.S3Action) domain.S3Action {
	for k, a := range subs {
		if q.Has(k) {
			return a
		}
	}
	return def
}

// hasSubresource reports a bucket sub-resource the front does not serve.
func hasSubresource(q url.Values) bool {
	for _, k := range otherBucketReads {
		if q.Has(k) {
			return true
		}
	}
	return false
}

func classifyObject(r *http.Request, key string, q url.Values) (operation, error) {
	op := operation{target: key}
	switch r.Method {
	case http.MethodGet:
		op.action = domain.ActGetObject
		if q.Has("tagging") {
			op.action = domain.ActGetObjectTagging
		} else if q.Has("uploadId") {
			op.action = domain.ActListParts
		}
	case http.MethodHead:
		op.action = domain.ActHeadObject
	case http.MethodPut:
		src, err := parseCopySource(r.Header.Get("X-Amz-Copy-Source"), r.Header.Get("X-Amz-Copy-Source-Range"))
		if err != nil {
			return operation{}, err
		}
		op.copy = src
		switch {
		case q.Has("tagging"):
			op.action = domain.ActPutObjectTagging
		case q.Has("uploadId") && src != nil:
			op.action = domain.ActUploadPartCopy
		case q.Has("uploadId"):
			op.action = domain.ActUploadPart
		case src != nil:
			op.action = domain.ActCopyObject
		default:
			op.action = domain.ActPutObject
		}
	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			op.action = domain.ActCreateMultipartUpload
		case q.Has("uploadId"):
			op.action = domain.ActCompleteMultipartUpload
		default:
			return operation{}, notImplemented("unsupported object operation POST (restore and select are not supported)")
		}
	case http.MethodDelete:
		op.action = domain.ActDeleteObject
		if q.Has("tagging") {
			op.action = domain.ActDeleteObjectTagging
		} else if q.Has("uploadId") {
			op.action = domain.ActAbortMultipartUpload
		}
	default:
		return operation{}, notImplemented("unsupported object operation " + r.Method)
	}
	return op, nil
}

// parseCopySource parses "[/]bucket/key[?versionId=…]" and an optional
// "bytes=first-last" range. An empty header is no copy.
func parseCopySource(src, rng string) (*copySource, error) {
	if src == "" {
		return nil, nil //nolint:nilnil // no copy source is not an error
	}
	path, query, _ := strings.Cut(src, "?")
	if u, err := url.PathUnescape(path); err == nil {
		path = u
	}
	bucket, key, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if !ok || bucket == "" || key == "" {
		return nil, invalidArgument("x-amz-copy-source must be <bucket>/<key>")
	}
	cs := &copySource{Bucket: bucket, Key: key, Length: -1}
	if v, err := url.ParseQuery(query); err == nil {
		cs.VersionID = v.Get("versionId")
	}
	if rng != "" {
		first, last, ok := strings.Cut(strings.TrimPrefix(rng, "bytes="), "-")
		a, errA := strconv.ParseInt(first, 10, 64)
		b, errB := strconv.ParseInt(last, 10, 64)
		if !ok || errA != nil || errB != nil || a < 0 || b < a {
			return nil, invalidArgument("invalid x-amz-copy-source-range " + rng)
		}
		cs.Length = b - a + 1
	}
	return cs, nil
}
