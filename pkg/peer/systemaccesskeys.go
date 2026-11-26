package peer

import "context"

// SystemAccessKeysKey is the capability provided by the iam module: S3-style
// access keys (iam.v2 AccessKey) created for a service account on behalf of
// another service, e.g. the key a Managed MLflow cluster's tracking server
// uses to write artifacts to its bucket.
var SystemAccessKeysKey = NewKey[SystemAccessKeys]("iam.system-access-keys")

// SystemAccessKeys creates and deletes access keys for other services.
//
// It is an internal, privileged call: the provider runs no IAM checks,
// because the consumer has already authorized its own request. The keys are
// ordinary access keys (listed by AccessKeyService, usable on the S3 front,
// acting with the account's permissions).
type SystemAccessKeys interface {
	// CreateSystemAccessKey creates an active key for the account. Errors:
	// errs.NotFound for a missing account or project; errs.AlreadyExists
	// when the name is taken in the project.
	CreateSystemAccessKey(ctx context.Context, spec SystemAccessKeySpec) (SystemAccessKey, error)
	// DeleteSystemAccessKey removes a key created by CreateSystemAccessKey.
	// A missing key is not an error.
	DeleteSystemAccessKey(ctx context.Context, keyID string) error
}

// SystemAccessKeySpec is the input of CreateSystemAccessKey.
type SystemAccessKeySpec struct {
	// ParentID is the project the key is created in.
	ParentID    string
	Name        string
	Description string
	// AccountID is the service account ("serviceaccount-…") the key acts as.
	AccountID string
}

// SystemAccessKey is a created key and its S3 credentials.
type SystemAccessKey struct {
	// KeyID is the Nebius resource ID ("accesskey-…").
	KeyID string
	// AccessKeyID is the AWS-style key ID; Secret its signing secret.
	AccessKeyID string
	Secret      string
}
