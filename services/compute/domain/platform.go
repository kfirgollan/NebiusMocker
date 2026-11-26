package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
)

// KindPlatform is the NID type prefix of compute platforms. Platforms are
// addressed by name only, so no authz kind is registered for it.
const KindPlatform = "computeplatform"

// PlatformDeps are what PlatformService needs.
type PlatformDeps struct {
	Catalog  PlatformCatalog
	Regions  []Region
	Projects Projects
	// ProjectRegions resolves a project's region; optional.
	ProjectRegions PlatformRegions
}

// PlatformRegions resolves a project's region (peer.ProjectRegions, provided
// by the iam module, looked up at call time). ok is false when no provider
// is enabled.
type PlatformRegions interface {
	Region(ctx context.Context, projectID string) (region string, ok bool, err error)
}

// PlatformPage is one page of a platform listing.
type PlatformPage struct {
	Items         []*computepb.Platform
	NextPageToken string
}

// PlatformPreset is a platform and one of its presets, as offered in a project's region.
type PlatformPreset struct {
	Platform *computepb.Platform
	Preset   *computepb.Preset
}

type regionPlatforms struct {
	name string
	// platforms are in catalog order; parent_id is filled per request.
	platforms []*computepb.Platform
}

// PlatformService serves the platform catalog of a project's region.
type PlatformService struct {
	projects Projects
	regions  PlatformRegions
	byCode   map[string]*regionPlatforms
	byName   map[string]*regionPlatforms
	// nvl maps platform names to their NVL instance group type.
	nvl map[string]computepb.NVLInstanceGroupSpec_NVLInstanceGroupType
}

// NewPlatformService builds the per-region platforms from the catalog.
// Catalog regions that are not in d.Regions are never offered.
func NewPlatformService(d PlatformDeps) (*PlatformService, error) {
	if d.Projects == nil {
		return nil, errors.New("platform service: projects are required")
	}
	s := &PlatformService{
		projects: d.Projects, regions: d.ProjectRegions,
		byCode: map[string]*regionPlatforms{}, byName: map[string]*regionPlatforms{},
		nvl: map[string]computepb.NVLInstanceGroupSpec_NVLInstanceGroupType{},
	}
	for i := range d.Catalog.Platforms {
		def := &d.Catalog.Platforms[i]
		if def.NVLInstanceGroupType != "" {
			s.nvl[def.Name] = nvlType(def.NVLInstanceGroupType)
		}
	}
	for _, r := range d.Regions {
		rp := &regionPlatforms{name: r.Name}
		for i := range d.Catalog.Platforms {
			def := &d.Catalog.Platforms[i]
			if !def.offeredIn(r.Name) {
				continue
			}
			p, err := buildPlatform(def, r)
			if err != nil {
				return nil, err
			}
			if len(p.GetSpec().GetPresets()) > 0 {
				rp.platforms = append(rp.platforms, p)
			}
		}
		s.byCode[r.RoutingCode] = rp
		s.byName[r.Name] = rp
	}
	return s, nil
}

// buildPlatform's ID is derived from (routing code, name), like public image IDs.
func buildPlatform(def *PlatformDef, r Region) (*computepb.Platform, error) {
	id, err := nid.Deterministic(KindPlatform, r.RoutingCode, def.Name)
	if err != nil {
		return nil, err
	}
	spec := &computepb.PlatformSpec{
		GpuCountQuotaType:      def.GPUCountQuotaType,
		HumanReadableName:      def.HumanReadableName,
		AllowPresetChange:      def.AllowPresetChange,
		ShortHumanReadableName: def.ShortHumanReadableName,
		GpuMemoryGigabytes:     def.GPUMemoryGigabytes,
	}
	for i := range def.Presets {
		pr := &def.Presets[i]
		if !pr.offeredIn(r.Name) {
			continue
		}
		spec.Presets = append(spec.Presets, &computepb.Preset{
			Name: pr.Name, AllowGpuClustering: pr.AllowGPUClustering,
			Resources: &computepb.PresetResources{VcpuCount: pr.VCPUCount, MemoryGibibytes: pr.MemoryGibibytes, GpuCount: pr.GPUCount},
		})
	}
	return &computepb.Platform{
		Metadata: &commonpb.ResourceMetadata{Id: id, Name: def.Name, ResourceVersion: 1},
		Spec:     spec,
		Status:   &computepb.PlatformStatus{AllowedForPreemptibles: def.AllowedForPreemptibles},
	}, nil
}

// regionOf returns the platforms of projectID's region: the project's
// spec.region (from the iam module), or, when iam is not enabled, the routing
// code in the project NID ("project-<code><rest>"). A project whose region is
// not a configured one cannot be served: BadRequest on parent_id.
func (s *PlatformService) regionOf(ctx context.Context, projectID string) (*regionPlatforms, error) {
	if projectID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	if _, err := s.projects.Parent(ctx, projectID); err != nil {
		return nil, err
	}
	if s.regions != nil {
		region, ok, err := s.regions.Region(ctx, projectID)
		if err != nil {
			return nil, err
		}
		if ok {
			if rp, known := s.byName[region]; known {
				return rp, nil
			}
			return nil, errs.BadRequest(errs.Violation{Field: "parent_id", Message: fmt.Sprintf(
				"region %q of project %s is not configured (NEBIUS_MOCKER_REGIONS)", region, projectID)})
		}
	}
	if rest, ok := strings.CutPrefix(projectID, "project-"); ok && len(rest) >= 3 {
		if rp, known := s.byCode[rest[:3]]; known {
			return rp, nil
		}
	}
	return nil, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "unknown region"})
}

func forProject(p *computepb.Platform, projectID string) *computepb.Platform {
	c := proto.CloneOf(p)
	c.Metadata.ParentId = projectID
	return c
}

// GetByName returns the platform called name in the project's region.
func (s *PlatformService) GetByName(ctx context.Context, parentID, name string) (*computepb.Platform, error) {
	rp, err := s.regionOf(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "name", Message: "required"})
	}
	for _, p := range rp.platforms {
		if p.GetMetadata().GetName() == name {
			return forProject(p, parentID), nil
		}
	}
	return nil, errs.NotFound(name)
}

// List pages through the platforms offered in the project's region.
func (s *PlatformService) List(ctx context.Context, parentID string, size int64, token string) (PlatformPage, error) {
	n, err := pageSize(size)
	if err != nil {
		return PlatformPage{}, err
	}
	off, err := decodeOffset(token)
	if err != nil {
		return PlatformPage{}, err
	}
	rp, err := s.regionOf(ctx, parentID)
	if err != nil {
		return PlatformPage{}, err
	}
	off = min(off, len(rp.platforms))
	end := min(off+n, len(rp.platforms))
	page := PlatformPage{}
	for _, p := range rp.platforms[off:end] {
		page.Items = append(page.Items, forProject(p, parentID))
	}
	if end < len(rp.platforms) {
		page.NextPageToken = encodeOffset(end)
	}
	return page, nil
}

// NVLInstanceGroupType returns the type of the NVL instance groups that
// instances of platform can join (UNSPECIFIED: none).
func (s *PlatformService) NVLInstanceGroupType(platform string) computepb.NVLInstanceGroupSpec_NVLInstanceGroupType {
	return s.nvl[platform]
}

// Resolve returns the platform and preset for an instance in projectID.
// Errors: errs.BadRequest with Field "spec.resources.platform" when the
// platform does not exist or is not offered in the project's region (the
// message names the region), "spec.resources.preset" when the platform has no
// such preset in that region (the message lists the valid presets).
func (s *PlatformService) Resolve(ctx context.Context, projectID, platform, preset string) (PlatformPreset, error) {
	rp, err := s.regionOf(ctx, projectID)
	if err != nil {
		return PlatformPreset{}, err
	}
	for _, p := range rp.platforms {
		if p.GetMetadata().GetName() != platform {
			continue
		}
		var names []string
		for _, pr := range p.GetSpec().GetPresets() {
			if pr.GetName() == preset {
				return PlatformPreset{Platform: forProject(p, projectID), Preset: proto.CloneOf(pr)}, nil
			}
			names = append(names, pr.GetName())
		}
		return PlatformPreset{}, errs.BadRequest(errs.Violation{
			Field:   "spec.resources.preset",
			Message: fmt.Sprintf("platform %q has no preset %q in %s; valid presets: %s", platform, preset, rp.name, strings.Join(names, ", ")),
		})
	}
	return PlatformPreset{}, errs.BadRequest(errs.Violation{
		Field:   "spec.resources.platform",
		Message: fmt.Sprintf("platform %q is not available in region %s", platform, rp.name),
	})
}
