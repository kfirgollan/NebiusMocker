package domain_test

import (
	"context"
	"testing"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// namedTenants is fakeTenants plus GetByName (the seeded tenant is "default").
type namedTenants struct{ fakeTenants }

func (n namedTenants) GetByName(ctx context.Context, name string) (*iampb.Container, error) {
	if name != "default" {
		return nil, errs.NotFound(name)
	}
	return n.Get(ctx, tenantID)
}

func (namedTenants) List(context.Context, int, string) (domain.ContainerPage, error) {
	return domain.ContainerPage{}, nil
}

func TestTenantGetByNameParentID(t *testing.T) {
	t.Parallel()
	svc, err := domain.NewTenantService(namedTenants{}, newProjectFixture(t).vis)
	if err != nil {
		t.Fatalf("NewTenantService: %v", err)
	}
	tests := []struct {
		name, parent, tenant string
		want                 errs.Kind // KindUnknown: success
	}{
		{"empty parent", "", "default", errs.KindUnknown},
		{"the tenant's own ID (the provider sends one)", tenantID, "default", errs.KindUnknown},
		{"any other parent", "project-e00other", "default", errs.KindBadRequest},
		{"unknown name", "", "nope", errs.KindNotFound},
		{"no name", "", "", errs.KindBadRequest},
	}
	for _, tt := range tests {
		got, err := svc.GetByName(t.Context(), tt.parent, tt.tenant)
		if errs.KindOf(err) != tt.want {
			t.Errorf("%s: err = %v, want kind %v", tt.name, err, tt.want)
			continue
		}
		if err == nil && got.GetMetadata().GetId() != tenantID {
			t.Errorf("%s: tenant = %v", tt.name, got.GetMetadata())
		}
	}
}
