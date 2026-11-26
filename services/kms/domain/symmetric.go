package domain

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
)

const (
	// DefaultRotationPeriod is the rotation period of keys created without one
	// (90 days, per the Nebius docs).
	DefaultRotationPeriod = 90 * 24 * time.Hour
	// MinRotationPeriod is the shortest spec.rotation_period (1 day).
	MinRotationPeriod = 24 * time.Hour
	// MaxRotationPeriod is the longest spec.rotation_period (3650 days).
	MaxRotationPeriod = 3650 * 24 * time.Hour
	// maxManualRotations is the per-key limit on Rotate calls.
	maxManualRotations = 100
)

// SymmetricKeyService manages symmetric keys and performs symmetric crypto.
type SymmetricKeyService struct {
	lc *lifecycle[*kmspb.SymmetricKey]
}

// KeyDeps are the dependencies of a key service. All are required.
type KeyDeps[T any] struct {
	Keys      KeyRepository[T]
	Hierarchy Hierarchy
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
	Quota     QuotaGate
}

// NewSymmetricKeyService wires a SymmetricKeyService.
func NewSymmetricKeyService(d KeyDeps[*kmspb.SymmetricKey]) (*SymmetricKeyService, error) {
	lc := &lifecycle[*kmspb.SymmetricKey]{
		kind: KindSymmetricKey, label: "symmetric key",
		repo: d.Keys, ops: d.Ops, ids: d.IDs, hierarchy: d.Hierarchy, clk: d.Clock,
		quota: d.Quota, quotaName: quota.KMSSymmetricKeyCount,
		status: func(k *kmspb.SymmetricKey) keyStatus {
			s := k.GetStatus()
			return keyStatus{State: s.GetState(), DeletedAt: s.GetDeletedAt(), PurgeAt: s.GetPurgeAt()}
		},
		setStatus: func(k *kmspb.SymmetricKey, s keyStatus) {
			k.Status = &kmspb.SymmetricKeyStatus{State: s.State, DeletedAt: s.DeletedAt, PurgeAt: s.PurgeAt}
		},
	}
	if err := lc.validate(); err != nil {
		return nil, err
	}
	s := &SymmetricKeyService{lc: lc}
	lc.catchUp = s.rotateDue
	lc.restored = s.rotateOverdue
	return s, nil
}

// CreateSymmetricKeyCmd is the input of Create.
type CreateSymmetricKeyCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *kmspb.SymmetricKeySpec
	// Request is echoed in Operation.request.
	Request proto.Message
}

// UpdateSymmetricKeyCmd is the input of Update.
type UpdateSymmetricKeyCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *kmspb.SymmetricKeySpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// Get returns a key by ID.
func (s *SymmetricKeyService) Get(ctx context.Context, id string, vis Visibility) (*kmspb.SymmetricKey, error) {
	return s.lc.Get(ctx, id, vis)
}

// GetByName returns the ACTIVE key with the name in the project.
func (s *SymmetricKeyService) GetByName(ctx context.Context, parentID, name string) (*kmspb.SymmetricKey, error) {
	return s.lc.GetByName(ctx, parentID, name)
}

// List returns one page of the project's keys.
func (s *SymmetricKeyService) List(ctx context.Context, parentID string, pageSize int64, pageToken string, vis Visibility) (Page[*kmspb.SymmetricKey], error) {
	return s.lc.List(ctx, parentID, pageSize, pageToken, vis)
}

// Create generates key material and stores a new ACTIVE key.
func (s *SymmetricKeyService) Create(ctx context.Context, cmd CreateSymmetricKeyCmd) (*operations.Operation, error) {
	spec := proto.CloneOf(cmd.Spec)
	if spec == nil {
		spec = &kmspb.SymmetricKeySpec{}
	}
	if err := checkSymmetricAlgorithm("spec.algorithm", spec.GetAlgorithm()); err != nil {
		return nil, err
	}
	if err := defaultRotationPeriod(spec); err != nil {
		return nil, err
	}
	md, err := s.lc.newMetadata(ctx, cmd.Metadata)
	if err != nil {
		return nil, err
	}
	key := &kmspb.SymmetricKey{
		Metadata: md, Spec: spec,
		Status: &kmspb.SymmetricKeyStatus{State: kmspb.KeyState_ACTIVE},
	}
	return s.lc.submit(ctx, md.GetId(), "Create symmetric key", cmd.Request, func(ctx context.Context) error {
		m, err := newSymMaterial(defaultRand, spec.GetAlgorithm())
		if err != nil {
			return err
		}
		blob, err := encodeSymMaterial(m)
		if err != nil {
			return err
		}
		return s.lc.admitCreate(ctx, key, func(ctx context.Context) error {
			return s.lc.repo.Create(ctx, key, blob)
		})
	})
}

// Update changes the name, labels, description or rotation period.
func (s *SymmetricKeyService) Update(ctx context.Context, cmd UpdateSymmetricKeyCmd) (*operations.Operation, error) {
	patch := &kmspb.SymmetricKey{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     cmd.Spec,
	}
	return s.lc.update(ctx, cmd.Metadata, patch, cmd.ResetMask, cmd.Request, func(stored, updated *kmspb.SymmetricKey) error {
		if updated.GetSpec() == nil {
			updated.Spec = &kmspb.SymmetricKeySpec{}
		}
		alg := cmd.Spec.GetAlgorithm()
		if alg != kmspb.SymmetricAlgorithm_SYMMETRIC_ALGORITHM_UNSPECIFIED && alg != stored.GetSpec().GetAlgorithm() {
			return errs.BadRequest(errs.Violation{Field: "spec.algorithm", Message: "field is immutable"})
		}
		updated.Spec.Algorithm = stored.GetSpec().GetAlgorithm()
		return defaultRotationPeriod(updated.GetSpec())
	})
}

// Delete schedules the key for deletion.
func (s *SymmetricKeyService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.lc.Delete(ctx, id, request)
}

// UpdateDeletionDelay reschedules the purge of a key scheduled for deletion.
func (s *SymmetricKeyService) UpdateDeletionDelay(ctx context.Context, id string, delay time.Duration, request proto.Message) (*operations.Operation, error) {
	return s.lc.UpdateDeletionDelay(ctx, id, delay, request)
}

// Undelete restores a key scheduled for deletion.
func (s *SymmetricKeyService) Undelete(ctx context.Context, id, newName string, request proto.Message) (*operations.Operation, error) {
	return s.lc.Undelete(ctx, id, newName, request)
}

// Rotate adds a new primary key version. Older versions keep decrypting. Like
// every rotation it increments resource_version, and it restarts the
// automatic rotation schedule: the next automatic rotation is due
// rotation_period after this one.
func (s *SymmetricKeyService) Rotate(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.lc.submit(ctx, id, "Rotate symmetric key", request, func(ctx context.Context) error {
		for range maxWriteAttempts {
			k, err := s.lc.Get(ctx, id, ActiveOnly)
			if err != nil {
				return err
			}
			m, err := s.material(ctx, id)
			if err != nil {
				return err
			}
			if m.Rotations >= maxManualRotations {
				return errs.BadState(id, fmt.Sprintf("the number of manual rotations of a key can't exceed %d", maxManualRotations))
			}
			upd, next, err := rotateAt(k, m, s.lc.clk.Now())
			if err != nil {
				return err
			}
			next.Rotations++
			if err = s.save(ctx, k, upd, next); !errs.IsKind(err, errs.KindConflict) {
				return err
			}
		}
		return errConcurrent(id)
	})
}

// rotateDue performs the automatic rotations of an ACTIVE key that are due by
// now: one at every rotation_period boundary after the last rotation (or
// after creation, before the first one). Each is a full rotation (new primary
// version, resource_version + 1, updated_at = its time), exactly as a
// background rotator running at that boundary would have left the key.
func (s *SymmetricKeyService) rotateDue(ctx context.Context, k *kmspb.SymmetricKey) (*kmspb.SymmetricKey, error) {
	return s.autoRotate(ctx, k, dueRotations)
}

// rotateOverdue runs after Undelete. A key scheduled for deletion is not
// rotated, so if one or more rotations came due meanwhile the restored key
// rotates once, now, and its schedule restarts from there.
func (s *SymmetricKeyService) rotateOverdue(ctx context.Context, k *kmspb.SymmetricKey) error {
	_, err := s.autoRotate(ctx, k, func(first, now time.Time, _ time.Duration) []time.Time {
		if now.Before(first) {
			return nil
		}
		return []time.Time{now}
	})
	return err
}

// rotationPlan lists the times of the rotations to perform, given the first
// due time, now and the rotation period.
type rotationPlan func(first, now time.Time, period time.Duration) []time.Time

// dueRotations is every period boundary from first up to now.
func dueRotations(first, now time.Time, period time.Duration) []time.Time {
	var out []time.Time
	for at := first; !at.After(now); at = at.Add(period) {
		out = append(out, at)
	}
	return out
}

// firstDue is when the next automatic rotation of k is due.
func firstDue(k *kmspb.SymmetricKey, m symMaterial, period time.Duration) time.Time {
	last := m.RotatedAt
	if last.IsZero() {
		last = k.GetMetadata().GetCreatedAt().AsTime()
	}
	at := last.Add(period)
	// Every write catches up first, so a key updated after its due time had
	// its rotation_period shortened by that update: the new period applies
	// from then on, not retroactively.
	if u := k.GetMetadata().GetUpdatedAt().AsTime(); at.Before(u) {
		at = u
	}
	return at
}

// autoRotate applies the rotations plan selects to an ACTIVE key with a
// rotation period, retrying when another request changed the key first.
func (s *SymmetricKeyService) autoRotate(ctx context.Context, k *kmspb.SymmetricKey, plan rotationPlan) (*kmspb.SymmetricKey, error) {
	id := k.GetMetadata().GetId()
	for range maxWriteAttempts {
		period := k.GetSpec().GetRotationPeriod().AsDuration()
		if period <= 0 || k.GetStatus().GetState() != kmspb.KeyState_ACTIVE {
			return k, nil
		}
		m, err := s.material(ctx, id)
		if err != nil {
			return nil, err
		}
		times := plan(firstDue(k, m, period), s.lc.clk.Now(), period)
		if len(times) == 0 {
			return k, nil
		}
		upd, next := k, m
		for _, at := range times {
			if upd, next, err = rotateAt(upd, next, at); err != nil {
				return nil, err
			}
		}
		err = s.save(ctx, k, upd, next)
		if !errs.IsKind(err, errs.KindConflict) {
			if err != nil {
				return nil, err
			}
			return upd, nil
		}
		// Another request rotated or updated the key first: start over from
		// what it stored.
		if k, err = s.lc.repo.Get(ctx, id); err != nil {
			return nil, err
		}
	}
	return nil, errConcurrent(id)
}

// rotateAt returns k and m after one rotation at time at.
func rotateAt(k *kmspb.SymmetricKey, m symMaterial, at time.Time) (*kmspb.SymmetricKey, symMaterial, error) {
	next, err := m.rotate(defaultRand, at)
	if err != nil {
		return nil, symMaterial{}, err
	}
	upd := proto.CloneOf(k)
	upd.GetMetadata().ResourceVersion++
	upd.GetMetadata().UpdatedAt = timestamppb.New(at)
	return upd, next, nil
}

// save stores the rotated key and material if the stored key still equals
// read (errs.Conflict otherwise).
func (s *SymmetricKeyService) save(ctx context.Context, read, upd *kmspb.SymmetricKey, m symMaterial) error {
	blob, err := encodeSymMaterial(m)
	if err != nil {
		return err
	}
	return s.lc.repo.UpdateWithMaterial(ctx, upd, blob, read)
}

func (s *SymmetricKeyService) material(ctx context.Context, id string) (symMaterial, error) {
	blob, err := s.lc.repo.Material(ctx, id)
	if err != nil {
		return symMaterial{}, fmt.Errorf("material of %s: %w", id, err)
	}
	return decodeSymMaterial(blob)
}

// usableMaterial returns the material of an ACTIVE key.
func (s *SymmetricKeyService) usableMaterial(ctx context.Context, keyID string) (symMaterial, error) {
	if keyID == "" {
		return symMaterial{}, errs.BadRequest(errs.Violation{Field: "key_id", Message: "required"})
	}
	if _, err := s.lc.usable(ctx, keyID); err != nil {
		return symMaterial{}, err
	}
	return s.material(ctx, keyID)
}

// Encrypt encrypts plaintext (at most MaxPlaintextSize bytes) with the key's
// primary version. aad, if any, must be passed again to Decrypt. The
// ciphertext records the key version, so it still decrypts after rotation.
func (s *SymmetricKeyService) Encrypt(ctx context.Context, keyID string, plaintext, aad []byte) ([]byte, error) {
	if len(plaintext) > MaxPlaintextSize {
		return nil, errs.BadRequest(errs.Violation{Field: "plaintext", Message: fmt.Sprintf("must be at most %d bytes", MaxPlaintextSize)})
	}
	m, err := s.usableMaterial(ctx, keyID)
	if err != nil {
		return nil, err
	}
	return m.seal(defaultRand, keyID, plaintext, aad)
}

// Decrypt decrypts a ciphertext produced by Encrypt or GenerateDataKey with
// the same key and aad. Any mismatch or tampering is errs.BadRequest.
func (s *SymmetricKeyService) Decrypt(ctx context.Context, keyID string, ciphertext, aad []byte) ([]byte, error) {
	m, err := s.usableMaterial(ctx, keyID)
	if err != nil {
		return nil, err
	}
	pt, err := m.open(keyID, ciphertext, aad)
	if err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: "ciphertext", Message: err.Error()})
	}
	return pt, nil
}

// DataKey is a generated data encryption key.
type DataKey struct {
	// Plaintext is the raw data key, to encrypt data locally.
	Plaintext []byte
	// Ciphertext is the data key encrypted with the KMS key (decrypt it with
	// Decrypt and the same aad).
	Ciphertext []byte
}

// GenerateDataKey creates a random data key of spec and returns it in plain
// and wrapped (encrypted under keyID) form, for envelope encryption.
func (s *SymmetricKeyService) GenerateDataKey(ctx context.Context, keyID string, spec kmspb.SymmetricAlgorithm, aad []byte) (DataKey, error) {
	if err := checkSymmetricAlgorithm("data_key_spec", spec); err != nil {
		return DataKey{}, err
	}
	m, err := s.usableMaterial(ctx, keyID)
	if err != nil {
		return DataKey{}, err
	}
	size, err := symKeySize(spec)
	if err != nil {
		return DataKey{}, err
	}
	dk, err := randomBytes(defaultRand, size)
	if err != nil {
		return DataKey{}, err
	}
	wrapped, err := m.seal(defaultRand, keyID, dk, aad)
	if err != nil {
		return DataKey{}, err
	}
	return DataKey{Plaintext: dk, Ciphertext: wrapped}, nil
}

// checkSymmetricAlgorithm accepts AES_256 only: AES_128 is deprecated upstream
// ("it is impossible to create new keys with this algorithm").
func checkSymmetricAlgorithm(field string, alg kmspb.SymmetricAlgorithm) error {
	switch alg {
	case kmspb.SymmetricAlgorithm_AES_256:
		return nil
	case kmspb.SymmetricAlgorithm_AES_128:
		return errs.BadRequest(errs.Violation{Field: field, Message: "AES_128 is deprecated; use AES_256"})
	case kmspb.SymmetricAlgorithm_SYMMETRIC_ALGORITHM_UNSPECIFIED:
		return errs.BadRequest(errs.Violation{Field: field, Message: "required; supported: AES_256"})
	default:
		return errs.BadRequest(errs.Violation{Field: field, Message: "required; supported: AES_256"})
	}
}

// defaultRotationPeriod fills in the default and checks the range.
func defaultRotationPeriod(spec *kmspb.SymmetricKeySpec) error {
	if spec.GetRotationPeriod() == nil {
		spec.RotationPeriod = durationpb.New(DefaultRotationPeriod)
		return nil
	}
	if err := spec.GetRotationPeriod().CheckValid(); err != nil {
		return errs.BadRequest(errs.Violation{Field: "spec.rotation_period", Message: err.Error()})
	}
	if d := spec.GetRotationPeriod().AsDuration(); d < MinRotationPeriod || d > MaxRotationPeriod {
		return errs.BadRequest(errs.Violation{
			Field: "spec.rotation_period", Message: fmt.Sprintf("must be between %s and %s", MinRotationPeriod, MaxRotationPeriod),
		})
	}
	return nil
}
