package contract

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk"
	"github.com/nebius/gosdk/operations"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"
	kmssvc "github.com/nebius/gosdk/services/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/kms"
)

// kmsClients are the four KMS services of one SDK connection.
type kmsClients struct {
	sym        kmssvc.SymmetricKeyService
	asym       kmssvc.AsymmetricKeyService
	symCrypto  kmssvc.SymmetricCryptoService
	asymCrypto kmssvc.AsymmetricCryptoService
}

func newKMSClients(sdk *gosdk.SDK) kmsClients {
	return kmsClients{
		sym:        kmssvc.NewSymmetricKeyService(sdk),
		asym:       kmssvc.NewAsymmetricKeyService(sdk),
		symCrypto:  kmssvc.NewSymmetricCryptoService(sdk),
		asymCrypto: kmssvc.NewAsymmetricCryptoService(sdk),
	}
}

func startKMS(t *testing.T) (*testkit.Instance, kmsClients) {
	t.Helper()
	inst := testkit.StartT(t, []mocker.Module{iam.New(), kms.New()})
	return inst, newKMSClients(inst.SDK(t))
}

// kmsCode is the gRPC code of a failed call or of a failed operation.
func kmsCode(err error) codes.Code {
	if opErr, ok := errors.AsType[*operations.Error](err); ok {
		return opErr.Code
	}
	return status.Code(err)
}

func kmsWantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := kmsCode(err); got != want {
		t.Fatalf("code = %v, want %v (err: %v)", got, want, err)
	}
}

// kmsWait waits for an operation, returning the call error or the operation
// failure.
func kmsWait(ctx context.Context, op operations.Operation, err error) error {
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func kmsMustWait(t *testing.T, op operations.Operation, err error) {
	t.Helper()
	if err := kmsWait(t.Context(), op, err); err != nil {
		t.Fatalf("operation: %v", err)
	}
}

// kmsKey is the part of a symmetric or asymmetric key the lifecycle tests
// compare.
type kmsKey struct {
	id, name, description string
	parentID              string
	labels                map[string]string
	version               int64
	state                 kmspb.KeyState
	deletedAt, purgeAt    *timestamppb.Timestamp
	rotation              time.Duration
}

func kmsSymView(k *kmspb.SymmetricKey) kmsKey {
	md := k.GetMetadata()
	return kmsKey{
		id: md.GetId(), name: md.GetName(), parentID: md.GetParentId(), labels: md.GetLabels(),
		version: md.GetResourceVersion(), description: k.GetSpec().GetDescription(),
		state: k.GetStatus().GetState(), deletedAt: k.GetStatus().GetDeletedAt(), purgeAt: k.GetStatus().GetPurgeAt(),
		rotation: k.GetSpec().GetRotationPeriod().AsDuration(),
	}
}

func kmsAsymView(k *kmspb.AsymmetricKey) kmsKey {
	md := k.GetMetadata()
	return kmsKey{
		id: md.GetId(), name: md.GetName(), parentID: md.GetParentId(), labels: md.GetLabels(),
		version: md.GetResourceVersion(), description: k.GetSpec().GetDescription(),
		state: k.GetStatus().GetState(), deletedAt: k.GetStatus().GetDeletedAt(), purgeAt: k.GetStatus().GetPurgeAt(),
	}
}

// kmsKind abstracts the two key services so one lifecycle suite covers both.
type kmsKind struct {
	name string
	// create returns the call error and the operation (not yet waited).
	create func(ctx context.Context, parent, name string, labels map[string]string) (operations.Operation, error)
	get    func(ctx context.Context, id string, showScheduled bool) (kmsKey, error)
	byName func(ctx context.Context, parent, name string) (kmsKey, error)
	// list returns one page.
	list func(ctx context.Context, parent string, pageSize int64, token string, showScheduled bool) (keys []kmsKey, next string, err error)
	// update replaces name, labels and description (the SDK sends the reset
	// mask for everything left empty).
	update      func(ctx context.Context, k kmsKey, name string, labels map[string]string, description string) (operations.Operation, error)
	del         func(ctx context.Context, id string) (operations.Operation, error)
	setDelay    func(ctx context.Context, id string, d time.Duration) (operations.Operation, error)
	undelete    func(ctx context.Context, id, name string) (operations.Operation, error)
	changeAlgo  func(ctx context.Context, k kmsKey) (operations.Operation, error)
	hasRotation bool
}

func kmsSymKind(c kmsClients) kmsKind {
	return kmsKind{
		name: "symmetric", hasRotation: true,
		create: func(ctx context.Context, parent, name string, labels map[string]string) (operations.Operation, error) {
			return c.sym.Create(ctx, &kmspb.CreateSymmetricKeyRequest{
				Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name, Labels: labels},
				Spec:     &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256, Description: "created"},
			})
		},
		get: func(ctx context.Context, id string, show bool) (kmsKey, error) {
			k, err := c.sym.Get(ctx, &kmspb.GetSymmetricKeyRequest{Id: id, ShowScheduledForDeletion: show})
			return kmsSymView(k), err
		},
		byName: func(ctx context.Context, parent, name string) (kmsKey, error) {
			k, err := c.sym.GetByName(ctx, &kmspb.GetSymmetricKeyByNameRequest{ParentId: parent, Name: name})
			return kmsSymView(k), err
		},
		list: func(ctx context.Context, parent string, size int64, token string, show bool) ([]kmsKey, string, error) {
			r, err := c.sym.List(ctx, &kmspb.ListSymmetricKeysRequest{ParentId: parent, PageSize: size, PageToken: token, ShowScheduledForDeletion: show})
			var out []kmsKey
			for _, k := range r.GetItems() {
				out = append(out, kmsSymView(k))
			}
			return out, r.GetNextPageToken(), err
		},
		update: func(ctx context.Context, k kmsKey, name string, labels map[string]string, description string) (operations.Operation, error) {
			return c.sym.Update(ctx, &kmspb.UpdateSymmetricKeyRequest{
				Metadata: &commonpb.ResourceMetadata{Id: k.id, ParentId: k.parentID, Name: name, Labels: labels},
				Spec: &kmspb.SymmetricKeySpec{
					Algorithm: kmspb.SymmetricAlgorithm_AES_256, Description: description,
					RotationPeriod: durationpb.New(k.rotation),
				},
			})
		},
		del: func(ctx context.Context, id string) (operations.Operation, error) {
			return c.sym.Delete(ctx, &kmspb.DeleteSymmetricKeyRequest{Id: id})
		},
		setDelay: func(ctx context.Context, id string, d time.Duration) (operations.Operation, error) {
			return c.sym.UpdateDeletionDelay(ctx, &kmspb.UpdateSymmetricKeyDeletionDelayRequest{Id: id, DeletionDelay: durationpb.New(d)})
		},
		undelete: func(ctx context.Context, id, name string) (operations.Operation, error) {
			return c.sym.Undelete(ctx, &kmspb.UndeleteSymmetricKeyRequest{Id: id, Name: name})
		},
		changeAlgo: func(ctx context.Context, k kmsKey) (operations.Operation, error) {
			return c.sym.Update(ctx, &kmspb.UpdateSymmetricKeyRequest{
				Metadata: &commonpb.ResourceMetadata{Id: k.id, Name: k.name},
				Spec:     &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_128},
			})
		},
	}
}

func kmsAsymKind(c kmsClients) kmsKind {
	return kmsKind{
		name: "asymmetric",
		create: func(ctx context.Context, parent, name string, labels map[string]string) (operations.Operation, error) {
			return c.asym.Create(ctx, &kmspb.CreateAsymmetricKeyRequest{
				Metadata: &commonpb.ResourceMetadata{ParentId: parent, Name: name, Labels: labels},
				Spec:     &kmspb.AsymmetricKeySpec{Algorithm: kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256, Description: "created"},
			})
		},
		get: func(ctx context.Context, id string, show bool) (kmsKey, error) {
			k, err := c.asym.Get(ctx, &kmspb.GetAsymmetricKeyRequest{Id: id, ShowScheduledForDeletion: show})
			return kmsAsymView(k), err
		},
		byName: func(ctx context.Context, parent, name string) (kmsKey, error) {
			k, err := c.asym.GetByName(ctx, &kmspb.GetAsymmetricKeyByNameRequest{ParentId: parent, Name: name})
			return kmsAsymView(k), err
		},
		list: func(ctx context.Context, parent string, size int64, token string, show bool) ([]kmsKey, string, error) {
			r, err := c.asym.List(ctx, &kmspb.ListAsymmetricKeysRequest{ParentId: parent, PageSize: size, PageToken: token, ShowScheduledForDeletion: show})
			var out []kmsKey
			for _, k := range r.GetItems() {
				out = append(out, kmsAsymView(k))
			}
			return out, r.GetNextPageToken(), err
		},
		update: func(ctx context.Context, k kmsKey, name string, labels map[string]string, description string) (operations.Operation, error) {
			return c.asym.Update(ctx, &kmspb.UpdateAsymmetricKeyRequest{
				Metadata: &commonpb.ResourceMetadata{Id: k.id, ParentId: k.parentID, Name: name, Labels: labels},
				Spec: &kmspb.AsymmetricKeySpec{
					Algorithm: kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256, Description: description,
				},
			})
		},
		del: func(ctx context.Context, id string) (operations.Operation, error) {
			return c.asym.Delete(ctx, &kmspb.DeleteAsymmetricKeyRequest{Id: id})
		},
		setDelay: func(ctx context.Context, id string, d time.Duration) (operations.Operation, error) {
			return c.asym.UpdateDeletionDelay(ctx, &kmspb.UpdateAsymmetricKeyDeletionDelayRequest{Id: id, DeletionDelay: durationpb.New(d)})
		},
		undelete: func(ctx context.Context, id, name string) (operations.Operation, error) {
			return c.asym.Undelete(ctx, &kmspb.UndeleteAsymmetricKeyRequest{Id: id, Name: name})
		},
		changeAlgo: func(ctx context.Context, k kmsKey) (operations.Operation, error) {
			return c.asym.Update(ctx, &kmspb.UpdateAsymmetricKeyRequest{
				Metadata: &commonpb.ResourceMetadata{Id: k.id, Name: k.name},
				Spec:     &kmspb.AsymmetricKeySpec{Algorithm: kmspb.AsymmetricAlgorithm_ECDSA_NIST_P384_SHA_384},
			})
		},
	}
}

// mustCreate makes an ACTIVE key and returns its stored view.
func (k kmsKind) mustCreate(t *testing.T, parent, name string, labels map[string]string) kmsKey {
	t.Helper()
	op, err := k.create(t.Context(), parent, name, labels)
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	if op.ResourceID() == "" {
		t.Fatalf("Create(%q): empty resource_id", name)
	}
	kmsMustWait(t, op, nil)
	got, err := k.get(t.Context(), op.ResourceID(), false)
	if err != nil {
		t.Fatalf("Get(%s): %v", op.ResourceID(), err)
	}
	return got
}

func (k kmsKind) mustDelete(t *testing.T, id string) {
	t.Helper()
	op, err := k.del(t.Context(), id)
	kmsMustWait(t, op, err)
}

// kmsEachKind runs fn as a parallel subtest for symmetric and asymmetric keys,
// each against its own server.
func kmsEachKind(t *testing.T, fn func(t *testing.T, inst *testkit.Instance, k kmsKind)) {
	t.Helper()
	for _, name := range []string{"symmetric", "asymmetric"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inst, c := startKMS(t)
			k := kmsSymKind(c)
			if name == "asymmetric" {
				k = kmsAsymKind(c)
			}
			fn(t, inst, k)
		})
	}
}

func TestKMSKeyCreateGet(t *testing.T) {
	t.Parallel()
	kmsEachKind(t, func(t *testing.T, inst *testkit.Instance, k kmsKind) {
		t.Helper()
		ctx := t.Context()
		got := k.mustCreate(t, inst.ProjectID, "main", map[string]string{"team": "core"})
		if got.parentID != inst.ProjectID || got.name != "main" || got.labels["team"] != "core" {
			t.Errorf("created key = %+v", got)
		}
		if got.version != 1 {
			t.Errorf("resource_version = %d, want 1", got.version)
		}
		if got.state != kmspb.KeyState_ACTIVE || got.deletedAt != nil || got.purgeAt != nil {
			t.Errorf("status = %v deleted=%v purge=%v, want plain ACTIVE", got.state, got.deletedAt, got.purgeAt)
		}
		if got.description != "created" {
			t.Errorf("description = %q", got.description)
		}
		if k.hasRotation && got.rotation != 90*24*time.Hour {
			t.Errorf("default rotation_period = %v, want 90d", got.rotation)
		}

		byName, err := k.byName(ctx, inst.ProjectID, "main")
		if err != nil || byName.id != got.id {
			t.Errorf("GetByName = %+v, %v; want id %s", byName, err, got.id)
		}
		_, err = k.byName(ctx, inst.ProjectID, "nope")
		kmsWantCode(t, err, codes.NotFound)
		_, err = k.get(ctx, "nokey-e00doesnotexist", false)
		kmsWantCode(t, err, codes.NotFound)

		// Duplicate name in the same project.
		op, err := k.create(ctx, inst.ProjectID, "main", nil)
		kmsWantCode(t, kmsWait(ctx, op, err), codes.AlreadyExists)
	})
}

func TestKMSKeyCreateErrors(t *testing.T) {
	t.Parallel()
	inst, c := startKMS(t)
	ctx := t.Context()

	op, err := c.sym.Create(ctx, &kmspb.CreateSymmetricKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "weak"},
		Spec:     &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_128},
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.InvalidArgument)

	op, err = c.sym.Create(ctx, &kmspb.CreateSymmetricKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "short-rotation"},
		Spec: &kmspb.SymmetricKeySpec{
			Algorithm: kmspb.SymmetricAlgorithm_AES_256, RotationPeriod: durationpb.New(time.Hour),
		},
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.InvalidArgument)

	op, err = c.asym.Create(ctx, &kmspb.CreateAsymmetricKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "no-algo"},
		Spec:     &kmspb.AsymmetricKeySpec{},
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.InvalidArgument)

	// An unknown project is NOT_FOUND.
	const missing = "project-e00doesnotexist"
	op, err = c.sym.Create(ctx, &kmspb.CreateSymmetricKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: missing, Name: "orphan"},
		Spec:     &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256},
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.NotFound)
	op, err = c.asym.Create(ctx, &kmspb.CreateAsymmetricKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: missing, Name: "orphan"},
		Spec:     &kmspb.AsymmetricKeySpec{Algorithm: kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256},
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.NotFound)

	// A parent that is not a project is rejected.
	op, err = c.sym.Create(ctx, &kmspb.CreateSymmetricKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.TenantID, Name: "wrong-parent"},
		Spec:     &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256},
	})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.InvalidArgument)
}

func TestKMSKeyList(t *testing.T) {
	t.Parallel()
	kmsEachKind(t, func(t *testing.T, inst *testkit.Instance, k kmsKind) {
		t.Helper()
		ctx := t.Context()
		want := map[string]bool{}
		var victim string
		for _, n := range []string{"a", "b", "c"} {
			victim = k.mustCreate(t, inst.ProjectID, n, nil).id
			want[victim] = true
		}

		// Page through one key at a time.
		got := map[string]bool{}
		token := ""
		for range 10 {
			items, next, err := k.list(ctx, inst.ProjectID, 1, token, false)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(items) > 1 {
				t.Fatalf("page has %d items, want at most 1", len(items))
			}
			for _, it := range items {
				got[it.id] = true
			}
			if next == "" {
				break
			}
			token = next
		}
		if len(got) != len(want) {
			t.Fatalf("paged ids = %v, want %v", got, want)
		}
		for id := range want {
			if !got[id] {
				t.Errorf("id %s missing from paged list", id)
			}
		}
		_, _, err := k.list(ctx, inst.ProjectID, 100000, "", false)
		kmsWantCode(t, err, codes.InvalidArgument)

		// Scheduled keys are hidden unless asked for.
		k.mustDelete(t, victim)
		items, _, err := k.list(ctx, inst.ProjectID, 0, "", false)
		if err != nil || len(items) != 2 {
			t.Fatalf("List = %d items, %v; want 2", len(items), err)
		}
		items, _, err = k.list(ctx, inst.ProjectID, 0, "", true)
		if err != nil || len(items) != 3 {
			t.Fatalf("List(show_scheduled) = %d items, %v; want 3", len(items), err)
		}
	})
}

func TestKMSKeyUpdate(t *testing.T) {
	t.Parallel()
	kmsEachKind(t, func(t *testing.T, inst *testkit.Instance, k kmsKind) {
		t.Helper()
		ctx := t.Context()
		key := k.mustCreate(t, inst.ProjectID, "upd", map[string]string{"a": "1", "b": "2"})

		op, err := k.update(ctx, key, "upd2", map[string]string{"a": "1"}, "changed")
		kmsMustWait(t, op, err)
		got, err := k.get(ctx, key.id, false)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.name != "upd2" || got.description != "changed" {
			t.Errorf("after update: name=%q description=%q", got.name, got.description)
		}
		if len(got.labels) != 1 || got.labels["a"] != "1" {
			t.Errorf("labels = %v, want only a=1 (b reset)", got.labels)
		}
		if got.version != 2 {
			t.Errorf("resource_version = %d, want 2", got.version)
		}
		if got.state != kmspb.KeyState_ACTIVE {
			t.Errorf("state = %v after update, want ACTIVE", got.state)
		}

		// Clearing every label through the reset mask.
		op, err = k.update(ctx, got, "upd2", nil, "changed")
		kmsMustWait(t, op, err)
		if got, err = k.get(ctx, key.id, false); err != nil || len(got.labels) != 0 {
			t.Errorf("labels after reset = %v, %v; want none", got.labels, err)
		}

		// The algorithm is immutable.
		op, err = k.changeAlgo(ctx, got)
		kmsWantCode(t, kmsWait(ctx, op, err), codes.InvalidArgument)

		// Renaming onto another key's name fails.
		other := k.mustCreate(t, inst.ProjectID, "other", nil)
		op, err = k.update(ctx, other, "upd2", nil, "")
		kmsWantCode(t, kmsWait(ctx, op, err), codes.AlreadyExists)
	})
}

func TestKMSSymmetricUpdateRotationPeriod(t *testing.T) {
	t.Parallel()
	inst, c := startKMS(t)
	k := kmsSymKind(c)
	ctx := t.Context()
	key := k.mustCreate(t, inst.ProjectID, "rot", nil)

	key.rotation = 30 * 24 * time.Hour
	op, err := k.update(ctx, key, "rot", nil, "")
	kmsMustWait(t, op, err)
	got, err := k.get(ctx, key.id, false)
	if err != nil || got.rotation != 30*24*time.Hour {
		t.Fatalf("rotation_period = %v, %v; want 720h", got.rotation, err)
	}

	key.rotation = time.Hour
	op, err = k.update(ctx, key, "rot", nil, "")
	kmsWantCode(t, kmsWait(ctx, op, err), codes.InvalidArgument)
}

func TestKMSKeyDeleteUndelete(t *testing.T) {
	t.Parallel()
	kmsEachKind(t, func(t *testing.T, inst *testkit.Instance, k kmsKind) {
		t.Helper()
		ctx := t.Context()
		key := k.mustCreate(t, inst.ProjectID, "doomed", nil)

		// Delay changes and undelete need a scheduled key.
		op, err := k.setDelay(ctx, key.id, 48*time.Hour)
		kmsWantCode(t, kmsWait(ctx, op, err), codes.FailedPrecondition)
		op, err = k.undelete(ctx, key.id, "")
		kmsWantCode(t, kmsWait(ctx, op, err), codes.FailedPrecondition)

		k.mustDelete(t, key.id)
		_, err = k.get(ctx, key.id, false)
		kmsWantCode(t, err, codes.NotFound)
		_, err = k.byName(ctx, inst.ProjectID, "doomed")
		kmsWantCode(t, err, codes.NotFound)

		sched, err := k.get(ctx, key.id, true)
		if err != nil {
			t.Fatalf("Get(show_scheduled): %v", err)
		}
		if sched.state != kmspb.KeyState_SCHEDULED_FOR_DELETION {
			t.Fatalf("state = %v, want SCHEDULED_FOR_DELETION", sched.state)
		}
		if sched.deletedAt == nil || sched.purgeAt == nil {
			t.Fatalf("deleted_at=%v purge_at=%v, want both set", sched.deletedAt, sched.purgeAt)
		}
		if d := sched.purgeAt.AsTime().Sub(sched.deletedAt.AsTime()); d != 30*24*time.Hour {
			t.Errorf("purge_at - deleted_at = %v, want 720h", d)
		}

		// Moving the purge time.
		op, err = k.setDelay(ctx, key.id, 2*24*time.Hour)
		kmsMustWait(t, op, err)
		moved, err := k.get(ctx, key.id, true)
		if err != nil {
			t.Fatalf("Get after delay: %v", err)
		}
		if d := moved.purgeAt.AsTime().Sub(sched.deletedAt.AsTime()); d < 2*24*time.Hour || d > 2*24*time.Hour+time.Minute {
			t.Errorf("purge_at - deleted_at = %v after delay update, want about 48h", d)
		}
		for _, bad := range []time.Duration{time.Hour, 31 * 24 * time.Hour, 0} {
			op, err = k.setDelay(ctx, key.id, bad)
			kmsWantCode(t, kmsWait(ctx, op, err), codes.InvalidArgument)
		}

		// The freed name can be taken by a new key; undelete then conflicts.
		taken := k.mustCreate(t, inst.ProjectID, "doomed", nil)
		op, err = k.undelete(ctx, key.id, "")
		kmsWantCode(t, kmsWait(ctx, op, err), codes.AlreadyExists)
		op, err = k.undelete(ctx, key.id, "revived")
		kmsMustWait(t, op, err)
		back, err := k.get(ctx, key.id, false)
		if err != nil {
			t.Fatalf("Get after undelete: %v", err)
		}
		if back.state != kmspb.KeyState_ACTIVE || back.name != "revived" || back.deletedAt != nil || back.purgeAt != nil {
			t.Errorf("after undelete: %+v, want ACTIVE named revived with no deletion times", back)
		}
		items, _, err := k.list(ctx, inst.ProjectID, 0, "", false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		ids := map[string]bool{}
		for _, it := range items {
			ids[it.id] = true
		}
		if len(ids) != 2 || !ids[key.id] || !ids[taken.id] {
			t.Errorf("List ids = %v, want %s and %s", ids, key.id, taken.id)
		}

		// A key that is already scheduled cannot be deleted again.
		k.mustDelete(t, key.id)
		op, err = k.del(ctx, key.id)
		kmsWantCode(t, kmsWait(ctx, op, err), codes.NotFound)
	})
}

func TestKMSSymmetricRotate(t *testing.T) {
	t.Parallel()
	inst, c := startKMS(t)
	ctx := t.Context()
	k := kmsSymKind(c)
	key := k.mustCreate(t, inst.ProjectID, "rotating", nil)

	op, err := c.sym.Rotate(ctx, &kmspb.RotateSymmetricKeyRequest{Id: key.id})
	kmsMustWait(t, op, err)
	op, err = c.sym.Rotate(ctx, &kmspb.RotateSymmetricKeyRequest{Id: "symkey-e00doesnotexist"})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.NotFound)

	k.mustDelete(t, key.id)
	op, err = c.sym.Rotate(ctx, &kmspb.RotateSymmetricKeyRequest{Id: key.id})
	kmsWantCode(t, kmsWait(ctx, op, err), codes.NotFound)
}
