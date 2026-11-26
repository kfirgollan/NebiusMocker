package domain_test

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
)

// contentFake is an in-memory domain.ContentBackend. Versions are kept per
// key, newest first, as ListVersions yields them.
type contentFake struct {
	versioned bool
	keys      []string
	versions  map[string][]objstore.Version
	uploads   []objstore.Upload
	tags      map[string]map[string]string // key + "@" + versionID -> tags
	calls     []string
}

func newContentFake(versioned bool) *contentFake {
	return &contentFake{versioned: versioned, versions: map[string][]objstore.Version{}, tags: map[string]map[string]string{}}
}

// put adds a version as the newest of key.
func (f *contentFake) put(key, versionID string, at time.Time, size int64, etag, class string) *contentFake {
	if _, ok := f.versions[key]; !ok {
		f.keys = append(f.keys, key)
	}
	vs := f.versions[key]
	if len(vs) > 0 {
		vs[0].IsLatest = false
	}
	v := objstore.Version{Key: key, VersionID: versionID, IsLatest: true, Size: size, ETag: etag, StorageClass: class, LastModified: at}
	f.versions[key] = append([]objstore.Version{v}, vs...)
	return f
}

func (f *contentFake) putMarker(key, versionID string, at time.Time) *contentFake {
	f.put(key, versionID, at, 0, "", "")
	f.versions[key][0].DeleteMarker = true
	return f
}

func (f *contentFake) tag(key, versionID string, kv map[string]string) *contentFake {
	f.tags[key+"@"+versionID] = kv
	return f
}

func (f *contentFake) ListVersions(_ context.Context, _, prefix string) iter.Seq2[objstore.Version, error] {
	return func(yield func(objstore.Version, error) bool) {
		for _, k := range f.keys {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			vs := slices.Clone(f.versions[k])
			for i := range vs {
				if !yield(vs[i], nil) {
					return
				}
			}
		}
	}
}

func (f *contentFake) ListUploads(context.Context, string) iter.Seq2[objstore.Upload, error] {
	return func(yield func(objstore.Upload, error) bool) {
		for _, u := range slices.Clone(f.uploads) {
			if !yield(u, nil) {
				return
			}
		}
	}
}

func (f *contentFake) DeleteVersion(_ context.Context, _, key, versionID string) error {
	f.calls = append(f.calls, fmt.Sprintf("DeleteVersion %s %s", key, versionID))
	f.versions[key] = slices.DeleteFunc(f.versions[key], func(v objstore.Version) bool { return v.VersionID == versionID })
	if len(f.versions[key]) > 0 {
		f.versions[key][0].IsLatest = true
	}
	return nil
}

func (f *contentFake) AbortUpload(_ context.Context, _, key, uploadID string) error {
	f.calls = append(f.calls, fmt.Sprintf("AbortUpload %s %s", key, uploadID))
	f.uploads = slices.DeleteFunc(f.uploads, func(u objstore.Upload) bool { return u.Key == key && u.UploadID == uploadID })
	return nil
}

func (f *contentFake) ObjectTags(_ context.Context, _, key, versionID string) (map[string]string, error) {
	return f.tags[key+"@"+versionID], nil
}

func (f *contentFake) Delete(_ context.Context, _, key string) error {
	f.calls = append(f.calls, "Delete "+key)
	if !f.versioned {
		delete(f.versions, key)
		f.keys = slices.DeleteFunc(f.keys, func(k string) bool { return k == key })
		return nil
	}
	f.putMarker(key, "marker-of-"+key, time.Time{})
	return nil
}
