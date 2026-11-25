package s3

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
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

// action is what a request does, as far as authorization is concerned.
type action int

const (
	actionForward     action = iota // relay to the backend
	actionListBuckets               // answered by the mocker from Nebius metadata
	actionGetLocation               // answered by the mocker (Nebius region)
)

// operation is a classified S3 request.
type operation struct {
	action action
	perm   authz.Permission
	// copySource is the source bucket of CopyObject / UploadPartCopy.
	copySource string
}

// bucketSubresources are bucket-level read APIs relayed as storage.buckets.get.
var listParams = []string{"list-type", "prefix", "delimiter", "marker", "max-keys", "continuation-token", "start-after", "encoding-type", "fetch-owner", "versions", "uploads"}

// classify maps an S3 request to the permission it needs. Bucket management
// over S3 (CreateBucket, DeleteBucket, PutBucket*) is refused: buckets are
// Nebius resources, managed with nebius.storage.v1.BucketService.
func classify(r *http.Request, t target) (operation, error) {
	q := r.URL.Query()
	switch {
	case t.Bucket == "":
		if r.Method == http.MethodGet {
			return operation{action: actionListBuckets, perm: domain.PermBucketList}, nil
		}
		return operation{}, notImplemented("unsupported service-level operation")
	case t.Key == "":
		return classifyBucket(r.Method, q)
	default:
		return classifyObject(r, q)
	}
}

func classifyBucket(method string, q url.Values) (operation, error) {
	switch method {
	case http.MethodGet:
		if q.Has("location") {
			return operation{action: actionGetLocation, perm: domain.PermBucketGet}, nil
		}
		if len(q) == 0 || hasAny(q, listParams) {
			return operation{perm: domain.PermObjectList}, nil
		}
		return operation{perm: domain.PermBucketGet}, nil // versioning, lifecycle, cors, policy, acl, …
	case http.MethodHead:
		return operation{perm: domain.PermBucketGet}, nil
	case http.MethodPost:
		if q.Has("delete") {
			return operation{perm: domain.PermObjectDelete}, nil
		}
	}
	return operation{}, notImplemented("bucket configuration is managed with the Nebius API (nebius.storage.v1.BucketService), not over S3")
}

func classifyObject(r *http.Request, q url.Values) (operation, error) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return operation{perm: domain.PermObjectGet}, nil
	case http.MethodPut:
		op := operation{perm: domain.PermObjectWrite}
		if src := r.Header.Get("X-Amz-Copy-Source"); src != "" {
			op.copySource = copySourceBucket(src)
		}
		return op, nil
	case http.MethodPost:
		return operation{perm: domain.PermObjectWrite}, nil // multipart create/complete, restore
	case http.MethodDelete:
		if q.Has("uploadId") {
			return operation{perm: domain.PermObjectWrite}, nil // abort multipart upload
		}
		return operation{perm: domain.PermObjectDelete}, nil
	}
	return operation{}, notImplemented("unsupported object operation " + r.Method)
}

// copySourceBucket extracts the bucket of "[/]bucket/key[?versionId=…]".
func copySourceBucket(src string) string {
	if u, err := url.PathUnescape(src); err == nil {
		src = u
	}
	b, _, _ := strings.Cut(strings.TrimPrefix(src, "/"), "/")
	return b
}

func hasAny(q url.Values, keys []string) bool {
	for _, k := range keys {
		if q.Has(k) {
			return true
		}
	}
	return false
}
