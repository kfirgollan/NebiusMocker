package domain

import (
	"context"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// PayloadRef selects the payload to read: a secret, and optionally one of its
// versions (empty means the secret's primary version).
type PayloadRef struct {
	SecretID  string
	VersionID string
}

// GetPayload returns every entry of a version's payload, decrypted.
func (s *Service) GetPayload(ctx context.Context, ref PayloadRef) (*mysteryboxpb.SecretPayload, error) {
	versionID, entries, err := s.openPayload(ctx, ref)
	if err != nil {
		return nil, err
	}
	return &mysteryboxpb.SecretPayload{VersionId: versionID, Data: entries}, nil
}

// GetPayloadByKey returns one entry of a version's payload, decrypted.
func (s *Service) GetPayloadByKey(ctx context.Context, ref PayloadRef, key string) (*mysteryboxpb.SecretPayloadEntry, error) {
	if key == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "key", Message: "required"})
	}
	versionID, entries, err := s.openPayload(ctx, ref)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(entries, func(p *mysteryboxpb.Payload) bool { return p.GetKey() == key })
	if i < 0 {
		// The key is sensitive: name the version, not the key.
		return nil, &errs.Error{Kind: errs.KindNotFound, ResourceID: versionID, Message: "the version has no entry with this key"}
	}
	return &mysteryboxpb.SecretPayloadEntry{VersionId: versionID, Data: entries[i]}, nil
}

// openPayload resolves ref to an active version of an active secret and
// decrypts its payload.
func (s *Service) openPayload(ctx context.Context, ref PayloadRef) (string, []*mysteryboxpb.Payload, error) {
	if ref.SecretID == "" {
		return "", nil, errs.BadRequest(errs.Violation{Field: "secret_id", Message: "required"})
	}
	sec, err := s.secret(ctx, ref.SecretID, activeOnly)
	if err != nil {
		return "", nil, err
	}
	versionID := ref.VersionID
	if versionID == "" {
		versionID = sec.GetSpec().GetPrimaryVersionId()
		if versionID == "" {
			return "", nil, errs.BadState(ref.SecretID, "the secret has no primary version; pass version_id")
		}
	}
	v, err := s.version(ctx, versionID, activeOnly)
	if err != nil {
		return "", nil, err
	}
	if v.GetMetadata().GetParentId() != ref.SecretID {
		return "", nil, errs.NotFound(versionID)
	}
	entries, err := s.unseal(ctx, versionID)
	if err != nil {
		return "", nil, err
	}
	return versionID, entries, nil
}

// ResolvedPayload is a decrypted payload with the secret and version it
// belongs to.
type ResolvedPayload struct {
	SecretID  string
	VersionID string
	Entries   []*mysteryboxpb.Payload
}

// ResolvePayload is the privileged in-process payload read for other modules
// (e.g. AI endpoints reading an auth token). It runs no authorization: the
// caller has authorized its own request. Unlike GetPayload it accepts a
// version alone, and it reports unusable resources as errs.BadState:
//   - both IDs empty: errs.BadRequest;
//   - unknown secret or version, or a version of another secret: errs.NotFound;
//   - no primary version, or the secret or version scheduled for deletion:
//     errs.BadState.
func (s *Service) ResolvePayload(ctx context.Context, ref PayloadRef) (ResolvedPayload, error) {
	if ref.SecretID == "" && ref.VersionID == "" {
		return ResolvedPayload{}, errs.BadRequest(errs.Violation{Field: "secret_id", Message: "a secret or version ID is required"})
	}
	out := ResolvedPayload{SecretID: ref.SecretID, VersionID: ref.VersionID}
	if out.VersionID != "" {
		v, err := s.version(ctx, out.VersionID, withDeleted)
		if err != nil {
			return ResolvedPayload{}, err
		}
		if out.SecretID == "" {
			out.SecretID = v.GetMetadata().GetParentId()
		}
		if v.GetMetadata().GetParentId() != out.SecretID {
			return ResolvedPayload{}, errs.NotFound(out.VersionID)
		}
		if versionDeleted(v) {
			return ResolvedPayload{}, errs.BadState(out.VersionID, "the version is scheduled for deletion")
		}
	}
	sec, err := s.secret(ctx, out.SecretID, withDeleted)
	if err != nil {
		return ResolvedPayload{}, err
	}
	if secretDeleted(sec) {
		return ResolvedPayload{}, errs.BadState(out.SecretID, "the secret is scheduled for deletion")
	}
	if out.VersionID == "" {
		out.VersionID = sec.GetSpec().GetPrimaryVersionId()
		if out.VersionID == "" {
			return ResolvedPayload{}, errs.BadState(out.SecretID, "the secret has no primary version")
		}
		v, err := s.version(ctx, out.VersionID, withDeleted)
		switch {
		case errs.IsKind(err, errs.KindNotFound):
			return ResolvedPayload{}, errs.BadState(out.SecretID, "the primary version no longer exists")
		case err != nil:
			return ResolvedPayload{}, err
		case versionDeleted(v):
			return ResolvedPayload{}, errs.BadState(out.VersionID, "the primary version is scheduled for deletion")
		}
	}
	if out.Entries, err = s.unseal(ctx, out.VersionID); err != nil {
		return ResolvedPayload{}, err
	}
	return out, nil
}

// unseal reads and decrypts a version's payload.
func (s *Service) unseal(ctx context.Context, versionID string) ([]*mysteryboxpb.Payload, error) {
	sealed, err := s.payloads.Get(ctx, versionID)
	if err != nil {
		return nil, fmt.Errorf("payload of %s: %w", versionID, err)
	}
	plain, err := s.sealer.Open(ctx, sealed, []byte(versionID))
	if err != nil {
		return nil, errs.Internal("cannot decrypt the payload of " + versionID)
	}
	var p mysteryboxpb.SecretPayload
	if err := proto.Unmarshal(plain, &p); err != nil {
		return nil, errs.Internal("corrupt payload of " + versionID)
	}
	return p.GetData(), nil
}

// sealPayload encrypts entries for storage under versionID.
func (s *Service) sealPayload(ctx context.Context, versionID string, entries []*mysteryboxpb.Payload) ([]byte, error) {
	plain, err := proto.MarshalOptions{Deterministic: true}.Marshal(&mysteryboxpb.SecretPayload{Data: entries})
	if err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}
	sealed, err := s.sealer.Seal(ctx, plain, []byte(versionID))
	if err != nil {
		return nil, fmt.Errorf("seal payload: %w", err)
	}
	return sealed, nil
}

// validatePayload enforces the payload rules and limits. field is the
// request path of the entry list. Messages never quote keys or values.
func (s *Service) validatePayload(field string, entries []*mysteryboxpb.Payload) error {
	if len(entries) == 0 {
		return errs.BadRequest(errs.Violation{Field: field, Message: "at least one payload entry is required"})
	}
	if n := s.limits.MaxEntriesPerVersion; len(entries) > n {
		return errs.BadRequest(errs.Violation{
			Field: field, Message: fmt.Sprintf("a version holds at most %d entries, got %d", n, len(entries)),
		})
	}
	seen := make(map[string]struct{}, len(entries))
	var violations []errs.Violation
	for i, p := range entries {
		at := fmt.Sprintf("%s[%d]", field, i)
		violations = append(violations, s.entryViolations(at, p)...)
		if _, dup := seen[p.GetKey()]; dup && p.GetKey() != "" {
			violations = append(violations, errs.Violation{Field: at + ".key", Message: "duplicate key"})
		}
		seen[p.GetKey()] = struct{}{}
	}
	if len(violations) > 0 {
		return errs.BadRequest(violations...)
	}
	return nil
}

func (s *Service) entryViolations(at string, p *mysteryboxpb.Payload) []errs.Violation {
	var out []errs.Violation
	switch k := len(p.GetKey()); {
	case k == 0:
		out = append(out, errs.Violation{Field: at + ".key", Message: "required"})
	case k > s.limits.MaxKeyBytes:
		out = append(out, errs.Violation{Field: at + ".key", Message: fmt.Sprintf("must be at most %d bytes", s.limits.MaxKeyBytes)})
	}
	var size int
	switch v := p.GetPayload().(type) {
	case *mysteryboxpb.Payload_StringValue:
		size = len(v.StringValue)
	case *mysteryboxpb.Payload_BinaryValue:
		size = len(v.BinaryValue)
	default:
		return append(out, errs.Violation{Field: at, Message: "exactly one of string_value or binary_value is required"})
	}
	switch {
	case size == 0:
		out = append(out, errs.Violation{Field: at, Message: "the value must not be empty"})
	case size > s.limits.MaxValueBytes:
		out = append(out, errs.Violation{Field: at, Message: fmt.Sprintf("the value must be at most %d bytes", s.limits.MaxValueBytes)})
	}
	return out
}
