package domain

import (
	"context"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Names of the scopes every project gets with its first root pool of each
// visibility.
const (
	DefaultPrivateScopeName = "default-private-scope"
	DefaultPublicScopeName  = "default-public-scope"
)

// ScopeService implements the read-only nebius.vpc.v1alpha1.ScopeService.
//
// A scope is the address space a tree of pools lives in (pool.proto: a root
// pool is "a top-level pool within scope"; a child pool is "created with the
// same scope" as its source). The public API cannot create or change scopes,
// so they are system-managed: each project has one PRIVATE and one PUBLIC
// scope, created the first time a root pool of that visibility is created in
// it and deleted with the last pool in it (so an emptied project can be
// deleted). Every pool reports its scope in status.scope_id (v1 and v1alpha1).
type ScopeService struct {
	*kind[*vpcv1alpha1pb.Scope, *vpcv1alpha1pb.ScopeSpec]
}

func newScopeService(c *core) *ScopeService {
	s := &ScopeService{}
	s.kind = &kind[*vpcv1alpha1pb.Scope, *vpcv1alpha1pb.ScopeSpec]{
		c: c, prefix: KindScope, what: "scope", repo: c.r.Scopes,
		newMsg:  func() *vpcv1alpha1pb.Scope { return &vpcv1alpha1pb.Scope{} },
		newSpec: func() *vpcv1alpha1pb.ScopeSpec { return &vpcv1alpha1pb.ScopeSpec{} },
		// The API has no Create: scopes are made only by ensureLocked.
		build: func(context.Context, *commonpb.ResourceMetadata, *vpcv1alpha1pb.ScopeSpec) (*vpcv1alpha1pb.Scope, error) {
			return nil, errs.Internal("scopes are managed by the system")
		},
	}
	return s
}

// ensureLocked returns the ID of the project's scope for vis, creating the
// scope if needed; core.mu must be held.
func (s *ScopeService) ensureLocked(ctx context.Context, projectID string, vis vpcpb.IpVisibility) (string, error) {
	name, typ := DefaultPrivateScopeName, vpcv1alpha1pb.ScopeSpec_PRIVATE
	if vis == vpcpb.IpVisibility_PUBLIC {
		name, typ = DefaultPublicScopeName, vpcv1alpha1pb.ScopeSpec_PUBLIC
	}
	if sc, err := s.repo.GetByName(ctx, projectID, name); err == nil {
		return sc.GetMetadata().GetId(), nil
	} else if !errs.IsKind(err, errs.KindNotFound) {
		return "", err
	}
	id, err := s.c.ids.New(KindScope)
	if err != nil {
		return "", err
	}
	sc := &vpcv1alpha1pb.Scope{
		Metadata: s.c.metadata(id, projectID, name, nil),
		Spec:     &vpcv1alpha1pb.ScopeSpec{Type: typ},
		Status:   &vpcv1alpha1pb.ScopeStatus{State: vpcv1alpha1pb.ScopeStatus_READY},
	}
	if err := s.repo.Create(ctx, sc); err != nil {
		return "", err
	}
	return id, nil
}
