package domain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Action is what a docker client wants to do with a repository.
type Action int

// Docker data-plane actions.
const (
	ActionUnspecified Action = iota
	// ActionPull reads manifests, blobs and tags.
	ActionPull
	// ActionPush uploads blobs and manifests.
	ActionPush
	// ActionDelete deletes manifests or blobs.
	ActionDelete
)

// String implements fmt.Stringer.
func (a Action) String() string {
	switch a {
	case ActionPull:
		return "pull"
	case ActionPush:
		return "push"
	case ActionDelete:
		return "delete"
	case ActionUnspecified:
	}
	return "unspecified"
}

// actionPermissions maps docker actions onto the standard verbs the role
// catalog already grants, so viewers can pull and editors can push without
// new verbs: pull = registry.artifacts.get, push = registry.artifacts.create,
// delete = registry.artifacts.delete.
var actionPermissions = map[Action]authz.Permission{
	ActionPull:   authz.Perm("registry", "artifacts", authz.VerbGet),
	ActionPush:   authz.Perm("registry", "artifacts", authz.VerbCreate),
	ActionDelete: authz.Perm("registry", "artifacts", authz.VerbDelete),
}

// PermissionFor returns the IAM permission a docker action needs.
func PermissionFor(a Action) (authz.Permission, bool) {
	p, ok := actionPermissions[a]
	return p, ok
}

// TokenVerifier resolves a bearer token to a principal ID.
type TokenVerifier interface {
	Principal(token string) (string, bool)
}

// AccessService decides docker data-plane access with Nebius IAM tokens and
// the same authorization engine and mode as the gRPC API.
type AccessService struct {
	tokens     TokenVerifier
	authorizer Authorizer
	registries RegistryRepository
	mode       authz.Mode
	log        *slog.Logger
}

// NewAccessService wires an AccessService. All dependencies are required.
func NewAccessService(tokens TokenVerifier, authorizer Authorizer, registries RegistryRepository, mode authz.Mode, log *slog.Logger) (*AccessService, error) {
	if tokens == nil || authorizer == nil || registries == nil || log == nil {
		return nil, errors.New("registry: access service dependencies must not be nil")
	}
	return &AccessService{tokens: tokens, authorizer: authorizer, registries: registries, mode: mode, log: log}, nil
}

// Authenticate resolves a docker credential (the Nebius IAM token sent as the
// password, or as a bearer token) to a principal: errs.Unauthenticated if
// missing or unknown.
func (s *AccessService) Authenticate(token string) (string, error) {
	if token == "" {
		return "", errs.Unauthenticated("registry credentials required: docker login with username iam and an IAM token as password")
	}
	principal, ok := s.tokens.Principal(token)
	if !ok {
		return "", errs.Unauthenticated("invalid IAM token")
	}
	return principal, nil
}

// AccessRequest is one docker request to authorize.
type AccessRequest struct {
	Token string
	// Namespace is the first segment of the image name (registry ID suffix).
	Namespace string
	Action    Action
}

// Authorize authenticates the caller and checks that the namespace belongs to
// an ACTIVE registry on which the caller holds the action's permission. It
// returns errs.Unauthenticated, errs.NotFound, errs.BadState or
// errs.PermissionDenied (only under authz enforce).
func (s *AccessService) Authorize(ctx context.Context, req AccessRequest) error {
	principal, err := s.Authenticate(req.Token)
	if err != nil {
		return err
	}
	perm, ok := PermissionFor(req.Action)
	if !ok {
		return errs.BadRequest(errs.Violation{Message: "unsupported registry action"})
	}
	registryID := RegistryIDOf(req.Namespace)
	if req.Namespace == "" {
		return errs.NotFound(registryID)
	}
	r, err := s.registries.Get(ctx, registryID)
	if err != nil {
		return err
	}
	if r.GetStatus().GetState() != registrypb.RegistryStatus_ACTIVE {
		return errs.BadState(registryID, "registry is not ACTIVE")
	}
	if s.mode == authz.ModeOff {
		return nil
	}
	d, err := s.authorizer.Check(ctx, principal, perm, r.GetMetadata().GetId())
	if err != nil {
		return fmt.Errorf("authorize %s on %s: %w", perm, registryID, err)
	}
	if d.Allowed {
		return nil
	}
	s.log.WarnContext(ctx, "registry access denied",
		"principal", principal, "action", req.Action.String(), "registry", registryID,
		"enforced", s.mode == authz.ModeEnforce, "reason", d.Reason)
	if s.mode != authz.ModeEnforce {
		return nil
	}
	return &errs.Error{Kind: errs.KindPermissionDenied, ResourceID: registryID, Message: "permission " + string(perm) + " denied"}
}
