package contract

import (
	"encoding/binary"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/kms"
)

// TestKMSAutomaticRotation drives rotation_period through the official gosdk
// on a manual clock: keys rotate when the period elapses, crypto follows the
// new primary version, old ciphertexts keep decrypting, and manual Rotate and
// Update(rotation_period) reschedule.
func TestKMSAutomaticRotation(t *testing.T) {
	t.Parallel()
	const day = 24 * time.Hour
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewManual(start)
	inst := testkit.StartT(t, []mocker.Module{iam.New(), kms.New()}, testkit.WithClock(clk))
	c := newKMSClients(inst.SDK(t))
	ctx := t.Context()

	spec := func(period time.Duration) *kmspb.SymmetricKeySpec {
		return &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256, RotationPeriod: durationpb.New(period)}
	}
	op, err := c.sym.Create(ctx, &kmspb.CreateSymmetricKeyRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "auto"},
		Spec:     spec(2 * day),
	})
	kmsMustWait(t, op, err)
	id := op.ResourceID()
	aad := []byte("ctx")
	version := func(ct []byte) uint32 { return binary.BigEndian.Uint32(ct[1:5]) }
	wantKey := func(rv int64, updated time.Time) {
		t.Helper()
		k, err := c.sym.Get(ctx, &kmspb.GetSymmetricKeyRequest{Id: id})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		md := k.GetMetadata()
		if md.GetResourceVersion() != rv || !md.GetUpdatedAt().AsTime().Equal(updated) {
			t.Fatalf("resource_version, updated_at = %d, %v; want %d, %v", md.GetResourceVersion(), md.GetUpdatedAt().AsTime(), rv, updated)
		}
	}

	v1 := kmsEncrypt(t, c, id, aad, []byte("one"))
	if version(v1) != 1 {
		t.Fatalf("first ciphertext version = %d", version(v1))
	}
	clk.Advance(2*day - time.Second)
	wantKey(1, start)

	// The period elapses: the key rotates and Encrypt uses version 2.
	clk.Advance(time.Second)
	v2 := kmsEncrypt(t, c, id, aad, []byte("two"))
	if version(v2) != 2 {
		t.Fatalf("ciphertext version after the period = %d, want 2", version(v2))
	}
	wantKey(2, start.Add(2*day))
	for ct, want := range map[string]string{string(v1): "one", string(v2): "two"} {
		if got, err := kmsDecrypt(t, c, id, aad, []byte(ct)); err != nil || string(got) != want {
			t.Errorf("Decrypt(version %d) = %q, %v; want %q", version([]byte(ct)), got, err, want)
		}
	}

	// GetByName and List see the rotation too.
	clk.Advance(2 * day)
	byName, err := c.sym.GetByName(ctx, &kmspb.GetSymmetricKeyByNameRequest{ParentId: inst.ProjectID, Name: "auto"})
	if err != nil || byName.GetMetadata().GetResourceVersion() != 3 {
		t.Fatalf("GetByName = %v, %v; want resource_version 3", byName.GetMetadata(), err)
	}
	list, err := c.sym.List(ctx, &kmspb.ListSymmetricKeysRequest{ParentId: inst.ProjectID})
	if err != nil || len(list.GetItems()) != 1 || list.GetItems()[0].GetMetadata().GetResourceVersion() != 3 {
		t.Fatalf("List = %v, %v; want one key at resource_version 3", list.GetItems(), err)
	}

	// A manual Rotate restarts the schedule.
	clk.Advance(day)
	op, err = c.sym.Rotate(ctx, &kmspb.RotateSymmetricKeyRequest{Id: id})
	kmsMustWait(t, op, err)
	wantKey(4, start.Add(5*day))
	clk.Advance(day) // 6d: would have been a boundary of the old schedule
	wantKey(4, start.Add(5*day))
	clk.Advance(day) // 7d = manual rotation + 2d
	wantKey(5, start.Add(7*day))

	// Lengthening rotation_period moves the next rotation out.
	k, err := c.sym.Get(ctx, &kmspb.GetSymmetricKeyRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	op, err = c.sym.Update(ctx, &kmspb.UpdateSymmetricKeyRequest{Metadata: k.GetMetadata(), Spec: spec(10 * day)})
	kmsMustWait(t, op, err)
	wantKey(6, start.Add(7*day))
	clk.Advance(10*day - time.Second)
	wantKey(6, start.Add(7*day))
	clk.Advance(time.Second)
	wantKey(7, start.Add(17*day))
	if ct := kmsEncrypt(t, c, id, aad, []byte("x")); version(ct) != 6 {
		t.Errorf("ciphertext version = %d, want 6", version(ct))
	}
	if got, err := kmsDecrypt(t, c, id, aad, v1); err != nil || string(got) != "one" {
		t.Errorf("Decrypt(version 1) after 5 rotations = %q, %v", got, err)
	}
}
