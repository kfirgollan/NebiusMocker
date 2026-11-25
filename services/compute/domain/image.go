package domain

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// ImageService implements nebius.compute.v1.ImageService behavior.
type ImageService struct {
	images   ImageRepository
	opIndex  OperationIndex
	blobs    Blobs
	projects Projects
	disks    DiskReader
	locks    DiskLocker
	ops      Operations
	ids      IDGenerator
	clk      clock.Clock
	regions  []Region
	public   map[string]string // public-images project ID → region name
}

// ImageDeps are the dependencies of an ImageService. All are required;
// Regions must not be empty and its first entry is the default region.
type ImageDeps struct {
	Images    ImageRepository
	OpIndex   OperationIndex
	Blobs     Blobs
	Projects  Projects
	Disks     DiskReader
	DiskLocks DiskLocker
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
	Regions   []Region
}

// NewImageService wires an ImageService.
func NewImageService(d ImageDeps) (*ImageService, error) {
	if d.Images == nil || d.OpIndex == nil || d.Blobs == nil || d.Projects == nil || d.Disks == nil || d.DiskLocks == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("compute: image service dependencies must not be nil")
	}
	if len(d.Regions) == 0 {
		return nil, errors.New("compute: at least one region is required")
	}
	public := make(map[string]string, len(d.Regions))
	for _, r := range d.Regions {
		public[PublicProjectID(r.RoutingCode)] = r.Name
	}
	return &ImageService{
		images: d.Images, opIndex: d.OpIndex, blobs: d.Blobs, projects: d.Projects, disks: d.Disks, locks: d.DiskLocks,
		ops: d.Ops, ids: d.IDs, clk: d.Clock, regions: slices.Clone(d.Regions), public: public,
	}, nil
}

// CreateImageCmd is the input of Create.
type CreateImageCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *computepb.ImageSpec
	// Request is echoed in Operation.request.
	Request proto.Message
}

// UpdateImageCmd is the input of Update (full replace guarded by a reset mask).
type UpdateImageCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *computepb.ImageSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// OperationPage is one page of operations.
type OperationPage struct {
	Items         []*operations.Operation
	NextPageToken string
}

// IsPublicProject reports whether id is a region's public-images project.
func (s *ImageService) IsPublicProject(id string) bool {
	_, ok := s.public[id]
	return ok
}

// IsPublicImage reports whether id is an existing public image. Public images
// are readable by every authenticated principal.
func (s *ImageService) IsPublicImage(ctx context.Context, id string) bool {
	img, err := s.images.Get(ctx, id)
	return err == nil && s.IsPublicProject(img.GetMetadata().GetParentId())
}

// DefaultPublicProject is the public-images project of the default region,
// the upstream default parent of GetLatestByFamily.
func (s *ImageService) DefaultPublicProject() string {
	return PublicProjectID(s.regions[0].RoutingCode)
}

// Get returns an image by ID.
func (s *ImageService) Get(ctx context.Context, id string) (*computepb.Image, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	return s.images.Get(ctx, id)
}

// GetByName returns an image by its name within a project.
func (s *ImageService) GetByName(ctx context.Context, parentID, name string) (*computepb.Image, error) {
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	return s.images.GetByName(ctx, parentID, name)
}

// List returns one page of a project's images in creation order. The upstream
// filter field is deprecated and "not implemented", so it is ignored.
func (s *ImageService) List(ctx context.Context, q ListImagesQuery) (ImagePage, error) {
	if q.Filter != "" {
		// Upstream: "it is not implemented, filtering could be done on client side".
		// Rejected by name rather than silently ignored.
		return ImagePage{}, errs.BadRequest(errs.Violation{
			Field: "filter", Message: "filter is not supported (deprecated upstream); filter on the client side",
		})
	}
	return s.list(ctx, q.ParentID, q.PageSize, q.PageToken)
}

// ListImagesQuery is the input of List.
type ListImagesQuery struct {
	ParentID  string
	PageSize  int64
	PageToken string
	// Filter is the deprecated ListImagesRequest.filter; it must be empty.
	Filter string
}

func (s *ImageService) list(ctx context.Context, parentID string, size int64, token string) (ImagePage, error) {
	if parentID == "" {
		return ImagePage{}, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	n, err := pageSize(size)
	if err != nil {
		return ImagePage{}, err
	}
	return s.images.List(ctx, parentID, n, token)
}

// ListPublic returns one page of a region's public images.
func (s *ImageService) ListPublic(ctx context.Context, region string, size int64, token string) (ImagePage, error) {
	i := slices.IndexFunc(s.regions, func(r Region) bool { return r.Name == region })
	if i < 0 {
		return ImagePage{}, errs.BadRequest(errs.Violation{Field: "region", Message: fmt.Sprintf("unknown region %q", region)})
	}
	return s.list(ctx, PublicProjectID(s.regions[i].RoutingCode), size, token)
}

// GetLatestByFamily returns the newest READY image of a family that is not
// deprecated. If every version is deprecated it returns the newest deprecated
// one, whose status.image_family_deprecation tells the caller (disks cannot be
// created from a deprecated family, but the image is still readable).
// parentID defaults to the default region's public-images project.
func (s *ImageService) GetLatestByFamily(ctx context.Context, family, parentID string) (*computepb.Image, error) {
	if family == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "image_family", Message: "required"})
	}
	if parentID == "" {
		parentID = s.DefaultPublicProject()
	}
	all, err := s.images.All(ctx, parentID)
	if err != nil {
		return nil, err
	}
	candidates := slices.DeleteFunc(all, func(img *computepb.Image) bool {
		return img.GetSpec().GetImageFamily() != family || img.GetStatus().GetState() != computepb.ImageStatus_READY
	})
	if len(candidates) == 0 {
		return nil, &errs.Error{Kind: errs.KindNotFound, ResourceID: family, Message: fmt.Sprintf("no image of family %q in %s", family, parentID)}
	}
	slices.SortStableFunc(candidates, newerFirst)
	for _, img := range candidates {
		if !s.deprecated(img) {
			return img, nil
		}
	}
	return candidates[0], nil
}

// newerFirst orders images by version (descending), then creation time.
func newerFirst(a, b *computepb.Image) int {
	if c := cmp.Compare(b.GetSpec().GetVersion(), a.GetSpec().GetVersion()); c != 0 {
		return c
	}
	return b.GetMetadata().GetCreatedAt().AsTime().Compare(a.GetMetadata().GetCreatedAt().AsTime())
}

func (s *ImageService) deprecated(img *computepb.Image) bool {
	d := img.GetStatus().GetImageFamilyDeprecation()
	return d != nil && d.GetDeprecatedAt() != nil && !s.clk.Now().Before(d.GetDeprecatedAt().AsTime())
}

// Create validates the request, stores the image as CREATING and imports its
// payload into the image store when the operation completes.
func (s *ImageService) Create(ctx context.Context, cmd CreateImageCmd) (*operations.Operation, error) {
	img, src, err := s.newImage(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return s.submit(ctx, img.GetMetadata().GetParentId(), operations.Spec{
		ResourceID:  img.GetMetadata().GetId(),
		Description: "Create image",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.prepareCreate(ctx, img, src) },
		Work:        func(ctx context.Context) error { return s.importSource(ctx, img.GetMetadata().GetId(), src) },
	})
}

// Update applies a full-replace update with reset-mask semantics. Only
// metadata (name, labels) and the mutable spec fields may change.
func (s *ImageService) Update(ctx context.Context, cmd UpdateImageCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	stored, err := s.mutable(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.submit(ctx, stored.GetMetadata().GetParentId(), operations.Spec{
		ResourceID:  id,
		Description: "Update image",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.applyUpdate(ctx, cmd) },
	})
}

// Delete marks the image DELETING, then removes its payload and record.
func (s *ImageService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	stored, err := s.mutable(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.submit(ctx, stored.GetMetadata().GetParentId(), operations.Spec{
		ResourceID:  id,
		Description: "Delete image",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			return s.setState(ctx, id, computepb.ImageStatus_DELETING, "")
		},
		Work: func(ctx context.Context) error {
			if err := s.blobs.DeleteImage(ctx, id); err != nil {
				return fmt.Errorf("delete payload of %s: %w", id, err)
			}
			// An image that is gone holds no lock on its source disk (the lock is
			// normally released when creation finishes; this covers a failed one).
			if diskID := stored.GetSpec().GetSourceDiskId(); diskID != "" {
				if err := s.locks.UnlockForImage(ctx, diskID, id); err != nil {
					return fmt.Errorf("unlock disk %s: %w", diskID, err)
				}
			}
			return s.images.Delete(ctx, id)
		},
	})
}

// ListOperationsByParent returns the operations on images of a project,
// newest first, including operations on images deleted since.
func (s *ImageService) ListOperationsByParent(ctx context.Context, parentID string, size int64, token string) (OperationPage, error) {
	if parentID == "" {
		return OperationPage{}, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	n, err := pageSize(size)
	if err != nil {
		return OperationPage{}, err
	}
	offset, err := decodeOffset(token)
	if err != nil {
		return OperationPage{}, err
	}
	ids, err := s.opIndex.IDs(ctx, parentID)
	if err != nil {
		return OperationPage{}, err
	}
	slices.Reverse(ids)
	var page OperationPage
	for i := offset; i < len(ids) && len(page.Items) < n; i++ {
		op, err := s.ops.Get(ctx, ids[i])
		if err != nil {
			return OperationPage{}, err
		}
		page.Items = append(page.Items, op)
	}
	if next := offset + n; next < len(ids) {
		page.NextPageToken = encodeOffset(next)
	}
	return page, nil
}

// submit starts an operation and records it under the image's parent.
func (s *ImageService) submit(ctx context.Context, parentID string, spec operations.Spec) (*operations.Operation, error) {
	op, err := s.ops.Submit(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := s.opIndex.Add(ctx, parentID, op.ID); err != nil {
		return nil, fmt.Errorf("index operation %s: %w", op.ID, err)
	}
	return op, nil
}

// mutable returns a stored image that callers may change: public images belong
// to Nebius and are read-only.
func (s *ImageService) mutable(ctx context.Context, id string) (*computepb.Image, error) {
	img, err := s.images.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.IsPublicProject(img.GetMetadata().GetParentId()) {
		return nil, &errs.Error{Kind: errs.KindPermissionDenied, ResourceID: id, Message: "public images are read-only"}
	}
	return img, nil
}

func (s *ImageService) setState(ctx context.Context, id string, state computepb.ImageStatus_State, description string) error {
	img, err := s.images.Get(ctx, id)
	if err != nil {
		return err
	}
	img.Status.State = state
	img.Status.StateDescription = description
	img.Status.Reconciling = state == computepb.ImageStatus_CREATING || state == computepb.ImageStatus_DELETING
	return s.images.Update(ctx, img)
}

// newImage builds the CREATING image for a Create request.
func (s *ImageService) newImage(ctx context.Context, cmd CreateImageCmd) (*computepb.Image, *imageSource, error) {
	md := cmd.Metadata
	if md.GetId() != "" {
		return nil, nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	parentID := md.GetParentId()
	if err := s.checkParent(ctx, parentID); err != nil {
		return nil, nil, err
	}
	src, err := sourceOf(cmd.Spec)
	if err != nil {
		return nil, nil, err
	}
	id, err := s.ids.New(KindImage)
	if err != nil {
		return nil, nil, fmt.Errorf("new image id: %w", err)
	}
	spec := proto.CloneOf(cmd.Spec) // cpu_architecture defaults are applied by resolveSource
	now := timestamppb.New(s.clk.Now())
	return &computepb.Image{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: parentID, Name: md.GetName(), Labels: maps.Clone(md.GetLabels()),
			ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec:   spec,
		Status: &computepb.ImageStatus{State: computepb.ImageStatus_CREATING, Reconciling: true},
	}, src, nil
}

func (s *ImageService) checkParent(ctx context.Context, parentID string) error {
	if parentID == "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if s.IsPublicProject(parentID) {
		return &errs.Error{Kind: errs.KindPermissionDenied, ResourceID: parentID, Message: "public image projects are read-only"}
	}
	if _, err := s.projects.Parent(ctx, parentID); err != nil {
		return fmt.Errorf("image parent %s: %w", parentID, err)
	}
	return nil
}

// prepareCreate runs under the resource lock: the source must exist (and a
// disk or snapshot must be READY), the family/version pair must be free, then
// the CREATING image is stored.
func (s *ImageService) prepareCreate(ctx context.Context, img *computepb.Image, src *imageSource) error {
	if err := s.checkFamilyVersion(ctx, img); err != nil {
		return err
	}
	if err := s.resolveSource(ctx, img, src); err != nil {
		return err
	}
	if err := s.images.Create(ctx, img); err != nil {
		// No image, so no lock: release what resolveSource took.
		return errors.Join(err, s.unlockSource(ctx, img.GetMetadata().GetId(), src))
	}
	return nil
}

func (s *ImageService) checkFamilyVersion(ctx context.Context, img *computepb.Image) error {
	family, version := img.GetSpec().GetImageFamily(), img.GetSpec().GetVersion()
	if family == "" || version == "" {
		return nil
	}
	all, err := s.images.All(ctx, img.GetMetadata().GetParentId())
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.GetSpec().GetImageFamily() == family && other.GetSpec().GetVersion() == version {
			return errs.AlreadyExists(other.GetMetadata().GetId(), fmt.Sprintf("image family %q already has version %q", family, version))
		}
	}
	return nil
}

func (s *ImageService) applyUpdate(ctx context.Context, cmd UpdateImageCmd) error {
	stored, err := s.images.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	if err := checkUpdatable(stored, cmd.Metadata); err != nil {
		return err
	}
	updated, err := patchImage(stored, cmd)
	if err != nil {
		return err
	}
	if proto.Equal(updated, stored) {
		return nil // nothing changed: keep resource_version and updated_at
	}
	updated.Metadata.ResourceVersion = stored.GetMetadata().GetResourceVersion() + 1
	updated.Metadata.UpdatedAt = timestamppb.New(s.clk.Now())
	return s.images.Update(ctx, updated)
}
