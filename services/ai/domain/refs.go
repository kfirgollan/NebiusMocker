package domain

import (
	"context"
	"fmt"
	"strings"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

// Payload keys the endpoint docs require in referenced secrets.
const (
	authTokenKey        = "AUTH_TOKEN"
	registryUsernameKey = "REGISTRY_USERNAME"
	registryPasswordKey = "REGISTRY_PASSWORD"
)

// checkRef validates a reference to a resource of another area, generically:
// the ID must be a well-formed NID of kind; when the owning service is
// enabled (its kind is registered) the resource must also exist.
func (s *EndpointService) checkRef(ctx context.Context, field, id, kind string) error {
	if id == "" {
		return nil
	}
	if !strings.HasPrefix(id, kind+"-") || len(id) == len(kind)+1 {
		return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("%q is not a %s ID", id, kind)})
	}
	if !s.hierarchy.HasKind(kind) {
		s.log.DebugContext(ctx, "reference not checked: service not enabled", "field", field, "id", id, "kind", kind)
		return nil
	}
	if _, err := s.hierarchy.Parent(ctx, id); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

// resolveRefs checks every cross-service reference of spec and fills the
// server-defaulted secret IDs (NON_EMPTY_DEFAULT) in place.
func (s *EndpointService) resolveRefs(ctx context.Context, spec *aipb.EndpointSpec) error {
	if err := s.checkRef(ctx, "spec.subnet_id", spec.GetSubnetId(), kindSubnet); err != nil {
		return err
	}
	if id := spec.GetPricingModel().GetSpotPricingPolicy().GetId(); id != "" {
		if err := s.checkRef(ctx, "spec.pricing_model.spot_pricing_policy.id", id, kindPricingPolicy); err != nil {
			return err
		}
	}
	if ref := spec.GetAuthTokenMysteryboxSecret(); ref != nil {
		if err := s.resolveSecret(ctx, "spec.auth_token_mysterybox_secret", ref, authTokenKey); err != nil {
			return err
		}
	}
	for i, e := range spec.GetEnvironmentVariables() {
		if ref := e.GetMysteryboxSecret(); ref != nil {
			if err := s.resolveSecret(ctx, fmt.Sprintf("spec.environment_variables[%d].mysterybox_secret", i), ref); err != nil {
				return err
			}
		}
	}
	for i, vol := range spec.GetVolumes() {
		if ref := vol.GetS3Config().GetMysteryboxSecret(); ref != nil {
			if err := s.resolveS3Secret(ctx, fmt.Sprintf("spec.volumes[%d].s3_config.mysterybox_secret", i), ref); err != nil {
				return err
			}
		}
	}
	if v := spec.GetRegistryCredentials().GetMysteryboxSecretVersion(); v != "" {
		return s.resolveVersion(ctx, "spec.registry_credentials.mysterybox_secret_version", v, registryUsernameKey, registryPasswordKey)
	}
	return nil
}

// secretRef is the common shape of the two MysteryBoxSecretRef messages.
type secretRef interface {
	GetSecretId() string
	GetVersionId() string
}

// resolveSecret validates ref and pins it to a concrete version (the primary
// version when only the secret is given), checking required payload keys.
func (s *EndpointService) resolveSecret(ctx context.Context, field string, ref *aipb.EndpointSpec_MysteryBoxSecretRef, keys ...string) error {
	secretID, versionID, err := s.pinSecret(ctx, field, ref, keys...)
	if err != nil {
		return err
	}
	ref.SecretId, ref.VersionId = secretID, versionID
	return nil
}

func (s *EndpointService) resolveS3Secret(ctx context.Context, field string, ref *aipb.EndpointSpec_VolumeMount_S3Config_MysteryBoxSecretRef) error {
	secretID, versionID, err := s.pinSecret(ctx, field, ref)
	if err != nil {
		return err
	}
	ref.SecretId, ref.VersionId = secretID, versionID
	return nil
}

func (s *EndpointService) resolveVersion(ctx context.Context, field, versionID string, keys ...string) error {
	ref := &aipb.EndpointSpec_MysteryBoxSecretRef{VersionId: versionID}
	_, _, err := s.pinSecret(ctx, field, ref, keys...)
	return err
}

// pinSecret resolves ref through MysteryBox when the capability is available,
// and otherwise validates the IDs generically.
func (s *EndpointService) pinSecret(ctx context.Context, field string, ref secretRef, keys ...string) (secretID, versionID string, err error) {
	secretID, versionID = ref.GetSecretId(), ref.GetVersionId()
	if secretID == "" && versionID == "" {
		return "", "", errs.BadRequest(errs.Violation{Field: field, Message: "must set secret_id or version_id"})
	}
	if err := s.checkRefShape(field+".secret_id", secretID, kindSecret); err != nil {
		return "", "", err
	}
	if err := s.checkRefShape(field+".version_id", versionID, kindSecretVersion); err != nil {
		return "", "", err
	}
	sp, ok := s.secrets.SecretPayloads()
	if !ok {
		if err := s.checkRef(ctx, field+".secret_id", secretID, kindSecret); err != nil {
			return "", "", err
		}
		return secretID, versionID, s.checkRef(ctx, field+".version_id", versionID, kindSecretVersion)
	}
	p, err := sp.SecretPayload(ctx, peer.SecretRef{SecretID: secretID, VersionID: versionID})
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", field, err)
	}
	for _, k := range keys {
		if _, ok := p.Entry(k); !ok {
			return "", "", errs.BadRequest(errs.Violation{Field: field, Message: "secret payload must contain " + k})
		}
	}
	return p.SecretID, p.VersionID, nil
}

func (s *EndpointService) checkRefShape(field, id, kind string) error {
	if id == "" || (strings.HasPrefix(id, kind+"-") && len(id) > len(kind)+1) {
		return nil
	}
	return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("%q is not a %s ID", id, kind)})
}

// authToken returns the token the data plane must see, or "" when the
// endpoint is open.
func (s *EndpointService) authToken(ctx context.Context, spec *aipb.EndpointSpec) (string, error) {
	if t := spec.GetAuthToken(); t != "" {
		return t, nil
	}
	ref := spec.GetAuthTokenMysteryboxSecret()
	if ref == nil {
		return "", nil
	}
	sp, ok := s.secrets.SecretPayloads()
	if !ok {
		return "", errs.BadState(ref.GetSecretId(), "the auth token secret cannot be read: the mysterybox service is not enabled")
	}
	p, err := sp.SecretPayload(ctx, peer.SecretRef{SecretID: ref.GetSecretId(), VersionID: ref.GetVersionId()})
	if err != nil {
		return "", errs.BadState(ref.GetSecretId(), "the auth token secret cannot be read: "+err.Error())
	}
	e, ok := p.Entry(authTokenKey)
	if !ok {
		return "", errs.BadState(ref.GetSecretId(), "the auth token secret has no "+authTokenKey+" entry")
	}
	if e.Binary {
		return string(e.BinaryValue), nil
	}
	return e.StringValue, nil
}
