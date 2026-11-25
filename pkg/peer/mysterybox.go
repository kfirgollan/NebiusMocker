package peer

import "context"

// SecretPayloadsKey is the capability provided by the mysterybox module.
var SecretPayloadsKey = NewKey[SecretPayloads]("mysterybox.secret-payloads")

// SecretPayloads resolves MysteryBox secret references to their payloads, for
// services that accept a MysteryBoxSecretRef (AI endpoints, access keys, …).
//
// It is an internal, privileged call: the provider runs no IAM checks on it,
// because the consumer has already authorized its own request.
type SecretPayloads interface {
	// SecretPayload resolves ref to one secret version and its payload.
	//
	//   - SecretID and VersionID set: the version must belong to the secret.
	//   - only VersionID set: SecretID is filled from the version.
	//   - only SecretID set: the secret's primary version is used.
	//
	// Errors: errs.BadRequest when both IDs are empty; errs.NotFound when the
	// secret or version does not exist, or the version belongs to another
	// secret; errs.BadState when there is no usable version (no primary
	// version, or the version is scheduled for deletion or destroyed).
	SecretPayload(ctx context.Context, ref SecretRef) (SecretPayload, error)
}

// SecretRef points at a MysteryBox secret ("mbsec-…") and/or one of its
// versions ("mbsecver-…").
type SecretRef struct {
	SecretID  string
	VersionID string
}

// SecretPayload is a resolved secret version.
type SecretPayload struct {
	SecretID  string
	VersionID string
	Entries   []PayloadEntry
}

// PayloadEntry is one key of a secret payload. Binary tells which of
// StringValue and BinaryValue holds the value.
type PayloadEntry struct {
	Key         string
	StringValue string
	BinaryValue []byte
	Binary      bool
}

// Entry returns the entry called key.
func (p SecretPayload) Entry(key string) (PayloadEntry, bool) {
	for _, e := range p.Entries {
		if e.Key == key {
			return e, true
		}
	}
	return PayloadEntry{}, false
}
