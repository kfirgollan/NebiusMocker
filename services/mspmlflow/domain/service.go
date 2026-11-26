package domain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Listing page sizes (default 100, like the other areas).
const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// experimentsTimeout bounds the backend call that fills
// status.experiments_count on reads.
const experimentsTimeout = 2 * time.Second

// Config holds the settings the domain needs from the mocker configuration.
type Config struct {
	// ProjectID is the seeded project; it always exists, even without IAM.
	ProjectID string
	// MLflowVersion is status.mlflow_version.
	MLflowVersion string
	// Sizes is the spec.size catalog (size -> "<n>vcpu-<m>gb"); SizeOrder
	// lists it smallest first, and its first entry is the default size.
	Sizes     map[string]string
	SizeOrder []string
	// Detached runs operation Work on its own goroutine. Work waits for
	// other areas' operations (the artifact bucket), which the engine's
	// scheduler goroutine cannot do for itself, so the module sets it.
	Detached bool
}

// Deps are the dependencies of a Service. All but Log are required.
type Deps struct {
	Clusters  ClusterRepository
	Internals InternalsRepository
	Backend   Backend
	Peers     Peers
	Hierarchy Hierarchy
	Quota     QuotaGate
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
	Config    Config
	Log       *slog.Logger
}

// Service implements nebius.msp.mlflow.v1alpha1.ClusterService. The
// operation engine serializes operations per cluster, so Prepare steps and
// Work never overlap on one cluster.
type Service struct {
	d   Deps
	log *slog.Logger
}

// New wires the Managed MLflow domain.
func New(d Deps) (*Service, error) {
	if d.Clusters == nil || d.Internals == nil || d.Backend == nil || d.Peers == nil || d.Hierarchy == nil || d.Quota == nil ||
		d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("mspmlflow: service dependencies must not be nil")
	}
	if len(d.Config.SizeOrder) == 0 {
		return nil, errors.New("mspmlflow: the size catalog is empty")
	}
	for _, name := range d.Config.SizeOrder {
		if _, _, ok := parseSize(d.Config.Sizes[name]); !ok {
			return nil, fmt.Errorf("mspmlflow: size %q has no valid <n>vcpu-<m>gb resources", name)
		}
	}
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{d: d, log: log}, nil
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

// notEnabled is the error for a capability whose module is disabled.
func notEnabled(field, area string) error {
	return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf(
		"Managed MLflow needs the %s service, which is not enabled in this NebiusMocker (NEBIUS_MOCKER_SERVICES)", area)})
}

// checkProject requires an existing project as a cluster's parent.
func (s *Service) checkProject(ctx context.Context, parentID string) error {
	const field = "metadata.parent_id"
	if !strings.HasPrefix(parentID, KindProject+"-") {
		return errs.BadRequest(errs.Violation{Field: field, Message: "must be a project ID"})
	}
	if parentID == s.d.Config.ProjectID {
		return nil
	}
	if _, err := s.d.Hierarchy.Parent(ctx, parentID); err != nil {
		return fmt.Errorf("cluster parent %s: %w", parentID, err)
	}
	return nil
}

// newMetadata builds the metadata of a cluster being created.
func (s *Service) newMetadata(id string, in *commonpb.ResourceMetadata) (*commonpb.ResourceMetadata, error) {
	if in.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	if in.GetResourceVersion() != 0 {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.resource_version", Message: "must not be set on create"})
	}
	now := timestamppb.New(s.d.Clock.Now())
	return &commonpb.ResourceMetadata{
		Id: id, ParentId: in.GetParentId(), Name: in.GetName(), Labels: in.GetLabels(),
		ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// bump marks a changed resource: next version, new updated_at.
func (s *Service) bump(md *commonpb.ResourceMetadata) {
	md.ResourceVersion++
	md.UpdatedAt = timestamppb.New(s.d.Clock.Now())
}

// RedactRequest returns the request without the INPUT_ONLY admin password;
// operations keep it, never the secret.
func RedactRequest(req *mlflowpb.CreateClusterRequest) *mlflowpb.CreateClusterRequest {
	if req == nil {
		return nil
	}
	out := proto.CloneOf(req)
	if out.GetSpec() != nil {
		out.Spec.AdminPassword = ""
	}
	return out
}
