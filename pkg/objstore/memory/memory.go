// Package memory is a map-backed objstore.Store for Docker-free tests.
// It keeps only current object data (versioning is recorded, not applied).
package memory

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // S3 ETags are MD5 by definition, not a security control
	"encoding/hex"
	"fmt"
	"io"
	"iter"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
)

type object struct {
	info objstore.ObjectInfo
	data []byte
}

type bucket struct {
	versioning objstore.Versioning
	objects    map[string]object
}

// Store is an in-memory objstore.Store. It is safe for concurrent use.
type Store struct {
	clk     clock.Clock
	mu      sync.RWMutex
	buckets map[string]*bucket
}

var _ objstore.Store = (*Store)(nil)

// New returns an empty store that stamps LastModified with clk.
func New(clk clock.Clock) *Store {
	return &Store{clk: clk, buckets: map[string]*bucket{}}
}

// EnsureBucket implements objstore.Store.
func (s *Store) EnsureBucket(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[name]; !ok {
		s.buckets[name] = &bucket{objects: map[string]object{}}
	}
	return nil
}

// DeleteBucket implements objstore.Store.
func (s *Store) DeleteBucket(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.buckets, name)
	return nil
}

// BucketEmpty implements objstore.Store.
func (s *Store) BucketEmpty(_ context.Context, name string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.buckets[name]
	if !ok {
		return false, fmt.Errorf("bucket %s: %w", name, objstore.ErrBucketNotFound)
	}
	return len(b.objects) == 0, nil
}

// SetVersioning implements objstore.Store.
func (s *Store) SetVersioning(_ context.Context, name string, v objstore.Versioning) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return fmt.Errorf("bucket %s: %w", name, objstore.ErrBucketNotFound)
	}
	if v != objstore.VersioningUnset {
		b.versioning = v
	}
	return nil
}

// Versioning returns the recorded versioning state (for tests).
func (s *Store) Versioning(name string) (objstore.Versioning, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.buckets[name]
	if !ok {
		return objstore.VersioningUnset, false
	}
	return b.versioning, true
}

// Put implements objstore.Store.
func (s *Store) Put(ctx context.Context, name, key string, r io.Reader, size int64, opts objstore.PutOptions) (objstore.ObjectInfo, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return objstore.ObjectInfo{}, fmt.Errorf("read object %s/%s: %w", name, key, err)
	}
	if size >= 0 && int64(len(data)) != size {
		return objstore.ObjectInfo{}, fmt.Errorf("object %s/%s: read %d bytes, declared %d", name, key, len(data), size)
	}
	if err := ctx.Err(); err != nil {
		return objstore.ObjectInfo{}, err
	}
	sum := md5.Sum(data) //nolint:gosec // S3 ETag semantics
	info := objstore.ObjectInfo{
		Bucket: name, Key: key, Size: int64(len(data)), ETag: hex.EncodeToString(sum[:]),
		ContentType: opts.ContentType, Metadata: lowerKeys(opts.Metadata), LastModified: s.clk.Now(),
	}
	if info.ContentType == "" {
		info.ContentType = "binary/octet-stream"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return objstore.ObjectInfo{}, fmt.Errorf("bucket %s: %w", name, objstore.ErrBucketNotFound)
	}
	b.objects[key] = object{info: info, data: data}
	return cloneInfo(info), nil
}

// Get implements objstore.Store.
func (s *Store) Get(_ context.Context, name, key string) (io.ReadCloser, objstore.ObjectInfo, error) {
	o, err := s.lookup(name, key)
	if err != nil {
		return nil, objstore.ObjectInfo{}, err
	}
	return io.NopCloser(bytes.NewReader(o.data)), cloneInfo(o.info), nil
}

// Stat implements objstore.Store.
func (s *Store) Stat(_ context.Context, name, key string) (objstore.ObjectInfo, error) {
	o, err := s.lookup(name, key)
	if err != nil {
		return objstore.ObjectInfo{}, err
	}
	return cloneInfo(o.info), nil
}

// Delete implements objstore.Store.
func (s *Store) Delete(_ context.Context, name, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return fmt.Errorf("bucket %s: %w", name, objstore.ErrBucketNotFound)
	}
	delete(b.objects, key)
	return nil
}

// Copy implements objstore.Store.
func (s *Store) Copy(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) (objstore.ObjectInfo, error) {
	o, err := s.lookup(srcBucket, srcKey)
	if err != nil {
		return objstore.ObjectInfo{}, err
	}
	return s.Put(ctx, dstBucket, dstKey, bytes.NewReader(o.data), int64(len(o.data)),
		objstore.PutOptions{ContentType: o.info.ContentType, Metadata: o.info.Metadata})
}

// List implements objstore.Store.
func (s *Store) List(_ context.Context, name, prefix string) iter.Seq2[objstore.ObjectInfo, error] {
	return func(yield func(objstore.ObjectInfo, error) bool) {
		s.mu.RLock()
		b, ok := s.buckets[name]
		var infos []objstore.ObjectInfo
		if ok {
			for _, k := range slices.Sorted(maps.Keys(b.objects)) {
				if strings.HasPrefix(k, prefix) {
					infos = append(infos, cloneInfo(b.objects[k].info))
				}
			}
		}
		s.mu.RUnlock()
		if !ok {
			yield(objstore.ObjectInfo{}, fmt.Errorf("bucket %s: %w", name, objstore.ErrBucketNotFound))
			return
		}
		for _, info := range infos {
			if !yield(info, nil) {
				return
			}
		}
	}
}

func (s *Store) lookup(name, key string) (object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.buckets[name]
	if !ok {
		return object{}, fmt.Errorf("bucket %s: %w", name, objstore.ErrBucketNotFound)
	}
	o, ok := b.objects[key]
	if !ok {
		return object{}, fmt.Errorf("object %s/%s: %w", name, key, objstore.ErrNotFound)
	}
	return o, nil
}

func cloneInfo(i objstore.ObjectInfo) objstore.ObjectInfo {
	i.Metadata = maps.Clone(i.Metadata)
	return i
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
