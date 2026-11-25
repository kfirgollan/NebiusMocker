package domain

import "github.com/kfirgollan/NebiusMocker/pkg/authz"

// S3Action is an S3 API operation, named as in the S3 API reference and the
// Nebius "supported actions" table. Authorization is decided per action: by
// IAM through the action's permission, or by a bucket policy role that
// includes the action.
type S3Action string

// S3 actions served by the S3 front.
const (
	ActListBuckets S3Action = "ListBuckets"

	// Bucket-level.
	ActCreateBucket          S3Action = "CreateBucket"
	ActDeleteBucket          S3Action = "DeleteBucket"
	ActHeadBucket            S3Action = "HeadBucket"
	ActGetBucketLocation     S3Action = "GetBucketLocation"
	ActGetBucketVersioning   S3Action = "GetBucketVersioning"
	ActPutBucketVersioning   S3Action = "PutBucketVersioning"
	ActGetBucketLifecycle    S3Action = "GetBucketLifecycleConfiguration"
	ActPutBucketLifecycle    S3Action = "PutBucketLifecycleConfiguration"
	ActDeleteBucketLifecycle S3Action = "DeleteBucketLifecycle"
	ActGetBucketCors         S3Action = "GetBucketCors"
	ActPutBucketCors         S3Action = "PutBucketCors"
	ActDeleteBucketCors      S3Action = "DeleteBucketCors"
	ActListObjects           S3Action = "ListObjects" // V1 and V2
	ActListObjectVersions    S3Action = "ListObjectVersions"
	ActListMultipartUploads  S3Action = "ListMultipartUploads"
	ActDeleteObjects         S3Action = "DeleteObjects"
	ActGetBucketOther        S3Action = "GetBucketOther" // other bucket sub-resource reads relayed to the backend

	// Object-level.
	ActGetObject               S3Action = "GetObject"
	ActHeadObject              S3Action = "HeadObject"
	ActGetObjectTagging        S3Action = "GetObjectTagging"
	ActPutObject               S3Action = "PutObject"
	ActCopyObject              S3Action = "CopyObject"
	ActPutObjectTagging        S3Action = "PutObjectTagging"
	ActDeleteObjectTagging     S3Action = "DeleteObjectTagging"
	ActDeleteObject            S3Action = "DeleteObject"
	ActCreateMultipartUpload   S3Action = "CreateMultipartUpload"
	ActUploadPart              S3Action = "UploadPart"
	ActUploadPartCopy          S3Action = "UploadPartCopy"
	ActCompleteMultipartUpload S3Action = "CompleteMultipartUpload"
	ActAbortMultipartUpload    S3Action = "AbortMultipartUpload"
	ActListParts               S3Action = "ListParts"
)

// S3 data-plane permissions. Object operations use the standard verbs, so the
// seeded viewer/editor/admin roles apply to objects as they do to buckets.
var (
	PermObjectGet    = authz.Perm("storage", "objects", authz.VerbGet)
	PermObjectList   = authz.Perm("storage", "objects", authz.VerbList)
	PermObjectWrite  = authz.Perm("storage", "objects", authz.VerbCreate)
	PermObjectDelete = authz.Perm("storage", "objects", authz.VerbDelete)
	PermBucketGet    = authz.Perm("storage", "buckets", authz.VerbGet)
	PermBucketList   = authz.Perm("storage", "buckets", authz.VerbList)
	PermBucketCreate = authz.Perm("storage", "buckets", authz.VerbCreate)
	PermBucketUpdate = authz.Perm("storage", "buckets", authz.VerbUpdate)
	PermBucketDelete = authz.Perm("storage", "buckets", authz.VerbDelete)
)

// actionScope says what bucket policy paths are matched against.
type actionScope int

const (
	scopeBucket  actionScope = iota // only the "*" path matches
	scopeObject                     // the object key
	scopeListing                    // the listing prefix
)

type actionInfo struct {
	perm  authz.Permission
	scope actionScope
	// addsBytes marks actions that store new object data (max_size_bytes).
	addsBytes bool
}

// actionTable is the per-action authorization metadata.
var actionTable = map[S3Action]actionInfo{
	ActListBuckets:             {perm: PermBucketList},
	ActCreateBucket:            {perm: PermBucketCreate},
	ActDeleteBucket:            {perm: PermBucketDelete},
	ActHeadBucket:              {perm: PermBucketGet},
	ActGetBucketLocation:       {perm: PermBucketGet},
	ActGetBucketVersioning:     {perm: PermBucketGet},
	ActPutBucketVersioning:     {perm: PermBucketUpdate},
	ActGetBucketLifecycle:      {perm: PermBucketGet},
	ActPutBucketLifecycle:      {perm: PermBucketUpdate},
	ActDeleteBucketLifecycle:   {perm: PermBucketUpdate},
	ActGetBucketCors:           {perm: PermBucketGet},
	ActPutBucketCors:           {perm: PermBucketUpdate},
	ActDeleteBucketCors:        {perm: PermBucketUpdate},
	ActGetBucketOther:          {perm: PermBucketGet},
	ActListObjects:             {perm: PermObjectList, scope: scopeListing},
	ActListObjectVersions:      {perm: PermObjectList, scope: scopeListing},
	ActListMultipartUploads:    {perm: PermObjectList, scope: scopeListing},
	ActDeleteObjects:           {perm: PermObjectDelete},
	ActGetObject:               {perm: PermObjectGet, scope: scopeObject},
	ActHeadObject:              {perm: PermObjectGet, scope: scopeObject},
	ActGetObjectTagging:        {perm: PermObjectGet, scope: scopeObject},
	ActPutObject:               {perm: PermObjectWrite, scope: scopeObject, addsBytes: true},
	ActCopyObject:              {perm: PermObjectWrite, scope: scopeObject, addsBytes: true},
	ActPutObjectTagging:        {perm: PermObjectWrite, scope: scopeObject},
	ActDeleteObjectTagging:     {perm: PermObjectWrite, scope: scopeObject},
	ActDeleteObject:            {perm: PermObjectDelete, scope: scopeObject},
	ActCreateMultipartUpload:   {perm: PermObjectWrite, scope: scopeObject},
	ActUploadPart:              {perm: PermObjectWrite, scope: scopeObject, addsBytes: true},
	ActUploadPartCopy:          {perm: PermObjectWrite, scope: scopeObject, addsBytes: true},
	ActCompleteMultipartUpload: {perm: PermObjectWrite, scope: scopeObject},
	ActAbortMultipartUpload:    {perm: PermObjectWrite, scope: scopeObject},
	ActListParts:               {perm: PermObjectList, scope: scopeObject},
}

// Permission is the IAM permission an action needs on its bucket (on the
// project for CreateBucket and ListBuckets). Unknown actions need
// storage.buckets.get.
func (a S3Action) Permission() authz.Permission {
	if info, ok := actionTable[a]; ok {
		return info.perm
	}
	return PermBucketGet
}

// ObjectScoped reports whether the action addresses one object key (bucket
// policy paths are matched against that key). Listings are matched against
// the requested prefix; other bucket-level actions only match the "*" path.
func (a S3Action) ObjectScoped() bool { return actionTable[a].scope == scopeObject }

// Listing reports whether the action lists keys under a prefix.
func (a S3Action) Listing() bool { return actionTable[a].scope == scopeListing }

// AddsBytes reports whether the action stores new object data (checked
// against spec.max_size_bytes).
func (a S3Action) AddsBytes() bool { return actionTable[a].addsBytes }
