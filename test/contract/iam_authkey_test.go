package contract

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamsvc "github.com/nebius/gosdk/services/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

func authKeyAccount(saID string) *iampb.Account {
	return &iampb.Account{Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: saID}}}
}

func authKeyRSA(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("generate RSA-%d: %v", bits, err)
	}
	return k
}

func authKeyPKIX(t *testing.T, k *rsa.PrivateKey) (pemText string, der []byte) {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), der
}

func authKeyECPEM(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func authKeyFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// authKeyCreate creates a key through the SDK and returns its resource ID.
func authKeyCreate(t *testing.T, svc iamsvc.AuthPublicKeyService, req *iampb.CreateAuthPublicKeyRequest) string {
	t.Helper()
	op, err := svc.Create(t.Context(), req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create wait: %v", err)
	}
	return op.ResourceID()
}

func authKeyReq(parentID, saID, name, data string) *iampb.CreateAuthPublicKeyRequest {
	return &iampb.CreateAuthPublicKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: parentID, Name: name},
		Spec:     &iampb.AuthPublicKeySpec{Account: authKeyAccount(saID), Data: data},
	}
}

func authKeyNew(t *testing.T, svc iamsvc.AuthPublicKeyService, parentID, saID, name string) string {
	t.Helper()
	data, _ := authKeyPKIX(t, authKeyRSA(t, 2048))
	return authKeyCreate(t, svc, authKeyReq(parentID, saID, name, data))
}

// authKeyWaitOp waits for an operation started by a mutating RPC.
func authKeyWaitOp(t *testing.T, what string, wait func() error) {
	t.Helper()
	if err := wait(); err != nil {
		t.Fatalf("%s wait: %v", what, err)
	}
}

// authKeyViolationFields returns the BadRequest violation fields carried by err.
func authKeyViolationFields(err error) []string {
	var fields []string
	for _, d := range status.Convert(err).Details() {
		if se, ok := d.(*commonpb.ServiceError); ok {
			for _, v := range se.GetBadRequest().GetViolations() {
				fields = append(fields, v.GetField())
			}
		}
	}
	return fields
}

func authKeyWantInvalid(t *testing.T, what string, err error, field string) {
	t.Helper()
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("%s: code %v, want InvalidArgument (%v)", what, status.Code(err), err)
		return
	}
	if fields := authKeyViolationFields(err); !slices.Contains(fields, field) {
		t.Errorf("%s: violations %v, want one on %q (%v)", what, fields, field, err)
	}
}

func TestAuthKeyCreateGet(t *testing.T) {
	t.Parallel()
	env := saAuthStartIAM(t)
	ctx := t.Context()
	saID := env.newSA(t, "owner")

	priv := authKeyRSA(t, 2048)
	data, der := authKeyPKIX(t, priv)
	exp := timestamppb.New(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
	id := authKeyCreate(t, env.keys, &iampb.CreateAuthPublicKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: env.inst.ProjectID, Name: "full", Labels: map[string]string{"env": "dev"}},
		Spec:     &iampb.AuthPublicKeySpec{Account: authKeyAccount(saID), Description: "desc", ExpiresAt: exp, Data: data},
	})
	const prefix = "publickey-"
	if len(id) < len(prefix) || id[:len(prefix)] != prefix {
		t.Fatalf("resource id %q, want prefix %s", id, prefix)
	}
	got, err := env.keys.Get(ctx, &iampb.GetAuthPublicKeyRequest{Id: id})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	spec := got.GetSpec()
	if spec.GetData() != data || spec.GetAccount().GetServiceAccount().GetId() != saID || spec.GetDescription() != "desc" ||
		!spec.GetExpiresAt().AsTime().Equal(exp.AsTime()) {
		t.Errorf("spec = %v, want round-trip of the request", spec)
	}
	md := got.GetMetadata()
	if md.GetName() != "full" || md.GetLabels()["env"] != "dev" || md.GetParentId() != env.inst.ProjectID || md.GetCreatedAt() == nil {
		t.Errorf("metadata = %v", md)
	}
	st := got.GetStatus()
	if st.GetState() != iampb.AuthPublicKeyStatus_ACTIVE || st.GetFingerprint() != authKeyFingerprint(der) ||
		st.GetAlgorithm() != "RSA" || st.GetKeySize() != 2048 {
		t.Errorf("status = %v, want ACTIVE, RSA 2048, fingerprint %s", st, authKeyFingerprint(der))
	}

	// The PKCS#1 form of the same key has the same fingerprint.
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&priv.PublicKey)}))
	id1 := authKeyCreate(t, env.keys, authKeyReq(env.inst.ProjectID, saID, "pkcs1", pkcs1))
	k1, err := env.keys.Get(ctx, &iampb.GetAuthPublicKeyRequest{Id: id1})
	if err != nil || k1.GetStatus().GetFingerprint() != authKeyFingerprint(der) || k1.GetStatus().GetKeySize() != 2048 {
		t.Errorf("PKCS#1 key status = %v, %v; want fingerprint %s", k1.GetStatus(), err, authKeyFingerprint(der))
	}

	// 4096-bit keys report their size.
	data4k, _ := authKeyPKIX(t, authKeyRSA(t, 4096))
	id4 := authKeyCreate(t, env.keys, authKeyReq(env.inst.ProjectID, saID, "big", data4k))
	k4, err := env.keys.Get(ctx, &iampb.GetAuthPublicKeyRequest{Id: id4})
	if err != nil || k4.GetStatus().GetKeySize() != 4096 {
		t.Errorf("4096-bit key status = %v, %v", k4.GetStatus(), err)
	}
}

func TestAuthKeyCreateErrors(t *testing.T) {
	t.Parallel()
	env := saAuthStartIAM(t)
	saID := env.newSA(t, "owner")
	project := env.inst.ProjectID
	small, _ := authKeyPKIX(t, authKeyRSA(t, 1024))
	good, _ := authKeyPKIX(t, authKeyRSA(t, 2048))

	for _, tc := range []struct{ name, data string }{
		{"EC P-256 key", authKeyECPEM(t)},
		{"garbage", "not a pem"},
		{"empty", ""},
		{"1024-bit RSA", small},
	} {
		_, err := env.keys.Create(t.Context(), authKeyReq(project, saID, "", tc.data))
		authKeyWantInvalid(t, tc.name, err, "spec.data")
	}

	userReq := authKeyReq(project, saID, "", good)
	userReq.Spec.Account = &iampb.Account{Type: &iampb.Account_UserAccount_{UserAccount: &iampb.Account_UserAccount{Id: "useraccount-e00mock0000000000user"}}}
	_, err := env.keys.Create(t.Context(), userReq)
	authKeyWantInvalid(t, "user account", err, "spec.account")

	// A parent other than the service account's project is rejected; the tenant
	// is not a project, so the parent lookup fails.
	_, err = env.keys.Create(t.Context(), authKeyReq(env.inst.TenantID, saID, "", good))
	if c := status.Code(err); c != codes.NotFound && c != codes.InvalidArgument {
		t.Errorf("foreign parent: code %v, want NotFound or InvalidArgument (%v)", c, err)
	}
	if status.Code(err) == codes.InvalidArgument {
		authKeyWantInvalid(t, "foreign parent", err, "metadata.parent_id")
	}

	_, err = env.keys.Create(t.Context(), authKeyReq(project, "serviceaccount-e00mock00000000nobody", "", good))
	if status.Code(err) != codes.NotFound {
		t.Errorf("unknown service account: code %v, want NotFound (%v)", status.Code(err), err)
	}

	pastReq := authKeyReq(project, saID, "", good)
	pastReq.Spec.ExpiresAt = timestamppb.New(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	_, err = env.keys.Create(t.Context(), pastReq)
	authKeyWantInvalid(t, "expires_at in the past", err, "spec.expires_at")
}

func TestAuthKeyListing(t *testing.T) {
	t.Parallel()
	env := saAuthStartIAM(t)
	ctx := t.Context()
	saA := env.newSA(t, "a")
	saB := env.newSA(t, "b")
	project := env.inst.ProjectID

	a1 := authKeyNew(t, env.keys, project, saA, "a1")
	a2 := authKeyNew(t, env.keys, project, saA, "a2")
	a3 := authKeyNew(t, env.keys, project, saA, "a3")
	b1 := authKeyNew(t, env.keys, project, saB, "b1")

	byName, err := env.keys.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: project, Name: "a2"})
	if err != nil || byName.GetMetadata().GetId() != a2 {
		t.Fatalf("GetByName = %v, %v; want %s", byName, err, a2)
	}
	if _, err := env.keys.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: project, Name: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetByName missing: code %v, want NotFound", status.Code(err))
	}

	// List across the project, one item per page.
	var listed []string
	token := ""
	for range 10 {
		page, err := env.keys.List(ctx, &iampb.ListAuthPublicKeyRequest{ParentId: project, PageSize: 1, PageToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page.GetItems()) > 1 {
			t.Fatalf("page_size 1 returned %d items", len(page.GetItems()))
		}
		for _, k := range page.GetItems() {
			listed = append(listed, k.GetMetadata().GetId())
		}
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	slices.Sort(listed)
	if want := slices.Sorted(slices.Values([]string{a1, a2, a3, b1})); !slices.Equal(listed, want) {
		t.Errorf("paged List = %v, want %v", listed, want)
	}

	// ListByAccount returns only that account's keys, paged two at a time.
	var byAcc []string
	token = ""
	for range 10 {
		page, err := env.keys.ListByAccount(ctx, &iampb.ListAuthPublicKeyByAccountRequest{Account: authKeyAccount(saA), PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatalf("ListByAccount: %v", err)
		}
		for _, k := range page.GetItems() {
			byAcc = append(byAcc, k.GetMetadata().GetId())
		}
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	if want := []string{a1, a2, a3}; !slices.Equal(byAcc, want) {
		t.Errorf("paged ListByAccount = %v, want %v", byAcc, want)
	}

	_, err = env.keys.List(ctx, &iampb.ListAuthPublicKeyRequest{ParentId: project, Filter: `name = "a1"`})
	authKeyWantInvalid(t, "List filter", err, "filter")
	_, err = env.keys.ListByAccount(ctx, &iampb.ListAuthPublicKeyByAccountRequest{Account: authKeyAccount(saA), Filter: `name = "a1"`})
	authKeyWantInvalid(t, "ListByAccount filter", err, "filter")
}

func TestAuthKeyUpdate(t *testing.T) {
	t.Parallel()
	env := saAuthStartIAM(t)
	ctx := t.Context()
	saID := env.newSA(t, "owner")
	project := env.inst.ProjectID
	id := authKeyNew(t, env.keys, project, saID, "orig")

	get := func() *iampb.AuthPublicKey {
		t.Helper()
		k, err := env.keys.Get(ctx, &iampb.GetAuthPublicKeyRequest{Id: id})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	update := func(md *commonpb.ResourceMetadata, spec *iampb.AuthPublicKeySpec, reset string) error {
		t.Helper()
		c := ctx
		if reset != "" {
			var err error
			if c, err = grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust(reset)); err != nil {
				t.Fatal(err)
			}
		}
		md.Id = id
		op, err := env.keys.Update(c, &iampb.UpdateAuthPublicKeyRequest{Metadata: md, Spec: spec})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}

	before := get()
	if err := update(&commonpb.ResourceMetadata{ParentId: project, Name: "orig"}, &iampb.AuthPublicKeySpec{}, ""); err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	if v := get().GetMetadata().GetResourceVersion(); v != before.GetMetadata().GetResourceVersion() {
		t.Errorf("no-op update bumped resource_version %d -> %d", before.GetMetadata().GetResourceVersion(), v)
	}

	err := update(&commonpb.ResourceMetadata{ParentId: project, Name: "renamed", Labels: map[string]string{"a": "b"}},
		&iampb.AuthPublicKeySpec{Description: "new desc"}, "")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	after := get()
	if after.GetMetadata().GetName() != "renamed" || after.GetMetadata().GetLabels()["a"] != "b" || after.GetSpec().GetDescription() != "new desc" ||
		after.GetMetadata().GetResourceVersion() <= before.GetMetadata().GetResourceVersion() {
		t.Errorf("after update = %v", after)
	}
	if after.GetSpec().GetData() != before.GetSpec().GetData() || after.GetStatus().GetFingerprint() != before.GetStatus().GetFingerprint() {
		t.Errorf("update changed data or status: %v", after)
	}

	// X-ResetMask clears labels and description.
	err = update(&commonpb.ResourceMetadata{ParentId: project, Name: "renamed"}, &iampb.AuthPublicKeySpec{}, "metadata.labels,spec.description")
	if err != nil {
		t.Fatalf("Update with reset mask: %v", err)
	}
	cleared := get()
	if len(cleared.GetMetadata().GetLabels()) != 0 || cleared.GetSpec().GetDescription() != "" {
		t.Errorf("after reset mask labels=%v description=%q, want both cleared", cleared.GetMetadata().GetLabels(), cleared.GetSpec().GetDescription())
	}

	other, _ := authKeyPKIX(t, authKeyRSA(t, 2048))
	otherSA := env.newSA(t, "other")
	for _, tc := range []struct {
		field string
		spec  *iampb.AuthPublicKeySpec
	}{
		{"spec.data", &iampb.AuthPublicKeySpec{Data: other}},
		{"spec.expires_at", &iampb.AuthPublicKeySpec{ExpiresAt: timestamppb.New(time.Date(2101, 1, 1, 0, 0, 0, 0, time.UTC))}},
		{"spec.account", &iampb.AuthPublicKeySpec{Account: authKeyAccount(otherSA)}},
	} {
		err := update(&commonpb.ResourceMetadata{ParentId: project, Name: "renamed"}, tc.spec, "")
		authKeyWantInvalid(t, "change "+tc.field, err, tc.field)
		if !proto.Equal(get().GetSpec(), cleared.GetSpec()) {
			t.Errorf("rejected %s change altered the stored spec", tc.field)
		}
	}
}

func TestAuthKeyStateAndExpiry(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewManual(t0)
	env := saAuthStartIAM(t, testkit.WithClock(clk))
	ctx := t.Context()
	saID := env.newSA(t, "owner")

	id := authKeyNew(t, env.keys, env.inst.ProjectID, saID, "toggle")
	env.deactivate(t, id)
	if got := env.keyState(t, id); got != iampb.AuthPublicKeyStatus_INACTIVE {
		t.Errorf("after Deactivate state = %v, want INACTIVE", got)
	}
	env.activate(t, id)
	if got := env.keyState(t, id); got != iampb.AuthPublicKeyStatus_ACTIVE {
		t.Errorf("after Activate state = %v, want ACTIVE", got)
	}

	data, _ := authKeyPKIX(t, authKeyRSA(t, 2048))
	req := authKeyReq(env.inst.ProjectID, saID, "expiring", data)
	req.Spec.ExpiresAt = timestamppb.New(t0.Add(time.Hour))
	expID := authKeyCreate(t, env.keys, req)
	if got := env.keyState(t, expID); got != iampb.AuthPublicKeyStatus_ACTIVE {
		t.Fatalf("before expiry state = %v, want ACTIVE", got)
	}
	clk.Advance(2 * time.Hour)
	if got := env.keyState(t, expID); got != iampb.AuthPublicKeyStatus_EXPIRED {
		t.Errorf("Get after expiry state = %v, want EXPIRED", got)
	}
	list, err := env.keys.List(ctx, &iampb.ListAuthPublicKeyRequest{ParentId: env.inst.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	byAcc, err := env.keys.ListByAccount(ctx, &iampb.ListAuthPublicKeyByAccountRequest{Account: authKeyAccount(saID)})
	if err != nil {
		t.Fatal(err)
	}
	for what, items := range map[string][]*iampb.AuthPublicKey{"List": list.GetItems(), "ListByAccount": byAcc.GetItems()} {
		idx := slices.IndexFunc(items, func(k *iampb.AuthPublicKey) bool { return k.GetMetadata().GetId() == expID })
		if idx < 0 {
			t.Errorf("%s: expired key %s missing", what, expID)
		} else if st := items[idx].GetStatus().GetState(); st != iampb.AuthPublicKeyStatus_EXPIRED {
			t.Errorf("%s after expiry state = %v, want EXPIRED", what, st)
		}
	}
}

func TestAuthKeyDelete(t *testing.T) {
	t.Parallel()
	env := saAuthStartIAM(t)
	ctx := t.Context()
	saID := env.newSA(t, "owner")
	doomedSA := env.newSA(t, "doomed")

	id := authKeyNew(t, env.keys, env.inst.ProjectID, saID, "gone")
	env.deleteKey(t, id)
	if _, err := env.keys.Get(ctx, &iampb.GetAuthPublicKeyRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Errorf("Get after Delete: code %v, want NotFound", status.Code(err))
	}
	if _, err := env.keys.Delete(ctx, &iampb.DeleteAuthPublicKeyRequest{Id: id}); status.Code(err) != codes.NotFound {
		t.Errorf("second Delete: code %v, want NotFound", status.Code(err))
	}

	cascade := authKeyNew(t, env.keys, env.inst.ProjectID, doomedSA, "cascade")
	op, err := env.sas.Delete(ctx, &iampb.DeleteServiceAccountRequest{Id: doomedSA})
	if err != nil {
		t.Fatalf("Delete service account: %v", err)
	}
	authKeyWaitOp(t, "Delete service account", func() error { _, err := op.Wait(ctx); return err })
	if _, err := env.keys.Get(ctx, &iampb.GetAuthPublicKeyRequest{Id: cascade}); status.Code(err) != codes.NotFound {
		t.Errorf("Get after service account delete: code %v, want NotFound (cascade)", status.Code(err))
	}
}

func TestAuthKeyAuthz(t *testing.T) {
	t.Parallel()
	env := saAuthStartIAM(t, testkit.WithAuthz(authz.ModeEnforce))
	ctx := t.Context()
	project := env.inst.ProjectID
	saID := env.newSA(t, "owner")
	keyID := authKeyNew(t, env.keys, project, saID, "seed")
	newData, _ := authKeyPKIX(t, authKeyRSA(t, 2048))

	viewer := iamsvc.NewAuthPublicKeyService(env.inst.SDKAs(t, config.DefaultViewerToken))
	if _, err := viewer.Get(ctx, &iampb.GetAuthPublicKeyRequest{Id: keyID}); err != nil {
		t.Errorf("viewer Get: %v", err)
	}
	if _, err := viewer.List(ctx, &iampb.ListAuthPublicKeyRequest{ParentId: project}); err != nil {
		t.Errorf("viewer List: %v", err)
	}
	if _, err := viewer.ListByAccount(ctx, &iampb.ListAuthPublicKeyByAccountRequest{Account: authKeyAccount(saID)}); err != nil {
		t.Errorf("viewer ListByAccount: %v", err)
	}
	_, err := viewer.Create(ctx, authKeyReq(project, saID, "v", newData))
	assertCode(t, err, codes.PermissionDenied)
	_, err = viewer.Update(ctx, &iampb.UpdateAuthPublicKeyRequest{
		Metadata: &commonpb.ResourceMetadata{Id: keyID, ParentId: project, Name: "x"}, Spec: &iampb.AuthPublicKeySpec{},
	})
	assertCode(t, err, codes.PermissionDenied)
	_, err = viewer.Activate(ctx, &iampb.ActivateAuthPublicKeyRequest{Id: keyID})
	assertCode(t, err, codes.PermissionDenied)
	_, err = viewer.Deactivate(ctx, &iampb.DeactivateAuthPublicKeyRequest{Id: keyID})
	assertCode(t, err, codes.PermissionDenied)
	_, err = viewer.Delete(ctx, &iampb.DeleteAuthPublicKeyRequest{Id: keyID})
	assertCode(t, err, codes.PermissionDenied)

	editor := iamsvc.NewAuthPublicKeyService(env.inst.SDKAs(t, config.DefaultEditorToken))
	id := authKeyCreate(t, editor, authKeyReq(project, saID, "by-editor", newData))
	upd, err := editor.Update(ctx, &iampb.UpdateAuthPublicKeyRequest{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: project, Name: "by-editor-2"}, Spec: &iampb.AuthPublicKeySpec{},
	})
	if err != nil {
		t.Fatalf("editor Update: %v", err)
	}
	authKeyWaitOp(t, "editor Update", func() error { _, err := upd.Wait(ctx); return err })
	deact, err := editor.Deactivate(ctx, &iampb.DeactivateAuthPublicKeyRequest{Id: id})
	if err != nil {
		t.Fatalf("editor Deactivate: %v", err)
	}
	authKeyWaitOp(t, "editor Deactivate", func() error { _, err := deact.Wait(ctx); return err })
	del, err := editor.Delete(ctx, &iampb.DeleteAuthPublicKeyRequest{Id: id})
	if err != nil {
		t.Fatalf("editor Delete: %v", err)
	}
	authKeyWaitOp(t, "editor Delete", func() error { _, err := del.Wait(ctx); return err })

	anon := iamsvc.NewAuthPublicKeyService(env.inst.SDKAs(t, ""))
	_, err = anon.Get(ctx, &iampb.GetAuthPublicKeyRequest{Id: keyID})
	assertCode(t, err, codes.Unauthenticated)
	_, err = anon.List(ctx, &iampb.ListAuthPublicKeyRequest{ParentId: project})
	assertCode(t, err, codes.Unauthenticated)
	_, err = anon.Create(ctx, authKeyReq(project, saID, "anon", newData))
	assertCode(t, err, codes.Unauthenticated)
}
