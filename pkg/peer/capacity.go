package peer

import "context"

// CapacityReservationsKey is the capability provided by the capacity module:
// the capacity block groups (reservations) that compute instances with a
// reservation_policy draw GPUs from.
var CapacityReservationsKey = NewKey[CapacityReservations]("capacity.reservations")

// CapacityReservations allocates GPUs of capacity block groups to compute
// consumers (Nebius: instance.spec.reservation_policy, whose reservation_ids
// are capacity block group IDs). It enforces the block group's current limit
// and the project's capacity allowance (effective limit = min(allowance
// limit, current block group limit)), and feeds the block group's and the
// allowance's status.usage and CapacityBlockGroupService.ListResources.
//
// It is an internal, privileged call: no IAM checks; the caller authorized
// its own request. Every method is safe for concurrent use.
type CapacityReservations interface {
	// Reserve allocates r.Quantity units for r.ConsumerID and returns the ID
	// of the block group it drew from. With r.ReservationIDs, those groups
	// are tried in order; otherwise every active group of the project's
	// tenant that matches r.Region, r.Platform and r.Fabric (when set).
	// Reserving again for a consumer that already holds a reservation
	// replaces it. Errors: errs.NotFound for an unknown reservation ID,
	// errs.BadRequest when a named reservation does not match the
	// region/platform/fabric or belongs to another tenant, and
	// errs.KindQuotaExceeded when no candidate has enough free capacity
	// (or none exists).
	Reserve(ctx context.Context, r CapacityRequest) (string, error)

	// Release frees the reservation of consumerID. A missing one is not an
	// error.
	Release(ctx context.Context, consumerID string) error
}

// CapacityRequest asks for reserved capacity for one consumer.
type CapacityRequest struct {
	// ConsumerID is the resource that uses the capacity, e.g. a compute
	// instance ID; ListResources reports it.
	ConsumerID string
	// ProjectID is the consumer's project; its capacity allowance applies.
	ProjectID string
	// Region, Platform and Fabric select matching block groups (Fabric may be
	// empty: any fabric).
	Region   string
	Platform string
	Fabric   string
	// Quantity is the number of units (GPUs) to reserve.
	Quantity uint64
	// ReservationIDs are block group IDs to try in order (empty: any match).
	ReservationIDs []string
}
