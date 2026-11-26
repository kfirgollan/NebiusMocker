// Package domain holds the Nebius DNS business logic: custom (private) DNS
// zones bound to VPC networks, their resource records, the records the
// service manages itself (SOA and NS at the apex), and name resolution for
// the mock's DNS responder. It is transport-free and depends only on the
// ports declared in this file.
package domain

import (
	"context"

	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// Resource type prefixes (NID types) owned by DNS, and the kinds it refers to.
const (
	KindZone   = "dnszone"
	KindRecord = "dnsrecord"
	// KindProject is the only parent kind a zone may have (owned by IAM).
	KindProject = "project"
	// KindNetwork is the VPC network a zone is bound to (owned by VPC).
	KindNetwork = "vpcnetwork"
)

// Page is one page of a listing.
type Page[T any] struct {
	Items         []T
	NextPageToken string
}

// ZoneRepository stores zones. Names are unique per project when set.
type ZoneRepository interface {
	// Get returns the zone or errs.NotFound.
	Get(ctx context.Context, id string) (*dnspb.Zone, error)
	// GetByName returns the zone named name in the project, or errs.NotFound.
	GetByName(ctx context.Context, parentID, name string) (*dnspb.Zone, error)
	// List returns one page of the project's zones in creation order.
	List(ctx context.Context, parentID string, pageSize int, pageToken string) (Page[*dnspb.Zone], error)
	// All returns every zone of every project in creation order.
	All(ctx context.Context) ([]*dnspb.Zone, error)
	// Create fails with errs.AlreadyExists if the name is taken in the project.
	Create(ctx context.Context, z *dnspb.Zone) error
	// Update fails with errs.NotFound if the zone does not exist, and with
	// errs.AlreadyExists if the (new) name is taken.
	Update(ctx context.Context, z *dnspb.Zone) error
	Delete(ctx context.Context, id string) error
}

// RecordRepository stores records. Names are unique per zone when set; the
// records the service creates have no name.
type RecordRepository interface {
	// Get returns the record or errs.NotFound.
	Get(ctx context.Context, id string) (*dnspb.Record, error)
	// GetByName returns the record named name in the zone, or errs.NotFound.
	GetByName(ctx context.Context, zoneID, name string) (*dnspb.Record, error)
	// List returns one page of the zone's records in creation order.
	List(ctx context.Context, zoneID string, pageSize int, pageToken string) (Page[*dnspb.Record], error)
	// All returns every record of the zone in creation order.
	All(ctx context.Context, zoneID string) ([]*dnspb.Record, error)
	// Create fails with errs.AlreadyExists if the name is taken in the zone.
	Create(ctx context.Context, r *dnspb.Record) error
	// Update fails with errs.NotFound if the record does not exist, and with
	// errs.AlreadyExists if the (new) name is taken.
	Update(ctx context.Context, r *dnspb.Record) error
	Delete(ctx context.Context, id string) error
}

// Network is a VPC network as DNS sees it.
type Network struct {
	ID        string
	ProjectID string
}

// Networks resolves VPC networks and binds zones to them, so that a network
// cannot be deleted while a zone is attached (satisfied by an adapter over
// the vpc module's peer.Networks capability).
type Networks interface {
	// Network returns the network, or errs.NotFound.
	Network(ctx context.Context, id string) (Network, error)
	// Bind atomically checks that the network exists and records zoneID as
	// bound to it.
	Bind(ctx context.Context, networkID, zoneID string) error
	// Unbind removes the binding; a missing one is not an error.
	Unbind(ctx context.Context, networkID, zoneID string) error
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

// QuotaGate admits the consumption of new zones against the project's quotas
// (satisfied by peer.QuotaGateFor). apply is the store write that makes the
// zone countable by the usage source.
type QuotaGate interface {
	Admit(ctx context.Context, c peer.QuotaClaim, apply func(ctx context.Context) error) error
}
