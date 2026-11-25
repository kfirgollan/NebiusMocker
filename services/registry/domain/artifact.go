package domain

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// ArtifactService exposes the images stored in a registry's backend
// namespace as Nebius artifacts. Image data is always read live from the
// backend; the index only keeps artifact IDs stable.
type ArtifactService struct {
	registries RegistryRepository
	index      ArtifactIndex
	backend    Backend
	ops        Operations
	ids        IDGenerator
}

// NewArtifactService wires an ArtifactService. All dependencies are required.
func NewArtifactService(registries RegistryRepository, index ArtifactIndex, backend Backend, ops Operations, ids IDGenerator) (*ArtifactService, error) {
	if registries == nil || index == nil || backend == nil || ops == nil || ids == nil {
		return nil, errors.New("registry: artifact service dependencies must not be nil")
	}
	return &ArtifactService{registries: registries, index: index, backend: backend, ops: ops, ids: ids}, nil
}

// ArtifactPage is one page of an artifact listing.
type ArtifactPage struct {
	Items         []*registrypb.Artifact
	NextPageToken string
}

// List returns one page of a registry's artifacts ordered by (image name, digest).
func (s *ArtifactService) List(ctx context.Context, registryID string, pageSize int64, pageToken string) (ArtifactPage, error) {
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return ArtifactPage{}, err
	}
	offset, err := decodeOffset(pageToken)
	if err != nil {
		return ArtifactPage{}, err
	}
	images, err := s.images(ctx, registryID)
	if err != nil {
		return ArtifactPage{}, err
	}
	var page ArtifactPage
	end := min(offset+size, len(images))
	if offset < end {
		for _, img := range images[offset:end] {
			a, err := s.toArtifact(ctx, registryID, img)
			if err != nil {
				return ArtifactPage{}, err
			}
			page.Items = append(page.Items, a)
		}
	}
	if end < len(images) {
		page.NextPageToken = encodeOffset(end)
	}
	return page, nil
}

// Get returns one artifact by ID.
func (s *ArtifactService) Get(ctx context.Context, id string) (*registrypb.Artifact, error) {
	_, img, err := s.resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	return toArtifactProto(id, img), nil
}

// Delete removes the image manifest (and its tags) from the backend.
func (s *ArtifactService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	var ref ArtifactRef
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete artifact",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			var err error
			ref, _, err = s.resolve(ctx, id)
			return err
		},
		Work: func(ctx context.Context) error {
			err := s.backend.DeleteImage(ctx, Namespace(ref.RegistryID), ref.Repository, ref.Digest)
			if err != nil && !errs.IsKind(err, errs.KindNotFound) {
				return fmt.Errorf("delete image %s@%s: %w", ref.Repository, ref.Digest, err)
			}
			return s.index.Delete(ctx, id)
		},
	})
}

// RegistryOf returns the registry that holds an artifact (its authz parent).
func (s *ArtifactService) RegistryOf(ctx context.Context, id string) (string, error) {
	ref, err := s.index.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return ref.RegistryID, nil
}

// resolve finds the live image of an artifact ID. An ID whose image is gone
// from the backend is forgotten and reported as not found.
func (s *ArtifactService) resolve(ctx context.Context, id string) (ArtifactRef, Image, error) {
	ref, err := s.index.Get(ctx, id)
	if err != nil {
		return ArtifactRef{}, Image{}, err
	}
	images, err := s.images(ctx, ref.RegistryID)
	if err != nil {
		return ArtifactRef{}, Image{}, err
	}
	i := slices.IndexFunc(images, func(img Image) bool {
		return img.Repository == ref.Repository && img.Digest == ref.Digest
	})
	if i < 0 {
		if err := s.index.Delete(ctx, id); err != nil && !errs.IsKind(err, errs.KindNotFound) {
			return ArtifactRef{}, Image{}, fmt.Errorf("forget stale artifact %s: %w", id, err)
		}
		return ArtifactRef{}, Image{}, errs.NotFound(id)
	}
	return ref, images[i], nil
}

// images lists the registry's images in a stable order.
func (s *ArtifactService) images(ctx context.Context, registryID string) ([]Image, error) {
	r, err := s.registries.Get(ctx, registryID)
	if err != nil {
		return nil, err
	}
	if r.GetStatus().GetState() != registrypb.RegistryStatus_ACTIVE {
		return nil, nil
	}
	images, err := s.backend.ListImages(ctx, Namespace(registryID))
	if err != nil {
		return nil, fmt.Errorf("images of registry %s: %w", registryID, err)
	}
	slices.SortFunc(images, func(a, b Image) int {
		return cmp.Or(cmp.Compare(a.Repository, b.Repository), cmp.Compare(a.Digest, b.Digest))
	})
	return images, nil
}

// toArtifact converts an image, assigning a stable ID on first sight.
func (s *ArtifactService) toArtifact(ctx context.Context, registryID string, img Image) (*registrypb.Artifact, error) {
	ref := ArtifactRef{RegistryID: registryID, Repository: img.Repository, Digest: img.Digest}
	id, err := s.idOf(ctx, ref)
	if err != nil {
		return nil, err
	}
	return toArtifactProto(id, img), nil
}

func (s *ArtifactService) idOf(ctx context.Context, ref ArtifactRef) (string, error) {
	id, err := s.index.Lookup(ctx, ref)
	if err == nil || !errs.IsKind(err, errs.KindNotFound) {
		return id, err
	}
	id, err = s.ids.New(KindArtifact)
	if err != nil {
		return "", fmt.Errorf("new artifact id: %w", err)
	}
	err = s.index.Assign(ctx, id, ref)
	if errs.IsKind(err, errs.KindAlreadyExists) {
		return s.index.Lookup(ctx, ref) // a concurrent List assigned it first
	}
	return id, err
}

func toArtifactProto(id string, img Image) *registrypb.Artifact {
	typ := registrypb.Artifact_MANIFEST
	if img.List {
		typ = registrypb.Artifact_MANIFEST_LIST
	}
	tags := slices.Clone(img.Tags)
	slices.Sort(tags)
	pushed := timestamppb.New(img.PushedAt)
	return &registrypb.Artifact{
		Id:        id,
		Name:      img.Repository,
		MediaType: img.MediaType,
		Digest:    img.Digest,
		Size:      img.Size,
		Status:    registrypb.Artifact_ACTIVE,
		Type:      typ,
		CreatedAt: pushed,
		UpdatedAt: pushed,
		Tags:      tags,
	}
}

func encodeOffset(n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(n)))
}

func decodeOffset(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err == nil {
		if n, perr := strconv.Atoi(string(b)); perr == nil && n >= 0 {
			return n, nil
		}
	}
	return 0, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
}
