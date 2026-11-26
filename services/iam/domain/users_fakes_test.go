package domain_test

import (
	"context"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// fakeTUAs is a slice-backed domain.TenantUserAccountRepository that also
// stores user attributes.
type fakeTUAs struct {
	items []*iampb.TenantUserAccount
	attrs map[string]*iampb.UserAttributes
}

func newFakeTUAs() *fakeTUAs { return &fakeTUAs{attrs: map[string]*iampb.UserAttributes{}} }

func (f *fakeTUAs) Get(_ context.Context, id string) (*iampb.TenantUserAccount, error) {
	for _, a := range f.items {
		if a.GetMetadata().GetId() == id {
			return proto.CloneOf(a), nil
		}
	}
	return nil, errs.NotFound(id)
}

func (f *fakeTUAs) List(_ context.Context, tenantID string, _ int, _ string) (domain.TenantUserAccountPage, error) {
	var out []*iampb.TenantUserAccount
	for _, a := range f.items {
		if a.GetMetadata().GetParentId() == tenantID {
			out = append(out, proto.CloneOf(a))
		}
	}
	return domain.TenantUserAccountPage{Items: out}, nil
}

func (f *fakeTUAs) Ensure(ctx context.Context, a *iampb.TenantUserAccount) error {
	if _, err := f.Get(ctx, a.GetMetadata().GetId()); err == nil {
		return nil
	}
	return f.Create(ctx, a)
}

func (f *fakeTUAs) Create(_ context.Context, a *iampb.TenantUserAccount) error {
	f.items = append(f.items, proto.CloneOf(a))
	return nil
}

func (f *fakeTUAs) Update(_ context.Context, a *iampb.TenantUserAccount) error {
	for i, e := range f.items {
		if e.GetMetadata().GetId() == a.GetMetadata().GetId() {
			f.items[i] = proto.CloneOf(a)
			return nil
		}
	}
	return errs.NotFound(a.GetMetadata().GetId())
}

func (f *fakeTUAs) Delete(_ context.Context, id string) error {
	n := len(f.items)
	f.items = slices.DeleteFunc(f.items, func(a *iampb.TenantUserAccount) bool { return a.GetMetadata().GetId() == id })
	if len(f.items) == n {
		return errs.NotFound(id)
	}
	delete(f.attrs, id)
	return nil
}

func (f *fakeTUAs) CountByFederation(_ context.Context, federationID string) (int, error) {
	n := 0
	for _, a := range f.items {
		if a.GetStatus().GetFederationId() == federationID {
			n++
		}
	}
	return n, nil
}

func (f *fakeTUAs) Attributes(_ context.Context, id string) (*iampb.UserAttributes, error) {
	if a, ok := f.attrs[id]; ok {
		return proto.CloneOf(a), nil
	}
	return &iampb.UserAttributes{}, nil
}

func (f *fakeTUAs) SetAttributes(_ context.Context, id string, a *iampb.UserAttributes) error {
	f.attrs[id] = proto.CloneOf(a)
	return nil
}

// fakeInvitations is a slice-backed domain.InvitationRepository.
type fakeInvitations struct {
	items    []*iampb.Invitation
	validity map[string]time.Duration
}

func newFakeInvitations() *fakeInvitations {
	return &fakeInvitations{validity: map[string]time.Duration{}}
}

func (f *fakeInvitations) Get(_ context.Context, id string) (*iampb.Invitation, error) {
	for _, i := range f.items {
		if i.GetMetadata().GetId() == id {
			return proto.CloneOf(i), nil
		}
	}
	return nil, errs.NotFound(id)
}

func (f *fakeInvitations) List(_ context.Context, tenantID string, _ int, _ string) (domain.InvitationPage, error) {
	var out []*iampb.Invitation
	for _, i := range f.items {
		if i.GetMetadata().GetParentId() == tenantID {
			out = append(out, proto.CloneOf(i))
		}
	}
	return domain.InvitationPage{Items: out}, nil
}

func (f *fakeInvitations) Create(_ context.Context, inv *iampb.Invitation, validity time.Duration) error {
	f.items = append(f.items, proto.CloneOf(inv))
	f.validity[inv.GetMetadata().GetId()] = validity
	return nil
}

func (f *fakeInvitations) Update(_ context.Context, inv *iampb.Invitation) error {
	for i, e := range f.items {
		if e.GetMetadata().GetId() == inv.GetMetadata().GetId() {
			f.items[i] = proto.CloneOf(inv)
			return nil
		}
	}
	return errs.NotFound(inv.GetMetadata().GetId())
}

func (f *fakeInvitations) Delete(_ context.Context, id string) error {
	f.items = slices.DeleteFunc(f.items, func(i *iampb.Invitation) bool { return i.GetMetadata().GetId() == id })
	delete(f.validity, id)
	return nil
}

func (f *fakeInvitations) Validity(_ context.Context, id string) (time.Duration, error) {
	if v, ok := f.validity[id]; ok {
		return v, nil
	}
	return 0, errs.NotFound(id)
}
