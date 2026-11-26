// Package serviceerror maps pkg/errs domain errors to gRPC statuses carrying
// nebius.common.v1.ServiceError details, the wire format real Nebius uses.
//
// This is the single mapping point between domain and transport. Only api
// layers, interceptors and the operations API may import it.
package serviceerror

import (
	"context"
	"errors"

	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// Service is reported in ServiceError.service for errors raised by the mocker.
const Service = "nebius-mocker"

type mapping struct {
	code   codes.Code
	retry  commonpb.ServiceError_RetryType
	detail func(ctx context.Context, e *errs.Error) *commonpb.ServiceError
}

var mappings = map[errs.Kind]mapping{
	errs.KindNotFound: {codes.NotFound, commonpb.ServiceError_NOTHING, func(_ context.Context, e *errs.Error) *commonpb.ServiceError {
		return &commonpb.ServiceError{Details: &commonpb.ServiceError_ResourceNotFound{
			ResourceNotFound: &commonpb.ResourceNotFound{ResourceId: e.ResourceID},
		}}
	}},
	errs.KindAlreadyExists: {codes.AlreadyExists, commonpb.ServiceError_NOTHING, func(_ context.Context, e *errs.Error) *commonpb.ServiceError {
		return &commonpb.ServiceError{Details: &commonpb.ServiceError_ResourceAlreadyExists{
			ResourceAlreadyExists: &commonpb.ResourceAlreadyExists{ResourceId: e.ResourceID},
		}}
	}},
	errs.KindBadRequest: {codes.InvalidArgument, commonpb.ServiceError_NOTHING, func(_ context.Context, e *errs.Error) *commonpb.ServiceError {
		br := &commonpb.BadRequest{}
		for _, v := range e.Violations {
			br.Violations = append(br.Violations, &commonpb.BadRequest_Violation{Field: v.Field, Message: v.Message})
		}
		return &commonpb.ServiceError{Details: &commonpb.ServiceError_BadRequest{BadRequest: br}}
	}},
	errs.KindBadState: {codes.FailedPrecondition, commonpb.ServiceError_NOTHING, func(_ context.Context, e *errs.Error) *commonpb.ServiceError {
		return &commonpb.ServiceError{Details: &commonpb.ServiceError_BadResourceState{
			BadResourceState: &commonpb.BadResourceState{ResourceId: e.ResourceID, Message: e.Message},
		}}
	}},
	errs.KindConflict: {codes.Aborted, commonpb.ServiceError_CALL, func(_ context.Context, e *errs.Error) *commonpb.ServiceError {
		return &commonpb.ServiceError{Details: &commonpb.ServiceError_ResourceConflict{
			ResourceConflict: &commonpb.ResourceConflict{ResourceId: e.ResourceID, Message: e.Message},
		}}
	}},
	errs.KindOperationConflict: {codes.FailedPrecondition, commonpb.ServiceError_CALL, func(_ context.Context, e *errs.Error) *commonpb.ServiceError {
		return &commonpb.ServiceError{Details: &commonpb.ServiceError_OperationConflict{
			OperationConflict: &commonpb.OperationConflict{ResourceId: e.ResourceID, ConflictingOperationId: e.OperationID},
		}}
	}},
	errs.KindPermissionDenied: {codes.PermissionDenied, commonpb.ServiceError_NOTHING, func(_ context.Context, e *errs.Error) *commonpb.ServiceError {
		return &commonpb.ServiceError{Details: &commonpb.ServiceError_PermissionDenied{
			PermissionDenied: &commonpb.PermissionDenied{ResourceId: e.ResourceID},
		}}
	}},
	errs.KindUnauthenticated: {codes.Unauthenticated, commonpb.ServiceError_NOTHING, nil},
	errs.KindQuotaExceeded:   {codes.ResourceExhausted, commonpb.ServiceError_NOTHING, quotaFailureDetail},
	errs.KindInternal:        {codes.Internal, commonpb.ServiceError_CALL, internalDetail},
}

func quotaFailureDetail(_ context.Context, e *errs.Error) *commonpb.ServiceError {
	qf := &commonpb.QuotaFailure{}
	for _, v := range e.QuotaViolations {
		qf.Violations = append(qf.Violations, &commonpb.QuotaFailure_Violation{
			Quota: v.Quota, Message: v.Message, Limit: v.Limit, Requested: v.Requested, Region: v.Region,
		})
	}
	return &commonpb.ServiceError{Details: &commonpb.ServiceError_QuotaFailure{QuotaFailure: qf}}
}

func internalDetail(ctx context.Context, _ *errs.Error) *commonpb.ServiceError {
	return &commonpb.ServiceError{Details: &commonpb.ServiceError_InternalError{
		InternalError: &commonpb.InternalError{RequestId: reqctx.RequestID(ctx)},
	}}
}

// FromDomain converts err into a gRPC status error. Errors that already carry a
// gRPC status pass through; unknown errors become INTERNAL without leaking
// their text to the client.
func FromDomain(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	return status.ErrorProto(StatusProto(ctx, err))
}

// StatusProto converts err into a google.rpc.Status, as stored in
// Operation.status. A nil err yields an OK status.
func StatusProto(ctx context.Context, err error) *rpcstatus.Status {
	if err == nil {
		return &rpcstatus.Status{Code: int32(codes.OK)}
	}
	if s, ok := status.FromError(err); ok && s.Code() != codes.Unknown {
		return s.Proto()
	}
	if errors.Is(err, context.Canceled) {
		return status.New(codes.Canceled, "request canceled").Proto()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.New(codes.DeadlineExceeded, "deadline exceeded").Proto()
	}
	e, ok := errors.AsType[*errs.Error](err)
	if !ok {
		e = errs.Internal("internal error")
	}
	m, ok := mappings[e.Kind]
	if !ok {
		m = mappings[errs.KindInternal]
	}
	msg := e.Error()
	if e.Kind == errs.KindInternal {
		msg = "internal error" // never leak internal details
	}
	st := &rpcstatus.Status{Code: int32(m.code), Message: msg} //nolint:gosec // gRPC codes fit in int32
	if m.detail == nil {
		return st
	}
	se := m.detail(ctx, e)
	se.Service = Service
	se.Code = e.Kind.String()
	se.RetryType = m.retry
	if a, err := anypb.New(se); err == nil {
		st.Details = append(st.Details, a)
	}
	return st
}
