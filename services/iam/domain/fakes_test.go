package domain_test

import (
	"context"
	"slices"

	"google.golang.org/protobuf/proto"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// fakeMemberships is an ordered, map-free domain.MembershipRepository.
type fakeMemberships struct{ items []*iampb.GroupMembership }

func newFakeMemberships() *fakeMemberships { return &fakeMemberships{} }

func (f *fakeMemberships) Get(_ context.Context, id string) (*iampb.GroupMembership, error) {
	for _, m := range f.items {
		if m.GetMetadata().GetId() == id {
			return proto.CloneOf(m), nil
		}
	}
	return nil, errs.NotFound(id)
}

func (f *fakeMemberships) Create(_ context.Context, m *iampb.GroupMembership) error {
	for _, e := range f.items {
		if e.GetMetadata().GetParentId() == m.GetMetadata().GetParentId() && e.GetSpec().GetMemberId() == m.GetSpec().GetMemberId() {
			return errs.AlreadyExists(e.GetMetadata().GetId(), "dup")
		}
	}
	f.items = append(f.items, proto.CloneOf(m))
	return nil
}

func (f *fakeMemberships) Delete(_ context.Context, id string) error {
	f.items = slices.DeleteFunc(f.items, func(m *iampb.GroupMembership) bool { return m.GetMetadata().GetId() == id })
	return nil
}

func (f *fakeMemberships) ListByGroup(_ context.Context, groupID string, _ int, _ string) (domain.MembershipPage, error) {
	var out []*iampb.GroupMembership
	for _, m := range f.items {
		if m.GetMetadata().GetParentId() == groupID {
			out = append(out, proto.CloneOf(m))
		}
	}
	return domain.MembershipPage{Items: out}, nil
}

func (f *fakeMemberships) ListByMember(_ context.Context, memberID string) ([]*iampb.GroupMembership, error) {
	var out []*iampb.GroupMembership
	for _, m := range f.items {
		if m.GetSpec().GetMemberId() == memberID {
			out = append(out, proto.CloneOf(m))
		}
	}
	return out, nil
}

// fakePermits is a slice-backed domain.PermitRepository.
type fakePermits struct{ items []*iampb.AccessPermit }

func newFakePermits() *fakePermits { return &fakePermits{} }

func (f *fakePermits) Get(_ context.Context, id string) (*iampb.AccessPermit, error) {
	for _, p := range f.items {
		if p.GetMetadata().GetId() == id {
			return proto.CloneOf(p), nil
		}
	}
	return nil, errs.NotFound(id)
}

func (f *fakePermits) Create(_ context.Context, p *iampb.AccessPermit) error {
	f.items = append(f.items, proto.CloneOf(p))
	return nil
}

func (f *fakePermits) Delete(_ context.Context, id string) error {
	f.items = slices.DeleteFunc(f.items, func(p *iampb.AccessPermit) bool { return p.GetMetadata().GetId() == id })
	return nil
}

func (f *fakePermits) ListByGroup(_ context.Context, groupID string, _ int, _ string) (domain.PermitPage, error) {
	var out []*iampb.AccessPermit
	for _, p := range f.items {
		if p.GetMetadata().GetParentId() == groupID {
			out = append(out, proto.CloneOf(p))
		}
	}
	return domain.PermitPage{Items: out}, nil
}

// fakeHierarchy maps every known ID to tenantID.
type fakeHierarchy struct{ known map[string]bool }

func (h fakeHierarchy) Root(_ context.Context, id string) (string, error) {
	if !h.known[id] {
		return "", errs.NotFound(id)
	}
	return tenantID, nil
}
