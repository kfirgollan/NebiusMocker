package domain

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

// DefaultRegion is the region of seeded tenants.
const DefaultRegion = "eu-north1"

// SeedTenant creates the tenant with the given ID if it does not exist yet.
// Tenants cannot be created through the public API, so the mocker seeds them.
func SeedTenant(ctx context.Context, tenants TenantRepository, clk clock.Clock, id, name string) error {
	now := timestamppb.New(clk.Now())
	t := &iampb.Container{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, Name: name, ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec: &iampb.ContainerSpec{Region: DefaultRegion},
		Status: &iampb.ContainerStatus{
			ContainerState: iampb.State_ACTIVE,
			Region:         DefaultRegion,
		},
	}
	if err := tenants.Ensure(ctx, t); err != nil {
		return fmt.Errorf("seed tenant %s: %w", id, err)
	}
	return nil
}
