package api

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// TransferAlphaHandler implements the deprecated
// nebius.storage.v1alpha1.TransferService. It only sees the transfers that
// were created through it.
type TransferAlphaHandler struct {
	storagev1alpha1pb.UnimplementedTransferServiceServer
	svc *domain.TransferService
}

var _ storagev1alpha1pb.TransferServiceServer = (*TransferAlphaHandler)(nil) //nolint:staticcheck // SA1019: the deprecated v1alpha1 service is served on purpose

// NewTransferAlphaHandler returns a handler backed by svc.
func NewTransferAlphaHandler(svc *domain.TransferService) *TransferAlphaHandler {
	return &TransferAlphaHandler{svc: svc}
}

// Get returns a transfer by ID.
func (h *TransferAlphaHandler) Get(ctx context.Context, req *storagev1alpha1pb.GetTransferRequest) (*storagev1alpha1pb.Transfer, error) {
	rec, err := h.svc.Get(ctx, req.GetId(), domain.TransferAPIV1Alpha1)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return transferAlphaView(rec), nil
}

// GetByName returns a transfer by project and name.
func (h *TransferAlphaHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*storagev1alpha1pb.Transfer, error) {
	rec, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName(), domain.TransferAPIV1Alpha1)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return transferAlphaView(rec), nil
}

// List returns one page of a project's v1alpha1 transfers.
func (h *TransferAlphaHandler) List(ctx context.Context, req *storagev1alpha1pb.ListTransfersRequest) (*storagev1alpha1pb.ListTransfersResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), domain.TransferAPIV1Alpha1, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	items := make([]*storagev1alpha1pb.Transfer, len(page.Items))
	for i, rec := range page.Items {
		items[i] = transferAlphaView(rec)
	}
	return &storagev1alpha1pb.ListTransfersResponse{Items: items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a transfer creation operation.
func (h *TransferAlphaHandler) Create(ctx context.Context, req *storagev1alpha1pb.CreateTransferRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.CreateV1Alpha1(ctx, domain.CreateTransferV1Alpha1Cmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req}))
}

// Update starts a transfer update operation; X-ResetMask selects fields to clear.
func (h *TransferAlphaHandler) Update(ctx context.Context, req *storagev1alpha1pb.UpdateTransferRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	return opResult(ctx)(h.svc.UpdateV1Alpha1(ctx, domain.UpdateTransferV1Alpha1Cmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	}))
}

// Stop starts stopping an ACTIVE transfer.
func (h *TransferAlphaHandler) Stop(ctx context.Context, req *storagev1alpha1pb.StopTransferRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Stop(ctx, req.GetId(), domain.TransferAPIV1Alpha1, req))
}

// Resume restarts a STOPPED or FAILED transfer.
func (h *TransferAlphaHandler) Resume(ctx context.Context, req *storagev1alpha1pb.ResumeTransferRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Resume(ctx, req.GetId(), domain.TransferAPIV1Alpha1, req))
}

// Delete starts a transfer deletion operation.
func (h *TransferAlphaHandler) Delete(ctx context.Context, req *storagev1alpha1pb.DeleteTransferRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Delete(ctx, req.GetId(), req.GetResourceVersion(), domain.TransferAPIV1Alpha1, req))
}

// GetIterationHistory returns one page of a transfer's iterations, newest first.
func (h *TransferAlphaHandler) GetIterationHistory(ctx context.Context, req *storagev1alpha1pb.GetIterationHistoryRequest) (*storagev1alpha1pb.GetIterationHistoryResponse, error) {
	page, err := h.svc.History(ctx, req.GetTransferId(), domain.TransferAPIV1Alpha1, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	out := make([]*storagev1alpha1pb.TransferIteration, len(page.Items))
	for i := range page.Items {
		out[i] = iterationAlpha(&page.Items[i])
	}
	return &storagev1alpha1pb.GetIterationHistoryResponse{Iterations: out, NextPageToken: page.NextPageToken}, nil
}

// transferAlphaView renders a record as a v1alpha1 Transfer (the domain only
// hands out v1alpha1-created records here, already stripped of credentials).
func transferAlphaView(rec *domain.TransferRecord) *storagev1alpha1pb.Transfer {
	out := rec.Alpha
	out.Status = statusAlpha(rec.Status)
	return out
}

func statusAlpha(s domain.TransferStatus) *storagev1alpha1pb.TransferStatus {
	return &storagev1alpha1pb.TransferStatus{
		State:           stateAlpha(s.State),
		Error:           errorAlpha(s.Error),
		SuspensionState: storagev1alpha1pb.TransferStatus_NOT_SUSPENDED,
		LastIteration:   iterationAlpha(s.LastIteration),
	}
}

// stateAlpha: v1alpha1 has no DELETING; a transfer being deleted shows STOPPING.
func stateAlpha(s domain.TransferState) storagev1alpha1pb.TransferStatus_State {
	switch s {
	case domain.TransferActive:
		return storagev1alpha1pb.TransferStatus_ACTIVE
	case domain.TransferStopping, domain.TransferDeleting:
		return storagev1alpha1pb.TransferStatus_STOPPING
	case domain.TransferStopped:
		return storagev1alpha1pb.TransferStatus_STOPPED
	case domain.TransferFailing:
		return storagev1alpha1pb.TransferStatus_FAILING
	case domain.TransferFailed:
		return storagev1alpha1pb.TransferStatus_FAILED
	}
	return storagev1alpha1pb.TransferStatus_STATE_UNSPECIFIED
}

// errorAlpha renders an error as the v1alpha1 string "<code>: <message>".
func errorAlpha(e *domain.TransferError) string {
	switch {
	case e == nil:
		return ""
	case e.Code == "":
		return e.Message
	case e.Message == "":
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func iterationAlpha(it *domain.Iteration) *storagev1alpha1pb.TransferIteration {
	if it == nil {
		return nil
	}
	out := &storagev1alpha1pb.TransferIteration{
		SequenceNumber: it.Seq, Error: errorAlpha(it.Error), StartTime: timestamppb.New(it.Start),
		ObjectsDiscoveredCount: it.Discovered, ObjectsMigratedCount: it.Transferred, ObjectsSkippedCount: it.Skipped,
		ObjectsMigratedSize: it.TransferredBytes, AverageThroughputBytes: it.ThroughputBytes,
	}
	if !it.End.IsZero() {
		out.EndTime = timestamppb.New(it.End)
	}
	switch it.State {
	case domain.IterationInProgress:
		out.State = storagev1alpha1pb.TransferIteration_IN_PROGRESS
	case domain.IterationCompleted:
		out.State = storagev1alpha1pb.TransferIteration_COMPLETED
	case domain.IterationInterrupted:
		out.State = storagev1alpha1pb.TransferIteration_STOPPED
	case domain.IterationFailed:
		out.State = storagev1alpha1pb.TransferIteration_FAILED
	}
	return out
}
