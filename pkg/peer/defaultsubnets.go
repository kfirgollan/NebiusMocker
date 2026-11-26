package peer

import "context"

// DefaultSubnetsKey is the capability provided by the vpc module: the
// default subnet of a project, for resources whose subnet defaults to it
// (mk8s ClusterSpec.control_plane.subnet_id).
var DefaultSubnetsKey = NewKey[DefaultSubnets]("vpc.default-subnets")

// DefaultSubnets resolves a project's default subnet (the default subnet of
// its default network). It is an internal call: the caller authorizes its
// own request.
type DefaultSubnets interface {
	// DefaultSubnet returns the subnet, or errs.NotFound when the project
	// has no default network.
	DefaultSubnet(ctx context.Context, projectID string) (Subnet, error)
}
