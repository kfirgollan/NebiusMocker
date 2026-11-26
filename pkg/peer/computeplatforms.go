package peer

import "context"

// ComputePlatformsKey is the capability provided by the compute module: the
// platform catalog as offered in a project's region (what
// nebius.compute.v1.PlatformService serves to that project).
var ComputePlatformsKey = NewKey[ComputePlatforms]("compute.platforms")

// ComputePlatforms resolves compute platforms for other areas (billing prices
// instances by preset; pricing policies apply to preemptible platforms).
// It is an internal call: no IAM checks.
type ComputePlatforms interface {
	// Platform returns the platform named name as offered in the region of
	// projectID, or errs.NotFound when the region does not offer it.
	Platform(ctx context.Context, projectID, name string) (ComputePlatform, error)
}

// ComputePlatform is one platform of the catalog, as offered in a region.
type ComputePlatform struct {
	Name string
	// AllowedForPreemptibles reports whether preemptible instances may run
	// on the platform.
	AllowedForPreemptibles bool
	Presets                []ComputePreset
}

// ComputePreset is what one preset of a platform provides.
type ComputePreset struct {
	Name      string
	GPUs      int
	VCPUs     int
	MemoryGiB int64
}
