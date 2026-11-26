package domain

import (
	"context"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
)

// NetworkBindings lets other services bind their resources to a network. A
// network with bindings cannot be deleted. Bind and Unbind take the same
// lock as network Delete's reference check, so they cannot interleave.
type NetworkBindings struct{ c *core }

// NetworkBindings returns the network binding operations.
func (s *Service) NetworkBindings() *NetworkBindings { return &NetworkBindings{c: s.c} }

// Network returns the network or errs.NotFound.
func (b *NetworkBindings) Network(ctx context.Context, id string) (*vpcpb.Network, error) {
	return b.c.r.Networks.Get(ctx, id)
}

// Bind records userID as bound to the network; errs.NotFound if the network
// does not exist. Binding twice is a no-op.
func (b *NetworkBindings) Bind(ctx context.Context, networkID, userID string) error {
	b.c.mu.Lock()
	defer b.c.mu.Unlock()
	if _, err := b.c.r.Networks.Get(ctx, networkID); err != nil {
		return err
	}
	return b.c.r.NetworkUsers.Add(ctx, networkID, userID)
}

// Unbind removes the binding; a missing one is not an error.
func (b *NetworkBindings) Unbind(ctx context.Context, networkID, userID string) error {
	b.c.mu.Lock()
	defer b.c.mu.Unlock()
	return b.c.r.NetworkUsers.Remove(ctx, networkID, userID)
}
