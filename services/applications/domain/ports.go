// Package domain holds the Nebius Applications business logic: Marketplace
// K8s releases (nebius.applications.v1alpha1.K8sReleaseService).
//
// A release is inventory only: the mock validates the request, stores the
// release and walks its status.state, but installs nothing into the
// Kubernetes cluster it names. The package is transport-free and depends only
// on the ports declared in this file.
package domain

import (
	"context"

	applicationsv1alpha1pb "github.com/nebius/gosdk/proto/nebius/applications/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// Resource type prefixes (NID types) owned by Applications, and the kinds it
// refers to.
const (
	KindRelease = "applicationrelease"
	// KindProject is the only parent kind a release may have (owned by IAM).
	KindProject = "project"
	// KindCluster is the mk8s cluster a release is installed into.
	KindCluster = "mk8scluster"
)

// Page is one page of a listing.
type Page[T any] struct {
	Items         []T
	NextPageToken string
}

// ReleaseRepository stores releases. Releases are unnamed, so nothing about
// them is unique except their ID.
type ReleaseRepository interface {
	// Get returns the release (with its input-only fields) or errs.NotFound.
	Get(ctx context.Context, id string) (*applicationsv1alpha1pb.K8SRelease, error)
	// List returns every release of the project in creation order.
	List(ctx context.Context, parentID string) ([]*applicationsv1alpha1pb.K8SRelease, error)
	// All returns every release of every project in creation order.
	All(ctx context.Context) ([]*applicationsv1alpha1pb.K8SRelease, error)
	// Create fails with errs.AlreadyExists if the ID exists.
	Create(ctx context.Context, r *applicationsv1alpha1pb.K8SRelease) error
	// Update fails with errs.NotFound if the release does not exist.
	Update(ctx context.Context, r *applicationsv1alpha1pb.K8SRelease) error
	// Delete fails with errs.NotFound if the release does not exist.
	Delete(ctx context.Context, id string) error
}

// Cluster is an mk8s cluster as Applications sees it.
type Cluster struct {
	ID        string
	ProjectID string
	// Running is true when the cluster's state is RUNNING.
	Running bool
}

// Clusters resolves mk8s clusters (satisfied by an adapter over the mk8s
// module's peer.MK8sClusters capability).
type Clusters interface {
	// Cluster returns the cluster, or errs.NotFound.
	Cluster(ctx context.Context, id string) (Cluster, error)
}

// Hierarchy resolves resource ancestry (satisfied by *authz.Engine). It is
// how the domain checks that a parent project exists without importing IAM.
type Hierarchy interface {
	// Parent returns the parent of id, or errs.NotFound if id does not exist.
	Parent(ctx context.Context, id string) (string, error)
}

// Operations starts long-running operations (satisfied by *operations.Engine).
type Operations interface {
	Submit(ctx context.Context, spec operations.Spec) (*operations.Operation, error)
}

// IDGenerator creates NIDs (satisfied by pkg/nid generators).
type IDGenerator interface {
	New(typ string) (string, error)
}
