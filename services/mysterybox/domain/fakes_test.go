package domain_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox/domain"
)

const projectID = "project-e00mock000000000default"

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const testPurgeDelay = 72 * time.Hour

// fakeSecrets is a slice-backed domain.SecretRepository. Only active secrets
// hold their name, like the real repository.
type fakeSecrets struct{ items []*mysteryboxpb.Secret }

func (f *fakeSecrets) index(id string) int {
	return slices.IndexFunc(f.items, func(s *mysteryboxpb.Secret) bool { return s.GetMetadata().GetId() == id })
}

func (f *fakeSecrets) active(parentID, name string) *mysteryboxpb.Secret {
	for _, s := range f.items {
		md := s.GetMetadata()
		if md.GetParentId() == parentID && md.GetName() == name &&
			s.GetStatus().GetState() != mysteryboxpb.SecretStatus_SCHEDULED_FOR_DELETION {
			return s
		}
	}
	return nil
}

func (f *fakeSecrets) Get(_ context.Context, id string) (*mysteryboxpb.Secret, error) {
	i := f.index(id)
	if i < 0 {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(f.items[i]), nil
}

func (f *fakeSecrets) GetByName(_ context.Context, parentID, name string) (*mysteryboxpb.Secret, error) {
	s := f.active(parentID, name)
	if s == nil {
		return nil, errs.NotFound(name)
	}
	return proto.CloneOf(s), nil
}

func (f *fakeSecrets) List(_ context.Context, parentID string, _ int, _ string) (domain.SecretPage, error) {
	var out []*mysteryboxpb.Secret
	for _, s := range f.items {
		if s.GetMetadata().GetParentId() == parentID {
			out = append(out, proto.CloneOf(s))
		}
	}
	return domain.SecretPage{Items: out}, nil
}

func (f *fakeSecrets) Create(_ context.Context, s *mysteryboxpb.Secret) error {
	md := s.GetMetadata()
	if other := f.active(md.GetParentId(), md.GetName()); other != nil {
		return errs.AlreadyExists(other.GetMetadata().GetId(), "name taken")
	}
	f.items = append(f.items, proto.CloneOf(s))
	return nil
}

func (f *fakeSecrets) Update(_ context.Context, s *mysteryboxpb.Secret) error {
	md := s.GetMetadata()
	i := f.index(md.GetId())
	if i < 0 {
		return errs.NotFound(md.GetId())
	}
	if s.GetStatus().GetState() != mysteryboxpb.SecretStatus_SCHEDULED_FOR_DELETION {
		if other := f.active(md.GetParentId(), md.GetName()); other != nil && other.GetMetadata().GetId() != md.GetId() {
			return errs.AlreadyExists(other.GetMetadata().GetId(), "name taken")
		}
	}
	f.items[i] = proto.CloneOf(s)
	return nil
}

func (f *fakeSecrets) Delete(_ context.Context, id string) error {
	i := f.index(id)
	if i < 0 {
		return errs.NotFound(id)
	}
	f.items = slices.Delete(f.items, i, i+1)
	return nil
}

// fakeVersions is a slice-backed domain.VersionRepository (creation order).
type fakeVersions struct{ items []*mysteryboxpb.SecretVersion }

func (f *fakeVersions) index(id string) int {
	return slices.IndexFunc(f.items, func(v *mysteryboxpb.SecretVersion) bool { return v.GetMetadata().GetId() == id })
}

func (f *fakeVersions) Get(_ context.Context, id string) (*mysteryboxpb.SecretVersion, error) {
	i := f.index(id)
	if i < 0 {
		return nil, errs.NotFound(id)
	}
	return proto.CloneOf(f.items[i]), nil
}

func (f *fakeVersions) All(_ context.Context, secretID string) ([]*mysteryboxpb.SecretVersion, error) {
	var out []*mysteryboxpb.SecretVersion
	for _, v := range f.items {
		if v.GetMetadata().GetParentId() == secretID {
			out = append(out, proto.CloneOf(v))
		}
	}
	return out, nil
}

func (f *fakeVersions) List(ctx context.Context, secretID string, _ int, _ string) (domain.VersionPage, error) {
	items, err := f.All(ctx, secretID)
	return domain.VersionPage{Items: items}, err
}

func (f *fakeVersions) Create(_ context.Context, v *mysteryboxpb.SecretVersion) error {
	if f.index(v.GetMetadata().GetId()) >= 0 {
		return errs.AlreadyExists(v.GetMetadata().GetId(), "exists")
	}
	f.items = append(f.items, proto.CloneOf(v))
	return nil
}

func (f *fakeVersions) Update(_ context.Context, v *mysteryboxpb.SecretVersion) error {
	i := f.index(v.GetMetadata().GetId())
	if i < 0 {
		return errs.NotFound(v.GetMetadata().GetId())
	}
	f.items[i] = proto.CloneOf(v)
	return nil
}

func (f *fakeVersions) Delete(_ context.Context, id string) error {
	i := f.index(id)
	if i < 0 {
		return errs.NotFound(id)
	}
	f.items = slices.Delete(f.items, i, i+1)
	return nil
}

// fakePayloads is a map-backed domain.PayloadRepository holding sealed bytes.
type fakePayloads struct{ sealed map[string][]byte }

// versions counts the sealed version payloads (keys are version IDs).
func (f *fakePayloads) versions() int {
	n := 0
	for k := range f.sealed {
		if strings.HasPrefix(k, domain.KindSecretVersion+"-") {
			n++
		}
	}
	return n
}

// hasInline reports whether the secret's sealed inline-version record exists.
func (f *fakePayloads) hasInline(secretID string) bool {
	_, ok := f.sealed[secretID]
	return ok
}

func (f *fakePayloads) Put(_ context.Context, versionID, _ string, sealed []byte) error {
	f.sealed[versionID] = bytes.Clone(sealed)
	return nil
}

func (f *fakePayloads) Get(_ context.Context, versionID string) ([]byte, error) {
	b, ok := f.sealed[versionID]
	if !ok {
		return nil, errs.NotFound(versionID)
	}
	return bytes.Clone(b), nil
}

func (f *fakePayloads) Delete(_ context.Context, versionID string) error {
	if _, ok := f.sealed[versionID]; !ok {
		return errs.NotFound(versionID)
	}
	delete(f.sealed, versionID)
	return nil
}

const (
	sealPrefix = "sealed:"
	sealXOR    = 0x5a
	fakeKeyID  = "kmssymkey-e00test000000000001"
)

// fakeSealer is a reversible, aad-binding sealer: prefix || aad || 0 || xor(plain).
type fakeSealer struct{}

func (fakeSealer) KeyID() string { return fakeKeyID }

func (fakeSealer) Seal(_ context.Context, plaintext, aad []byte) ([]byte, error) {
	out := append([]byte(sealPrefix), aad...)
	out = append(out, 0)
	for _, b := range plaintext {
		out = append(out, b^sealXOR)
	}
	return out, nil
}

func (fakeSealer) Open(_ context.Context, sealed, aad []byte) ([]byte, error) {
	head := append([]byte(sealPrefix), aad...)
	head = append(head, 0)
	rest, ok := bytes.CutPrefix(sealed, head)
	if !ok {
		return nil, errors.New("fake sealer: bad prefix or aad")
	}
	out := make([]byte, len(rest))
	for i, b := range rest {
		out[i] = b ^ sealXOR
	}
	return out, nil
}

// fakeHierarchy knows exactly one project.
type fakeHierarchy struct{}

func (fakeHierarchy) Parent(_ context.Context, id string) (string, error) {
	if id != projectID {
		return "", errs.NotFound(id)
	}
	return "tenant-e00mock000000000default", nil
}

// inlineOps runs Prepare and Work immediately, like a zero-delay engine.
type inlineOps struct{}

func (inlineOps) Submit(ctx context.Context, s operations.Spec) (*operations.Operation, error) {
	if s.Prepare != nil {
		if err := s.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	op := &operations.Operation{
		ID: "op-" + s.ResourceID, Description: s.Description, CreatedBy: s.CreatedBy,
		ResourceID: s.ResourceID, Request: s.Request, CreatedAt: t0, FinishedAt: t0,
	}
	if s.Work != nil {
		op.Err = s.Work(ctx)
	}
	return op, nil
}

// env is a Service wired to fakes.
type env struct {
	svc      *domain.Service
	secrets  *fakeSecrets
	versions *fakeVersions
	payloads *fakePayloads
	clk      *clock.Manual
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvLimits(t, domain.Limits{})
}

func newEnvLimits(t *testing.T, l domain.Limits) *env {
	t.Helper()
	return newEnvGate(t, l, passGate{})
}

// passGate admits every claim and just runs apply.
type passGate struct{}

func (passGate) Admit(ctx context.Context, _ peer.QuotaClaim, apply func(ctx context.Context) error) error {
	return apply(ctx)
}

func newEnvGate(t *testing.T, l domain.Limits, gate domain.QuotaGate) *env {
	t.Helper()
	ids, err := nid.NewGenerator(nid.Sequential, "e00", 0)
	if err != nil {
		t.Fatalf("nid.NewGenerator: %v", err)
	}
	if l.PurgeDelay == 0 {
		l.PurgeDelay = testPurgeDelay
	}
	e := &env{
		secrets:  &fakeSecrets{},
		versions: &fakeVersions{},
		payloads: &fakePayloads{sealed: map[string][]byte{}},
		clk:      clock.NewManual(t0),
	}
	e.svc, err = domain.NewService(domain.Deps{
		Secrets: e.secrets, Versions: e.versions, Payloads: e.payloads, Sealer: fakeSealer{},
		Hierarchy: fakeHierarchy{}, Ops: inlineOps{}, IDs: ids, Clock: e.clk, Quota: gate, Limits: l,
	})
	if err != nil {
		t.Fatalf("domain.NewService: %v", err)
	}
	return e
}

func strEntry(key, value string) *mysteryboxpb.Payload {
	return &mysteryboxpb.Payload{Key: key, Payload: &mysteryboxpb.Payload_StringValue{StringValue: value}}
}

func binEntry(key string, value []byte) *mysteryboxpb.Payload {
	return &mysteryboxpb.Payload{Key: key, Payload: &mysteryboxpb.Payload_BinaryValue{BinaryValue: value}}
}

// done checks that an operation was accepted and succeeded and returns its
// resource ID. Use as done(t)(svc.Method(...)).
func done(t *testing.T) func(*operations.Operation, error) string {
	t.Helper()
	return func(op *operations.Operation, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("operation: unexpected error: %v", err)
		}
		if op.Err != nil {
			t.Fatalf("operation %s failed: %v", op.ID, op.Err)
		}
		return op.ResourceID
	}
}

// newSecret creates a secret named name; with entries it carries an inline
// first version.
func (e *env) newSecret(t *testing.T, name string, entries ...*mysteryboxpb.Payload) string {
	t.Helper()
	spec := &mysteryboxpb.SecretSpec{Description: "desc"}
	if len(entries) > 0 {
		spec.SecretVersion = &mysteryboxpb.SecretVersionSpec{Description: "first", Payload: entries}
	}
	op, err := e.svc.CreateSecret(t.Context(), domain.CreateSecretCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: name}, Spec: spec,
	})
	return done(t)(op, err)
}

// newVersion adds a version to secretID.
func (e *env) newVersion(t *testing.T, secretID string, setPrimary bool, entries ...*mysteryboxpb.Payload) string {
	t.Helper()
	op, err := e.svc.CreateVersion(t.Context(), domain.CreateVersionCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: secretID},
		Spec:     &mysteryboxpb.SecretVersionSpec{Payload: entries, SetPrimary: setPrimary},
	})
	return done(t)(op, err)
}

// secret reads a secret, failing the test on error.
func (e *env) secret(t *testing.T, id string) *mysteryboxpb.Secret {
	t.Helper()
	s, err := e.svc.GetSecret(t.Context(), id, false)
	if err != nil {
		t.Fatalf("GetSecret(%s): %v", id, err)
	}
	return s
}

// primary returns the secret's primary version ID.
func (e *env) primary(t *testing.T, id string) string {
	t.Helper()
	return e.secret(t, id).GetSpec().GetPrimaryVersionId()
}
