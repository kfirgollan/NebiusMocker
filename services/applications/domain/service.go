package domain

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	applicationsv1alpha1pb "github.com/nebius/gosdk/proto/nebius/applications/v1alpha1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// Listing page sizes (k8s_release_service.proto leaves them to the service).
const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// Config holds the settings the domain needs from the mocker configuration.
type Config struct {
	// ProjectID is the seeded project; it always exists, even without IAM.
	ProjectID string
	// Products is the Marketplace product catalog (product slugs).
	Products []string
}

// Deps are the dependencies of a Service. All are required.
type Deps struct {
	Releases  ReleaseRepository
	Clusters  Clusters
	Hierarchy Hierarchy
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
	Config    Config
}

// Service implements the K8s release service. mu serializes every mutation,
// so the read-check-write sequences are atomic.
type Service struct {
	mu        sync.Mutex
	releases  ReleaseRepository
	clusters  Clusters
	hierarchy Hierarchy
	ops       Operations
	ids       IDGenerator
	clk       clock.Clock
	cfg       Config
}

// New wires the Applications domain.
func New(d Deps) (*Service, error) {
	if d.Releases == nil || d.Clusters == nil || d.Hierarchy == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("applications: service dependencies must not be nil")
	}
	return &Service{
		releases: d.Releases, clusters: d.Clusters, hierarchy: d.Hierarchy,
		ops: d.Ops, ids: d.IDs, clk: d.Clock, cfg: d.Config,
	}, nil
}

// CreateCmd is the input of Create.
type CreateCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *applicationsv1alpha1pb.K8SReleaseSpec
	Request  proto.Message
}

// UpdateCmd is the input of Update (a full replace of the mutable fields).
type UpdateCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *applicationsv1alpha1pb.K8SReleaseSpec
	Request  proto.Message
}

// Get returns a release without its input-only fields.
func (s *Service) Get(ctx context.Context, id string) (*applicationsv1alpha1pb.K8SRelease, error) {
	r, err := s.releases.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return redact(r), nil
}

// ListCmd is the input of List.
type ListCmd struct {
	ParentID  string
	ClusterID string
	PageSize  int64
	PageToken string
	Filter    string
}

// List returns one page of the releases installed into a cluster, in
// creation order.
func (s *Service) List(ctx context.Context, cmd ListCmd) (Page[*applicationsv1alpha1pb.K8SRelease], error) {
	var zero Page[*applicationsv1alpha1pb.K8SRelease]
	if cmd.Filter != "" {
		return zero, errs.BadRequest(errs.Violation{Field: "filter", Message: "filtering is not implemented in NebiusMocker yet"})
	}
	size, err := pageSize(cmd.PageSize)
	if err != nil {
		return zero, err
	}
	if err := s.checkParent(ctx, cmd.ParentID, "parent_id"); err != nil {
		return zero, err
	}
	if _, err := s.checkCluster(ctx, cmd.ClusterID, cmd.ParentID, "cluster_id"); err != nil {
		return zero, err
	}
	all, err := s.releases.List(ctx, cmd.ParentID)
	if err != nil {
		return zero, err
	}
	var items []*applicationsv1alpha1pb.K8SRelease
	for _, r := range all {
		if r.GetSpec().GetClusterId() == cmd.ClusterID {
			items = append(items, redact(r))
		}
	}
	return paginate(items, size, cmd.PageToken)
}

// Create installs (in inventory only) a release into a Running cluster.
func (s *Service) Create(ctx context.Context, cmd CreateCmd) (*operations.Operation, error) {
	id, err := s.ids.New(KindRelease)
	if err != nil {
		return nil, fmt.Errorf("new release id: %w", err)
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Create K8s release",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.create(ctx, id, cmd)
		},
		Work: s.settle(id),
	})
}

func (s *Service) create(ctx context.Context, id string, cmd CreateCmd) error {
	if err := s.checkParent(ctx, cmd.Metadata.GetParentId(), "metadata.parent_id"); err != nil {
		return err
	}
	if err := s.validateSpec(cmd.Spec); err != nil {
		return err
	}
	c, err := s.checkCluster(ctx, cmd.Spec.GetClusterId(), cmd.Metadata.GetParentId(), "spec.cluster_id")
	if err != nil {
		return err
	}
	if !c.Running {
		return errs.BadState(c.ID, fmt.Sprintf("cluster %s is not RUNNING", c.ID))
	}
	if cmd.Metadata.GetId() != "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	if cmd.Metadata.GetResourceVersion() != 0 {
		return errs.BadRequest(errs.Violation{Field: "metadata.resource_version", Message: "must not be set on create"})
	}
	now := timestamppb.New(s.clk.Now())
	return s.releases.Create(ctx, &applicationsv1alpha1pb.K8SRelease{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: cmd.Metadata.GetParentId(), Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels(),
			ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec:   proto.CloneOf(cmd.Spec),
		Status: &applicationsv1alpha1pb.K8SReleaseStatus{State: applicationsv1alpha1pb.K8SReleaseStatus_CREATED},
	})
}

// settle is the Work of Create and Update: the release finishes
// "installing" and is DEPLOYED. Nothing is installed anywhere.
func (s *Service) settle(id string) func(context.Context) error {
	return func(ctx context.Context) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		r, err := s.releases.Get(ctx, id)
		if errs.IsKind(err, errs.KindNotFound) {
			return nil // deleted meanwhile
		}
		if err != nil {
			return err
		}
		r.Status = &applicationsv1alpha1pb.K8SReleaseStatus{State: applicationsv1alpha1pb.K8SReleaseStatus_DEPLOYED}
		return s.releases.Update(ctx, r)
	}
}

// Update changes a release's name, labels, values and set. cluster_id,
// product_slug, namespace and application_name are immutable: an unchanged
// copy (Terraform resends them) is accepted, a different value is rejected.
func (s *Service) Update(ctx context.Context, cmd UpdateCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update K8s release",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.update(ctx, cmd)
		},
		Work: s.settle(id),
	})
}

func (s *Service) update(ctx context.Context, cmd UpdateCmd) error {
	stored, err := s.releases.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	md := stored.GetMetadata()
	if p := cmd.Metadata.GetParentId(); p != "" && p != md.GetParentId() {
		return immutable("metadata.parent_id")
	}
	if want := cmd.Metadata.GetResourceVersion(); want != 0 && want != md.GetResourceVersion() {
		return errs.BadState(md.GetId(), fmt.Sprintf("resource_version %d does not match the current version %d", want, md.GetResourceVersion()))
	}
	old, in := stored.GetSpec(), cmd.Spec
	for _, f := range []struct{ name, was, got string }{
		{"spec.cluster_id", old.GetClusterId(), in.GetClusterId()},
		{"spec.product_slug", old.GetProductSlug(), in.GetProductSlug()},
		{"spec.namespace", old.GetNamespace(), in.GetNamespace()},
		{"spec.application_name", old.GetApplicationName(), in.GetApplicationName()},
	} {
		if f.was != f.got {
			return immutable(f.name)
		}
	}
	if err := validateValues(in.GetValues(), in.GetSet()); err != nil {
		return err
	}
	md.Name, md.Labels = cmd.Metadata.GetName(), cmd.Metadata.GetLabels()
	md.ResourceVersion++
	md.UpdatedAt = timestamppb.New(s.clk.Now())
	stored.Spec.Values, stored.Spec.Set = in.GetValues(), in.GetSet()
	stored.Status = &applicationsv1alpha1pb.K8SReleaseStatus{State: applicationsv1alpha1pb.K8SReleaseStatus_INSTALLING}
	return s.releases.Update(ctx, stored)
}

// Delete removes a release.
func (s *Service) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete K8s release",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.releases.Delete(ctx, id)
		},
	})
}

// DeleteClusterReleases deletes every release of the cluster
// (peer.ClusterReleases; called when the mk8s cluster is deleted).
func (s *Service) DeleteClusterReleases(ctx context.Context, clusterID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.releases.All(ctx)
	if err != nil {
		return err
	}
	for _, r := range all {
		if r.GetSpec().GetClusterId() != clusterID {
			continue
		}
		if err := s.releases.Delete(ctx, r.GetMetadata().GetId()); err != nil && !errs.IsKind(err, errs.KindNotFound) {
			return err
		}
	}
	return nil
}

// checkParent requires an existing project as a release's parent.
func (s *Service) checkParent(ctx context.Context, parentID, field string) error {
	if parentID == "" {
		return errs.BadRequest(errs.Violation{Field: field, Message: "required"})
	}
	if !strings.HasPrefix(parentID, KindProject+"-") {
		return errs.BadRequest(errs.Violation{Field: field, Message: "must be a project ID"})
	}
	if parentID == s.cfg.ProjectID {
		return nil
	}
	if _, err := s.hierarchy.Parent(ctx, parentID); err != nil {
		return fmt.Errorf("%s %s: %w", field, parentID, err)
	}
	return nil
}

// checkCluster requires an existing cluster of the project and returns it.
func (s *Service) checkCluster(ctx context.Context, clusterID, projectID, field string) (Cluster, error) {
	if !strings.HasPrefix(clusterID, KindCluster+"-") {
		return Cluster{}, errs.BadRequest(errs.Violation{Field: field, Message: "must be an mk8s cluster ID"})
	}
	c, err := s.clusters.Cluster(ctx, clusterID)
	if err != nil {
		return Cluster{}, fmt.Errorf("%s: %w", field, err)
	}
	if c.ProjectID != projectID {
		return Cluster{}, errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("cluster %s belongs to %s, not to project %s", clusterID, c.ProjectID, projectID)})
	}
	return c, nil
}

// validateSpec checks a new release's spec; the cluster is checked
// separately.
func (s *Service) validateSpec(spec *applicationsv1alpha1pb.K8SReleaseSpec) error {
	if !slices.Contains(s.cfg.Products, spec.GetProductSlug()) {
		return errs.BadRequest(errs.Violation{Field: "spec.product_slug", Message: fmt.Sprintf("unknown product %q", spec.GetProductSlug())})
	}
	if err := checkLabel("spec.namespace", spec.GetNamespace()); err != nil {
		return err
	}
	if err := checkLabel("spec.application_name", spec.GetApplicationName()); err != nil {
		return err
	}
	return validateValues(spec.GetValues(), spec.GetSet())
}

// redact returns a copy of r without its input-only fields (values, set).
func redact(r *applicationsv1alpha1pb.K8SRelease) *applicationsv1alpha1pb.K8SRelease {
	out := proto.CloneOf(r)
	if out.GetSpec() != nil {
		out.Spec.Values, out.Spec.Set = "", nil
	}
	return out
}

func immutable(field string) error {
	return errs.BadRequest(errs.Violation{Field: field, Message: "field is immutable"})
}

func pageSize(n int64) (int, error) {
	if n < 0 || n > maxPageSize {
		return 0, errs.BadRequest(errs.Violation{Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize)})
	}
	if n == 0 {
		return defaultPageSize, nil
	}
	return int(n), nil
}

// paginate returns one page of items after the offset in pageToken.
func paginate[T any](items []T, size int, pageToken string) (Page[T], error) {
	offset := 0
	if pageToken != "" {
		b, err := base64.RawURLEncoding.DecodeString(pageToken)
		n, perr := strconv.Atoi(string(b))
		if err != nil || perr != nil || n < 0 {
			return Page[T]{}, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
		}
		offset = n
	}
	if offset >= len(items) {
		return Page[T]{Items: []T{}}, nil
	}
	end := min(offset+size, len(items))
	page := Page[T]{Items: items[offset:end]}
	if end < len(items) {
		page.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	}
	return page, nil
}
