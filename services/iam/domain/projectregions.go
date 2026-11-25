package domain

import (
	"context"

	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// ProjectRegions implements peer.ProjectRegions over the project repository.
type ProjectRegions struct{ projects ContainerRepository }

var _ peer.ProjectRegions = (*ProjectRegions)(nil)

// NewProjectRegions returns a ProjectRegions.
func NewProjectRegions(projects ContainerRepository) *ProjectRegions {
	return &ProjectRegions{projects: projects}
}

// Region implements peer.ProjectRegions.
func (p *ProjectRegions) Region(ctx context.Context, projectID string) (string, error) {
	c, err := p.projects.Get(ctx, projectID)
	if err != nil {
		return "", err
	}
	return c.GetSpec().GetRegion(), nil
}
