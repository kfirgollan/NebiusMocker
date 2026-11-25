package serviceerror

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func detail(t *testing.T, err error) *commonpb.ServiceError {
	t.Helper()
	for _, d := range status.Convert(err).Details() {
		if se, ok := d.(*commonpb.ServiceError); ok {
			return se
		}
	}
	return nil
}

func TestFromDomain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		code codes.Code
		has  func(*commonpb.ServiceError) bool
	}{
		{
			"not found (wrapped)", fmt.Errorf("get: %w", errs.NotFound("group-1")), codes.NotFound,
			func(se *commonpb.ServiceError) bool { return se.GetResourceNotFound().GetResourceId() == "group-1" },
		},
		{
			"already exists", errs.AlreadyExists("group-2", "taken"), codes.AlreadyExists,
			func(se *commonpb.ServiceError) bool {
				return se.GetResourceAlreadyExists().GetResourceId() == "group-2"
			},
		},
		{
			"bad request", errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "x"}), codes.InvalidArgument,
			func(se *commonpb.ServiceError) bool {
				return se.GetBadRequest().GetViolations()[0].GetField() == "metadata.name"
			},
		},
		{
			"op conflict is retryable", errs.OperationConflict("operation-1", "group-1"), codes.FailedPrecondition,
			func(se *commonpb.ServiceError) bool { return se.GetRetryType() == commonpb.ServiceError_CALL },
		},
		{
			"unknown error hides text", errors.New("secret db path /x"), codes.Internal,
			func(se *commonpb.ServiceError) bool { return se.GetInternalError() != nil },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := FromDomain(t.Context(), tt.err)
			if got := status.Code(err); got != tt.code {
				t.Fatalf("code = %v, want %v", got, tt.code)
			}
			se := detail(t, err)
			if se == nil || !tt.has(se) || se.GetService() != Service {
				t.Fatalf("ServiceError detail = %v", se)
			}
			if tt.code == codes.Internal && status.Convert(err).Message() != "internal error" {
				t.Fatalf("internal message leaked: %q", status.Convert(err).Message())
			}
		})
	}
}

func TestStatusProtoOK(t *testing.T) {
	t.Parallel()
	if got := StatusProto(t.Context(), nil); got.GetCode() != int32(codes.OK) {
		t.Fatalf("nil error → code %d, want OK", got.GetCode())
	}
}
