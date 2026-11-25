package domain_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/domain"
)

func TestGetPayloadRoundTrip(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	bin := []byte{0x00, 0xff, 0x10, 0x80, 0x00}
	const text = "hunter2 é世"
	id := e.newSecret(t, "db", strEntry("password", text), binEntry("blob", bin))
	v1 := e.primary(t, id)
	v2 := e.newVersion(t, id, false, strEntry("other", "second-value"))

	tests := []struct {
		name string
		ref  domain.PayloadRef
		want *mysteryboxpb.SecretPayload
	}{
		{
			name: "default is primary",
			ref:  domain.PayloadRef{SecretID: id},
			want: &mysteryboxpb.SecretPayload{VersionId: v1, Data: []*mysteryboxpb.Payload{strEntry("password", text), binEntry("blob", bin)}},
		},
		{
			name: "explicit primary",
			ref:  domain.PayloadRef{SecretID: id, VersionID: v1},
			want: &mysteryboxpb.SecretPayload{VersionId: v1, Data: []*mysteryboxpb.Payload{strEntry("password", text), binEntry("blob", bin)}},
		},
		{
			name: "non-primary version",
			ref:  domain.PayloadRef{SecretID: id, VersionID: v2},
			want: &mysteryboxpb.SecretPayload{VersionId: v2, Data: []*mysteryboxpb.Payload{strEntry("other", "second-value")}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := e.svc.GetPayload(ctx, tt.ref)
			if err != nil {
				t.Fatalf("GetPayload(%+v): %v", tt.ref, err)
			}
			if diff := cmp.Diff(tt.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("GetPayload(%+v) mismatch (-want +got):\n%s", tt.ref, diff)
			}
		})
	}
}

func TestPayloadRepoHoldsOnlySealedBytes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	bin := []byte("binary-secret-bytes")
	id := e.newSecret(t, "db", strEntry("password", "plaintext-secret"), binEntry("blob", bin))
	// The version's payload, and the secret's record of its inline version.
	if e.payloads.versions() != 1 || !e.payloads.hasInline(id) || len(e.payloads.sealed) != 2 {
		t.Fatalf("stored payloads = %d (versions %d, inline %v), want the version's and the inline record",
			len(e.payloads.sealed), e.payloads.versions(), e.payloads.hasInline(id))
	}
	for id, sealed := range e.payloads.sealed {
		if !bytes.HasPrefix(sealed, []byte(sealPrefix)) {
			t.Errorf("payload %s is not sealed: %q", id, sealed)
		}
		for _, secret := range [][]byte{[]byte("plaintext-secret"), bin} {
			if bytes.Contains(sealed, secret) {
				t.Errorf("payload %s contains plaintext %q", id, secret)
			}
		}
	}
}

func TestGetPayloadByKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	id := e.newSecret(t, "db", strEntry("password", "hunter2"), binEntry("blob", []byte{1, 2, 3}))
	v1 := e.primary(t, id)
	otherSecret := e.newSecret(t, "other", strEntry("k", "v"))
	foreignVersion := e.primary(t, otherSecret)

	tests := []struct {
		name     string
		ref      domain.PayloadRef
		key      string
		want     *mysteryboxpb.Payload
		wantKind errs.Kind
	}{
		{name: "string", ref: domain.PayloadRef{SecretID: id}, key: "password", want: strEntry("password", "hunter2")},
		{name: "binary with version", ref: domain.PayloadRef{SecretID: id, VersionID: v1}, key: "blob", want: binEntry("blob", []byte{1, 2, 3})},
		{name: "missing key", ref: domain.PayloadRef{SecretID: id}, key: "nope-key", wantKind: errs.KindNotFound},
		{name: "empty key", ref: domain.PayloadRef{SecretID: id}, key: "", wantKind: errs.KindBadRequest},
		{name: "version of another secret", ref: domain.PayloadRef{SecretID: id, VersionID: foreignVersion}, key: "k", wantKind: errs.KindNotFound},
		{name: "unknown secret", ref: domain.PayloadRef{SecretID: "mbsec-e00mock000000000999"}, key: "k", wantKind: errs.KindNotFound},
		{name: "missing secret id", ref: domain.PayloadRef{}, key: "k", wantKind: errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := e.svc.GetPayloadByKey(ctx, tt.ref, tt.key)
			if k := errs.KindOf(err); k != tt.wantKind {
				t.Fatalf("GetPayloadByKey kind = %v, want %v (err: %v)", k, tt.wantKind, err)
			}
			if err != nil {
				if tt.name == "missing key" && strings.Contains(err.Error(), tt.key) {
					t.Errorf("error %q quotes the key", err)
				}
				return
			}
			if diff := cmp.Diff(tt.want, got.GetData(), protocmp.Transform()); diff != "" {
				t.Errorf("entry mismatch (-want +got):\n%s", diff)
			}
			if got.GetVersionId() != v1 {
				t.Errorf("version_id = %q, want %q", got.GetVersionId(), v1)
			}
		})
	}
}

func TestGetPayloadOfOtherSecretsVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.newSecret(t, "a", strEntry("k", "va"))
	b := e.newSecret(t, "b", strEntry("k", "vb"))
	_, err := e.svc.GetPayload(t.Context(), domain.PayloadRef{SecretID: a, VersionID: e.primary(t, b)})
	if got := errs.KindOf(err); got != errs.KindNotFound {
		t.Fatalf("GetPayload(foreign version) kind = %v, want NotFound (err: %v)", got, err)
	}
}

func TestPayloadValidation(t *testing.T) {
	t.Parallel()
	limits := domain.Limits{MaxEntriesPerVersion: 2, MaxValueBytes: 4, MaxKeyBytes: 8, MaxVersionsPerSecret: 2}
	tests := []struct {
		name    string
		entries []*mysteryboxpb.Payload
		// secrets are strings that must never appear in the error.
		secrets []string
		want    errs.Kind
	}{
		{name: "valid at limits", entries: []*mysteryboxpb.Payload{strEntry("12345678", "abcd"), binEntry("b", []byte("wxyz"))}},
		{name: "empty payload", entries: nil, want: errs.KindBadRequest},
		{
			name:    "too many entries",
			entries: []*mysteryboxpb.Payload{strEntry("a", "s3c1"), strEntry("b", "s3c2"), strEntry("c", "s3c3")},
			secrets: []string{"s3c1", "s3c2", "s3c3"}, want: errs.KindBadRequest,
		},
		{name: "string too big", entries: []*mysteryboxpb.Payload{strEntry("a", "bigsecretvalue")}, secrets: []string{"bigsecretvalue"}, want: errs.KindBadRequest},
		{name: "binary too big", entries: []*mysteryboxpb.Payload{binEntry("a", []byte("bigbinsecret"))}, secrets: []string{"bigbinsecret"}, want: errs.KindBadRequest},
		{name: "key too long", entries: []*mysteryboxpb.Payload{strEntry("averyverylongkey", "s3cr")}, secrets: []string{"averyverylongkey", "s3cr"}, want: errs.KindBadRequest},
		{name: "empty key", entries: []*mysteryboxpb.Payload{strEntry("", "s3cr")}, secrets: []string{"s3cr"}, want: errs.KindBadRequest},
		{name: "duplicate keys", entries: []*mysteryboxpb.Payload{strEntry("dup", "s3c1"), strEntry("dup", "s3c2")}, secrets: []string{"s3c1", "s3c2"}, want: errs.KindBadRequest},
		{name: "empty string value", entries: []*mysteryboxpb.Payload{strEntry("a", "")}, want: errs.KindBadRequest},
		{name: "empty binary value", entries: []*mysteryboxpb.Payload{binEntry("a", nil)}, want: errs.KindBadRequest},
		{name: "missing oneof", entries: []*mysteryboxpb.Payload{{Key: "a"}}, want: errs.KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnvLimits(t, limits)
			check := func(t *testing.T, where string, err error) {
				t.Helper()
				if got := errs.KindOf(err); got != tt.want {
					t.Fatalf("%s kind = %v, want %v (err: %v)", where, got, tt.want, err)
				}
				for _, s := range tt.secrets {
					if err != nil && strings.Contains(err.Error(), s) {
						t.Errorf("%s error %q contains secret %q", where, err, s)
					}
				}
			}
			_, err := e.svc.CreateSecret(t.Context(), domain.CreateSecretCmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "inline"},
				Spec:     &mysteryboxpb.SecretSpec{SecretVersion: &mysteryboxpb.SecretVersionSpec{Payload: tt.entries}},
			})
			check(t, "CreateSecret", err)

			id := e.newSecret(t, "plain")
			_, err = e.svc.CreateVersion(t.Context(), domain.CreateVersionCmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: id},
				Spec:     &mysteryboxpb.SecretVersionSpec{Payload: tt.entries},
			})
			check(t, "CreateVersion", err)
		})
	}
}
