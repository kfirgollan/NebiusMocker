package domain

import (
	"context"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// The v1alpha1 API is a second view of the same resources: a network, subnet,
// pool or allocation has one NID and one stored form (the v1 message), and
// the api layer converts. The helpers here answer what a v1alpha1 view needs
// beyond the v1 message.

// CheckFilter refuses a non-empty List filter: the v1alpha1 List requests
// carry a filter whose format upstream leaves "TBD", and NebiusMocker does not
// guess one, so it is rejected by name rather than ignored.
func CheckFilter(filter string) error {
	if filter != "" {
		return errs.BadRequest(errs.Violation{Field: "filter", Message: "filters are not supported by NebiusMocker; list without a filter"})
	}
	return nil
}

// Scope returns the scope of a network's private pools (v1alpha1
// NetworkStatus.scope_id, "Scope ID of all pools"). Every private pool of a
// network lives in the network's project, so they share that project's
// private scope.
func (s *NetworkService) Scope(ctx context.Context, n *vpcpb.Network) (string, error) {
	for _, id := range networkPoolIDs(n, vpcpb.IpVisibility_PRIVATE) {
		p, err := s.c.r.Pools.Get(ctx, id)
		if err != nil {
			return "", err
		}
		if scope := p.GetStatus().GetScopeId(); scope != "" {
			return scope, nil
		}
	}
	return "", nil
}
