package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

const (
	invitationService = "nebius.iam.v1.InvitationService"

	// AcceptInvitationPattern is the mock-only HTTP endpoint that stands in for
	// the invitee following the link in the invitation email.
	AcceptInvitationPattern = "POST /_mocker/iam/invitations/{id}/accept"
)

// RegisterInvitationRules declares the permissions of the invitation RPCs.
func RegisterInvitationRules(r *authz.Rules) {
	r.CRUD(invitationService, "iam", "invitations")
	r.Add("/"+invitationService+"/Resend", authz.OnID(authz.Perm("iam", "invitations", authz.VerbUpdate)))
}

// InvitationHandler implements nebius.iam.v1.InvitationService and the mock's
// invitation acceptance endpoint.
type InvitationHandler struct {
	iampb.UnimplementedInvitationServiceServer
	svc *domain.InvitationService
	log *slog.Logger
}

var _ iampb.InvitationServiceServer = (*InvitationHandler)(nil)

// NewInvitationHandler returns a handler backed by svc.
func NewInvitationHandler(svc *domain.InvitationService, log *slog.Logger) *InvitationHandler {
	return &InvitationHandler{svc: svc, log: log}
}

// Create starts an invitation creation operation.
func (h *InvitationHandler) Create(ctx context.Context, req *iampb.CreateInvitationRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Create(ctx, domain.CreateInvitationCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), NoSend: req.GetNoSend(), ExpiresIn: req.GetExpiresIn(), Request: req,
	}))
}

// Get returns an invitation.
func (h *InvitationHandler) Get(ctx context.Context, req *iampb.GetInvitationRequest) (*iampb.Invitation, error) {
	inv, err := h.svc.Get(ctx, req.GetId())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return inv, nil
}

// List returns one page of a tenant's invitations.
func (h *InvitationHandler) List(ctx context.Context, req *iampb.ListInvitationsRequest) (*iampb.ListInvitationsResponse, error) {
	page, err := h.svc.List(ctx, req.GetParentId(), req.GetPageSize(), req.GetPageToken(), req.GetFilter())
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return &iampb.ListInvitationsResponse{Items: page.Items, NextPageToken: page.NextPageToken}, nil
}

// Update starts an invitation update; X-ResetMask selects fields to clear.
func (h *InvitationHandler) Update(ctx context.Context, req *iampb.UpdateInvitationRequest) (*commonpb.Operation, error) {
	resetMask, err := resetMaskOf(ctx)
	if err != nil {
		return nil, serviceerror.FromDomain(ctx, err)
	}
	return opResult(ctx)(h.svc.Update(ctx, domain.UpdateInvitationCmd{
		Metadata: req.GetMetadata(), Spec: req.GetSpec(), ResetMask: resetMask, Request: req,
	}))
}

// Delete starts an invitation deletion operation.
func (h *InvitationHandler) Delete(ctx context.Context, req *iampb.DeleteInvitationRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Delete(ctx, req.GetId(), req))
}

// Resend starts an operation that sends the invitation again.
func (h *InvitationHandler) Resend(ctx context.Context, req *iampb.ResendInvitationRequest) (*commonpb.Operation, error) {
	return opResult(ctx)(h.svc.Resend(ctx, req.GetId(), req))
}

// acceptResponse is the JSON body of a successful acceptance.
type acceptResponse struct {
	InvitationID        string `json:"invitation_id"`
	TenantUserAccountID string `json:"tenant_user_account_id"`
	State               string `json:"state"`
}

// acceptError is the JSON body of a failed acceptance.
type acceptError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// ServeHTTP handles POST /_mocker/iam/invitations/{id}/accept with an optional
// form field federation_id. The invitee has no token, so it is unauthenticated.
func (h *InvitationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		h.writeAcceptError(w, errs.BadRequest(errs.Violation{Field: "body", Message: "malformed form"}))
		return
	}
	res, err := h.svc.Accept(r.Context(), r.PathValue("id"), r.PostForm.Get("federation_id"))
	if err != nil {
		h.writeAcceptError(w, err)
		return
	}
	writeAcceptJSON(w, http.StatusOK, acceptResponse{
		InvitationID: res.InvitationID, TenantUserAccountID: res.TenantUserAccountID, State: res.State.String(),
	})
}

// acceptErrors maps the domain errors Accept can return to HTTP.
var acceptErrors = map[errs.Kind]struct {
	status int
	code   string
}{
	errs.KindBadRequest: {http.StatusBadRequest, "invalid_argument"},
	errs.KindNotFound:   {http.StatusNotFound, "not_found"},
	errs.KindBadState:   {http.StatusConflict, "failed_precondition"},
}

func (h *InvitationHandler) writeAcceptError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	if m, ok := acceptErrors[errs.KindOf(err)]; ok {
		status, code = m.status, m.code
	} else {
		h.log.Error("accept invitation", "err", err)
	}
	msg := err.Error()
	if status == http.StatusInternalServerError {
		msg = "internal error"
	}
	writeAcceptJSON(w, status, acceptError{Error: msg, Code: code})
}

func writeAcceptJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
