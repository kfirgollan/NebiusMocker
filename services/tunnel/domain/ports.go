// Package domain holds the Nebius Tunnels business logic
// (nebius.tunnel.v1.TunnelService, endpoint applicationtunnel.mkt).
//
// A tunnel is inventory only. In Nebius the data plane is a proprietary agent
// (nebius-tunnel-agent) that dials a regional edge over an undocumented
// protocol. The mock has no edge, so no agent can connect: every tunnel reads
// as CREATED, CONNECTION_STATE_DISCONNECTED, with no services. See
// docs/services/tunnel.md. The package is transport-free and depends only on
// the ports declared in this file.
package domain

import (
	"context"

	tunnelpb "github.com/nebius/gosdk/proto/nebius/tunnel/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// Resource type prefixes (NID types) owned by Tunnels, and the kinds it
// refers to.
const (
	KindTunnel = "applicationtunnel"
	// KindProject is the only parent kind a tunnel may have (owned by IAM).
	KindProject = "project"
)

// Page is one page of a listing.
type Page[T any] struct {
	Items         []T
	NextPageToken string
}

// TunnelRepository stores tunnels. Tunnels are unnamed
// (resource_behavior = UNNAMED), so nothing about them is unique but the ID.
type TunnelRepository interface {
	// Get returns the tunnel or errs.NotFound.
	Get(ctx context.Context, id string) (*tunnelpb.Tunnel, error)
	// List returns every tunnel of the project in creation order.
	List(ctx context.Context, parentID string) ([]*tunnelpb.Tunnel, error)
	// Create fails with errs.AlreadyExists if the ID exists.
	Create(ctx context.Context, t *tunnelpb.Tunnel) error
	// Update fails with errs.NotFound if the tunnel does not exist.
	Update(ctx context.Context, t *tunnelpb.Tunnel) error
	// Delete fails with errs.NotFound if the tunnel does not exist.
	Delete(ctx context.Context, id string) error
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
