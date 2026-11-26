package domain

import (
	"context"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
)

// fakeNVLGroups is an in-memory NVLInstanceGroupRepository.
type fakeNVLGroups struct {
	repo memRepo[*computepb.NVLInstanceGroup]
}

var _ NVLInstanceGroupRepository = (*fakeNVLGroups)(nil)

func (f *fakeNVLGroups) Get(_ context.Context, id string) (*computepb.NVLInstanceGroup, error) {
	return f.repo.get(id)
}

func (f *fakeNVLGroups) GetByName(_ context.Context, parentID, name string) (*computepb.NVLInstanceGroup, error) {
	return f.repo.getByName(parentID, name)
}

func (f *fakeNVLGroups) List(_ context.Context, parentID string, size int, token string) (NVLInstanceGroupPage, error) {
	items, next, err := f.repo.page(func(g *computepb.NVLInstanceGroup) bool { return g.GetMetadata().GetParentId() == parentID }, size, token)
	return NVLInstanceGroupPage{Items: items, NextPageToken: next}, err
}

func (f *fakeNVLGroups) Create(_ context.Context, g *computepb.NVLInstanceGroup) error {
	return f.repo.create(g)
}

func (f *fakeNVLGroups) Mutate(_ context.Context, id string, fn func(*computepb.NVLInstanceGroup) error) (*computepb.NVLInstanceGroup, error) {
	return f.repo.mutate(id, fn)
}

func (f *fakeNVLGroups) DeleteIf(_ context.Context, id string, check func(*computepb.NVLInstanceGroup) error) error {
	g, err := f.repo.get(id)
	if err != nil {
		return err
	}
	if err := check(g); err != nil {
		return err
	}
	return f.repo.remove(id)
}

// seed stores a READY group of type typ and size in project.
func (f *fakeNVLGroups) seed(t *testing.T, id, project string, typ computepb.NVLInstanceGroupSpec_NVLInstanceGroupType, size int64) {
	t.Helper()
	err := f.repo.create(&computepb.NVLInstanceGroup{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: project, Name: id, ResourceVersion: 1},
		Spec:     &computepb.NVLInstanceGroupSpec{Type: typ, Size: size},
		Status:   &computepb.NVLInstanceGroupStatus{},
	})
	if err != nil {
		t.Fatal(err)
	}
}
