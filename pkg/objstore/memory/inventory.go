package memory

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
)

var _ objstore.Inventory = (*Store)(nil)

// ListVersions implements objstore.Inventory: each current object is its own
// null version, because the store keeps no history.
func (s *Store) ListVersions(_ context.Context, name, prefix string) iter.Seq2[objstore.Version, error] {
	return func(yield func(objstore.Version, error) bool) {
		s.mu.RLock()
		b, ok := s.buckets[name]
		var vs []objstore.Version
		if ok {
			for _, k := range slices.Sorted(maps.Keys(b.objects)) {
				if !strings.HasPrefix(k, prefix) {
					continue
				}
				info := b.objects[k].info
				vs = append(vs, objstore.Version{
					Key: k, VersionID: objstore.NullVersion, IsLatest: true, Size: info.Size,
					ETag: info.ETag, StorageClass: "STANDARD", LastModified: info.LastModified,
				})
			}
		}
		s.mu.RUnlock()
		if !ok {
			yield(objstore.Version{}, fmt.Errorf("bucket %s: %w", name, objstore.ErrBucketNotFound))
			return
		}
		for _, v := range vs {
			if !yield(v, nil) {
				return
			}
		}
	}
}

// ListUploads implements objstore.Inventory: the store has no multipart uploads.
func (s *Store) ListUploads(_ context.Context, name string) iter.Seq2[objstore.Upload, error] {
	return func(yield func(objstore.Upload, error) bool) {
		s.mu.RLock()
		_, ok := s.buckets[name]
		s.mu.RUnlock()
		if !ok {
			yield(objstore.Upload{}, fmt.Errorf("bucket %s: %w", name, objstore.ErrBucketNotFound))
		}
	}
}

// DeleteVersion implements objstore.Inventory: only the null version exists.
func (s *Store) DeleteVersion(ctx context.Context, name, key, versionID string) error {
	if versionID != objstore.NullVersion {
		return nil
	}
	return s.Delete(ctx, name, key)
}

// AbortUpload implements objstore.Inventory: there are no uploads to abort.
func (s *Store) AbortUpload(context.Context, string, string, string) error { return nil }

// ObjectTags implements objstore.Inventory: the store keeps no tags.
func (s *Store) ObjectTags(_ context.Context, name, key, _ string) (map[string]string, error) {
	if _, err := s.lookup(name, key); err != nil {
		return nil, err
	}
	return map[string]string{}, nil
}
