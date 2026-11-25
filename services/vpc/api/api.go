// Package api is the VPC gRPC view layer: it converts between protos and
// domain inputs and maps domain errors. It makes no business decisions.
package api

import (
	"context"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
)

// reply maps a domain result to an RPC result: reply[T](ctx)(svc.Get(...)).
func reply[T any](ctx context.Context) func(T, error) (T, error) {
	return func(v T, err error) (T, error) {
		if err != nil {
			var zero T
			return zero, serviceerror.FromDomain(ctx, err)
		}
		return v, nil
	}
}

// opReply maps a started operation to its proto.
func opReply(ctx context.Context) func(*operations.Operation, error) (*commonpb.Operation, error) {
	return func(op *operations.Operation, err error) (*commonpb.Operation, error) {
		if err != nil {
			return nil, serviceerror.FromDomain(ctx, err)
		}
		return opsapi.ToProto(ctx, op), nil
	}
}

// resetMask reads the X-ResetMask header of an Update call.
func resetMask(ctx context.Context) (*mask.Mask, error) {
	m, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	return m, nil
}
