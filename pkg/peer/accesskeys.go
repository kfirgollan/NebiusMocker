package peer

import "context"

// AccessKeysKey is the capability provided by the iam module: authentication
// of S3-style access keys (iam.v2 AccessKey) for data planes such as the
// Object Storage S3 front.
var AccessKeysKey = NewKey[AccessKeys]("iam.access-keys")

// AccessKeys resolves AWS-style access key IDs to usable keys.
//
// It is an internal, privileged call: it returns the signing secret, so only
// data planes that verify signatures themselves may use it.
type AccessKeys interface {
	// ResolveAccessKey returns the active, unexpired key with the AWS access
	// key ID, or an errs.Unauthenticated error for unknown, inactive or
	// expired keys and keys of missing or inactive accounts.
	ResolveAccessKey(ctx context.Context, awsAccessKeyID string) (AccessKey, error)
}

// AccessKey is a usable S3-style key.
type AccessKey struct {
	// AccessKeyID is the AWS-style key ID (status.aws_access_key_id).
	AccessKeyID string
	// Secret is the signing secret.
	Secret string
	// PrincipalID is the account the key acts as (a service account ID).
	PrincipalID string
	// KeyID is the Nebius resource ID of the key ("accesskey-…").
	KeyID string
}
