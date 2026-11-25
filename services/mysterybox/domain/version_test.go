package domain_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/domain"
)

func TestCreateVersionPrimary(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.newSecret(t, "db")
	if got := e.secret(t, id).GetMetadata().GetResourceVersion(); got != 1 {
		t.Fatalf("resource_version = %d, want 1", got)
	}

	v1 := e.newVersion(t, id, false, strEntry("k", "one"))
	if got := e.primary(t, id); got != v1 {
		t.Fatalf("first version primary = %q, want %q (auto-primary)", got, v1)
	}
	if got := e.secret(t, id).GetMetadata().GetResourceVersion(); got != 2 {
		t.Errorf("resource_version after auto-primary = %d, want 2", got)
	}

	v2 := e.newVersion(t, id, false, strEntry("k", "two"))
	if got := e.primary(t, id); got != v1 {
		t.Errorf("primary after second version = %q, want %q (unchanged)", got, v1)
	}
	if got := e.secret(t, id).GetMetadata().GetResourceVersion(); got != 2 {
		t.Errorf("resource_version after non-primary version = %d, want 2", got)
	}

	e.clk.Advance(time.Minute)
	v3 := e.newVersion(t, id, true, strEntry("k", "three"))
	if got := e.primary(t, id); got != v3 {
		t.Errorf("primary after set_primary = %q, want %q", got, v3)
	}
	s := e.secret(t, id)
	if s.GetMetadata().GetResourceVersion() != 3 || !s.GetMetadata().GetUpdatedAt().AsTime().Equal(t0.Add(time.Minute)) {
		t.Errorf("secret metadata = %v, want resource_version 3 updated at t0+1m", s.GetMetadata())
	}

	pl, err := e.svc.GetPayload(t.Context(), domain.PayloadRef{SecretID: id, VersionID: v2})
	if err != nil || pl.GetData()[0].GetStringValue() != "two" {
		t.Errorf("GetPayload(v2) = %v, %v; want two", pl, err)
	}
	v, err := e.svc.GetVersion(t.Context(), v3, false)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if v.GetSpec().GetSetPrimary() || len(v.GetSpec().GetPayload()) != 0 {
		t.Errorf("version spec = %v, want only the description", v.GetSpec())
	}
}

func TestCreateVersionRejects(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.newSecret(t, "db")
	gone := e.newSecret(t, "gone")
	done(t)(e.svc.DeleteSecret(t.Context(), gone, nil))
	good := []*mysteryboxpb.Payload{strEntry("k", "v")}
	tests := []struct {
		name string
		md   *commonpb.ResourceMetadata
		want errs.Kind
	}{
		{name: "id set", md: &commonpb.ResourceMetadata{Id: "mbsecver-x", ParentId: id}, want: errs.KindBadRequest},
		{name: "no secret", md: &commonpb.ResourceMetadata{}, want: errs.KindBadRequest},
		{name: "unknown secret", md: &commonpb.ResourceMetadata{ParentId: "mbsec-e00mock000000000999"}, want: errs.KindNotFound},
		{name: "deleted secret", md: &commonpb.ResourceMetadata{ParentId: gone}, want: errs.KindNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := e.svc.CreateVersion(t.Context(), domain.CreateVersionCmd{
				Metadata: tt.md, Spec: &mysteryboxpb.SecretVersionSpec{Payload: good},
			})
			if got := errs.KindOf(err); got != tt.want {
				t.Fatalf("CreateVersion kind = %v, want %v (err: %v)", got, tt.want, err)
			}
		})
	}
}

func TestVersionLimit(t *testing.T) {
	t.Parallel()
	e := newEnvLimits(t, domain.Limits{MaxVersionsPerSecret: 2})
	ctx := t.Context()
	id := e.newSecret(t, "db", strEntry("k", "1"))
	v2 := e.newVersion(t, id, false, strEntry("k", "2"))

	create := func() error {
		_, err := e.svc.CreateVersion(ctx, domain.CreateVersionCmd{
			Metadata: &commonpb.ResourceMetadata{ParentId: id},
			Spec:     &mysteryboxpb.SecretVersionSpec{Payload: []*mysteryboxpb.Payload{strEntry("k", "3")}},
		})
		return err
	}
	if got := errs.KindOf(create()); got != errs.KindQuotaExceeded {
		t.Fatalf("third version kind = %v, want QuotaExceeded", got)
	}
	// A version scheduled for deletion still counts.
	done(t)(e.svc.DeleteVersion(ctx, v2, nil))
	if got := errs.KindOf(create()); got != errs.KindQuotaExceeded {
		t.Fatalf("version with one soft-deleted kind = %v, want QuotaExceeded", got)
	}
	// A purged one does not.
	e.clk.Advance(testPurgeDelay)
	if err := create(); err != nil {
		t.Fatalf("version after purge: %v", err)
	}
	if len(e.versions.items) != 2 || e.payloads.versions() != 2 {
		t.Errorf("versions/payloads = %d/%d, want 2/2", len(e.versions.items), e.payloads.versions())
	}
}

func TestDeleteVersionLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	id := e.newSecret(t, "db", strEntry("k", "one"))
	v1 := e.primary(t, id)
	v2 := e.newVersion(t, id, false, strEntry("k", "two"))

	done(t)(e.svc.DeleteVersion(ctx, v2, nil))
	if _, err := e.svc.GetVersion(ctx, v2, false); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetVersion(deleted) kind = %v, want NotFound", errs.KindOf(err))
	}
	got, err := e.svc.GetVersion(ctx, v2, true)
	if err != nil {
		t.Fatalf("GetVersion(showDeleted): %v", err)
	}
	st := got.GetStatus()
	if st.GetState() != mysteryboxpb.SecretVersionStatus_SCHEDULED_FOR_DELETION ||
		!st.GetDeletedAt().AsTime().Equal(t0) || !st.GetPurgeAt().AsTime().Equal(t0.Add(testPurgeDelay)) {
		t.Errorf("status = %v, want SCHEDULED_FOR_DELETION at t0 purging at t0+%v", st, testPurgeDelay)
	}
	if _, err := e.svc.GetPayload(ctx, domain.PayloadRef{SecretID: id, VersionID: v2}); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetPayload(deleted version) kind = %v, want NotFound", errs.KindOf(err))
	}
	if _, err := e.svc.DeleteVersion(ctx, v2, nil); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("DeleteVersion(twice) kind = %v, want NotFound", errs.KindOf(err))
	}

	list := func(show bool) []string {
		t.Helper()
		page, err := e.svc.ListVersions(ctx, domain.ListVersionsQuery{SecretID: id, ShowDeleted: show})
		if err != nil {
			t.Fatalf("ListVersions(show=%v): %v", show, err)
		}
		var out []string
		for _, v := range page.Items {
			out = append(out, v.GetMetadata().GetId())
		}
		return out
	}
	if diff := cmp.Diff([]string{v1}, list(false)); diff != "" {
		t.Errorf("hidden listing (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{v1, v2}, list(true)); diff != "" {
		t.Errorf("show-deleted listing (-want +got):\n%s", diff)
	}

	e.clk.Advance(testPurgeDelay - time.Nanosecond)
	done(t)(e.svc.UndeleteVersion(ctx, v2, nil))
	got, err = e.svc.GetVersion(ctx, v2, false)
	if err != nil || got.GetStatus().GetState() != mysteryboxpb.SecretVersionStatus_ACTIVE ||
		got.GetStatus().GetDeletedAt() != nil || got.GetStatus().GetPurgeAt() != nil {
		t.Fatalf("GetVersion after undelete = %v, %v; want ACTIVE without times", got, err)
	}
	pl, err := e.svc.GetPayload(ctx, domain.PayloadRef{SecretID: id, VersionID: v2})
	if err != nil || pl.GetData()[0].GetStringValue() != "two" {
		t.Errorf("GetPayload after undelete = %v, %v; want two", pl, err)
	}
	if _, err := e.svc.UndeleteVersion(ctx, v2, nil); errs.KindOf(err) != errs.KindBadState {
		t.Errorf("UndeleteVersion(active) kind = %v, want BadState", errs.KindOf(err))
	}
}

func TestPurgeVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	id := e.newSecret(t, "db", strEntry("k", "one"))
	v2 := e.newVersion(t, id, false, strEntry("k", "two"))
	done(t)(e.svc.DeleteVersion(ctx, v2, nil))

	e.clk.Advance(testPurgeDelay - time.Nanosecond)
	if _, err := e.svc.GetVersion(ctx, v2, true); err != nil {
		t.Fatalf("GetVersion just before purge_at: %v", err)
	}
	e.clk.Advance(time.Nanosecond)
	if _, err := e.svc.UndeleteVersion(ctx, v2, nil); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("UndeleteVersion(purged) kind = %v, want NotFound", errs.KindOf(err))
	}
	if len(e.versions.items) != 1 || e.payloads.versions() != 1 {
		t.Errorf("versions/payloads = %d/%d, want 1/1", len(e.versions.items), e.payloads.versions())
	}
	page, err := e.svc.ListVersions(ctx, domain.ListVersionsQuery{SecretID: id, ShowDeleted: true})
	if err != nil || len(page.Items) != 1 {
		t.Errorf("ListVersions = %d items, %v; want 1", len(page.Items), err)
	}
}

func TestDeletePrimaryVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	id := e.newSecret(t, "db", strEntry("k", "one"))
	v1 := e.primary(t, id)
	done(t)(e.svc.DeleteVersion(ctx, v1, nil))
	if _, err := e.svc.GetPayload(ctx, domain.PayloadRef{SecretID: id}); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("GetPayload(deleted primary) kind = %v, want NotFound (err: %v)", errs.KindOf(err), err)
	}
	if got := e.primary(t, id); got != v1 {
		t.Errorf("primary = %q, want %q (kept)", got, v1)
	}
	done(t)(e.svc.UndeleteVersion(ctx, v1, nil))
	if _, err := e.svc.GetPayload(ctx, domain.PayloadRef{SecretID: id}); err != nil {
		t.Errorf("GetPayload after undelete: %v", err)
	}
}

func TestUndeleteVersionOfDeletedSecret(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	id := e.newSecret(t, "db", strEntry("k", "one"))
	v2 := e.newVersion(t, id, false, strEntry("k", "two"))
	done(t)(e.svc.DeleteVersion(ctx, v2, nil))
	done(t)(e.svc.DeleteSecret(ctx, id, nil))
	if _, err := e.svc.UndeleteVersion(ctx, v2, nil); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("UndeleteVersion(secret deleted) kind = %v, want NotFound (err: %v)", errs.KindOf(err), err)
	}
	// Versions of a deleted secret stay listable with show_deleted.
	if _, err := e.svc.ListVersions(ctx, domain.ListVersionsQuery{SecretID: id, ShowDeleted: true}); err != nil {
		t.Errorf("ListVersions(deleted secret): %v", err)
	}
	if _, err := e.svc.ListVersions(ctx, domain.ListVersionsQuery{SecretID: "mbsec-e00mock000000000999"}); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("ListVersions(unknown secret) kind = %v, want NotFound", errs.KindOf(err))
	}
}
