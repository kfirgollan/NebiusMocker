package peer

import "context"

// NetworksKey is the capability provided by the vpc module: VPC networks as
// seen by resources bound to them (DNS zones: dns.v1 ZoneSpec.vpc).
var NetworksKey = NewKey[Networks]("vpc.networks")

// Networks resolves VPC networks and records which resources are bound to
// them. A network with bound resources cannot be deleted: network Delete
// fails with errs.BadState naming them, and the check is atomic against
// Bind. All calls are internal: the caller authorizes its own request.
type Networks interface {
	// Network returns the network with the ID, or errs.NotFound.
	Network(ctx context.Context, id string) (Network, error)
	// Bind atomically checks that the network exists (errs.NotFound) and
	// records userID (a NID such as "dnszone-…") as bound to it. Binding
	// twice is a no-op.
	Bind(ctx context.Context, networkID, userID string) error
	// Unbind removes the record; a missing network or record is not an error.
	Unbind(ctx context.Context, networkID, userID string) error
}

// Network is a resolved VPC network.
type Network struct {
	ID        string
	ProjectID string
	Name      string
}
