// Package api serves nebius.common.v1.OperationService and converts domain
// operations to their proto form. It is a view: no logic beyond conversion.
package api

import (
	"context"
	"encoding/base64"
	"strconv"

	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
)

const defaultPageSize = 100

// Reader is the slice of the engine this view needs.
type Reader interface {
	Get(ctx context.Context, id string) (*operations.Operation, error)
	ListByResource(ctx context.Context, resourceID string) ([]*operations.Operation, error)
}

// Server implements commonpb.OperationServiceServer.
type Server struct {
	commonpb.UnimplementedOperationServiceServer
	ops Reader
}

var _ commonpb.OperationServiceServer = (*Server)(nil)

// NewServer returns an OperationService backed by ops.
func NewServer(ops Reader) *Server { return &Server{ops: ops} }

// Get returns one operation.
func (s *Server) Get(ctx context.Context, req *commonpb.GetOperationRequest) (*commonpb.Operation, error) {
	op, err := s.ops.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return ToProto(ctx, op), nil
}

// List returns a resource's operations, newest first, paged by offset token.
func (s *Server) List(ctx context.Context, req *commonpb.ListOperationsRequest) (*commonpb.ListOperationsResponse, error) {
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
	resp := &commonpb.ListOperationsResponse{}
	for i := offset; i < len(ops) && len(resp.GetOperations()) < size; i++ {
		resp.Operations = append(resp.Operations, ToProto(ctx, ops[i]))
	}
	if next := offset + size; next < len(ops) {
		resp.NextPageToken = encodeOffset(next)
	}
	return resp, nil
}

// ToProto converts a domain operation to nebius.common.v1.Operation.
// A running operation has a nil status, as the upstream API documents.
func ToProto(ctx context.Context, op *operations.Operation) *commonpb.Operation {
	out := &commonpb.Operation{
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

func encodeOffset(n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(n)))
}

func decodeOffset(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
	}
	n, err := strconv.Atoi(string(b))
	if err != nil || n < 0 {
		return 0, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
	}
	return n, nil
}

// RegisterAuthzRules declares the OperationService permissions. The creator of
// an operation may always read it (it must keep polling after deleting the
// resource); anyone else needs read access to the operation's resource.
func RegisterAuthzRules(r *authz.Rules, ops Reader) {
	creator := func(ctx context.Context, principalID string, req any) bool {
		g, ok := req.(*commonpb.GetOperationRequest)
		if !ok {
			return false
		}
		op, err := ops.Get(ctx, g.GetId())
		return err == nil && op.CreatedBy != "" && op.CreatedBy == principalID
	}
	r.Add("/nebius.common.v1.OperationService/Get", authz.Rule{
		Permission: authz.Perm("common", "operations", authz.VerbGet),
		AllowIf:    creator,
		Target: func(ctx context.Context, req any) (string, error) {
			g, _ := req.(*commonpb.GetOperationRequest)
			op, err := ops.Get(ctx, g.GetId())
			if err != nil {
				return "", err
			}
			return op.ResourceID, nil
		},
	})
	r.Add("/nebius.common.v1.OperationService/List", authz.Rule{
		Permission: authz.Perm("common", "operations", authz.VerbList),
		Target: func(_ context.Context, req any) (string, error) {
			l, _ := req.(*commonpb.ListOperationsRequest)
			return l.GetResourceId(), nil
		},
	})
}
