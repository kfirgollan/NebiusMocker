package domain

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func symSpec() *kmspb.SymmetricKeySpec {
	return &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256}
}

func createSym(t *testing.T, e *symEnv, name string, spec *kmspb.SymmetricKeySpec) string {
	t.Helper()
	op, err := e.svc.Create(t.Context(), CreateSymmetricKeyCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: name},
		Spec:     spec,
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return op.ResourceID
}

func TestSymmetricCreate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		md       *commonpb.ResourceMetadata
		spec     *kmspb.SymmetricKeySpec
		wantKind errs.Kind
	}{
		{name: "ok", md: &commonpb.ResourceMetadata{ParentId: testProject, Name: "k"}, spec: symSpec()},
		{
			name: "aes128", md: &commonpb.ResourceMetadata{ParentId: testProject, Name: "k"},
			spec: &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_128}, wantKind: errs.KindBadRequest,
		},
		{
			name: "unspecified", md: &commonpb.ResourceMetadata{ParentId: testProject, Name: "k"},
			spec: &kmspb.SymmetricKeySpec{}, wantKind: errs.KindBadRequest,
		},
		{name: "nil spec", md: &commonpb.ResourceMetadata{ParentId: testProject, Name: "k"}, wantKind: errs.KindBadRequest},
		{name: "bad parent kind", md: &commonpb.ResourceMetadata{ParentId: testTenant, Name: "k"}, spec: symSpec(), wantKind: errs.KindBadRequest},
		{name: "empty parent", md: &commonpb.ResourceMetadata{Name: "k"}, spec: symSpec(), wantKind: errs.KindBadRequest},
		{name: "unknown project", md: &commonpb.ResourceMetadata{ParentId: "project-nope", Name: "k"}, spec: symSpec(), wantKind: errs.KindNotFound},
		{name: "id set", md: &commonpb.ResourceMetadata{Id: "kmssymkey-e00x", ParentId: testProject, Name: "k"}, spec: symSpec(), wantKind: errs.KindBadRequest},
		{
			name: "rotation too short", md: &commonpb.ResourceMetadata{ParentId: testProject, Name: "k"},
			spec: &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256, RotationPeriod: durationpb.New(time.Hour)}, wantKind: errs.KindBadRequest,
		},
		{
			name: "rotation too long", md: &commonpb.ResourceMetadata{ParentId: testProject, Name: "k"},
			spec: &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256, RotationPeriod: durationpb.New(MaxRotationPeriod + time.Hour)}, wantKind: errs.KindBadRequest,
		},
		{
			name: "rotation at min", md: &commonpb.ResourceMetadata{ParentId: testProject, Name: "k"},
			spec: &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256, RotationPeriod: durationpb.New(MinRotationPeriod)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newSymEnv(t)
			op, err := e.svc.Create(t.Context(), CreateSymmetricKeyCmd{Metadata: tt.md, Spec: tt.spec})
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Create kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if err != nil {
				return
			}
			k, err := e.svc.Get(t.Context(), op.ResourceID, ActiveOnly)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			md := k.GetMetadata()
			if md.GetResourceVersion() != 1 || !md.GetCreatedAt().AsTime().Equal(t0) || !md.GetUpdatedAt().AsTime().Equal(t0) {
				t.Errorf("metadata = %v, want version 1 and created/updated at %v", md, t0)
			}
			if k.GetStatus().GetState() != kmspb.KeyState_ACTIVE {
				t.Errorf("state = %v, want ACTIVE", k.GetStatus().GetState())
			}
		})
	}
}

func TestSymmetricCreateDefaultRotation(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	id := createSym(t, e, "k", symSpec())
	k, err := e.svc.Get(t.Context(), id, ActiveOnly)
	if err != nil {
		t.Fatal(err)
	}
	if got := k.GetSpec().GetRotationPeriod().AsDuration(); got != 90*24*time.Hour {
		t.Errorf("rotation period = %v, want 90d", got)
	}
}

func TestSymmetricCreateDuplicateName(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	createSym(t, e, "dup", symSpec())
	_, err := e.svc.Create(t.Context(), CreateSymmetricKeyCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: testProject, Name: "dup"}, Spec: symSpec(),
	})
	if !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("duplicate Create err = %v, want AlreadyExists", err)
	}
}

func TestSymmetricGetByNameAndList(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	a := createSym(t, e, "a", symSpec())
	b := createSym(t, e, "b", symSpec())
	got, err := e.svc.GetByName(t.Context(), testProject, "b")
	if err != nil || got.GetMetadata().GetId() != b {
		t.Fatalf("GetByName(b) = %v, %v, want %s", got, err, b)
	}
	page, err := e.svc.List(t.Context(), testProject, 1, "", ActiveOnly)
	if err != nil || len(page.Items) != 1 || page.Items[0].GetMetadata().GetId() != a || page.NextPageToken == "" {
		t.Fatalf("List page 1 = %v, %v", page, err)
	}
	page, err = e.svc.List(t.Context(), testProject, 1, page.NextPageToken, ActiveOnly)
	if err != nil || len(page.Items) != 1 || page.Items[0].GetMetadata().GetId() != b || page.NextPageToken != "" {
		t.Fatalf("List page 2 = %v, %v", page, err)
	}
	if _, err := e.svc.List(t.Context(), testProject, -1, "", ActiveOnly); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("List(-1) err = %v, want BadRequest", err)
	}
	if _, err := e.svc.List(t.Context(), testProject, 1001, "", ActiveOnly); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("List(1001) err = %v, want BadRequest", err)
	}
}

func TestSymmetricUpdate(t *testing.T) {
	t.Parallel()
	update := func(t *testing.T, e *symEnv, cmd UpdateSymmetricKeyCmd) error {
		t.Helper()
		_, err := e.svc.Update(t.Context(), cmd)
		return err
	}
	md := func(id string) *commonpb.ResourceMetadata {
		return &commonpb.ResourceMetadata{Id: id, ParentId: testProject, Name: "k"}
	}

	tests := []struct {
		name        string
		cmd         func(id string) UpdateSymmetricKeyCmd
		wantKind    errs.Kind
		wantVersion int64
		check       func(t *testing.T, k *kmspb.SymmetricKey)
	}{
		{
			name: "labels", wantVersion: 2,
			cmd: func(id string) UpdateSymmetricKeyCmd {
				m := md(id)
				m.Labels = map[string]string{"a": "b"}
				return UpdateSymmetricKeyCmd{Metadata: m}
			},
			check: func(t *testing.T, k *kmspb.SymmetricKey) {
				t.Helper()
				if k.GetMetadata().GetLabels()["a"] != "b" {
					t.Errorf("labels = %v, want a=b", k.GetMetadata().GetLabels())
				}
			},
		},
		{
			name: "description", wantVersion: 2,
			cmd: func(id string) UpdateSymmetricKeyCmd {
				return UpdateSymmetricKeyCmd{Metadata: md(id), Spec: &kmspb.SymmetricKeySpec{Description: "d"}}
			},
			check: func(t *testing.T, k *kmspb.SymmetricKey) {
				t.Helper()
				if k.GetSpec().GetDescription() != "d" {
					t.Errorf("description = %q, want d", k.GetSpec().GetDescription())
				}
			},
		},
		{
			name: "rotation period", wantVersion: 2,
			cmd: func(id string) UpdateSymmetricKeyCmd {
				return UpdateSymmetricKeyCmd{Metadata: md(id), Spec: &kmspb.SymmetricKeySpec{RotationPeriod: durationpb.New(48 * time.Hour)}}
			},
			check: func(t *testing.T, k *kmspb.SymmetricKey) {
				t.Helper()
				if got := k.GetSpec().GetRotationPeriod().AsDuration(); got != 48*time.Hour {
					t.Errorf("rotation period = %v, want 48h", got)
				}
			},
		},
		{
			name: "rotation period out of range", wantKind: errs.KindBadRequest,
			cmd: func(id string) UpdateSymmetricKeyCmd {
				return UpdateSymmetricKeyCmd{Metadata: md(id), Spec: &kmspb.SymmetricKeySpec{RotationPeriod: durationpb.New(time.Minute)}}
			},
		},
		{
			name: "no-op", wantVersion: 1,
			cmd: func(id string) UpdateSymmetricKeyCmd { return UpdateSymmetricKeyCmd{Metadata: md(id)} },
		},
		{
			name: "same algorithm", wantVersion: 1,
			cmd: func(id string) UpdateSymmetricKeyCmd {
				return UpdateSymmetricKeyCmd{Metadata: md(id), Spec: &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256}}
			},
		},
		{
			name: "algorithm change", wantKind: errs.KindBadRequest,
			cmd: func(id string) UpdateSymmetricKeyCmd {
				return UpdateSymmetricKeyCmd{Metadata: md(id), Spec: &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_128}}
			},
		},
		{
			name: "parent change", wantKind: errs.KindBadRequest,
			cmd: func(id string) UpdateSymmetricKeyCmd {
				m := md(id)
				m.ParentId = "project-other"
				return UpdateSymmetricKeyCmd{Metadata: m}
			},
		},
		{
			name: "stale resource version", wantKind: errs.KindConflict,
			cmd: func(id string) UpdateSymmetricKeyCmd {
				m := md(id)
				m.ResourceVersion = 7
				m.Labels = map[string]string{"x": "y"}
				return UpdateSymmetricKeyCmd{Metadata: m}
			},
		},
		{
			name: "unknown key", wantKind: errs.KindNotFound,
			cmd: func(string) UpdateSymmetricKeyCmd { return UpdateSymmetricKeyCmd{Metadata: md("kmssymkey-nope")} },
		},
		{
			name: "missing id", wantKind: errs.KindBadRequest,
			cmd: func(string) UpdateSymmetricKeyCmd { return UpdateSymmetricKeyCmd{Metadata: md("")} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newSymEnv(t)
			id := createSym(t, e, "k", symSpec())
			e.clk.Advance(time.Hour)
			err := update(t, e, tt.cmd(id))
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("Update kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if err != nil {
				return
			}
			k, err := e.svc.Get(t.Context(), id, ActiveOnly)
			if err != nil {
				t.Fatal(err)
			}
			if got := k.GetMetadata().GetResourceVersion(); got != tt.wantVersion {
				t.Errorf("resource_version = %d, want %d", got, tt.wantVersion)
			}
			wantUpdated := t0
			if tt.wantVersion > 1 {
				wantUpdated = e.clk.Now()
			}
			if got := k.GetMetadata().GetUpdatedAt().AsTime(); !got.Equal(wantUpdated) {
				t.Errorf("updated_at = %v, want %v", got, wantUpdated)
			}
			if k.GetSpec().GetAlgorithm() != kmspb.SymmetricAlgorithm_AES_256 {
				t.Errorf("algorithm = %v, want AES_256", k.GetSpec().GetAlgorithm())
			}
			if tt.check != nil {
				tt.check(t, k)
			}
		})
	}
}

func TestSymmetricUpdateResetMask(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	id := createSym(t, e, "k", &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256, Description: "keep me"})
	md := &commonpb.ResourceMetadata{Id: id, ParentId: testProject, Name: "k"}

	// Omitted description without a reset mask is kept.
	if _, err := e.svc.Update(t.Context(), UpdateSymmetricKeyCmd{Metadata: md}); err != nil {
		t.Fatal(err)
	}
	k, _ := e.svc.Get(t.Context(), id, ActiveOnly)
	if k.GetSpec().GetDescription() != "keep me" || k.GetMetadata().GetResourceVersion() != 1 {
		t.Fatalf("after unmasked update: %v, want description kept at version 1", k)
	}

	e.clk.Advance(time.Minute)
	if _, err := e.svc.Update(t.Context(), UpdateSymmetricKeyCmd{Metadata: md, ResetMask: mask.ParseMust("spec.description")}); err != nil {
		t.Fatalf("Update with mask: %v", err)
	}
	k, _ = e.svc.Get(t.Context(), id, ActiveOnly)
	if k.GetSpec().GetDescription() != "" || k.GetMetadata().GetResourceVersion() != 2 {
		t.Errorf("after reset: description %q version %d, want empty and 2", k.GetSpec().GetDescription(), k.GetMetadata().GetResourceVersion())
	}
	if k.GetSpec().GetAlgorithm() != kmspb.SymmetricAlgorithm_AES_256 || k.GetSpec().GetRotationPeriod() == nil {
		t.Errorf("reset dropped immutable/default spec fields: %v", k.GetSpec())
	}
}

func TestSymmetricUpdateRenameConflict(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	createSym(t, e, "a", symSpec())
	b := createSym(t, e, "b", symSpec())
	_, err := e.svc.Update(t.Context(), UpdateSymmetricKeyCmd{
		Metadata: &commonpb.ResourceMetadata{Id: b, ParentId: testProject, Name: "a"},
	})
	if !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("rename onto existing name err = %v, want AlreadyExists", err)
	}
}

func TestSymmetricEncryptDecrypt(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	id := createSym(t, e, "k", symSpec())
	other := createSym(t, e, "other", symSpec())
	ctx := t.Context()

	ct, err := e.svc.Encrypt(ctx, id, []byte("secret"), []byte("aad"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if got, err := e.svc.Decrypt(ctx, id, ct, []byte("aad")); err != nil || string(got) != "secret" {
		t.Fatalf("Decrypt = %q, %v, want secret", got, err)
	}

	tampered := func(i int) []byte {
		b := bytes.Clone(ct)
		b[i] ^= 0xff
		return b
	}
	tests := []struct {
		name string
		key  string
		ct   []byte
		aad  []byte
	}{
		{"wrong aad", id, ct, []byte("nope")},
		{"missing aad", id, ct, nil},
		{"wrong key", other, ct, []byte("aad")},
		{"header", id, tampered(0), []byte("aad")},
		{"version", id, tampered(2), []byte("aad")},
		{"nonce", id, tampered(headerSize), []byte("aad")},
		{"body", id, tampered(headerSize + 12), []byte("aad")},
		{"tag", id, tampered(len(ct) - 1), []byte("aad")},
		{"truncated", id, ct[:3], []byte("aad")},
		{"empty", id, nil, []byte("aad")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := e.svc.Decrypt(t.Context(), tt.key, tt.ct, tt.aad)
			if !errs.IsKind(err, errs.KindBadRequest) {
				t.Errorf("Decrypt err = %v, want BadRequest", err)
			}
		})
	}
}

func TestSymmetricCryptoErrors(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	id := createSym(t, e, "k", symSpec())
	ctx := t.Context()

	if _, err := e.svc.Encrypt(ctx, id, make([]byte, MaxPlaintextSize), nil); err != nil {
		t.Errorf("Encrypt(64 KiB) err = %v, want nil", err)
	}
	if _, err := e.svc.Encrypt(ctx, id, make([]byte, MaxPlaintextSize+1), nil); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("Encrypt(64 KiB + 1) err = %v, want BadRequest", err)
	}
	if _, err := e.svc.Encrypt(ctx, "", nil, nil); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("Encrypt(no key) err = %v, want BadRequest", err)
	}
	if _, err := e.svc.Encrypt(ctx, "kmssymkey-nope", []byte("x"), nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Encrypt(unknown key) err = %v, want NotFound", err)
	}
	if ct, err := e.svc.Encrypt(ctx, id, nil, nil); err != nil {
		t.Errorf("Encrypt(empty) err = %v", err)
	} else if pt, err := e.svc.Decrypt(ctx, id, ct, nil); err != nil || len(pt) != 0 {
		t.Errorf("Decrypt(empty) = %q, %v", pt, err)
	}
}

func TestSymmetricRotate(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	id := createSym(t, e, "k", symSpec())
	ctx := t.Context()

	old, err := e.svc.Encrypt(ctx, id, []byte("v1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := binary.BigEndian.Uint32(old[1:5]); v != 1 {
		t.Errorf("first ciphertext version = %d, want 1", v)
	}
	if _, err := e.svc.Rotate(ctx, id, nil); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	fresh, err := e.svc.Encrypt(ctx, id, []byte("v2"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := binary.BigEndian.Uint32(fresh[1:5]); v != 2 {
		t.Errorf("post-rotation ciphertext version = %d, want 2", v)
	}
	for ct, want := range map[string]string{string(old): "v1", string(fresh): "v2"} {
		if got, err := e.svc.Decrypt(ctx, id, []byte(ct), nil); err != nil || string(got) != want {
			t.Errorf("Decrypt = %q, %v, want %q", got, err, want)
		}
	}
}

func TestSymmetricRotateLimit(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	id := createSym(t, e, "k", symSpec())
	for i := 1; i <= maxManualRotations; i++ {
		if _, err := e.svc.Rotate(t.Context(), id, nil); err != nil {
			t.Fatalf("Rotate #%d: %v", i, err)
		}
	}
	if _, err := e.svc.Rotate(t.Context(), id, nil); !errs.IsKind(err, errs.KindBadState) {
		t.Fatalf("Rotate #%d err = %v, want BadState", maxManualRotations+1, err)
	}
	if _, err := e.svc.Encrypt(t.Context(), id, []byte("x"), nil); err != nil {
		t.Errorf("Encrypt after hitting limit: %v", err)
	}
}

func TestSymmetricGenerateDataKey(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	id := createSym(t, e, "k", symSpec())
	ctx := t.Context()
	aad := []byte("ctx")

	dk, err := e.svc.GenerateDataKey(ctx, id, kmspb.SymmetricAlgorithm_AES_256, aad)
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	if len(dk.Plaintext) != 32 {
		t.Errorf("plaintext length = %d, want 32", len(dk.Plaintext))
	}
	if got, err := e.svc.Decrypt(ctx, id, dk.Ciphertext, aad); err != nil || !bytes.Equal(got, dk.Plaintext) {
		t.Errorf("Decrypt(data key) = %x, %v, want the plaintext", got, err)
	}
	if _, err := e.svc.Decrypt(ctx, id, dk.Ciphertext, nil); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("Decrypt(data key, wrong aad) err = %v, want BadRequest", err)
	}

	if _, err := e.svc.GenerateDataKey(ctx, id, kmspb.SymmetricAlgorithm_AES_128, aad); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("AES_128 data key err = %v, want BadRequest", err)
	}
	if _, err := e.svc.GenerateDataKey(ctx, id, kmspb.SymmetricAlgorithm_SYMMETRIC_ALGORITHM_UNSPECIFIED, aad); !errs.IsKind(err, errs.KindBadRequest) {
		t.Errorf("unspecified data key spec err = %v, want BadRequest", err)
	}
}

func TestSymmetricDeletion(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	ctx := t.Context()
	id := createSym(t, e, "k", symSpec())
	ct, err := e.svc.Encrypt(ctx, id, []byte("data"), nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.svc.UpdateDeletionDelay(ctx, id, 48*time.Hour, nil); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("UpdateDeletionDelay on active key err = %v, want BadState", err)
	}
	if _, err := e.svc.Undelete(ctx, id, "", nil); !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("Undelete on active key err = %v, want BadState", err)
	}

	if _, err := e.svc.Delete(ctx, id, nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := e.svc.Get(ctx, id, ActiveOnly); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after Delete err = %v, want NotFound", err)
	}
	k, err := e.svc.Get(ctx, id, IncludeScheduled)
	if err != nil {
		t.Fatalf("Get(showScheduled): %v", err)
	}
	st := k.GetStatus()
	if st.GetState() != kmspb.KeyState_SCHEDULED_FOR_DELETION ||
		!st.GetDeletedAt().AsTime().Equal(t0) || !st.GetPurgeAt().AsTime().Equal(t0.Add(30*24*time.Hour)) {
		t.Errorf("status = %v, want SCHEDULED_FOR_DELETION deleted_at %v purge_at +30d", st, t0)
	}
	if page, _ := e.svc.List(ctx, testProject, 0, "", ActiveOnly); len(page.Items) != 0 {
		t.Errorf("List hides scheduled: got %d items, want 0", len(page.Items))
	}
	if page, _ := e.svc.List(ctx, testProject, 0, "", IncludeScheduled); len(page.Items) != 1 {
		t.Errorf("List(showScheduled): got %d items, want 1", len(page.Items))
	}

	cryptoCalls := map[string]func() error{
		"Encrypt": func() error { _, err := e.svc.Encrypt(ctx, id, []byte("x"), nil); return err },
		"Decrypt": func() error { _, err := e.svc.Decrypt(ctx, id, ct, nil); return err },
		"GenerateDataKey": func() error {
			_, err := e.svc.GenerateDataKey(ctx, id, kmspb.SymmetricAlgorithm_AES_256, nil)
			return err
		},
	}
	for name, call := range cryptoCalls {
		if err := call(); !errs.IsKind(err, errs.KindBadState) {
			t.Errorf("%s on scheduled key err = %v, want BadState", name, err)
		}
	}
	if _, err := e.svc.Delete(ctx, id, nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("second Delete err = %v, want NotFound", err)
	}

	// The name is free again.
	id2 := createSym(t, e, "k", symSpec())
	if id2 == id {
		t.Fatal("new key reused the old ID")
	}

	// Undelete while the name is taken needs a new name.
	if _, err := e.svc.Undelete(ctx, id, "", nil); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Fatalf("Undelete with taken name err = %v, want AlreadyExists", err)
	}
	e.clk.Advance(time.Hour)
	if _, err := e.svc.Undelete(ctx, id, "k-restored", nil); err != nil {
		t.Fatalf("Undelete with new name: %v", err)
	}
	k, err = e.svc.Get(ctx, id, ActiveOnly)
	if err != nil {
		t.Fatal(err)
	}
	if k.GetStatus().GetState() != kmspb.KeyState_ACTIVE || k.GetMetadata().GetName() != "k-restored" ||
		k.GetStatus().GetPurgeAt() != nil || k.GetStatus().GetDeletedAt() != nil {
		t.Errorf("restored key = %v, want ACTIVE named k-restored without deletion times", k)
	}
	if k.GetMetadata().GetResourceVersion() != 2 {
		t.Errorf("resource_version = %d, want 2 (delete bumps nothing, rename does)", k.GetMetadata().GetResourceVersion())
	}
	if got, err := e.svc.Decrypt(ctx, id, ct, nil); err != nil || string(got) != "data" {
		t.Errorf("Decrypt old ciphertext after undelete = %q, %v, want data", got, err)
	}
}

func TestSymmetricDeletionDelayAndPurge(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	ctx := t.Context()
	id := createSym(t, e, "k", symSpec())
	if _, err := e.svc.Delete(ctx, id, nil); err != nil {
		t.Fatal(err)
	}

	for _, d := range []time.Duration{time.Hour, 23*time.Hour + 59*time.Minute, 30*24*time.Hour + time.Second, 0, -time.Hour} {
		if _, err := e.svc.UpdateDeletionDelay(ctx, id, d, nil); !errs.IsKind(err, errs.KindBadRequest) {
			t.Errorf("UpdateDeletionDelay(%v) err = %v, want BadRequest", d, err)
		}
	}

	e.clk.Advance(2 * time.Hour)
	if _, err := e.svc.UpdateDeletionDelay(ctx, id, 48*time.Hour, nil); err != nil {
		t.Fatalf("UpdateDeletionDelay: %v", err)
	}
	k, err := e.svc.Get(ctx, id, IncludeScheduled)
	if err != nil {
		t.Fatal(err)
	}
	wantPurge := e.clk.Now().Add(48 * time.Hour)
	if got := k.GetStatus().GetPurgeAt().AsTime(); !got.Equal(wantPurge) {
		t.Errorf("purge_at = %v, want %v", got, wantPurge)
	}
	if got := k.GetStatus().GetDeletedAt().AsTime(); !got.Equal(t0) {
		t.Errorf("deleted_at = %v, want unchanged %v", got, t0)
	}

	// Just before the purge time the key is still there.
	e.clk.Advance(48*time.Hour - time.Second)
	if _, err := e.svc.Get(ctx, id, IncludeScheduled); err != nil {
		t.Fatalf("Get just before purge: %v", err)
	}
	e.clk.Advance(time.Second)
	if _, err := e.svc.Get(ctx, id, IncludeScheduled); !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("Get at purge_at err = %v, want NotFound", err)
	}
	if _, err := e.repo.Material(ctx, id); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("material after purge err = %v, want NotFound", err)
	}
	if _, err := e.repo.Get(ctx, id); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("repo key after purge err = %v, want NotFound", err)
	}
	if _, err := e.svc.Undelete(ctx, id, "", nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Undelete after purge err = %v, want NotFound", err)
	}
}

func TestSymmetricListPurgesDue(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	ctx := t.Context()
	id := createSym(t, e, "k", symSpec())
	if _, err := e.svc.Delete(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(DefaultDeletionDelay)
	page, err := e.svc.List(ctx, testProject, 0, "", IncludeScheduled)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("List(showScheduled) after purge = %v, %v, want empty", page.Items, err)
	}
	if _, err := e.repo.Material(ctx, id); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("material after List purge err = %v, want NotFound", err)
	}
}

func TestSymmetricServiceReturnsClones(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	id := createSym(t, e, "k", symSpec())
	k, _ := e.svc.Get(t.Context(), id, ActiveOnly)
	k.Metadata.Name = "mutated"
	again, _ := e.svc.Get(t.Context(), id, ActiveOnly)
	if again.GetMetadata().GetName() != "k" {
		t.Errorf("name = %q, want k (repo must not alias caller protos)", again.GetMetadata().GetName())
	}
	if diff := cmp.Diff(k.GetSpec(), again.GetSpec(), protocmp.Transform()); diff != "" {
		t.Errorf("spec differs (-got +want):\n%s", diff)
	}
}

func TestNewSymmetricKeyServiceRequiresDeps(t *testing.T) {
	t.Parallel()
	if _, err := NewSymmetricKeyService(KeyDeps[*kmspb.SymmetricKey]{}); err == nil {
		t.Error("NewSymmetricKeyService with no deps succeeded, want error")
	}
}
