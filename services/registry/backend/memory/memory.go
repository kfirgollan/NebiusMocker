// Package memory is an in-process registry backend: namespaces of image
// metadata without any image bytes. It backs Docker-free unit and contract
// tests; images are added with Put instead of docker push.
package memory

import (
	"context"
	"slices"
	"sync"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/registry/domain"
)

// Backend is a map-backed domain.Backend. It is safe for concurrent use.
type Backend struct {
	mu         sync.Mutex
	namespaces map[string][]domain.Image
}

var _ domain.Backend = (*Backend)(nil)

// New returns an empty backend.
func New() *Backend { return &Backend{namespaces: map[string][]domain.Image{}} }

// EnsureNamespace implements domain.Backend.
func (b *Backend) EnsureNamespace(_ context.Context, namespace string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.namespaces[namespace]; !ok {
		b.namespaces[namespace] = nil
	}
	return nil
}

// DeleteNamespace implements domain.Backend.
func (b *Backend) DeleteNamespace(_ context.Context, namespace string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.namespaces, namespace)
	return nil
}

// ListImages implements domain.Backend.
func (b *Backend) ListImages(_ context.Context, namespace string) ([]domain.Image, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	images, ok := b.namespaces[namespace]
	if !ok {
		return nil, errs.NotFound(namespace)
	}
	out := make([]domain.Image, 0, len(images))
	for _, img := range images {
		img.Tags = slices.Clone(img.Tags)
		out = append(out, img)
	}
	return out, nil
}

// DeleteImage implements domain.Backend.
func (b *Backend) DeleteImage(_ context.Context, namespace, repository, digest string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	images, ok := b.namespaces[namespace]
	if !ok {
		return errs.NotFound(namespace)
	}
	i := slices.IndexFunc(images, func(img domain.Image) bool { return img.Repository == repository && img.Digest == digest })
	if i < 0 {
		return errs.NotFound(repository + "@" + digest)
	}
	b.namespaces[namespace] = slices.Delete(images, i, i+1)
	return nil
}

// Put stores an image in an existing namespace, replacing one with the same
// repository and digest (the test stand-in for docker push).
func (b *Backend) Put(namespace string, img domain.Image) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	images, ok := b.namespaces[namespace]
	if !ok {
		return errs.NotFound(namespace)
	}
	img.Tags = slices.Clone(img.Tags)
	images = slices.DeleteFunc(images, func(x domain.Image) bool { return x.Repository == img.Repository && x.Digest == img.Digest })
	b.namespaces[namespace] = append(images, img)
	return nil
}
