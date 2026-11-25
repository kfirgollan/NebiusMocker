package contract

import (
	"bytes"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"
	mysteryboxsvc "github.com/nebius/gosdk/services/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/kms"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox"
	"github.com/kfirgollan/NebiusMocker/services/storage"
)

const (
	secretValue = "TOP-SECRET-VALUE-123"
	hiddenMask  = "**HIDDEN**"
)

// rawTokens must never appear in an audit event: only masked forms may.
var rawTokens = []string{
	config.DefaultDevToken, config.DefaultViewerToken, config.DefaultEditorToken, auditorToken,
}

// assertNoRawTokens fails when any event contains a raw bearer token.
func assertNoRawTokens(tb testing.TB, evs []*auditv2pb.AuditEvent) {
	tb.Helper()
	for _, ev := range evs {
		raw, err := protojson.Marshal(ev)
		if err != nil {
			tb.Fatalf("marshal event %s: %v", ev.GetId(), err)
		}
		for _, tok := range rawTokens {
			if bytes.Contains(raw, []byte(tok)) {
				tb.Errorf("event %s (%s) contains the raw token %q:\n%s", ev.GetId(), ev.GetSource(), tok, raw)
			}
		}
	}
}

// createSecret creates a secret with one payload through the real
// MysteryBox module and returns its ID.
func createSecret(t *testing.T, env *auditEnv, token string) string {
	t.Helper()
	svc := mysteryboxsvc.New(env.inst.SDKAs(t, token)).Secret()
	op, err := svc.Create(t.Context(), &mysteryboxpb.CreateSecretRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: env.inst.ProjectID, Name: "db-credentials"},
		Spec: &mysteryboxpb.SecretSpec{SecretVersion: &mysteryboxpb.SecretVersionSpec{
			Payload: []*mysteryboxpb.Payload{{
				Key:     "password",
				Payload: &mysteryboxpb.Payload_StringValue{StringValue: secretValue},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("create secret as %s: %v", token, err)
	}
	if op, err = op.Wait(t.Context()); err != nil {
		t.Fatalf("wait for secret create: %v", err)
	}
	return op.ResourceID()
}

func TestAuditRedactsSecretPayloads(t *testing.T) {
	t.Parallel()
	mem := memory.New(clock.Real{})
	env := startAuditWith(t, []mocker.Module{mysterybox.New(), storage.New(storage.WithS3Listen("127.0.0.1:0"))},
		testkit.WithAuthz(authz.ModeEnforce), testkit.WithObjStore(mem), testkit.WithAuditReads(),
		testkit.WithTokens(map[string]string{auditorToken: auditorPrincipal}),
	)
	env.addExportBucket(t)
	ctx := t.Context()

	// Traffic from every static principal, reads included, so raw tokens
	// would show up wherever they leak. The viewer's create is denied.
	secretID := createSecret(t, env, config.DefaultDevToken)
	// Reading the payload back puts the secret in a response payload too.
	payload, err := mysteryboxsvc.New(env.inst.SDK(t)).Payload().Get(ctx, &mysteryboxpb.GetPayloadRequest{SecretId: secretID})
	if err != nil {
		t.Fatalf("get payload: %v", err)
	}
	if got := payload.GetData()[0].GetStringValue(); got != secretValue {
		t.Fatalf("payload = %q, want the secret back", got)
	}
	_, err = env.as(t, config.DefaultViewerToken).groups.Create(ctx, createReq(env.inst.TenantID, "by-viewer"))
	assertCode(t, err, codes.PermissionDenied)
	if _, err := env.as(t, config.DefaultEditorToken).groups.Create(ctx, createReq(env.inst.TenantID, "by-editor")); err != nil {
		t.Fatalf("editor Create: %v", err)
	}
	env.startExport(t, env.startReq("", ""))

	evs := env.list(t, "")
	assertNoRawTokens(t, evs)

	// The secret create is audited, with its payload masked.
	mbox := env.list(t, "resource.metadata.id='"+secretID+"' AND action='CREATE'")
	if len(mbox) != 1 {
		t.Fatalf("mysterybox create events = %d, want 1", len(mbox))
	}
	if reads := env.list(t, "service.name='MBOX' AND action='GET'"); len(reads) == 0 {
		t.Error("the payload read was not audited (reads are enabled)")
	}
	if got := mbox[0].GetService().GetName(); got != "MBOX" {
		t.Errorf("service.name = %q, want MBOX", got)
	}
	params, err := protojson.Marshal(mbox[0].GetRequest().GetParameters())
	if err != nil {
		t.Fatalf("marshal parameters: %v", err)
	}
	if !strings.Contains(string(params), hiddenMask) {
		t.Errorf("request.parameters = %s, want %s in place of the payload", params, hiddenMask)
	}
	if !strings.Contains(string(params), "db-credentials") {
		t.Errorf("request.parameters = %s, want the non-sensitive secret name kept", params)
	}

	for i, ev := range evs {
		raw, err := protojson.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal event %d: %v", i, err)
		}
		if strings.Contains(string(raw), secretValue) {
			t.Errorf("event %s (%s) leaks the secret:\n%s", ev.GetId(), ev.GetSource(), raw)
		}
	}

	// Exported objects are made of the same events.
	keys := exportedObjects(t, mem, "auditlogs/")
	if len(keys) == 0 {
		t.Fatal("export wrote no objects")
	}
	for _, key := range keys {
		rc, _, err := mem.Get(ctx, exportBucket, key)
		if err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		raw, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if bytes.Contains(raw, []byte(secretValue)) {
			t.Errorf("export object %s leaks the secret", key)
		}
		for _, tok := range rawTokens {
			if bytes.Contains(raw, []byte(tok)) {
				t.Errorf("export object %s contains the raw token %q", key, tok)
			}
		}
	}
	assertNoRawTokens(t, exportedEvents(t, mem, "auditlogs/"))
}

func TestAuditRedactsKMSPlaintext(t *testing.T) {
	t.Parallel()
	env := startAuditWith(t, []mocker.Module{kms.New()}, testkit.WithAuditReads())
	c := newKMSClients(env.inst.SDK(t))
	plaintext := []byte("KMS-PLAINTEXT-SECRET-456")

	key := kmsSymKind(c).mustCreate(t, env.inst.ProjectID, "redact-key", nil)
	ct := kmsEncrypt(t, c, key.id, nil, plaintext)
	if got, err := kmsDecrypt(t, c, key.id, nil, ct); err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("decrypt = %q, %v; want the plaintext back", got, err)
	}
	if _, err := c.symCrypto.Encrypt(t.Context(), &kmspb.SymmetricEncryptRequest{
		KeyId: key.id, Plaintext: []byte("second"),
	}); err != nil {
		t.Fatalf("second Encrypt: %v", err)
	}

	// Crypto calls name the key in key_id; the event's resource comes from
	// the method's authz target.
	evs := append(env.list(t, "action='ENCRYPT'"), env.list(t, "action='DECRYPT'")...)
	if len(evs) != 3 {
		t.Fatalf("crypto events = %d, want 2 ENCRYPT + 1 DECRYPT", len(evs))
	}
	for _, ev := range evs {
		h := ev.GetResource().GetHierarchy()
		if ev.GetResource().GetMetadata().GetId() != key.id || len(h) != 2 || h[1].GetId() != env.inst.ProjectID {
			t.Errorf("%s event resource = %v, want key %s under [tenant, %s]", ev.GetAction(), ev.GetResource(), key.id, env.inst.ProjectID)
		}
	}
	assertNoRawTokens(t, env.list(t, ""))
	needles := []string{string(plaintext), base64.StdEncoding.EncodeToString(plaintext), base64.RawStdEncoding.EncodeToString(plaintext)}
	for _, ev := range evs {
		raw, err := protojson.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		for _, n := range needles {
			if strings.Contains(string(raw), n) {
				t.Errorf("event %s (%s) leaks the plaintext as %q:\n%s", ev.GetId(), ev.GetSource(), n, raw)
			}
		}
		// Sensitive bytes are replaced by the mask, which a Struct carries base64-encoded.
		params, err := protojson.Marshal(ev.GetRequest().GetParameters())
		if err != nil {
			t.Fatalf("marshal parameters: %v", err)
		}
		if ev.GetAction() == "ENCRYPT" && !strings.Contains(string(params), base64.StdEncoding.EncodeToString([]byte(hiddenMask))) {
			t.Errorf("ENCRYPT request.parameters = %s, want the plaintext replaced by %s", params, hiddenMask)
		}
	}
}
