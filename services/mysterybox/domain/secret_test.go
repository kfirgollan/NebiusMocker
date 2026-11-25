package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/domain"
)

func TestCreateSecretInlineVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.newSecret(t, "db", strEntry("password", "hunter2"))

	got, err := e.svc.GetSecret(t.Context(), id, false)
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if got.GetStatus().GetState() != mysteryboxpb.SecretStatus_ACTIVE {
		t.Errorf("state = %v, want ACTIVE", got.GetStatus().GetState())
	}
	if got.GetStatus().GetEffectiveKmsKeyId() != fakeKeyID {
		t.Errorf("effective_kms_key_id = %q, want %q", got.GetStatus().GetEffectiveKmsKeyId(), fakeKeyID)
	}
	if got.GetSpec().GetSecretVersion() != nil {
		t.Errorf("Get returned spec.secret_version, want it absent: %v", got.GetSpec().GetSecretVersion())
	}
	if got.GetMetadata().GetResourceVersion() != 1 || got.GetSpec().GetDescription() != "desc" {
		t.Errorf("metadata/spec = %v / %v, want resource_version 1 and description desc", got.GetMetadata(), got.GetSpec())
	}
	vid := got.GetSpec().GetPrimaryVersionId()
	if vid == "" {
		t.Fatal("primary_version_id is empty, want the inline version")
	}
	v, err := e.svc.GetVersion(t.Context(), vid, false)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	wantSpec := &mysteryboxpb.SecretVersionSpec{Description: "first"}
	if diff := cmp.Diff(wantSpec, v.GetSpec(), protocmp.Transform()); diff != "" {
		t.Errorf("version spec mismatch (-want +got):\n%s", diff)
	}
	if v.GetMetadata().GetParentId() != id || v.GetStatus().GetState() != mysteryboxpb.SecretVersionStatus_ACTIVE {
		t.Errorf("version = %v, want active child of %s", v, id)
	}
}

func TestCreateSecretRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  domain.CreateSecretCmd
		want errs.Kind
	}{
		{
			name: "id set",
			cmd: domain.CreateSecretCmd{
				Metadata: &commonpb.ResourceMetadata{Id: "mbsec-x", ParentId: projectID, Name: "a"},
				Spec:     &mysteryboxpb.SecretSpec{},
			},
			want: errs.KindBadRequest,
		},
		{
			name: "primary version set",
			cmd: domain.CreateSecretCmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "a"},
				Spec:     &mysteryboxpb.SecretSpec{PrimaryVersionId: proto.String("mbsecver-x")},
			},
			want: errs.KindBadRequest,
		},
		{
			name: "missing parent",
			cmd: domain.CreateSecretCmd{
				Metadata: &commonpb.ResourceMetadata{Name: "a"}, Spec: &mysteryboxpb.SecretSpec{},
			},
			want: errs.KindBadRequest,
		},
		{
			name: "parent is not a project",
			cmd: domain.CreateSecretCmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: "tenant-e00mock000000000default", Name: "a"},
				Spec:     &mysteryboxpb.SecretSpec{},
			},
			want: errs.KindBadRequest,
		},
		{
			name: "unknown project",
			cmd: domain.CreateSecretCmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: "project-e00mock000000000other", Name: "a"},
				Spec:     &mysteryboxpb.SecretSpec{},
			},
			want: errs.KindNotFound,
		},
		{
			name: "invalid inline payload",
			cmd: domain.CreateSecretCmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "a"},
				Spec:     &mysteryboxpb.SecretSpec{SecretVersion: &mysteryboxpb.SecretVersionSpec{}},
			},
			want: errs.KindBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			_, err := e.svc.CreateSecret(t.Context(), tt.cmd)
			if got := errs.KindOf(err); got != tt.want {
				t.Fatalf("CreateSecret kind = %v, want %v (err: %v)", got, tt.want, err)
			}
			if len(e.secrets.items) != 0 {
				t.Errorf("secrets stored = %d, want 0", len(e.secrets.items))
			}
		})
	}
}

func TestCreateSecretDuplicateName(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.newSecret(t, "db")
	_, err := e.svc.CreateSecret(t.Context(), domain.CreateSecretCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "db"}, Spec: &mysteryboxpb.SecretSpec{},
	})
	if got := errs.KindOf(err); got != errs.KindAlreadyExists {
		t.Fatalf("CreateSecret duplicate kind = %v, want AlreadyExists (err: %v)", got, err)
	}
}

func TestCreateSecretNoVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.newSecret(t, "empty")
	if got := e.primary(t, id); got != "" {
		t.Errorf("primary_version_id = %q, want empty", got)
	}
	if _, err := e.svc.GetPayload(t.Context(), domain.PayloadRef{SecretID: id}); errs.KindOf(err) != errs.KindBadState {
		t.Errorf("GetPayload without primary kind = %v, want BadState (err: %v)", errs.KindOf(err), err)
	}
}

func TestUpdateSecret(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		cmd         func(id string) domain.UpdateSecretCmd
		wantVersion int64
		check       func(t *testing.T, s *mysteryboxpb.Secret)
	}{
		{
			name: "description",
			cmd: func(id string) domain.UpdateSecretCmd {
				return domain.UpdateSecretCmd{
					Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: &mysteryboxpb.SecretSpec{Description: "new"},
				}
			},
			wantVersion: 2,
			check: func(t *testing.T, s *mysteryboxpb.Secret) {
				t.Helper()
				if s.GetSpec().GetDescription() != "new" {
					t.Errorf("description = %q, want new", s.GetSpec().GetDescription())
				}
			},
		},
		{
			name: "name",
			cmd: func(id string) domain.UpdateSecretCmd {
				return domain.UpdateSecretCmd{Metadata: &commonpb.ResourceMetadata{Id: id, Name: "renamed"}, Spec: &mysteryboxpb.SecretSpec{}}
			},
			wantVersion: 2,
			check: func(t *testing.T, s *mysteryboxpb.Secret) {
				t.Helper()
				if s.GetMetadata().GetName() != "renamed" {
					t.Errorf("name = %q, want renamed", s.GetMetadata().GetName())
				}
			},
		},
		{
			name: "labels",
			cmd: func(id string) domain.UpdateSecretCmd {
				return domain.UpdateSecretCmd{
					Metadata: &commonpb.ResourceMetadata{Id: id, Labels: map[string]string{"env": "prod"}},
					Spec:     &mysteryboxpb.SecretSpec{},
				}
			},
			wantVersion: 2,
			check: func(t *testing.T, s *mysteryboxpb.Secret) {
				t.Helper()
				if got := s.GetMetadata().GetLabels()["env"]; got != "prod" {
					t.Errorf("labels[env] = %q, want prod", got)
				}
			},
		},
		{
			// Terraform resends the unchanged inline version on every update.
			name: "identical secret_version in request is accepted",
			cmd: func(id string) domain.UpdateSecretCmd {
				return domain.UpdateSecretCmd{
					Metadata: &commonpb.ResourceMetadata{Id: id},
					Spec: &mysteryboxpb.SecretSpec{
						Description: "desc",
						SecretVersion: &mysteryboxpb.SecretVersionSpec{
							Description: "first", Payload: []*mysteryboxpb.Payload{strEntry("k", "v")}, SetPrimary: true,
						},
					},
				}
			},
			wantVersion: 1,
			check: func(t *testing.T, s *mysteryboxpb.Secret) {
				t.Helper()
				if s.GetSpec().GetSecretVersion() != nil {
					t.Errorf("spec.secret_version = %v, want nil", s.GetSpec().GetSecretVersion())
				}
			},
		},
		{
			name: "no-op keeps resource_version",
			cmd: func(id string) domain.UpdateSecretCmd {
				return domain.UpdateSecretCmd{
					Metadata: &commonpb.ResourceMetadata{Id: id, Name: "db", ResourceVersion: 1},
					Spec:     &mysteryboxpb.SecretSpec{Description: "desc"},
				}
			},
			wantVersion: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			id := e.newSecret(t, "db", strEntry("k", "v"))
			before := e.secret(t, id)
			e.clk.Advance(time.Hour)
			done(t)(e.svc.UpdateSecret(t.Context(), tt.cmd(id)))
			got := e.secret(t, id)
			if got.GetMetadata().GetResourceVersion() != tt.wantVersion {
				t.Errorf("resource_version = %d, want %d", got.GetMetadata().GetResourceVersion(), tt.wantVersion)
			}
			if tt.wantVersion == 1 && !proto.Equal(got, before) {
				t.Errorf("no-op update changed the secret:\n%v\nvs\n%v", before, got)
			}
			if tt.wantVersion == 2 && !got.GetMetadata().GetUpdatedAt().AsTime().Equal(t0.Add(time.Hour)) {
				t.Errorf("updated_at = %v, want %v", got.GetMetadata().GetUpdatedAt().AsTime(), t0.Add(time.Hour))
			}
			if got.GetSpec().GetPrimaryVersionId() != before.GetSpec().GetPrimaryVersionId() {
				t.Errorf("primary changed to %q", got.GetSpec().GetPrimaryVersionId())
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestUpdateSecretPrimary(t *testing.T) {
	t.Parallel()
	// pick selects the requested primary from the versions of a fresh fixture.
	type fixture struct{ v1, v2, foreign, deleted string }
	tests := []struct {
		name string
		pick func(f fixture) string
		want errs.Kind
	}{
		{name: "another active version", pick: func(f fixture) string { return f.v2 }},
		{name: "same primary is a no-op", pick: func(f fixture) string { return f.v1 }},
		{name: "version of another secret", pick: func(f fixture) string { return f.foreign }, want: errs.KindBadRequest},
		{name: "unknown version", pick: func(fixture) string { return "mbsecver-e00mock000000000999" }, want: errs.KindBadRequest},
		{name: "soft-deleted version", pick: func(f fixture) string { return f.deleted }, want: errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			id := e.newSecret(t, "db", strEntry("k", "one"))
			f := fixture{
				v1:      e.primary(t, id),
				v2:      e.newVersion(t, id, false, strEntry("k", "two")),
				foreign: e.primary(t, e.newSecret(t, "other", strEntry("k", "x"))),
				deleted: e.newVersion(t, id, false, strEntry("k", "gone")),
			}
			done(t)(e.svc.DeleteVersion(t.Context(), f.deleted, nil))
			want := tt.pick(f)

			_, err := e.svc.UpdateSecret(t.Context(), domain.UpdateSecretCmd{
				Metadata: &commonpb.ResourceMetadata{Id: id},
				Spec:     &mysteryboxpb.SecretSpec{PrimaryVersionId: proto.String(want)},
			})
			if got := errs.KindOf(err); got != tt.want {
				t.Fatalf("UpdateSecret(primary=%s) kind = %v, want %v (err: %v)", want, got, tt.want, err)
			}
			wantPrimary := f.v1
			if tt.want == errs.KindUnknown {
				wantPrimary = want
			}
			if got := e.primary(t, id); got != wantPrimary {
				t.Errorf("primary = %q, want %q", got, wantPrimary)
			}
		})
	}
}

func TestUpdateSecretRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		md   func(id string) *commonpb.ResourceMetadata
		want errs.Kind
	}{
		{name: "parent change", md: func(id string) *commonpb.ResourceMetadata {
			return &commonpb.ResourceMetadata{Id: id, ParentId: "project-e00mock000000000other"}
		}, want: errs.KindBadRequest},
		{name: "stale resource_version", md: func(id string) *commonpb.ResourceMetadata {
			return &commonpb.ResourceMetadata{Id: id, ResourceVersion: 7}
		}, want: errs.KindConflict},
		{name: "missing id", md: func(string) *commonpb.ResourceMetadata {
			return &commonpb.ResourceMetadata{}
		}, want: errs.KindBadRequest},
		{name: "unknown secret", md: func(string) *commonpb.ResourceMetadata {
			return &commonpb.ResourceMetadata{Id: "mbsec-e00mock000000000999"}
		}, want: errs.KindNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			id := e.newSecret(t, "db", strEntry("k", "v"))
			_, err := e.svc.UpdateSecret(t.Context(), domain.UpdateSecretCmd{Metadata: tt.md(id), Spec: &mysteryboxpb.SecretSpec{Description: "x"}})
			if got := errs.KindOf(err); got != tt.want {
				t.Fatalf("UpdateSecret kind = %v, want %v (err: %v)", got, tt.want, err)
			}
		})
	}
}

func TestDeleteSecretLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	id := e.newSecret(t, "db", strEntry("k", "v"))
	vid := e.primary(t, id)
	done(t)(e.svc.DeleteSecret(ctx, id, nil))

	if _, err := e.svc.GetSecret(ctx, id, false); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetSecret(deleted) kind = %v, want NotFound", errs.KindOf(err))
	}
	got, err := e.svc.GetSecret(ctx, id, true)
	if err != nil {
		t.Fatalf("GetSecret(showDeleted): %v", err)
	}
	st := got.GetStatus()
	if st.GetState() != mysteryboxpb.SecretStatus_SCHEDULED_FOR_DELETION {
		t.Errorf("state = %v, want SCHEDULED_FOR_DELETION", st.GetState())
	}
	if !st.GetDeletedAt().AsTime().Equal(t0) || !st.GetPurgeAt().AsTime().Equal(t0.Add(testPurgeDelay)) {
		t.Errorf("deleted_at/purge_at = %v / %v, want %v / %v", st.GetDeletedAt().AsTime(), st.GetPurgeAt().AsTime(), t0, t0.Add(testPurgeDelay))
	}
	if _, err := e.svc.GetPayload(ctx, domain.PayloadRef{SecretID: id, VersionID: vid}); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetPayload(deleted secret) kind = %v, want NotFound", errs.KindOf(err))
	}
	if _, err := e.svc.GetSecretByName(ctx, projectID, "db"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetSecretByName(released name) kind = %v, want NotFound", errs.KindOf(err))
	}
	if _, err := e.svc.CreateVersion(ctx, domain.CreateVersionCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: id},
		Spec:     &mysteryboxpb.SecretVersionSpec{Payload: []*mysteryboxpb.Payload{strEntry("k", "v")}},
	}); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("CreateVersion(deleted secret) kind = %v, want NotFound", errs.KindOf(err))
	}
	if _, err := e.svc.DeleteSecret(ctx, id, nil); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("DeleteSecret(twice) kind = %v, want NotFound", errs.KindOf(err))
	}

	// The name is free: another secret takes it and is found by name.
	other := e.newSecret(t, "db", strEntry("k", "v2"))
	byName, err := e.svc.GetSecretByName(ctx, projectID, "db")
	if err != nil || byName.GetMetadata().GetId() != other {
		t.Fatalf("GetSecretByName = %v, %v; want secret %s", byName.GetMetadata().GetId(), err, other)
	}
}

func TestUndeleteSecret(t *testing.T) {
	t.Parallel()
	t.Run("restores", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.newSecret(t, "db", strEntry("k", "v"))
		done(t)(e.svc.DeleteSecret(t.Context(), id, nil))
		e.clk.Advance(testPurgeDelay - time.Nanosecond)
		done(t)(e.svc.UndeleteSecret(t.Context(), id, "", nil))
		got := e.secret(t, id)
		st := got.GetStatus()
		if st.GetState() != mysteryboxpb.SecretStatus_ACTIVE || st.GetDeletedAt() != nil || st.GetPurgeAt() != nil {
			t.Errorf("status = %v, want ACTIVE without times", st)
		}
		if got.GetMetadata().GetName() != "db" {
			t.Errorf("name = %q, want db", got.GetMetadata().GetName())
		}
		pl, err := e.svc.GetPayload(t.Context(), domain.PayloadRef{SecretID: id})
		if err != nil || pl.GetData()[0].GetStringValue() != "v" {
			t.Errorf("GetPayload after undelete = %v, %v; want v", pl, err)
		}
	})
	t.Run("name taken", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.newSecret(t, "db", strEntry("k", "v"))
		done(t)(e.svc.DeleteSecret(t.Context(), id, nil))
		e.newSecret(t, "db", strEntry("k", "v2"))
		_, err := e.svc.UndeleteSecret(t.Context(), id, "", nil)
		if got := errs.KindOf(err); got != errs.KindAlreadyExists {
			t.Fatalf("UndeleteSecret kind = %v, want AlreadyExists (err: %v)", got, err)
		}
		done(t)(e.svc.UndeleteSecret(t.Context(), id, "db-restored", nil))
		if got := e.secret(t, id).GetMetadata().GetName(); got != "db-restored" {
			t.Errorf("name = %q, want db-restored", got)
		}
	})
	t.Run("active secret", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.newSecret(t, "db")
		_, err := e.svc.UndeleteSecret(t.Context(), id, "", nil)
		if got := errs.KindOf(err); got != errs.KindBadState {
			t.Fatalf("UndeleteSecret(active) kind = %v, want BadState (err: %v)", got, err)
		}
	})
	t.Run("purged", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.newSecret(t, "db", strEntry("k", "v"))
		done(t)(e.svc.DeleteSecret(t.Context(), id, nil))
		e.clk.Advance(testPurgeDelay)
		_, err := e.svc.UndeleteSecret(t.Context(), id, "", nil)
		if got := errs.KindOf(err); got != errs.KindNotFound {
			t.Fatalf("UndeleteSecret(purged) kind = %v, want NotFound (err: %v)", got, err)
		}
	})
}

func TestPurgeSecret(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	id := e.newSecret(t, "db", strEntry("k", "v"))
	e.newVersion(t, id, false, strEntry("k", "v2"))
	done(t)(e.svc.DeleteSecret(ctx, id, nil))

	e.clk.Advance(testPurgeDelay - time.Nanosecond)
	if _, err := e.svc.GetSecret(ctx, id, true); err != nil {
		t.Fatalf("GetSecret just before purge_at: %v", err)
	}
	if len(e.versions.items) != 2 || e.payloads.versions() != 2 || !e.payloads.hasInline(id) {
		t.Fatalf("versions/payloads/inline before purge = %d/%d/%v, want 2/2/true",
			len(e.versions.items), e.payloads.versions(), e.payloads.hasInline(id))
	}

	e.clk.Advance(time.Nanosecond)
	if _, err := e.svc.GetSecret(ctx, id, true); errs.KindOf(err) != errs.KindNotFound {
		t.Fatalf("GetSecret at purge_at kind = %v, want NotFound (err: %v)", errs.KindOf(err), err)
	}
	if len(e.secrets.items) != 0 || len(e.versions.items) != 0 || len(e.payloads.sealed) != 0 {
		t.Errorf("after purge secrets/versions/payloads = %d/%d/%d, want 0/0/0",
			len(e.secrets.items), len(e.versions.items), len(e.payloads.sealed))
	}
}

func TestListSecrets(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	keep := e.newSecret(t, "keep")
	gone := e.newSecret(t, "gone")
	purged := e.newSecret(t, "purged", strEntry("k", "v"))
	done(t)(e.svc.DeleteSecret(ctx, purged, nil))
	e.clk.Advance(time.Hour)
	done(t)(e.svc.DeleteSecret(ctx, gone, nil))
	e.clk.Advance(testPurgeDelay - time.Hour) // purged is due, gone is not

	ids := func(items []*mysteryboxpb.Secret) []string {
		var out []string
		for _, s := range items {
			out = append(out, s.GetMetadata().GetId())
		}
		return out
	}
	page, err := e.svc.ListSecrets(ctx, domain.ListSecretsQuery{ParentID: projectID})
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if diff := cmp.Diff([]string{keep}, ids(page.Items)); diff != "" {
		t.Errorf("hidden listing (-want +got):\n%s", diff)
	}
	page, err = e.svc.ListSecrets(ctx, domain.ListSecretsQuery{ParentID: projectID, ShowDeleted: true})
	if err != nil {
		t.Fatalf("ListSecrets(showDeleted): %v", err)
	}
	if diff := cmp.Diff([]string{keep, gone}, ids(page.Items)); diff != "" {
		t.Errorf("show-deleted listing (-want +got):\n%s", diff)
	}
	if len(e.versions.items) != 0 {
		t.Errorf("versions after listing purge = %d, want 0", len(e.versions.items))
	}
	if _, err := e.svc.ListSecrets(ctx, domain.ListSecretsQuery{ParentID: projectID, PageSize: -1}); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("ListSecrets(page_size -1) kind = %v, want BadRequest", errs.KindOf(err))
	}
}

func TestCreateSystemSecret(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	got, err := e.svc.CreateSystemSecret(ctx, domain.SystemSecretCmd{
		ParentID: projectID, Name: "access-key", Description: "generated", Labels: map[string]string{"owner": "iam"},
		Payload: []*mysteryboxpb.Payload{strEntry("secret", "s3cr3t")},
	})
	if err != nil {
		t.Fatalf("CreateSystemSecret: %v", err)
	}
	vid := got.GetSpec().GetPrimaryVersionId()
	if vid == "" || got.GetMetadata().GetName() != "access-key" || got.GetMetadata().GetLabels()["owner"] != "iam" {
		t.Fatalf("secret = %v, want named access-key with a primary version", got)
	}
	pl, err := e.svc.GetPayload(ctx, domain.PayloadRef{SecretID: got.GetMetadata().GetId()})
	if err != nil {
		t.Fatalf("GetPayload: %v", err)
	}
	want := &mysteryboxpb.SecretPayload{VersionId: vid, Data: []*mysteryboxpb.Payload{strEntry("secret", "s3cr3t")}}
	if diff := cmp.Diff(want, pl, protocmp.Transform()); diff != "" {
		t.Errorf("payload mismatch (-want +got):\n%s", diff)
	}

	if _, err := e.svc.CreateSystemSecret(ctx, domain.SystemSecretCmd{ParentID: projectID, Name: "x"}); errs.KindOf(err) != errs.KindBadRequest {
		t.Errorf("CreateSystemSecret(no payload) kind = %v, want BadRequest", errs.KindOf(err))
	}
	if _, err := e.svc.CreateSystemSecret(ctx, domain.SystemSecretCmd{
		ParentID: "project-e00mock000000000other", Name: "x", Payload: []*mysteryboxpb.Payload{strEntry("k", "v")},
	}); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("CreateSystemSecret(unknown project) kind = %v, want NotFound", errs.KindOf(err))
	}
}

// TestDiscardSystemSecret: a discarded secret is gone for good (no soft
// delete, name released), and discarding again is not an error.
func TestDiscardSystemSecret(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	cmd := domain.SystemSecretCmd{ParentID: projectID, Name: "access-key", Payload: []*mysteryboxpb.Payload{strEntry("secret", "s")}}
	sec, err := e.svc.CreateSystemSecret(ctx, cmd)
	if err != nil {
		t.Fatalf("CreateSystemSecret: %v", err)
	}
	id := sec.GetMetadata().GetId()
	if err := e.svc.DiscardSystemSecret(ctx, id); err != nil {
		t.Fatalf("DiscardSystemSecret: %v", err)
	}
	if _, err := e.svc.GetSecret(ctx, id, true); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("GetSecret(showDeleted) after discard kind = %v, want NotFound", errs.KindOf(err))
	}
	if _, err := e.svc.ResolvePayload(ctx, domain.PayloadRef{VersionID: sec.GetSpec().GetPrimaryVersionId()}); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("ResolvePayload(version) after discard kind = %v, want NotFound", errs.KindOf(err))
	}
	if err := e.svc.DiscardSystemSecret(ctx, id); err != nil {
		t.Errorf("second DiscardSystemSecret: %v", err)
	}
	if _, err := e.svc.CreateSystemSecret(ctx, cmd); err != nil {
		t.Errorf("CreateSystemSecret with the released name: %v", err)
	}
}

// TestUpdateSecretInlineVersionImmutable: spec.secret_version may be resent
// unchanged, but never changed, added, or changed after its version is purged.
func TestUpdateSecretInlineVersionImmutable(t *testing.T) {
	t.Parallel()
	update := func(t *testing.T, e *env, id string, v *mysteryboxpb.SecretVersionSpec) error {
		t.Helper()
		op, err := e.svc.UpdateSecret(t.Context(), domain.UpdateSecretCmd{
			Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: &mysteryboxpb.SecretSpec{SecretVersion: v},
		})
		if err == nil && op.Err != nil {
			err = op.Err
		}
		return err
	}
	tests := []struct {
		name string
		spec *mysteryboxpb.SecretVersionSpec
		want errs.Kind
	}{
		{"same", &mysteryboxpb.SecretVersionSpec{Description: "first", Payload: []*mysteryboxpb.Payload{strEntry("k", "v")}}, errs.KindUnknown},
		{"same, set_primary differs", &mysteryboxpb.SecretVersionSpec{Description: "first", Payload: []*mysteryboxpb.Payload{strEntry("k", "v")}, SetPrimary: true}, errs.KindUnknown},
		{"payload omitted (write-only)", &mysteryboxpb.SecretVersionSpec{Description: "first"}, errs.KindUnknown},
		{"payload value changed", &mysteryboxpb.SecretVersionSpec{Description: "first", Payload: []*mysteryboxpb.Payload{strEntry("k", "other")}}, errs.KindBadRequest},
		{"payload entry added", &mysteryboxpb.SecretVersionSpec{Description: "first", Payload: []*mysteryboxpb.Payload{strEntry("k", "v"), strEntry("k2", "v")}}, errs.KindBadRequest},
		{"string became binary", &mysteryboxpb.SecretVersionSpec{Description: "first", Payload: []*mysteryboxpb.Payload{binEntry("k", []byte("v"))}}, errs.KindBadRequest},
		{"description changed", &mysteryboxpb.SecretVersionSpec{Description: "second", Payload: []*mysteryboxpb.Payload{strEntry("k", "v")}}, errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			id := e.newSecret(t, "db", strEntry("k", "v"))
			err := update(t, e, id, tt.spec)
			if got := errs.KindOf(err); got != tt.want {
				t.Fatalf("UpdateSecret kind = %v, want %v (err: %v)", got, tt.want, err)
			}
			if err != nil && (strings.Contains(err.Error(), "other") || strings.Contains(err.Error(), `"v"`)) {
				t.Errorf("error leaks a payload value: %v", err)
			}
		})
	}

	t.Run("not set on create", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.newSecret(t, "bare")
		spec := &mysteryboxpb.SecretVersionSpec{Payload: []*mysteryboxpb.Payload{strEntry("k", "v")}}
		if got := errs.KindOf(update(t, e, id, spec)); got != errs.KindBadRequest {
			t.Fatalf("adding secret_version on update kind = %v, want BadRequest", got)
		}
	})

	t.Run("still enforced after the inline version is purged", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.newSecret(t, "db", strEntry("k", "v"))
		first := e.primary(t, id)
		e.newVersion(t, id, true, strEntry("k", "v2"))
		done(t)(e.svc.DeleteVersion(t.Context(), first, nil))
		e.clk.Advance(testPurgeDelay)
		if _, err := e.svc.GetVersion(t.Context(), first, true); errs.KindOf(err) != errs.KindNotFound {
			t.Fatalf("inline version not purged: %v", err)
		}
		same := &mysteryboxpb.SecretVersionSpec{Description: "first", Payload: []*mysteryboxpb.Payload{strEntry("k", "v")}}
		if err := update(t, e, id, same); err != nil {
			t.Fatalf("unchanged secret_version after purge: %v", err)
		}
		changed := &mysteryboxpb.SecretVersionSpec{Description: "first", Payload: []*mysteryboxpb.Payload{strEntry("k", "x")}}
		if got := errs.KindOf(update(t, e, id, changed)); got != errs.KindBadRequest {
			t.Fatalf("changed secret_version after purge kind = %v, want BadRequest", got)
		}
	})
}
