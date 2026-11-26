package api

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	billingv1alpha1pb "github.com/nebius/gosdk/proto/nebius/billing/v1alpha1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
)

// oneTimeExportUnimplemented says why OneTimeExportService is not served
// (docs/services/billing.md, "Not yet").
const oneTimeExportUnimplemented = "nebius.billing.v1alpha1.OneTimeExportService is not implemented in NebiusMocker yet: " +
	"the mock has no billing contracts and meters no usage, so there is no FOCUS report to export"

// OneTimeExportHandler answers every OneTimeExportService RPC with
// UNIMPLEMENTED, naming the reason.
type OneTimeExportHandler struct {
	billingv1alpha1pb.UnimplementedOneTimeExportServiceServer
}

// NewOneTimeExportHandler returns the handler.
func NewOneTimeExportHandler() *OneTimeExportHandler { return &OneTimeExportHandler{} }

// Create implements OneTimeExportService.Create.
func (*OneTimeExportHandler) Create(context.Context, *billingv1alpha1pb.CreateOneTimeExportRequest) (*commonpb.Operation, error) {
	return nil, status.Error(codes.Unimplemented, oneTimeExportUnimplemented)
}

// Get implements OneTimeExportService.Get.
func (*OneTimeExportHandler) Get(context.Context, *billingv1alpha1pb.GetOneTimeExportRequest) (*billingv1alpha1pb.OneTimeExport, error) {
	return nil, status.Error(codes.Unimplemented, oneTimeExportUnimplemented)
}

// List implements OneTimeExportService.List.
func (*OneTimeExportHandler) List(context.Context, *billingv1alpha1pb.ListOneTimeExportsRequest) (*billingv1alpha1pb.ListOneTimeExportsResponse, error) {
	return nil, status.Error(codes.Unimplemented, oneTimeExportUnimplemented)
}
