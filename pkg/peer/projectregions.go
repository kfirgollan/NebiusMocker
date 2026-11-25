package peer

import "context"

// ProjectRegionsKey is the capability provided by the iam module: the region
// of a project, for resources whose validity depends on it (for example the
// InfiniBand fabric of a compute GPU cluster).
var ProjectRegionsKey = NewKey[ProjectRegions]("iam.project-regions")

// ProjectRegions resolves the region of a project.
type ProjectRegions interface {
	// Region returns the project's spec.region (e.g. "eu-north1"), or
	// errs.NotFound if the project does not exist.
	Region(ctx context.Context, projectID string) (string, error)
}
