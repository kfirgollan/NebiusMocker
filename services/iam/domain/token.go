package domain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// OAuth 2.0 token exchange (RFC 8693) identifiers used by Nebius IAM.
const (
	GrantTypeTokenExchange     = "urn:ietf:params:oauth:grant-type:token-exchange"       //nolint:gosec // G101: OAuth URN identifiers, not credentials
	TokenTypeAccessToken       = "urn:ietf:params:oauth:token-type:access_token"         //nolint:gosec // G101: OAuth URN identifiers, not credentials
	TokenTypeJWT               = "urn:ietf:params:oauth:token-type:jwt"                  //nolint:gosec // G101: OAuth URN identifiers, not credentials
	TokenTypeSubjectIdentifier = "urn:nebius:params:oauth:token-type:subject_identifier" //nolint:gosec // G101: OAuth URN identifiers, not credentials
	TokenTypeBearer            = "Bearer"
)

// PermImpersonate is the permission an actor needs on a service account to
// exchange its own access token for one of that service account.
var PermImpersonate = authz.Perm("iam", "serviceAccounts", "impersonate")

// OAuthErrorCode is an RFC 6749 §5.2 / RFC 8693 §2.2.2 error code.
type OAuthErrorCode string

// OAuth error codes the exchange reports.
const (
	OAuthInvalidRequest       OAuthErrorCode = "invalid_request"
	OAuthInvalidGrant         OAuthErrorCode = "invalid_grant"
	OAuthUnauthorizedClient   OAuthErrorCode = "unauthorized_client"
	OAuthUnsupportedGrantType OAuthErrorCode = "unsupported_grant_type"
	OAuthInvalidScope         OAuthErrorCode = "invalid_scope"
	OAuthInvalidTarget        OAuthErrorCode = "invalid_target"
)

// ExchangeError is a refused exchange: the OAuth error code for the HTTP
// endpoint, wrapping the domain error the gRPC API maps.
type ExchangeError struct {
	Code OAuthErrorCode
	Err  *errs.Error
}

func (e *ExchangeError) Error() string { return string(e.Code) + ": " + e.Err.Error() }

// Unwrap returns the domain error.
func (e *ExchangeError) Unwrap() error { return e.Err }

// Description is the human-readable error_description.
func (e *ExchangeError) Description() string {
	if len(e.Err.Violations) > 0 {
		v := e.Err.Violations[0]
		return v.Field + ": " + v.Message
	}
	return e.Err.Message
}

func badField(code OAuthErrorCode, field, msg string) *ExchangeError {
	return &ExchangeError{Code: code, Err: errs.BadRequest(errs.Violation{Field: field, Message: msg})}
}

func badGrant(format string, args ...any) *ExchangeError {
	return &ExchangeError{Code: OAuthInvalidGrant, Err: errs.Unauthenticated(fmt.Sprintf(format, args...))}
}

// ExchangeRequest is a token exchange request (iam.v1.ExchangeTokenRequest,
// or the same fields of the HTTP form).
type ExchangeRequest struct {
	GrantType          string
	RequestedTokenType string
	SubjectToken       string
	SubjectTokenType   string
	Scopes             []string
	Audience           string
	ActorToken         string
	ActorTokenType     string
	Resource           []string
}

// ExchangeResult is an issued access token.
type ExchangeResult struct {
	AccessToken     string
	IssuedTokenType string
	TokenType       string
	ExpiresIn       time.Duration
	// Principal is the service account the token authenticates as.
	Principal string
}

// PublicKeyGetter reads auth public keys as clients see them (expiry applied
// to status.state); satisfied by *AuthPublicKeyService.
type PublicKeyGetter interface {
	Get(ctx context.Context, id string) (*iampb.AuthPublicKey, error)
}

// TokenMinter issues access tokens (satisfied by *auth.Issuer).
type TokenMinter interface {
	Mint(g auth.Grant) (auth.Token, error)
}

// TokenAuthenticator resolves an access token to its principal (satisfied by
// *auth.Authenticator); it authenticates impersonation actors.
type TokenAuthenticator interface {
	Authenticate(token string) (string, error)
}

// PermissionChecker decides permissions (satisfied by *authz.Engine).
type PermissionChecker interface {
	Check(ctx context.Context, principalID string, perm authz.Permission, target string) (authz.Decision, error)
}

// TokenExchangeService implements nebius.iam.v1.TokenExchangeService and the
// HTTP OAuth endpoint: it exchanges a service account's self-signed RS256 JWT,
// an actor's access token (impersonation) or an external issuer's JWT
// (federated credentials) for a mocker access token.
type TokenExchangeService struct {
	keys     PublicKeyGetter
	sas      ServiceAccountRepository
	projects ContainerRepository
	minter   TokenMinter
	actors   TokenAuthenticator
	perms    PermissionChecker
	fedCreds FederatedCredentialsLister
	oidc     OIDCKeySource
	mode     authz.Mode
	clk      clock.Clock
	maxTTL   time.Duration
	log      *slog.Logger
}

// TokenExchangeDeps are the dependencies of a TokenExchangeService. All are
// required; Mode is the authorization mode impersonation checks follow.
type TokenExchangeDeps struct {
	Keys            PublicKeyGetter
	ServiceAccounts ServiceAccountRepository
	Projects        ContainerRepository
	Minter          TokenMinter
	Actors          TokenAuthenticator
	Permissions     PermissionChecker
	// FederatedCredentials and OIDC serve the federated exchange: the
	// credentials that trust an external issuer's subject, and the issuer's
	// keys when a credential has no jwk_set_json.
	FederatedCredentials FederatedCredentialsLister
	OIDC                 OIDCKeySource
	Mode                 authz.Mode
	Clock                clock.Clock
	// MaxAssertionTTL bounds a JWT assertion's exp (NEBIUS_MOCKER_IAM_JWT_MAX_TTL).
	MaxAssertionTTL time.Duration
	Log             *slog.Logger
}

// NewTokenExchangeService wires a TokenExchangeService.
func NewTokenExchangeService(d TokenExchangeDeps) (*TokenExchangeService, error) {
	if d.Keys == nil || d.ServiceAccounts == nil || d.Projects == nil || d.Minter == nil || d.Actors == nil ||
		d.Permissions == nil || d.FederatedCredentials == nil || d.OIDC == nil || d.Clock == nil || d.Log == nil {
		return nil, errors.New("iam: token exchange dependencies must not be nil")
	}
	if d.MaxAssertionTTL <= 0 {
		return nil, errors.New("iam: token exchange max assertion TTL must be positive")
	}
	return &TokenExchangeService{
		keys: d.Keys, sas: d.ServiceAccounts, projects: d.Projects, minter: d.Minter, actors: d.Actors,
		perms: d.Permissions, fedCreds: d.FederatedCredentials, oidc: d.OIDC, mode: d.Mode, clk: d.Clock, maxTTL: d.MaxAssertionTTL, log: d.Log,
	}, nil
}

// Exchange validates req and issues an access token for its subject. Every
// refusal is an *ExchangeError.
func (s *TokenExchangeService) Exchange(ctx context.Context, req ExchangeRequest) (ExchangeResult, error) {
	if err := checkExchangeRequest(req); err != nil {
		return ExchangeResult{}, err
	}
	var grant auth.Grant
	var err error
	switch req.SubjectTokenType {
	case TokenTypeJWT:
		if req.ActorToken != "" || req.ActorTokenType != "" {
			return ExchangeResult{}, badField(OAuthInvalidRequest, "actor_token",
				"impersonation takes subject_token_type "+TokenTypeSubjectIdentifier+" with the service account ID as subject_token")
		}
		grant, err = s.fromAssertion(ctx, req.SubjectToken)
	case TokenTypeSubjectIdentifier:
		if req.ActorTokenType == TokenTypeJWT {
			grant, err = s.fromFederated(ctx, req)
		} else {
			grant, err = s.fromImpersonation(ctx, req)
		}
	default:
		return ExchangeResult{}, badField(OAuthInvalidRequest, "subject_token_type", fmt.Sprintf(
			"%q is not supported: use %s (a service account JWT signed with an auth public key) or %s "+
				"(impersonation with an actor access token, or federated credentials with an external JWT as actor_token)",
			req.SubjectTokenType, TokenTypeJWT, TokenTypeSubjectIdentifier))
	}
	if err != nil {
		return ExchangeResult{}, err
	}
	tok, err := s.minter.Mint(grant)
	if err != nil {
		return ExchangeResult{}, fmt.Errorf("issue access token: %w", err)
	}
	return ExchangeResult{
		AccessToken: tok.AccessToken, IssuedTokenType: TokenTypeAccessToken, TokenType: TokenTypeBearer,
		ExpiresIn: tok.ExpiresIn, Principal: grant.Principal,
	}, nil
}

// checkExchangeRequest refuses what the mocker does not implement by name,
// rather than ignoring it.
func checkExchangeRequest(req ExchangeRequest) error {
	if req.GrantType != GrantTypeTokenExchange {
		return &ExchangeError{Code: OAuthUnsupportedGrantType, Err: errs.BadRequest(errs.Violation{
			Field: "grant_type", Message: fmt.Sprintf("%q is not supported, only %s", req.GrantType, GrantTypeTokenExchange),
		})}
	}
	if req.RequestedTokenType != "" && req.RequestedTokenType != TokenTypeAccessToken {
		return badField(OAuthInvalidRequest, "requested_token_type", "only "+TokenTypeAccessToken+" can be issued")
	}
	if req.SubjectToken == "" {
		return badField(OAuthInvalidRequest, "subject_token", "required")
	}
	if req.SubjectTokenType == "" {
		return badField(OAuthInvalidRequest, "subject_token_type", "required")
	}
	if len(req.Scopes) > 0 {
		return badField(OAuthInvalidScope, "scopes", "scoped tokens are not supported by NebiusMocker; omit scopes")
	}
	if req.Audience != "" {
		return badField(OAuthInvalidTarget, "audience", "audience-restricted tokens are not supported by NebiusMocker; omit audience")
	}
	if len(req.Resource) > 0 {
		return badField(OAuthInvalidTarget, "resource", "resource-restricted tokens are not supported by NebiusMocker; omit resource")
	}
	return nil
}

// fromAssertion verifies a service account's self-signed JWT: kid names an
// ACTIVE auth public key of the service account iss = sub, and the RS256
// signature verifies against it.
func (s *TokenExchangeService) fromAssertion(ctx context.Context, token string) (auth.Grant, error) {
	a, err := auth.ParseAssertion(token)
	if err != nil {
		return auth.Grant{}, badGrant("subject_token: %v", err)
	}
	if !strings.HasPrefix(a.KeyID, KindAuthPublicKey+"-") {
		return auth.Grant{}, badGrant("subject_token: kid %q is not an auth public key ID (%s-…); to exchange a JWT "+
			"of an external issuer, use federated credentials: subject_token_type %s with the service account ID as "+
			"subject_token, and the JWT as actor_token with actor_token_type %s",
			a.KeyID, KindAuthPublicKey, TokenTypeSubjectIdentifier, TokenTypeJWT)
	}
	key, err := s.keys.Get(ctx, a.KeyID)
	if errs.IsKind(err, errs.KindNotFound) {
		return auth.Grant{}, badGrant("subject_token: auth public key %s does not exist", a.KeyID)
	}
	if err != nil {
		return auth.Grant{}, fmt.Errorf("auth public key %s: %w", a.KeyID, err)
	}
	saID := key.GetSpec().GetAccount().GetServiceAccount().GetId()
	if saID == "" || a.Issuer != saID || a.Subject != saID {
		return auth.Grant{}, badGrant("subject_token: iss and sub must be %s, the service account of key %s", saID, a.KeyID)
	}
	if st := key.GetStatus().GetState(); st != iampb.AuthPublicKeyStatus_ACTIVE {
		return auth.Grant{}, badGrant("subject_token: auth public key %s is %s", a.KeyID, st)
	}
	pub, err := auth.ParsePublicKey(key.GetSpec().GetData())
	if err != nil {
		return auth.Grant{}, fmt.Errorf("auth public key %s: %w", a.KeyID, err)
	}
	if err := auth.VerifyAssertion(a, pub.Key, auth.AssertionPolicy{Now: s.clk.Now(), MaxTTL: s.maxTTL}); err != nil {
		return auth.Grant{}, badGrant("subject_token: %v", err)
	}
	tenant, err := s.activeAccountTenant(ctx, saID)
	if err != nil {
		return auth.Grant{}, err
	}
	return auth.Grant{Principal: saID, Tenant: tenant, KeyID: a.KeyID}, nil
}

// fromImpersonation issues a token of the service account subject_token to
// the actor of actor_token, who needs PermImpersonate on it.
func (s *TokenExchangeService) fromImpersonation(ctx context.Context, req ExchangeRequest) (auth.Grant, error) {
	if req.ActorToken == "" {
		return auth.Grant{}, badField(OAuthInvalidRequest, "actor_token", "required with subject_token_type "+TokenTypeSubjectIdentifier)
	}
	if req.ActorTokenType != TokenTypeAccessToken {
		return auth.Grant{}, badField(OAuthInvalidRequest, "actor_token_type",
			"must be "+TokenTypeAccessToken+" (impersonation) or "+TokenTypeJWT+" (federated credentials)")
	}
	saID := req.SubjectToken
	if !strings.HasPrefix(saID, KindServiceAccount+"-") {
		return auth.Grant{}, badField(OAuthInvalidRequest, "subject_token", "must be a service account ID ("+KindServiceAccount+"-…)")
	}
	actor, err := s.actors.Authenticate(req.ActorToken)
	if err != nil {
		return auth.Grant{}, badGrant("actor_token: %v", err)
	}
	tenant, err := s.activeAccountTenant(ctx, saID)
	if err != nil {
		return auth.Grant{}, err
	}
	if err := s.checkImpersonation(ctx, actor, saID); err != nil {
		return auth.Grant{}, err
	}
	return auth.Grant{Principal: saID, Tenant: tenant, Actor: actor}, nil
}

func (s *TokenExchangeService) checkImpersonation(ctx context.Context, actor, saID string) error {
	if s.mode == authz.ModeOff {
		return nil
	}
	d, err := s.perms.Check(ctx, actor, PermImpersonate, saID)
	if err != nil {
		return fmt.Errorf("impersonation check: %w", err)
	}
	if d.Allowed {
		return nil
	}
	s.log.WarnContext(ctx, "authz denied", "method", "TokenExchange impersonation", "principal", actor,
		"enforced", s.mode == authz.ModeEnforce, "reason", d.Reason)
	if s.mode != authz.ModeEnforce {
		return nil
	}
	return &ExchangeError{Code: OAuthUnauthorizedClient, Err: &errs.Error{
		Kind: errs.KindPermissionDenied, ResourceID: saID,
		Message: fmt.Sprintf("%s may not impersonate %s: permission %s denied", actor, saID, PermImpersonate),
	}}
}

// activeAccountTenant checks that the service account exists and is active,
// and returns its tenant.
func (s *TokenExchangeService) activeAccountTenant(ctx context.Context, saID string) (string, error) {
	sa, err := s.sas.Get(ctx, saID)
	if errs.IsKind(err, errs.KindNotFound) {
		return "", badGrant("service account %s does not exist", saID)
	}
	if err != nil {
		return "", fmt.Errorf("service account %s: %w", saID, err)
	}
	if !sa.GetStatus().GetActive() {
		return "", badGrant("service account %s is not active", saID)
	}
	project, err := s.projects.Get(ctx, sa.GetMetadata().GetParentId())
	if err != nil {
		return "", fmt.Errorf("project of service account %s: %w", saID, err)
	}
	return project.GetMetadata().GetParentId(), nil
}

// fromFederated exchanges a JWT of an external OIDC issuer (actor_token) for
// a token of the service account subject_token, through federated
// credentials of that service account whose issuer_url is the token's iss and
// whose federated_subject_id is its sub. The signature is checked with the
// credential's jwk_set_json or, without one, the issuer's published JWKS.
//
// The aud claim is not checked: Nebius documents no audience for this
// exchange, so a token minted for any audience is accepted once its issuer,
// subject, signature and lifetime check out.
func (s *TokenExchangeService) fromFederated(ctx context.Context, req ExchangeRequest) (auth.Grant, error) {
	saID := req.SubjectToken
	if !strings.HasPrefix(saID, KindServiceAccount+"-") {
		return auth.Grant{}, badField(OAuthInvalidRequest, "subject_token", "must be a service account ID ("+KindServiceAccount+"-…)")
	}
	if req.ActorToken == "" {
		return auth.Grant{}, badField(OAuthInvalidRequest, "actor_token", "required: the JWT of the external issuer")
	}
	// Token files are often written with a trailing newline.
	jwtToken, err := auth.ParseExternalJWT(strings.TrimSpace(req.ActorToken))
	if err != nil {
		return auth.Grant{}, badField(OAuthInvalidRequest, "actor_token", err.Error())
	}
	tenant, err := s.activeAccountTenant(ctx, saID)
	if err != nil {
		return auth.Grant{}, err
	}
	candidates, err := s.federatedCandidates(ctx, saID, jwtToken)
	if err != nil {
		return auth.Grant{}, err
	}
	var lastErr error
	for _, c := range candidates {
		lastErr = s.verifyFederated(ctx, c, jwtToken)
		if lastErr == nil {
			return auth.Grant{Principal: saID, Tenant: tenant, KeyID: c.GetMetadata().GetId()}, nil
		}
		if _, refused := errors.AsType[*ExchangeError](lastErr); !refused && !errors.Is(lastErr, auth.ErrInvalidExternalJWT) {
			return auth.Grant{}, lastErr
		}
	}
	if xe, ok := errors.AsType[*ExchangeError](lastErr); ok {
		return auth.Grant{}, xe
	}
	return auth.Grant{}, badGrant("actor_token: %v", lastErr)
}

// federatedCandidates returns the federated credentials of saID that trust
// the token's issuer and subject.
func (s *TokenExchangeService) federatedCandidates(ctx context.Context, saID string, j auth.ExternalJWT) ([]*iampb.FederatedCredentials, error) {
	all, err := s.fedCreds.ListBySubject(ctx, saID)
	if err != nil {
		return nil, fmt.Errorf("federated credentials of %s: %w", saID, err)
	}
	var byIssuer, out []*iampb.FederatedCredentials
	for _, c := range all {
		if issuerMatches(c.GetSpec().GetOidcProvider().GetIssuerUrl(), j.Issuer) {
			byIssuer = append(byIssuer, c)
		}
	}
	if len(byIssuer) == 0 {
		return nil, badGrant("actor_token: no federated credentials of service account %s trust issuer %q (the token's iss)", saID, j.Issuer)
	}
	for _, c := range byIssuer {
		if c.GetSpec().GetFederatedSubjectId() == j.Subject {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, badGrant("actor_token: the federated credentials of service account %s for issuer %q do not trust subject %q "+
			"(the token's sub must equal spec.federated_subject_id)", saID, j.Issuer, j.Subject)
	}
	return out, nil
}

// verifyFederated checks the token's signature and lifetime with the keys of
// credential c: its jwk_set_json, or the issuer's JWKS (refetched once when
// the cached set lacks the token's kid).
func (s *TokenExchangeService) verifyFederated(ctx context.Context, c *iampb.FederatedCredentials, j auth.ExternalJWT) error {
	p := c.GetSpec().GetOidcProvider()
	if inline := p.GetJwkSetJson(); inline != "" {
		set, err := auth.ParseJWKSet([]byte(inline))
		if err != nil {
			return fmt.Errorf("federated credentials %s: jwk_set_json: %w", c.GetMetadata().GetId(), err)
		}
		return auth.VerifyExternalJWT(j, set, s.clk.Now())
	}
	err := s.verifyWithIssuerKeys(ctx, p.GetIssuerUrl(), j, s.oidc.KeySet)
	if errors.Is(err, auth.ErrUnknownKeyID) {
		err = s.verifyWithIssuerKeys(ctx, p.GetIssuerUrl(), j, s.oidc.RefreshKeySet)
	}
	return err
}

func (s *TokenExchangeService) verifyWithIssuerKeys(ctx context.Context, issuerURL string, j auth.ExternalJWT,
	fetch func(context.Context, string) ([]byte, error),
) error {
	doc, err := fetch(ctx, issuerURL)
	if err != nil {
		return badGrant("actor_token: cannot fetch the signing keys of issuer %q: %v", issuerURL, err)
	}
	set, err := auth.ParseJWKSet(doc)
	if err != nil {
		return badGrant("actor_token: the JWKS of issuer %q is unusable: %v", issuerURL, err)
	}
	return auth.VerifyExternalJWT(j, set, s.clk.Now())
}
