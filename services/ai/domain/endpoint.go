package domain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// EndpointService implements nebius.ai.v1.EndpointService behavior and the
// stub data plane's decisions.
type EndpointService struct {
	repo      EndpointRepository
	ops       Operations
	ids       IDGenerator
	clk       clock.Clock
	hierarchy Hierarchy
	secrets   SecretSource
	dataPlane DataPlane
	catalog   Catalog
	log       *slog.Logger
}

// EndpointDeps are the dependencies of an EndpointService. All are required.
type EndpointDeps struct {
	Endpoints EndpointRepository
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
	Hierarchy Hierarchy
	Secrets   SecretSource
	DataPlane DataPlane
	Catalog   Catalog
	Log       *slog.Logger
}

// NewEndpointService wires an EndpointService.
func NewEndpointService(d EndpointDeps) (*EndpointService, error) {
	if d.Endpoints == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil || d.Hierarchy == nil ||
		d.Secrets == nil || d.DataPlane == nil || d.Log == nil {
		return nil, errors.New("ai: endpoint service dependencies must not be nil")
	}
	if len(d.Catalog) == 0 {
		return nil, errors.New("ai: the platform catalog is empty")
	}
	return &EndpointService{
		repo: d.Endpoints, ops: d.Ops, ids: d.IDs, clk: d.Clock, hierarchy: d.Hierarchy,
		secrets: d.Secrets, dataPlane: d.DataPlane, catalog: d.Catalog, log: d.Log,
	}, nil
}

// CreateEndpointCmd is the input of Create.
type CreateEndpointCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *aipb.EndpointSpec
	DryRun   bool
	// Request is echoed in Operation.request.
	Request proto.Message
}

// Get returns an endpoint without its input-only secrets (registry
// password, injected file content, inline S3 credentials).
func (s *EndpointService) Get(ctx context.Context, id string) (*aipb.Endpoint, error) {
	e, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return redacted(e), nil
}

// GetSecret returns an endpoint with its input-only secrets (view SECRET).
func (s *EndpointService) GetSecret(ctx context.Context, id string) (*aipb.Endpoint, error) {
	return s.repo.Get(ctx, id)
}

// GetByName returns an endpoint by its name within a project.
func (s *EndpointService) GetByName(ctx context.Context, parentID, name string) (*aipb.Endpoint, error) {
	e, err := s.repo.GetByName(ctx, parentID, name)
	if err != nil {
		return nil, err
	}
	return redacted(e), nil
}

// List returns one page of a project's endpoints in creation order.
func (s *EndpointService) List(ctx context.Context, parentID string, pageSize int64, pageToken string) (EndpointPage, error) {
	if pageSize < 0 || pageSize > maxPageSize {
		return EndpointPage{}, errs.BadRequest(errs.Violation{
			Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize),
		})
	}
	size := int(pageSize)
	if size == 0 {
		size = defaultPageSize
	}
	page, err := s.repo.List(ctx, parentID, size, pageToken)
	if err != nil {
		return EndpointPage{}, err
	}
	for i, e := range page.Items {
		page.Items[i] = redacted(e)
	}
	return page, nil
}

// Create validates the request, stores the endpoint as PROVISIONING and
// returns an operation that ends with the endpoint RUNNING.
func (s *EndpointService) Create(ctx context.Context, cmd CreateEndpointCmd) (*operations.Operation, error) {
	e, err := s.newEndpoint(ctx, cmd)
	if err != nil {
		return nil, err
	}
	id := e.GetMetadata().GetId()
	spec := operations.Spec{
		ResourceID:  id,
		Description: "Create endpoint",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
	}
	if cmd.DryRun {
		return s.ops.Submit(ctx, spec) // validated; nothing is stored
	}
	spec.Prepare = func(ctx context.Context) error { return s.repo.Create(ctx, e) }
	spec.Work = func(ctx context.Context) error { return s.settle(ctx, id, aipb.EndpointStatus_RUNNING) }
	return s.ops.Submit(ctx, spec)
}

// Start starts a stopped endpoint.
func (s *EndpointService) Start(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.transition(ctx, id, verbStart, "Start endpoint", request)
}

// Stop stops a running endpoint; its URLs are kept.
func (s *EndpointService) Stop(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.transition(ctx, id, verbStop, "Stop endpoint", request)
}

// Restart replaces a running endpoint's instance.
func (s *EndpointService) Restart(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.transition(ctx, id, verbRestart, "Restart endpoint", request)
}

// Delete moves the endpoint to DELETING and removes it when the operation
// completes.
func (s *EndpointService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.transition(ctx, id, verbDelete, "Delete endpoint", request)
}

// transition runs one lifecycle verb: the guard and the "during" state in
// Prepare, the settled state (or removal) in Work.
func (s *EndpointService) transition(ctx context.Context, id string, v verb, desc string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: desc,
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			e, err := s.repo.Get(ctx, id)
			if err != nil {
				return err
			}
			t, err := checkTransition(id, v, e.GetStatus().GetState())
			if err != nil {
				return err
			}
			setState(e, t.during, "")
			return s.repo.Update(ctx, e)
		},
		Work: func(ctx context.Context) error {
			if v == verbDelete {
				return s.repo.Delete(ctx, id)
			}
			return s.settle(ctx, id, lifecycle[v].settled)
		},
	})
}

// settle moves the endpoint to its settled state at the end of an operation.
func (s *EndpointService) settle(ctx context.Context, id string, st state) error {
	e, err := s.repo.Get(ctx, id)
	if err != nil {
		return err
	}
	instanceID := ""
	if st == aipb.EndpointStatus_RUNNING {
		if instanceID, err = s.ids.New(kindInstance); err != nil {
			return fmt.Errorf("new instance id: %w", err)
		}
	}
	setState(e, st, instanceID)
	return s.repo.Update(ctx, e)
}

func (s *EndpointService) newEndpoint(ctx context.Context, cmd CreateEndpointCmd) (*aipb.Endpoint, error) {
	md := cmd.Metadata
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	parentID := md.GetParentId()
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if err := s.checkRef(ctx, "metadata.parent_id", parentID, kindProject); err != nil {
		return nil, err
	}
	spec := proto.CloneOf(cmd.Spec)
	if spec == nil {
		return nil, errs.BadRequest(errs.Violation{Field: "spec", Message: "required"})
	}
	hw, err := validateSpec(spec, s.catalog)
	if err != nil {
		return nil, err
	}
	// Reads return the saved platform and preset of a selector-based endpoint.
	spec.Platform, spec.Preset = hw.platform.Name, hw.preset.Name
	if err := s.resolveRefs(ctx, spec); err != nil {
		return nil, err
	}
	id, err := s.ids.New(KindEndpoint)
	if err != nil {
		return nil, fmt.Errorf("new endpoint id: %w", err)
	}
	now := timestamppb.New(s.clk.Now())
	urls := s.urls(id, spec)
	e := &aipb.Endpoint{
		Metadata: &commonpb.ResourceMetadata{
			Id:              id,
			ParentId:        parentID,
			Name:            md.GetName(),
			Labels:          maps.Clone(md.GetLabels()),
			ResourceVersion: 1,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
		Spec:   spec,
		Status: &aipb.EndpointStatus{PrivateEndpoints: urls, PublicEndpoints: slices.Clone(urls)},
	}
	setState(e, aipb.EndpointStatus_PROVISIONING, "")
	return e, nil
}

// urls returns the stub data-plane URL of every HTTP port. An endpoint with
// one HTTP port is served at <base>/<id>; with several, each port is served
// at <base>/<id>:<container_port>.
func (s *EndpointService) urls(id string, spec *aipb.EndpointSpec) []string {
	ports := httpPorts(spec)
	base := s.dataPlane.BaseURL()
	if len(ports) == 1 {
		return []string{base + "/" + id}
	}
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, fmt.Sprintf("%s/%s:%d", base, id, p.GetContainerPort()))
	}
	return out
}

// redacted returns a copy of e without the input-only secrets.
func redacted(e *aipb.Endpoint) *aipb.Endpoint {
	out := proto.CloneOf(e)
	spec := out.GetSpec()
	if rc := spec.GetRegistryCredentials(); rc != nil {
		rc.Password = ""
	}
	for _, f := range spec.GetInjectedFiles() {
		f.Content = nil
	}
	for _, v := range spec.GetVolumes() {
		if s3 := v.GetS3Config(); s3 != nil && s3.GetCredentials() != nil {
			s3.Auth = &aipb.EndpointSpec_VolumeMount_S3Config_Credentials{
				Credentials: &aipb.EndpointSpec_VolumeMount_S3Config_S3Credentials{},
			}
		}
	}
	return out
}
