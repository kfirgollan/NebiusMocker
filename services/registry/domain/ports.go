// Package domain holds the Container Registry business logic: registries,
// artifacts and docker data-plane access decisions. It is transport-free and
// depends only on the ports declared in this file.
package domain

import (
	"context"
	"time"

	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// Resource type prefixes (NID types) owned by the registry area.
const (
	KindRegistry = "registry"
	KindArtifact = "artifact"
	// KindProject is the only allowed registry parent (owned by IAM).
	KindProject = "project"
)

// RegistryPage is one page of a registry listing.
type RegistryPage struct {
	Items         []*registrypb.Registry
	NextPageToken string
}

// RegistryRepository stores registry metadata (the source of truth; the
// backend only holds images).
type RegistryRepository interface {
	Get(ctx context.Context, id string) (*registrypb.Registry, error)
	GetByName(ctx context.Context, parentID, name string) (*registrypb.Registry, error)
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (RegistryPage, error)
	// Create fails with errs.AlreadyExists if the name is taken in the parent.
	Create(ctx context.Context, r *registrypb.Registry) error
	// Update fails with errs.NotFound if the registry does not exist.
	Update(ctx context.Context, r *registrypb.Registry) error
	Delete(ctx context.Context, id string) error
}

// ArtifactRef identifies one image manifest inside a registry.
type ArtifactRef struct {
	RegistryID string
	Repository string // image name inside the registry, e.g. "app" or "team/app"
	Digest     string // "sha256:…"
}

// ArtifactIndex gives backend images stable Nebius artifact IDs. It stores
// only the mapping; image data is always read live from the backend.
type ArtifactIndex interface {
	// Lookup returns the ID assigned to ref, or errs.NotFound.
	Lookup(ctx context.Context, ref ArtifactRef) (string, error)
	// Assign stores id for ref; errs.AlreadyExists if ref already has an ID.
	Assign(ctx context.Context, id string, ref ArtifactRef) error
	// Get returns the ref of an artifact ID, or errs.NotFound.
	Get(ctx context.Context, id string) (ArtifactRef, error)
	Delete(ctx context.Context, id string) error
	// DeleteRegistry forgets every artifact of a registry.
	DeleteRegistry(ctx context.Context, registryID string) error
}

// Image is one manifest stored in the backend.
type Image struct {
	Repository string
	Digest     string
	MediaType  string
	Size       int64
	Tags       []string
	// List is true for manifest lists / OCI image indexes.
	List     bool
	PushedAt time.Time
}

// Backend stores images. Each registry maps to one backend namespace (a
// Harbor project). Implementations translate their errors to pkg/errs kinds.
type Backend interface {
	// EnsureNamespace creates the namespace if it does not exist.
	EnsureNamespace(ctx context.Context, namespace string) error
	// DeleteNamespace removes the namespace and every image in it; a missing
	// namespace is not an error.
	DeleteNamespace(ctx context.Context, namespace string) error
	// ListImages returns every image of the namespace.
	ListImages(ctx context.Context, namespace string) ([]Image, error)
	// DeleteImage removes one manifest (and its tags); errs.NotFound if absent.
	DeleteImage(ctx context.Context, namespace, repository, digest string) error
}

// Hierarchy resolves resource ancestry (satisfied by *authz.Engine).
type Hierarchy interface {
	// Parent returns the parent of id, or errs.NotFound if id does not exist.
	Parent(ctx context.Context, id string) (string, error)
}

// Authorizer checks permissions (satisfied by *authz.Engine).
type Authorizer interface {
	Check(ctx context.Context, principalID string, perm authz.Permission, target string) (authz.Decision, error)
}

// Operations starts long-running operations (satisfied by *operations.Engine).
type Operations interface {
	Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error)
}

// IDGenerator creates NIDs (satisfied by pkg/nid generators).
type IDGenerator interface {
	New(typ string) (string, error)
}

// Endpoint reports the docker-facing registry host ("host:port"), used for
// status.registry_fqdn.
type Endpoint interface {
	FQDN() string
}

// QuotaGate admits a quota claim and runs apply, the store write that makes the
// consumption visible to the usage source (satisfied by peer.QuotaGate).
type QuotaGate interface {
	Admit(ctx context.Context, c peer.QuotaClaim, apply func(ctx context.Context) error) error
}
