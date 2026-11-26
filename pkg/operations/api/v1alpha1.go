package api

import (
	"context"

	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1alpha1pb "github.com/nebius/gosdk/proto/nebius/common/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
)

// AlphaServer implements the deprecated nebius.common.v1alpha1.OperationService,
// which v1alpha1 APIs (and their SDK clients and Terraform resources) poll. It
// reads the same operations as the v1 Server: the version is only a view.
type AlphaServer struct {
	commonv1alpha1pb.UnimplementedOperationServiceServer
	ops Reader
}

var _ commonv1alpha1pb.OperationServiceServer = (*AlphaServer)(nil) //nolint:staticcheck // SA1019: deprecated upstream, still called by v1alpha1 clients

// NewAlphaServer returns a v1alpha1 OperationService backed by ops.
func NewAlphaServer(ops Reader) *AlphaServer { return &AlphaServer{ops: ops} }

// Get returns one operation.
//
//nolint:staticcheck // SA1019: deprecated upstream, still called by v1alpha1 clients
func (s *AlphaServer) Get(ctx context.Context, req *commonv1alpha1pb.GetOperationRequest) (*commonv1alpha1pb.Operation, error) {
	op, err := s.ops.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return AlphaToProto(ctx, op), nil
}

// List returns a resource's operations, newest first, paged by offset token.
//
//nolint:staticcheck // SA1019: deprecated upstream, still called by v1alpha1 clients
func (s *AlphaServer) List(ctx context.Context, req *commonv1alpha1pb.ListOperationsRequest) (*commonv1alpha1pb.ListOperationsResponse, error) {
	ops, err := s.ops.ListByResource(ctx, req.GetResourceId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	offset, err := decodeOffset(req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	size := int(req.GetPageSize())
	if size <= 0 {
		size = defaultPageSize
	}
	resp := &commonv1alpha1pb.ListOperationsResponse{} //nolint:staticcheck // SA1019: see List
	for i := offset; i < len(ops) && len(resp.GetOperations()) < size; i++ {
		resp.Operations = append(resp.Operations, AlphaToProto(ctx, ops[i]))
	}
	if next := offset + size; next < len(ops) {
		resp.NextPageToken = encodeOffset(next)
	}
	return resp, nil
}

// AlphaToProto converts a domain operation to nebius.common.v1alpha1.Operation.
// As in v1, a running operation has a nil status: the SDK's alphaops treats a
// non-nil status as "done". The optional resource snapshot is left empty.
//
//nolint:staticcheck // SA1019: deprecated upstream, still returned by v1alpha1 APIs
func AlphaToProto(ctx context.Context, op *operations.Operation) *commonv1alpha1pb.Operation {
	out := &commonv1alpha1pb.Operation{ //nolint:staticcheck // SA1019: see AlphaToProto
		Id:          op.ID,
		Description: op.Description,
		CreatedAt:   timestamppb.New(op.CreatedAt),
		CreatedBy:   op.CreatedBy,
		ResourceId:  op.ResourceID,
	}
	if op.Request != nil {
		if a, err := anypb.New(op.Request); err == nil {
			out.Request = a
		}
	}
	if op.Done() {
		out.FinishedAt = timestamppb.New(op.FinishedAt)
		out.Status = serviceerror.StatusProto(ctx, op.Err)
	}
	return out
}

// RegisterAlphaAuthzRules declares the v1alpha1 OperationService permissions,
// identical to the v1 ones: the creator may always read its operation, anyone
// else needs read access to the operation's resource.
func RegisterAlphaAuthzRules(r *authz.Rules, ops Reader) {
	r.Add("/nebius.common.v1alpha1.OperationService/Get", authz.Rule{
		Permission: authz.Perm("common", "operations", authz.VerbGet),
		AllowIf: func(ctx context.Context, principalID string, req any) bool {
			op, err := ops.Get(ctx, alphaOperationID(req))
			return err == nil && op.CreatedBy != "" && op.CreatedBy == principalID
		},
		Target: func(ctx context.Context, req any) (string, error) {
			op, err := ops.Get(ctx, alphaOperationID(req))
			if err != nil {
				return "", err
			}
			return op.ResourceID, nil
		},
	})
	r.Add("/nebius.common.v1alpha1.OperationService/List", authz.Rule{
		Permission: authz.Perm("common", "operations", authz.VerbList),
		Target: func(_ context.Context, req any) (string, error) {
			return alphaListResourceID(req), nil
		},
	})
}

//nolint:staticcheck // SA1019: deprecated upstream, still called by v1alpha1 clients
func alphaOperationID(req any) string {
	g, _ := req.(*commonv1alpha1pb.GetOperationRequest)
	return g.GetId()
}

//nolint:staticcheck // SA1019: deprecated upstream, still called by v1alpha1 clients
func alphaListResourceID(req any) string {
	l, _ := req.(*commonv1alpha1pb.ListOperationsRequest)
	return l.GetResourceId()
}
