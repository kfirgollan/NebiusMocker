package domain

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/durationpb"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Field limits from the buf.validate rules of both transfer protos.
const (
	trMaxEndpoint = 1000
	trMaxRegion   = 50
	trMaxBucket   = 100
	trMaxPrefix   = 1000
	trMaxSecret   = 1000
)

const msgExternalToExternal = "transfers between two external providers are not supported: one side must be a Nebius bucket"

// violations collects field violations; the field is the proto path of the
// request's API version.
type violations []errs.Violation

func (v *violations) add(field, msg string) {
	*v = append(*v, errs.Violation{Field: field, Message: msg})
}

func (v *violations) err() error {
	if len(*v) == 0 {
		return nil
	}
	return errs.BadRequest(*v...)
}

// str checks a string field's buf.validate rules.
func (v *violations) str(field, val string, maxLen int, required bool) {
	switch {
	case required && val == "":
		v.add(field, "required")
	case len(val) > maxLen:
		v.add(field, fmt.Sprintf("must be at most %d characters", maxLen))
	}
}

// endpoint checks a provider URL: scheme://host[:port][/path] without a
// trailing slash, query, fragment or userinfo. The docs ask for https; the
// mocker also accepts http so that local S3 servers (including its own S3
// front) can be transfer endpoints.
// The endpoint is required.
func (v *violations) endpoint(field, val string) {
	v.str(field, val, trMaxEndpoint, true)
	if val == "" || len(val) > trMaxEndpoint {
		return
	}
	u, err := url.Parse(val)
	switch {
	case err != nil || u.Opaque != "":
		v.add(field, "must be an absolute URL such as https://storage.example.com")
	case u.Scheme != "https" && u.Scheme != "http":
		v.add(field, "must start with https:// (or http:// for local servers)")
	case u.Host == "" || u.Hostname() == "":
		v.add(field, "must name a host")
	case u.User != nil:
		v.add(field, "must not contain user information")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(val, "#"):
		v.add(field, "must not contain a query or fragment")
	case strings.HasSuffix(val, "/"):
		v.add(field, "must not end with a slash")
	}
}

// nebiusRegion checks the region of a Nebius side: it must be the served one.
func (v *violations) nebiusRegion(field, val, served string) {
	v.str(field, val, trMaxRegion, true)
	if val != "" && len(val) <= trMaxRegion && val != served {
		v.add(field, fmt.Sprintf("region %q is not served by NebiusMocker (only %q)", val, served))
	}
}

func (v *violations) accessKey(field string, ak *storagepb.TransferCredentialsAccessKey) {
	v.str(field+".access_key_id", ak.GetAccessKeyId(), trMaxSecret, true)
	v.str(field+".secret_access_key", ak.GetSecretAccessKey(), trMaxSecret, true)
}

func (v *violations) interval(field string, d *durationpb.Duration) {
	if d == nil {
		return
	}
	if err := d.CheckValid(); err != nil {
		v.add(field, "invalid duration: "+err.Error())
		return
	}
	if d.AsDuration() < 0 {
		v.add(field, "must not be negative")
	}
}

func (v *violations) threshold(field string, n uint32) {
	if n < 1 {
		v.add(field, "must be at least 1")
	}
}

// immutable reports every immutable field whose value changed. When a
// side's provider changed, only the provider is reported for that side.
func (v *violations) immutable(before, after []trField) {
	now := make(map[string]string, len(after))
	for _, f := range after {
		now[f.field] = f.value
	}
	moved := map[string]bool{} // side ("spec.source") whose provider changed
	for _, f := range before {
		if side, ok := strings.CutSuffix(f.field, ".provider"); ok && now[f.field] != f.value {
			moved[side] = true
		}
	}
	for _, f := range before {
		if got, ok := now[f.field]; ok && got == f.value {
			continue
		}
		parts := strings.Split(f.field, ".")
		if len(parts) >= 4 && moved[parts[0]+"."+parts[1]] {
			continue // a field of the replaced provider
		}
		v.add(f.field, "immutable")
	}
}

// trField is an immutable field and its value.
type trField struct{ field, value string }

func (s *TransferService) validateV1(spec *storagepb.TransferSpec) violations {
	var v violations
	if spec == nil {
		v.add("spec", "required")
		return v
	}
	src, dst := spec.GetSource(), spec.GetDestination()
	if src == nil {
		v.add("spec.source", "required")
	} else {
		s.validateV1Source(&v, src)
	}
	if dst == nil {
		v.add("spec.destination", "required")
	} else {
		s.validateV1Destination(&v, dst)
	}
	if src.GetProvider() != nil && dst.GetProvider() != nil && src.GetNebius() == nil && dst.GetNebius() == nil {
		v.add("spec.destination", msgExternalToExternal)
	}
	switch spec.GetStopCondition().(type) {
	case *storagepb.TransferSpec_AfterOneIteration, *storagepb.TransferSpec_Infinite:
	case *storagepb.TransferSpec_AfterNEmptyIterations:
		v.threshold("spec.after_n_empty_iterations.empty_iterations_threshold", spec.GetAfterNEmptyIterations().GetEmptyIterationsThreshold())
	default:
		v.add("spec.stop_condition", "required: one of after_one_iteration, after_n_empty_iterations, infinite")
	}
	v.interval("spec.inter_iteration_interval", spec.GetInterIterationInterval())
	switch spec.GetOverwriteStrategy() {
	case storagepb.TransferSpec_NEVER, storagepb.TransferSpec_IF_NEWER:
	case storagepb.TransferSpec_OVERWRITE_STRATEGY_UNSPECIFIED:
		v.add("spec.overwrite_strategy", "required")
	default:
		v.add("spec.overwrite_strategy", "must be NEVER or IF_NEWER")
	}
	return v
}

func (s *TransferService) validateV1Source(v *violations, src *storagepb.TransferSource) {
	v.str("spec.source.prefix", src.GetPrefix(), trMaxPrefix, false)
	switch src.GetProvider().(type) {
	case *storagepb.TransferSource_Nebius:
		const f = "spec.source.nebius"
		p := src.GetNebius()
		v.nebiusRegion(f+".region", p.GetRegion(), s.region)
		v.str(f+".bucket_name", p.GetBucketName(), trMaxBucket, true)
		switch p.GetCredentials().(type) {
		case *storagepb.TransferSource_NebiusProvider_Anonymous:
		case *storagepb.TransferSource_NebiusProvider_AccessKey:
			v.accessKey(f+".access_key", p.GetAccessKey())
		default:
			v.add(f+".credentials", "required: one of anonymous, access_key")
		}
	case *storagepb.TransferSource_S3Compatible:
		const f = "spec.source.s3_compatible"
		p := src.GetS3Compatible()
		v.endpoint(f+".endpoint", p.GetEndpoint())
		v.str(f+".region", p.GetRegion(), trMaxRegion, true)
		v.str(f+".bucket_name", p.GetBucketName(), trMaxBucket, true)
		switch p.GetCredentials().(type) {
		case *storagepb.TransferSource_S3CompatibleProvider_Anonymous:
		case *storagepb.TransferSource_S3CompatibleProvider_AccessKey:
			v.accessKey(f+".access_key", p.GetAccessKey())
		default:
			v.add(f+".credentials", "required: one of anonymous, access_key")
		}
	case *storagepb.TransferSource_AzureBlobStorage:
		const f = "spec.source.azure_blob_storage"
		p := src.GetAzureBlobStorage()
		v.endpoint(f+".endpoint", p.GetEndpoint())
		v.str(f+".container_name", p.GetContainerName(), trMaxBucket, true)
		switch p.GetCredentials().(type) {
		case *storagepb.TransferSource_AzureBlobStorageProvider_Anonymous:
		case *storagepb.TransferSource_AzureBlobStorageProvider_AzureStorageAccount:
			a := p.GetAzureStorageAccount()
			v.str(f+".azure_storage_account.account_name", a.GetAccountName(), trMaxSecret, true)
			v.str(f+".azure_storage_account.access_key", a.GetAccessKey(), trMaxSecret, true)
		default:
			v.add(f+".credentials", "required: one of anonymous, azure_storage_account")
		}
	default:
		v.add("spec.source.provider", "required: one of nebius, s3_compatible, azure_blob_storage")
	}
}

func (s *TransferService) validateV1Destination(v *violations, dst *storagepb.TransferDestination) {
	v.str("spec.destination.prefix", dst.GetPrefix(), trMaxPrefix, false)
	switch dst.GetProvider().(type) {
	case *storagepb.TransferDestination_Nebius:
		const f = "spec.destination.nebius"
		p := dst.GetNebius()
		v.nebiusRegion(f+".region", p.GetRegion(), s.region)
		v.str(f+".bucket_name", p.GetBucketName(), trMaxBucket, true)
		if p.GetAccessKey() == nil {
			v.add(f+".credentials", "required: access_key")
		} else {
			v.accessKey(f+".access_key", p.GetAccessKey())
		}
	case *storagepb.TransferDestination_S3Compatible:
		const f = "spec.destination.s3_compatible"
		p := dst.GetS3Compatible()
		v.endpoint(f+".endpoint", p.GetEndpoint())
		v.str(f+".region", p.GetRegion(), trMaxRegion, true)
		v.str(f+".bucket_name", p.GetBucketName(), trMaxBucket, true)
		switch p.GetCredentials().(type) {
		case *storagepb.TransferDestination_S3CompatibleProvider_Anonymous:
		case *storagepb.TransferDestination_S3CompatibleProvider_AccessKey:
			v.accessKey(f+".access_key", p.GetAccessKey())
		default:
			v.add(f+".credentials", "required: one of anonymous, access_key")
		}
	default:
		v.add("spec.destination.provider", "required: one of nebius, s3_compatible")
	}
}

func (s *TransferService) validateAlpha(spec *storagev1alpha1pb.TransferSpec) violations {
	var v violations
	if spec == nil {
		v.add("spec", "required")
		return v
	}
	if src := spec.GetSource(); src == nil {
		v.add("spec.source", "required")
	} else {
		v.endpoint("spec.source.endpoint", src.GetEndpoint())
		v.str("spec.source.bucket_name", src.GetBucketName(), trMaxBucket, true)
		v.str("spec.source.region", src.GetRegion(), trMaxRegion, true)
		v.str("spec.source.prefix", src.GetPrefix(), trMaxPrefix, false)
		if c := src.GetCredentials(); c != nil {
			alphaCredentials(&v, "spec.source.credentials", c)
		}
	}
	if dst := spec.GetDestination(); dst == nil {
		v.add("spec.destination", "required")
	} else {
		s.validateAlphaDestination(&v, dst)
	}
	switch spec.GetStopCondition().(type) {
	case *storagev1alpha1pb.TransferSpec_AfterOneIteration, *storagev1alpha1pb.TransferSpec_Infinite:
	case *storagev1alpha1pb.TransferSpec_AfterNEmptyIterations:
		v.threshold("spec.after_n_empty_iterations.empty_iterations_threshold", spec.GetAfterNEmptyIterations().GetEmptyIterationsThreshold())
	default:
		v.add("spec.stop_condition", "required: one of after_one_iteration, after_n_empty_iterations, infinite")
	}
	v.interval("spec.inter_iteration_interval", spec.GetInterIterationInterval())
	switch spec.GetOverwriteStrategy() {
	case storagev1alpha1pb.TransferSpec_NEVER, storagev1alpha1pb.TransferSpec_IF_NEWER, storagev1alpha1pb.TransferSpec_ALWAYS:
	case storagev1alpha1pb.TransferSpec_OVERWRITE_STRATEGY_UNSPECIFIED:
		v.add("spec.overwrite_strategy", "required")
	default:
		v.add("spec.overwrite_strategy", "must be NEVER, IF_NEWER or ALWAYS")
	}
	return v
}

// validateAlphaDestination: without an endpoint the destination is a bucket
// of this mocker; with one it is external, and since a v1alpha1 source is
// always external that is an unsupported external-to-external transfer.
func (s *TransferService) validateAlphaDestination(v *violations, dst *storagev1alpha1pb.TransferSpec_DestinationBucket) {
	const f = "spec.destination"
	v.str(f+".bucket_name", dst.GetBucketName(), trMaxBucket, true)
	v.str(f+".prefix", dst.GetPrefix(), trMaxPrefix, false)
	c := dst.GetCredentials()
	switch {
	case c == nil:
		v.add(f+".credentials", "required")
	case c.GetAzureAccessKey() != nil:
		v.add(f+".credentials.azure_access_key", "not supported for the destination")
	default:
		alphaCredentials(v, f+".credentials", c)
	}
	if dst.GetEndpoint() != "" {
		v.endpoint(f+".endpoint", dst.GetEndpoint())
		v.str(f+".region", dst.GetRegion(), trMaxRegion, true)
		v.add(f+".endpoint", msgExternalToExternal+"; omit the endpoint to write to a Nebius bucket")
		return
	}
	if r := dst.GetRegion(); r != "" {
		v.nebiusRegion(f+".region", r, s.region)
	}
	if c != nil && c.GetAnonymous() != nil {
		v.add(f+".credentials", "a Nebius destination requires access_key credentials")
	}
}

func alphaCredentials(v *violations, f string, c *storagev1alpha1pb.TransferSpec_BucketCredentials) {
	switch c.GetCredentials().(type) {
	case *storagev1alpha1pb.TransferSpec_BucketCredentials_Anonymous:
	case *storagev1alpha1pb.TransferSpec_BucketCredentials_AccessKey:
		ak := c.GetAccessKey()
		v.str(f+".access_key.access_key_id", ak.GetAccessKeyId(), trMaxSecret, true)
		v.str(f+".access_key.secret_access_key", ak.GetSecretAccessKey(), trMaxSecret, true)
	case *storagev1alpha1pb.TransferSpec_BucketCredentials_AzureAccessKey_:
		az := c.GetAzureAccessKey()
		v.str(f+".azure_access_key.account_name", az.GetAccountName(), trMaxSecret, true)
		v.str(f+".azure_access_key.access_key", az.GetAccessKey(), trMaxSecret, true)
	default:
		v.add(f, "required: one of anonymous, access_key, azure_access_key")
	}
}

// checkBuckets requires the Nebius bucket of a new transfer to exist, be
// ACTIVE and belong to the transfer's project (the docs: create the transfer
// in the project of the destination bucket). That is the destination, or the
// source when only the source is a Nebius bucket.
func (s *TransferService) checkBuckets(ctx context.Context, rec *TransferRecord) error {
	plan, err := s.PlanOf(rec)
	if err != nil {
		return err
	}
	field, bucket := "", ""
	switch {
	case plan.Destination.Kind == ProviderNebius && rec.API == TransferAPIV1Alpha1:
		field, bucket = "spec.destination.bucket_name", plan.Destination.Bucket
	case plan.Destination.Kind == ProviderNebius:
		field, bucket = "spec.destination.nebius.bucket_name", plan.Destination.Bucket
	case plan.Source.Kind == ProviderNebius:
		field, bucket = "spec.source.nebius.bucket_name", plan.Source.Bucket
	default:
		return nil
	}
	b, err := s.buckets.GetByBucketName(ctx, bucket)
	if errs.IsKind(err, errs.KindNotFound) {
		return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("bucket %q does not exist", bucket)})
	}
	if err != nil {
		return fmt.Errorf("look up bucket %s: %w", bucket, err)
	}
	project := rec.Metadata().GetParentId()
	if b.GetMetadata().GetParentId() != project {
		return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf(
			"bucket %q is not in project %s; create the transfer in the project of the bucket", bucket, project)})
	}
	if st := b.GetStatus().GetState(); st != storagepb.BucketStatus_ACTIVE {
		return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf("bucket %q is %s, not ACTIVE", bucket, st)})
	}
	return nil
}

// v1Immutables lists the immutable fields of a v1 spec.
func v1Immutables(spec *storagepb.TransferSpec) []trField {
	src, dst := spec.GetSource(), spec.GetDestination()
	out := []trField{{"spec.source.provider", sourceKind(src).String()}}
	switch {
	case src.GetNebius() != nil:
		p := src.GetNebius()
		out = append(out, trField{"spec.source.nebius.region", p.GetRegion()}, trField{"spec.source.nebius.bucket_name", p.GetBucketName()})
	case src.GetS3Compatible() != nil:
		p := src.GetS3Compatible()
		out = append(out, trField{"spec.source.s3_compatible.endpoint", p.GetEndpoint()},
			trField{"spec.source.s3_compatible.region", p.GetRegion()}, trField{"spec.source.s3_compatible.bucket_name", p.GetBucketName()})
	case src.GetAzureBlobStorage() != nil:
		p := src.GetAzureBlobStorage()
		out = append(out, trField{"spec.source.azure_blob_storage.endpoint", p.GetEndpoint()},
			trField{"spec.source.azure_blob_storage.container_name", p.GetContainerName()})
	}
	out = append(out, trField{"spec.source.prefix", src.GetPrefix()})
	dstKind := ProviderKind(0)
	if dst.GetNebius() != nil {
		dstKind = ProviderNebius
	} else if dst.GetS3Compatible() != nil {
		dstKind = ProviderS3Compatible
	}
	out = append(out, trField{"spec.destination.provider", dstKind.String()})
	switch {
	case dst.GetNebius() != nil:
		p := dst.GetNebius()
		out = append(out, trField{"spec.destination.nebius.region", p.GetRegion()}, trField{"spec.destination.nebius.bucket_name", p.GetBucketName()})
	case dst.GetS3Compatible() != nil:
		p := dst.GetS3Compatible()
		out = append(out, trField{"spec.destination.s3_compatible.endpoint", p.GetEndpoint()},
			trField{"spec.destination.s3_compatible.region", p.GetRegion()}, trField{"spec.destination.s3_compatible.bucket_name", p.GetBucketName()})
	}
	return append(out,
		trField{"spec.destination.prefix", dst.GetPrefix()},
		trField{"spec.overwrite_strategy", spec.GetOverwriteStrategy().String()},
		trField{"spec.enable_deletes_in_destination", strconv.FormatBool(spec.GetEnableDeletesInDestination())},
		trField{"spec.touch_unmanaged", strconv.FormatBool(spec.GetTouchUnmanaged())},
	)
}

func sourceKind(src *storagepb.TransferSource) ProviderKind {
	switch {
	case src.GetNebius() != nil:
		return ProviderNebius
	case src.GetS3Compatible() != nil:
		return ProviderS3Compatible
	case src.GetAzureBlobStorage() != nil:
		return ProviderAzureBlob
	}
	return 0
}

// alphaImmutables lists the immutable fields of a v1alpha1 spec.
func alphaImmutables(spec *storagev1alpha1pb.TransferSpec) []trField {
	src, dst := spec.GetSource(), spec.GetDestination()
	return []trField{
		{"spec.source.endpoint", src.GetEndpoint()},
		{"spec.source.bucket_name", src.GetBucketName()},
		{"spec.source.region", src.GetRegion()},
		{"spec.source.prefix", src.GetPrefix()},
		{"spec.destination.endpoint", dst.GetEndpoint()},
		{"spec.destination.bucket_name", dst.GetBucketName()},
		{"spec.destination.region", dst.GetRegion()},
		{"spec.destination.prefix", dst.GetPrefix()},
	}
}

// keepV1Secrets restores the stored INPUT_ONLY secrets that an update sends
// back empty (clients never see them) while the credential form is unchanged.
func keepV1Secrets(stored, updated *storagepb.TransferSpec) {
	keep := func(old, cur *storagepb.TransferCredentialsAccessKey) {
		if old != nil && cur != nil && cur.GetSecretAccessKey() == "" {
			cur.SecretAccessKey = old.GetSecretAccessKey()
		}
	}
	keep(stored.GetSource().GetNebius().GetAccessKey(), updated.GetSource().GetNebius().GetAccessKey())
	keep(stored.GetSource().GetS3Compatible().GetAccessKey(), updated.GetSource().GetS3Compatible().GetAccessKey())
	keep(stored.GetDestination().GetNebius().GetAccessKey(), updated.GetDestination().GetNebius().GetAccessKey())
	keep(stored.GetDestination().GetS3Compatible().GetAccessKey(), updated.GetDestination().GetS3Compatible().GetAccessKey())
	old := stored.GetSource().GetAzureBlobStorage().GetAzureStorageAccount()
	if cur := updated.GetSource().GetAzureBlobStorage().GetAzureStorageAccount(); old != nil && cur != nil && cur.GetAccessKey() == "" {
		cur.AccessKey = old.GetAccessKey()
	}
}

// keepAlphaSecrets: absent v1alpha1 credentials keep the stored ones; empty
// secrets of an unchanged form keep the stored secret.
func keepAlphaSecrets(stored, updated *storagev1alpha1pb.TransferSpec) {
	keep := func(old, cur *storagev1alpha1pb.TransferSpec_BucketCredentials) *storagev1alpha1pb.TransferSpec_BucketCredentials {
		if cur == nil {
			return old
		}
		if o, c := old.GetAccessKey(), cur.GetAccessKey(); o != nil && c != nil && c.GetSecretAccessKey() == "" {
			c.SecretAccessKey = o.GetSecretAccessKey()
		}
		if o, c := old.GetAzureAccessKey(), cur.GetAzureAccessKey(); o != nil && c != nil && c.GetAccessKey() == "" {
			c.AccessKey = o.GetAccessKey()
		}
		return cur
	}
	if src := updated.GetSource(); src != nil {
		src.Credentials = keep(stored.GetSource().GetCredentials(), src.GetCredentials())
	}
	if dst := updated.GetDestination(); dst != nil {
		dst.Credentials = keep(stored.GetDestination().GetCredentials(), dst.GetCredentials())
	}
}
