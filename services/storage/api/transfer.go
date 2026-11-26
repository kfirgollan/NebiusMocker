package api

import (
	"cmp"
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

const (
	transferService      = "nebius.storage.v1.TransferService"
	transferAlphaService = "nebius.storage.v1alpha1.TransferService"
	transferResources    = "transfers"
)

// RegisterTransferAuthzRules declares the permission of every RPC of both
// TransferService versions. Stop and Resume count as the stop and start verbs
// (the seeded editor role grants them, viewers do not); the iteration history
// needs get on the transfer.
func RegisterTransferAuthzRules(r *authz.Rules) {
	for _, svc := range []string{transferService, transferAlphaService} {
		r.CRUD(svc, "storage", transferResources)
		m := func(method string) string { return "/" + svc + "/" + method }
		r.Add(m("Stop"), authz.OnID(authz.Perm("storage", transferResources, authz.VerbStop)))
		r.Add(m("Resume"), authz.OnID(authz.Perm("storage", transferResources, authz.VerbStart)))
		r.Add(m("GetIterationHistory"), authz.Rule{
			Permission: authz.Perm("storage", transferResources, authz.VerbGet),
			Target: func(_ context.Context, req any) (string, error) {
				if g, ok := req.(interface{ GetTransferId() string }); ok && g.GetTransferId() != "" {
					return g.GetTransferId(), nil
				}
				return "", errs.BadRequest(errs.Violation{Field: "transfer_id", Message: "required"})
			},
		})
	}
}

// TransferHandler implements nebius.storage.v1.TransferService.
type TransferHandler struct {
	storagepb.UnimplementedTransferServiceServer
	svc    *domain.TransferService
	region string
}

var _ storagepb.TransferServiceServer = (*TransferHandler)(nil)

// NewTransferHandler returns a handler backed by svc; region is the served
// Nebius region (shown for transfers created through v1alpha1 without one).
func NewTransferHandler(svc *domain.TransferService, region string) *TransferHandler {
	return &TransferHandler{svc: svc, region: region}
}

// Get returns a transfer by ID.
func (h *TransferHandler) Get(ctx context.Context, req *storagepb.GetTransferRequest) (*storagepb.Transfer, error) {
	rec, err := h.svc.Get(ctx, req.GetId(), domain.TransferAPIV1)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return h.transferView(rec), nil
}

// GetByName returns a transfer by project and name.
func (h *TransferHandler) GetByName(ctx context.Context, req *commonpb.GetByNameRequest) (*storagepb.Transfer, error) {
	rec, err := h.svc.GetByName(ctx, req.GetParentId(), req.GetName(), domain.TransferAPIV1)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return h.transferView(rec), nil
}

// List returns one page of a project's transfers (created through either API).
func (h *TransferHandler) List(ctx context.Context, req *storagepb.ListTransfersRequest) (*storagepb.ListTransfersResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), domain.TransferAPIV1, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	items := make([]*storagepb.Transfer, len(page.Items))
	for i, rec := range page.Items {
		items[i] = h.transferView(rec)
	}
	return &storagepb.ListTransfersResponse{Items: items, NextPageToken: page.NextPageToken}, nil
}

// Create starts a transfer creation operation.
func (h *TransferHandler) Create(ctx context.Context, req *storagepb.CreateTransferRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.CreateV1(ctx, domain.CreateTransferV1Cmd{Metadata: req.GetMetadata(), Spec: req.GetSpec(), Request: req}))
}

// Update starts a transfer update operation; X-ResetMask selects fields to clear.
func (h *TransferHandler) Update(ctx context.Context, req *storagepb.UpdateTransferRequest) (*commonpb.Operation, error) {
	resetMask, err := grpcheader.GetResetMaskFromIncomingContext(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, errs.BadRequest(errs.Violation{Field: grpcheader.ResetMask, Message: err.Error()}))
	}
	return opResult(ctx)(h.svc.UpdateV1(ctx, domain.UpdateTransferV1Cmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	}))
}

// Stop starts stopping an ACTIVE transfer.
func (h *TransferHandler) Stop(ctx context.Context, req *storagepb.StopTransferRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Stop(ctx, req.GetId(), domain.TransferAPIV1, req))
}

// Resume restarts a STOPPED or FAILED transfer.
func (h *TransferHandler) Resume(ctx context.Context, req *storagepb.ResumeTransferRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Resume(ctx, req.GetId(), domain.TransferAPIV1, req))
}

// Delete starts a transfer deletion operation.
func (h *TransferHandler) Delete(ctx context.Context, req *storagepb.DeleteTransferRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Delete(ctx, req.GetId(), req.GetResourceVersion(), domain.TransferAPIV1, req))
}

// GetIterationHistory returns one page of a transfer's iterations, newest first.
func (h *TransferHandler) GetIterationHistory(ctx context.Context, req *storagepb.GetIterationHistoryRequest) (*storagepb.GetIterationHistoryResponse, error) {
	page, err := h.svc.History(ctx, req.GetTransferId(), domain.TransferAPIV1, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	out := make([]*storagepb.TransferIteration, len(page.Items))
	for i := range page.Items {
		out[i] = iterationV1(&page.Items[i])
	}
	return &storagepb.GetIterationHistoryResponse{Iterations: out, NextPageToken: page.NextPageToken}, nil
}

// transferView renders a record (already stripped of secrets) as a v1
// Transfer. Records created through v1alpha1 get an equivalent v1 spec.
func (h *TransferHandler) transferView(rec *domain.TransferRecord) *storagepb.Transfer {
	out := rec.V1
	if rec.API == domain.TransferAPIV1Alpha1 {
		out = &storagepb.Transfer{Metadata: rec.Alpha.GetMetadata(), Spec: alphaSpecAsV1(rec.Alpha.GetSpec(), rec.AlphaSourceKind, h.region)}
	}
	out.Status = statusV1(rec.Status)
	return out
}

func statusV1(s domain.TransferStatus) *storagepb.TransferStatus {
	return &storagepb.TransferStatus{
		State:           stateV1(s.State),
		Error:           errorV1(s.Error),
		SuspensionState: storagepb.TransferStatus_NOT_SUSPENDED, // suspension is billing-driven; never happens here
		LastIteration:   iterationV1(s.LastIteration),
	}
}

func stateV1(s domain.TransferState) storagepb.TransferStatus_State {
	switch s {
	case domain.TransferActive:
		return storagepb.TransferStatus_ACTIVE
	case domain.TransferStopping:
		return storagepb.TransferStatus_STOPPING
	case domain.TransferStopped:
		return storagepb.TransferStatus_STOPPED
	case domain.TransferFailing:
		return storagepb.TransferStatus_FAILING
	case domain.TransferFailed:
		return storagepb.TransferStatus_FAILED
	case domain.TransferDeleting:
		return storagepb.TransferStatus_DELETING
	}
	return storagepb.TransferStatus_STATE_UNSPECIFIED
}

func errorV1(e *domain.TransferError) *storagepb.TransferError {
	if e == nil {
		return nil
	}
	out := &storagepb.TransferError{Code: e.Code, Message: e.Message}
	switch e.Origin {
	case domain.OriginSource:
		out.Origin = storagepb.TransferError_SOURCE
	case domain.OriginDestination:
		out.Origin = storagepb.TransferError_DESTINATION
	case domain.OriginUnspecified:
	}
	return out
}

func iterationV1(it *domain.Iteration) *storagepb.TransferIteration {
	if it == nil {
		return nil
	}
	out := &storagepb.TransferIteration{
		SequenceNumber: it.Seq, Error: errorV1(it.Error), StartTime: timestamppb.New(it.Start),
		ObjectsTransferredCount: it.Transferred, ObjectsDeletedCount: it.Deleted,
		ObjectsTransferredSize: it.TransferredBytes, AverageThroughputBytes: it.ThroughputBytes,
	}
	if !it.End.IsZero() {
		out.EndTime = timestamppb.New(it.End)
	}
	switch it.State {
	case domain.IterationInProgress:
		out.State = storagepb.TransferIteration_IN_PROGRESS
	case domain.IterationCompleted:
		out.State = storagepb.TransferIteration_COMPLETED
	case domain.IterationInterrupted:
		out.State = storagepb.TransferIteration_INTERRUPTED
	case domain.IterationFailed:
		out.State = storagepb.TransferIteration_FAILED
	}
	return out
}

// alphaSpecAsV1 shows a spec created through v1alpha1 as the equivalent v1
// spec ("All existing transfers remain accessible through the v1 API").
// v1alpha1 credentials are input-only (and their key ids sensitive), so the
// credentials oneofs stay unset; srcKind (from the domain, which saw the
// credentials) tells an Azure source from an S3-compatible one.
func alphaSpecAsV1(spec *storagev1alpha1pb.TransferSpec, srcKind domain.ProviderKind, servedRegion string) *storagepb.TransferSpec {
	src, dst := spec.GetSource(), spec.GetDestination()
	out := &storagepb.TransferSpec{
		InterIterationInterval: spec.GetInterIterationInterval(),
		OverwriteStrategy:      overwriteAsV1(spec.GetOverwriteStrategy()),
		TouchUnmanaged:         true, // v1alpha1 transfers overwrite whatever is in the destination
	}
	if l := src.GetLimiters(); l != nil {
		out.Limiters = &storagepb.TransferSpec_Limiters{
			BandwidthBytesPerSecond: l.GetBandwidthBytesPerSecond(), RequestsPerSecond: l.GetRequestsPerSecond(),
		}
	}
	switch {
	case spec.GetAfterOneIteration() != nil:
		out.StopCondition = &storagepb.TransferSpec_AfterOneIteration{AfterOneIteration: &storagepb.TransferSpec_StopConditionAfterOneIteration{}}
	case spec.GetAfterNEmptyIterations() != nil:
		out.StopCondition = &storagepb.TransferSpec_AfterNEmptyIterations{AfterNEmptyIterations: &storagepb.TransferSpec_StopConditionAfterNEmptyIterations{
			EmptyIterationsThreshold: spec.GetAfterNEmptyIterations().GetEmptyIterationsThreshold(),
		}}
	case spec.GetInfinite() != nil:
		out.StopCondition = &storagepb.TransferSpec_Infinite{Infinite: &storagepb.TransferSpec_StopConditionInfinite{}}
	}
	out.Source = &storagepb.TransferSource{Prefix: src.GetPrefix()}
	if srcKind == domain.ProviderAzureBlob {
		out.Source.Provider = &storagepb.TransferSource_AzureBlobStorage{AzureBlobStorage: &storagepb.TransferSource_AzureBlobStorageProvider{
			Endpoint: src.GetEndpoint(), ContainerName: src.GetBucketName(),
		}}
	} else {
		out.Source.Provider = &storagepb.TransferSource_S3Compatible{S3Compatible: &storagepb.TransferSource_S3CompatibleProvider{
			Endpoint: src.GetEndpoint(), Region: src.GetRegion(), BucketName: src.GetBucketName(),
		}}
	}
	out.Destination = &storagepb.TransferDestination{Prefix: dst.GetPrefix()}
	if dst.GetEndpoint() == "" {
		out.Destination.Provider = &storagepb.TransferDestination_Nebius{Nebius: &storagepb.TransferDestination_NebiusProvider{
			Region: cmp.Or(dst.GetRegion(), servedRegion), BucketName: dst.GetBucketName(),
		}}
	} else {
		out.Destination.Provider = &storagepb.TransferDestination_S3Compatible{S3Compatible: &storagepb.TransferDestination_S3CompatibleProvider{
			Endpoint: dst.GetEndpoint(), Region: dst.GetRegion(), BucketName: dst.GetBucketName(),
		}}
	}
	return out
}

// overwriteAsV1 maps the v1alpha1 overwrite strategy. ALWAYS has no v1
// value; it is emitted as the raw enum number 3 (the v1alpha1 number), which
// the v1 enum does not name, so clients render it as an unknown value instead
// of silently showing a different strategy.
func overwriteAsV1(o storagev1alpha1pb.TransferSpec_OverwriteStrategy) storagepb.TransferSpec_OverwriteStrategy {
	switch o {
	case storagev1alpha1pb.TransferSpec_NEVER:
		return storagepb.TransferSpec_NEVER
	case storagev1alpha1pb.TransferSpec_IF_NEWER:
		return storagepb.TransferSpec_IF_NEWER
	case storagev1alpha1pb.TransferSpec_ALWAYS:
		return storagepb.TransferSpec_OverwriteStrategy(3)
	case storagev1alpha1pb.TransferSpec_OVERWRITE_STRATEGY_UNSPECIFIED:
	}
	return storagepb.TransferSpec_OVERWRITE_STRATEGY_UNSPECIFIED
}
