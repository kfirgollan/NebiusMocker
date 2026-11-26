// Package applications is the NebiusMocker module for Nebius Applications
// (nebius.applications.v1alpha1).
//
// Implemented: K8sReleaseService, with inventory-only semantics. A release is
// validated (cluster, product catalog, namespace, application name, values)
// and walks its status.state, but nothing is installed into the Kubernetes
// cluster it names. VmAppTemplateService is registered and answers
// UNIMPLEMENTED. See docs/services/applications.md.
package applications

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	applicationsv1alpha1pb "github.com/nebius/gosdk/proto/nebius/applications/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/applications/api"
	"github.com/kfirgollan/NebiusMocker/services/applications/dal"
	"github.com/kfirgollan/NebiusMocker/services/applications/domain"
)

// Module is the Applications service module.
type Module struct{}

var _ mocker.Module = (*Module)(nil)

// New returns the Applications module.
func New() *Module { return &Module{} }

// Name implements mocker.Module.
func (*Module) Name() string { return "applications" }

// Register wires the K8s release service, its authorization, and the
// capability mk8s uses to remove a deleted cluster's releases.
func (*Module) Register(_ context.Context, reg grpc.ServiceRegistrar, deps mocker.Deps) error {
	releases := dal.NewReleaseRepo(deps.Store)
	svc, err := domain.New(domain.Deps{
		Releases: releases, Clusters: peerClusters{deps.Peers}, Hierarchy: deps.Authz,
		Ops: deps.Ops, IDs: deps.IDs, Clock: deps.Clock,
		Config: domain.Config{ProjectID: deps.Config.ProjectID, Products: deps.Config.Applications.Products},
	})
	if err != nil {
		return fmt.Errorf("applications: %w", err)
	}
	deps.Authz.RegisterKind(domain.KindRelease, func(ctx context.Context, id string) (string, error) {
		r, err := releases.Get(ctx, id)
		return r.GetMetadata().GetParentId(), err
	})
	if err := peer.Provide[peer.ClusterReleases](deps.Peers, peer.ClusterReleasesKey, svc); err != nil {
		return fmt.Errorf("applications: %w", err)
	}
	api.RegisterAuthzRules(deps.Rules)
	applicationsv1alpha1pb.RegisterK8SReleaseServiceServer(reg, api.NewReleaseHandler(svc))
	applicationsv1alpha1pb.RegisterVmAppTemplateServiceServer(reg, api.VmAppTemplateHandler{})
	return nil
}

// peerClusters resolves clusters through the mk8s module's capability,
// looked up per call (module order is not guaranteed; mk8s may be disabled).
type peerClusters struct{ d *peer.Directory }

var _ domain.Clusters = peerClusters{}

func (p peerClusters) Cluster(ctx context.Context, id string) (domain.Cluster, error) {
	c, ok := peer.Lookup(p.d, peer.MK8sClustersKey)
	if !ok {
		return domain.Cluster{}, errs.BadRequest(errs.Violation{
			Field: "cluster_id", Message: "releases are installed into mk8s clusters, and the mk8s module is not enabled",
		})
	}
	cl, err := c.Cluster(ctx, id)
	if err != nil {
		return domain.Cluster{}, err
	}
	return domain.Cluster{ID: cl.ID, ProjectID: cl.ProjectID, Running: cl.Running}, nil
}
