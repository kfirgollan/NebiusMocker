package peer

import "context"

// SystemSecretsKey is the capability provided by the mysterybox module: other
// services store a secret in MysteryBox on a caller's behalf, e.g. IAM access
// keys created with secret_delivery_mode = MYSTERY_BOX.
var SystemSecretsKey = NewKey[SystemSecrets]("mysterybox.system-secrets")

// SystemSecrets creates MysteryBox secrets for other services.
//
// It is an internal, privileged call: the provider runs no IAM checks on it,
// because the consumer has already authorized its own request. The secrets it
// creates are ordinary MysteryBox secrets: reading their payload through
// PayloadService needs the usual MysteryBox permissions.
type SystemSecrets interface {
	// CreateSystemSecret creates a secret in a project with one primary
	// version holding the entries. Errors: errs.BadRequest for an invalid
	// payload; errs.NotFound for a missing project; errs.AlreadyExists when
	// the name is taken in the project.
	CreateSystemSecret(ctx context.Context, spec SystemSecretSpec) (SystemSecret, error)

	// DiscardSystemSecret removes a secret created by CreateSystemSecret for
	// good (no soft delete). It is meant for rolling back a create whose
	// consumer-side resource could not be stored. A missing secret is not an
	// error.
	DiscardSystemSecret(ctx context.Context, secretID string) error
}

// SystemSecretSpec is the input of CreateSystemSecret.
type SystemSecretSpec struct {
	ParentID    string
	Name        string
	Description string
	Labels      map[string]string
	Entries     []PayloadEntry
}

// SystemSecret identifies a created secret ("mbsec-…") and its primary
// version ("mbsecver-…").
type SystemSecret struct {
	SecretID  string
	VersionID string
}
