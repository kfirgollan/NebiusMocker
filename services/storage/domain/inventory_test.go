package domain_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

const (
	invSrc = "storagebucket-src"
	invDst = "storagebucket-dst"
	invFS  = "storagebucket-fs"
)

type invFixture struct {
	svc     *domain.InventoryService
	repo    *invRepo
	buckets *fakeBuckets
	writer  *invWriter
	clk     *clock.Manual
}

type invOpt func(*domain.InventoryDeps)

func newInvFixture(t *testing.T, opts ...invOpt) *invFixture {
	t.Helper()
	f := &invFixture{repo: newInvRepo(), buckets: &fakeBuckets{}, writer: &invWriter{}, clk: clock.NewManual(t0)}
	f.addBucket(t.Context(), invSrc, storagepb.BucketStatus_ACTIVE, storagepb.BucketStatus_REGULAR)
	f.addBucket(t.Context(), invDst, storagepb.BucketStatus_ACTIVE, storagepb.BucketStatus_REGULAR)
	f.addBucket(t.Context(), invFS, storagepb.BucketStatus_ACTIVE, storagepb.BucketStatus_FILESYSTEM)
	d := domain.InventoryDeps{Repo: f.repo, Buckets: f.buckets, Writer: f.writer, Ops: inlineOps{}, IDs: &seqIDs{}, Clock: f.clk}
	for _, o := range opts {
		o(&d)
	}
	svc, err := domain.NewInventoryService(d)
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func (f *invFixture) addBucket(ctx context.Context, id string, state storagepb.BucketStatus_State, typ storagepb.BucketStatus_BucketType) {
	b := &storagepb.Bucket{
		Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: projectID, Name: id},
		Spec:     &storagepb.BucketSpec{},
		Status:   &storagepb.BucketStatus{State: state, BucketType: typ},
	}
	if typ == storagepb.BucketStatus_FILESYSTEM {
		b.Spec.BucketType = &storagepb.BucketSpec_FilesystemBucket{FilesystemBucket: &storagepb.BucketSpec_FilesystemBucketType{FilesystemId: "fs"}}
	}
	_ = f.buckets.Create(ctx, b)
}

func invSpec() *storagepb.InventorySpec {
	return &storagepb.InventorySpec{
		Enabled: true, DestinationBucketId: invDst, DestinationPrefix: "reports",
		OutputFormat: storagepb.InventorySpec_CSV_GZIP, Schedule: storagepb.InventorySpec_DAILY,
	}
}

func (f *invFixture) create(t *testing.T, name string, spec *storagepb.InventorySpec) *storagepb.Inventory {
	t.Helper()
	inv, err := f.tryCreate(t.Context(), name, spec)
	if err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	return inv
}

func (f *invFixture) tryCreate(ctx context.Context, name string, spec *storagepb.InventorySpec) (*storagepb.Inventory, error) {
	op, err := f.svc.Create(ctx, domain.CreateInventoryCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: invSrc, Name: name}, Spec: spec,
	})
	if err != nil {
		return nil, err
	}
	if op.Err != nil {
		return nil, op.Err
	}
	return f.svc.Get(ctx, op.ResourceID)
}

func (f *invFixture) update(ctx context.Context, id string, spec *storagepb.InventorySpec, reset ...string) error {
	var rm *mask.Mask
	if len(reset) > 0 {
		rm = mask.ParseMust(strings.Join(reset, ","))
	}
	op, err := f.svc.Update(ctx, domain.UpdateInventoryCmd{Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: spec, ResetMask: rm})
	if err != nil {
		return err
	}
	return op.Err
}

func invViolations(t *testing.T, err error) []errs.Violation {
	t.Helper()
	e, ok := errors.AsType[*errs.Error](err)
	if !ok || e.Kind != errs.KindBadRequest {
		t.Fatalf("error = %v, want BadRequest", err)
	}
	return e.Violations
}

func TestInventoryCreateStoresSpecAsSent(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	spec := invSpec()
	spec.SourcePrefix = "logs/"
	spec.NoncurrentVersions = true
	spec.OptionalFields = []storagepb.InventorySpec_OptionalField{storagepb.InventorySpec_TAGS, storagepb.InventorySpec_LAST_ACCESS}
	inv := f.create(t, "daily-report", spec)
	if !proto.Equal(inv.GetSpec(), spec) {
		t.Errorf("spec = %v, want %v", inv.GetSpec(), spec)
	}
	md := inv.GetMetadata()
	if md.GetParentId() != invSrc || md.GetResourceVersion() != 1 || !md.GetCreatedAt().AsTime().Equal(t0) {
		t.Errorf("metadata = %v", md)
	}
	if !proto.Equal(inv.GetStatus(), &storagepb.InventoryStatus{}) {
		t.Errorf("status = %v, want empty", inv.GetStatus())
	}
	got, err := f.svc.GetByName(t.Context(), invSrc, "daily-report")
	if err != nil || got.GetMetadata().GetId() != md.GetId() {
		t.Errorf("GetByName = %v, %v", got, err)
	}
}

func TestInventoryCreateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		parent string
		inv    string
		mutate func(*storagepb.InventorySpec)
		field  string
		kind   errs.Kind
	}{
		{name: "no parent", parent: "", inv: "a", field: "metadata.parent_id"},
		{name: "parent not a bucket", parent: "project-x", inv: "a", field: "metadata.parent_id"},
		{name: "filesystem source", parent: invFS, inv: "a", field: "metadata.parent_id"},
		{name: "missing source", parent: "storagebucket-nope", inv: "a", kind: errs.KindNotFound},
		{name: "no name", parent: invSrc, inv: "", field: "metadata.name"},
		{name: "long name", parent: invSrc, inv: strings.Repeat("a", 65), field: "metadata.name"},
		{name: "bad name chars", parent: invSrc, inv: "a/b", field: "metadata.name"},
		{name: "no destination", parent: invSrc, inv: "a", mutate: func(s *storagepb.InventorySpec) { s.DestinationBucketId = "" }, field: "spec.destination_bucket_id"},
		{name: "destination not found", parent: invSrc, inv: "a", mutate: func(s *storagepb.InventorySpec) { s.DestinationBucketId = "storagebucket-nope" }, field: "spec.destination_bucket_id"},
		{name: "destination filesystem", parent: invSrc, inv: "a", mutate: func(s *storagepb.InventorySpec) { s.DestinationBucketId = invFS }, field: "spec.destination_bucket_id"},
		{name: "long prefix", parent: invSrc, inv: "a", mutate: func(s *storagepb.InventorySpec) { s.DestinationPrefix = strings.Repeat("p", 257) }, field: "spec.destination_prefix"},
		{name: "no format", parent: invSrc, inv: "a", mutate: func(s *storagepb.InventorySpec) { s.OutputFormat = 0 }, field: "spec.output_format"},
		{name: "no schedule", parent: invSrc, inv: "a", mutate: func(s *storagepb.InventorySpec) { s.Schedule = 0 }, field: "spec.schedule"},
		{name: "unspecified optional field", parent: invSrc, inv: "a", field: "spec.optional_fields[1]", mutate: func(s *storagepb.InventorySpec) {
			s.OptionalFields = []storagepb.InventorySpec_OptionalField{storagepb.InventorySpec_TAGS, 0}
		}},
		{name: "duplicate optional field", parent: invSrc, inv: "a", field: "spec.optional_fields[1]", mutate: func(s *storagepb.InventorySpec) {
			s.OptionalFields = []storagepb.InventorySpec_OptionalField{storagepb.InventorySpec_TAGS, storagepb.InventorySpec_TAGS}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newInvFixture(t)
			spec := invSpec()
			if tc.mutate != nil {
				tc.mutate(spec)
			}
			op, err := f.svc.Create(t.Context(), domain.CreateInventoryCmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: tc.parent, Name: tc.inv}, Spec: spec,
			})
			if op != nil {
				t.Fatalf("Create succeeded: %+v", op)
			}
			if tc.kind != errs.KindUnknown {
				if !errs.IsKind(err, tc.kind) {
					t.Fatalf("error = %v, want kind %v", err, tc.kind)
				}
				return
			}
			vs := invViolations(t, err)
			if len(vs) == 0 || vs[0].Field != tc.field {
				t.Errorf("violations = %v, want field %s", vs, tc.field)
			}
		})
	}
}

func TestInventoryFilesystemSourceMessage(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	_, err := f.svc.Create(t.Context(), domain.CreateInventoryCmd{Metadata: &commonpb.ResourceMetadata{ParentId: invFS, Name: "a"}, Spec: invSpec()})
	if vs := invViolations(t, err); !strings.Contains(vs[0].Message, "inventories of filesystem buckets are not supported") {
		t.Errorf("message = %q", vs[0].Message)
	}
}

func TestInventorySourceMustBeActive(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	f.addBucket(t.Context(), "storagebucket-del", storagepb.BucketStatus_SCHEDULED_FOR_DELETION, storagepb.BucketStatus_REGULAR)
	_, err := f.svc.Create(t.Context(), domain.CreateInventoryCmd{Metadata: &commonpb.ResourceMetadata{ParentId: "storagebucket-del", Name: "a"}, Spec: invSpec()})
	if !errs.IsKind(err, errs.KindBadState) {
		t.Errorf("error = %v, want BadState", err)
	}
}

func TestInventoryNameUniquenessAndLimit(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	f.create(t, "one", invSpec())
	if _, err := f.tryCreate(t.Context(), "one", invSpec()); !errs.IsKind(err, errs.KindAlreadyExists) {
		t.Errorf("duplicate name: %v, want AlreadyExists", err)
	}
	for _, n := range []string{"two", "three", "four", "five"} {
		f.create(t, n, invSpec())
	}
	_, err := f.tryCreate(t.Context(), "six", invSpec())
	if !errs.IsKind(err, errs.KindQuotaExceeded) || !strings.Contains(err.Error(), "5") {
		t.Errorf("sixth inventory: %v, want QuotaExceeded naming the limit of 5", err)
	}
	// The limit is per bucket: the destination bucket still has room.
	op, err := f.svc.Create(t.Context(), domain.CreateInventoryCmd{Metadata: &commonpb.ResourceMetadata{ParentId: invDst, Name: "one"}, Spec: invSpec()})
	if err != nil || op.Err != nil {
		t.Errorf("same name in another bucket: %v, %v", err, op)
	}
}

func TestInventoryUpdate(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	inv := f.create(t, "u", invSpec())
	id := inv.GetMetadata().GetId()
	f.clk.Advance(time.Minute)

	next := invSpec()
	next.Schedule = storagepb.InventorySpec_WEEKLY
	next.OptionalFields = []storagepb.InventorySpec_OptionalField{storagepb.InventorySpec_TAGS}
	if err := f.update(t.Context(), id, next); err != nil {
		t.Fatal(err)
	}
	got, _ := f.svc.Get(t.Context(), id)
	if got.GetSpec().GetSchedule() != storagepb.InventorySpec_WEEKLY || len(got.GetSpec().GetOptionalFields()) != 1 {
		t.Errorf("spec = %v", got.GetSpec())
	}
	if got.GetMetadata().GetResourceVersion() != 2 || !got.GetMetadata().GetUpdatedAt().AsTime().Equal(t0.Add(time.Minute)) {
		t.Errorf("metadata = %v", got.GetMetadata())
	}

	// A repeated update is a no-op and keeps the version.
	if err := f.update(t.Context(), id, next); err != nil {
		t.Fatal(err)
	}
	if again, _ := f.svc.Get(t.Context(), id); again.GetMetadata().GetResourceVersion() != 2 {
		t.Errorf("version after no-op update = %d, want 2", again.GetMetadata().GetResourceVersion())
	}

	// Disabling needs the reset mask because false is the zero value.
	off := invSpec()
	off.Enabled = false
	if err := f.update(t.Context(), id, off, "spec.enabled"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.Get(t.Context(), id); got.GetSpec().GetEnabled() {
		t.Error("inventory still enabled after reset of spec.enabled")
	}
}

func TestInventoryUpdateRules(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	inv := f.create(t, "r", invSpec())
	id := inv.GetMetadata().GetId()
	submit := func(md *commonpb.ResourceMetadata, spec *storagepb.InventorySpec) error {
		_, err := f.svc.Update(t.Context(), domain.UpdateInventoryCmd{Metadata: md, Spec: spec})
		return err
	}
	err := submit(&commonpb.ResourceMetadata{Id: id, Name: "other"}, invSpec())
	if vs := invViolations(t, err); vs[0].Field != "metadata.name" || !strings.Contains(vs[0].Message, "immutable") {
		t.Errorf("rename: %v", vs)
	}
	err = submit(&commonpb.ResourceMetadata{Id: id, ParentId: invDst}, invSpec())
	if vs := invViolations(t, err); vs[0].Field != "metadata.parent_id" {
		t.Errorf("move: %v", vs)
	}
	if err := submit(&commonpb.ResourceMetadata{Id: id, ResourceVersion: 9}, invSpec()); !errs.IsKind(err, errs.KindConflict) {
		t.Errorf("stale resource_version: %v, want Conflict", err)
	}
	bad := invSpec()
	bad.DestinationBucketId = "storagebucket-nope"
	if err := submit(&commonpb.ResourceMetadata{Id: id}, bad); invViolations(t, err)[0].Field != "spec.destination_bucket_id" {
		t.Errorf("bad destination: %v", err)
	}
	if err := submit(&commonpb.ResourceMetadata{}, invSpec()); invViolations(t, err)[0].Field != "metadata.id" {
		t.Errorf("no id: %v", err)
	}
	if err := submit(&commonpb.ResourceMetadata{Id: "storageinventory-nope"}, invSpec()); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("unknown id: %v, want NotFound", err)
	}
}

func TestInventoryDisableWithDeletedDestination(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	id := f.create(t, "d", invSpec()).GetMetadata().GetId()
	if err := f.buckets.Delete(t.Context(), invDst); err != nil {
		t.Fatal(err)
	}
	off := invSpec()
	off.Enabled = false
	if err := f.update(t.Context(), id, off, "spec.enabled"); err != nil {
		t.Errorf("disabling an inventory whose destination is gone: %v", err)
	}
}

func TestInventoryDelete(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	id := f.create(t, "x", invSpec()).GetMetadata().GetId()
	if op, err := f.svc.Delete(t.Context(), id, nil); err != nil || op.Err != nil {
		t.Fatalf("Delete: %v, %v", err, op)
	}
	if _, err := f.svc.Get(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("Get after delete: %v", err)
	}
	if _, err := f.svc.Delete(t.Context(), id, nil); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("second delete: %v, want NotFound", err)
	}
}

func TestInventoryList(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	f.create(t, "a", invSpec())
	f.create(t, "b", invSpec())
	page, err := f.svc.List(t.Context(), invSrc, 0, "")
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("List = %v, %v", page, err)
	}
	if _, err := f.svc.List(t.Context(), invSrc, 1001, ""); invViolations(t, err)[0].Field != "page_size" {
		t.Errorf("page_size 1001: %v", err)
	}
	if _, err := f.svc.List(t.Context(), "storagebucket-nope", 0, ""); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("unknown bucket: %v, want NotFound", err)
	}
}

// invChecker allows the permissions in allow for the listed principals.
type invChecker struct{ allow map[string]bool }

func (c invChecker) Check(_ context.Context, principal string, perm authz.Permission, target string) (authz.Decision, error) {
	return authz.Decision{Allowed: perm == domain.PermObjectWrite && c.allow[principal+"@"+target], Permission: perm, Target: target, Reason: "no grant"}, nil
}

func invAs(t *testing.T, principal string) context.Context {
	t.Helper()
	return reqctx.WithInfo(t.Context(), &reqctx.Info{Principal: reqctx.Principal{ID: principal}})
}

func TestInventoryDestinationPermission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mode    authz.Mode
		allowed bool
		same    bool
		wantErr bool
	}{
		{name: "enforce denied", mode: authz.ModeEnforce, wantErr: true},
		{name: "enforce allowed", mode: authz.ModeEnforce, allowed: true},
		{name: "enforce same bucket needs nothing", mode: authz.ModeEnforce, same: true},
		{name: "audit denied only logs", mode: authz.ModeAudit},
		{name: "off skips", mode: authz.ModeOff},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			allow := map[string]bool{}
			if tc.allowed {
				allow["user-1@"+invDst] = true
			}
			f := newInvFixture(t, func(d *domain.InventoryDeps) { d.Access, d.Mode = invChecker{allow: allow}, tc.mode })
			spec := invSpec()
			if tc.same {
				spec.DestinationBucketId = invSrc
			}
			_, err := f.tryCreate(invAs(t, "user-1"), "p", spec)
			if tc.wantErr != errs.IsKind(err, errs.KindPermissionDenied) || (!tc.wantErr && err != nil) {
				t.Errorf("error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// invState reads the scheduler state through the repository.
func (f *invFixture) state(t *testing.T, id string) domain.InventoryState {
	t.Helper()
	st, err := f.repo.State(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (f *invFixture) runDue(t *testing.T) domain.InventoryRunResult {
	t.Helper()
	r, err := f.svc.RunDue(t.Context())
	if err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	return r
}

func TestInventorySchedulerDailyAndWeekly(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	daily := f.create(t, "daily", invSpec()).GetMetadata().GetId()
	weeklySpec := invSpec()
	weeklySpec.Schedule = storagepb.InventorySpec_WEEKLY
	weekly := f.create(t, "weekly", weeklySpec).GetMetadata().GetId()
	if st := f.state(t, daily); !st.NextRun.Equal(t0) || st.CreatedBy != "" {
		t.Errorf("initial state = %+v", st)
	}

	// First report at the next sweep.
	if r := f.runDue(t); r.Reports != 2 || f.writer.count() != 2 {
		t.Fatalf("first sweep = %+v, writes %d", r, f.writer.count())
	}
	d, _ := f.svc.Get(t.Context(), daily)
	if !d.GetStatus().GetLastSuccess().AsTime().Equal(t0) || d.GetStatus().GetErrorCode() != storagepb.InventoryStatus_NO_ERROR {
		t.Errorf("status = %v", d.GetStatus())
	}
	if d.GetMetadata().GetResourceVersion() != 1 {
		t.Errorf("status-only write changed resource_version to %d", d.GetMetadata().GetResourceVersion())
	}
	if st := f.state(t, daily); !st.NextRun.Equal(t0.Add(24*time.Hour)) || !st.LastRun.Equal(t0) {
		t.Errorf("daily state = %+v", st)
	}
	if st := f.state(t, weekly); !st.NextRun.Equal(t0.Add(7 * 24 * time.Hour)) {
		t.Errorf("weekly state = %+v", st)
	}

	f.clk.Advance(23 * time.Hour)
	if r := f.runDue(t); r != (domain.InventoryRunResult{}) {
		t.Errorf("sweep before the period = %+v", r)
	}
	f.clk.Advance(time.Hour)
	if r := f.runDue(t); r.Reports != 1 {
		t.Errorf("sweep after a day = %+v, want 1 report", r)
	}
	f.clk.Advance(6 * 24 * time.Hour)
	if r := f.runDue(t); r.Reports != 2 { // the daily one again plus the weekly one
		t.Errorf("sweep after a week = %+v, want 2 reports", r)
	}
	last := f.writer.calls[len(f.writer.calls)-1]
	if !last.Now.Equal(f.clk.Now()) || last.Source.GetMetadata().GetId() != invSrc || last.Destination.GetMetadata().GetId() != invDst {
		t.Errorf("report source = %+v", last)
	}
}

func TestInventorySchedulerSkips(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	off := invSpec()
	off.Enabled = false
	f.create(t, "off", off)
	f.addBucket(t.Context(), "storagebucket-gone", storagepb.BucketStatus_ACTIVE, storagepb.BucketStatus_REGULAR)
	op, err := f.svc.Create(t.Context(), domain.CreateInventoryCmd{Metadata: &commonpb.ResourceMetadata{ParentId: "storagebucket-gone", Name: "x"}, Spec: invSpec()})
	if err != nil || op.Err != nil {
		t.Fatal(err, op)
	}
	b, _ := f.buckets.Get(t.Context(), "storagebucket-gone")
	b.Status.State = storagepb.BucketStatus_SCHEDULED_FOR_DELETION
	_ = f.buckets.Update(t.Context(), b)
	if r := f.runDue(t); r != (domain.InventoryRunResult{}) || f.writer.count() != 0 {
		t.Errorf("sweep = %+v, writes %d; disabled configs and soft-deleted sources are skipped", r, f.writer.count())
	}
	inv, _ := f.svc.GetByName(t.Context(), "storagebucket-gone", "x")
	if inv.GetStatus().GetErrorCode() != storagepb.InventoryStatus_NO_ERROR {
		t.Errorf("a skipped source set an error: %v", inv.GetStatus())
	}
}

func TestInventoryReEnable(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	id := f.create(t, "e", invSpec()).GetMetadata().GetId()
	f.runDue(t) // LastRun = t0
	off := invSpec()
	off.Enabled = false
	if err := f.update(t.Context(), id, off, "spec.enabled"); err != nil {
		t.Fatal(err)
	}

	f.clk.Advance(2 * time.Hour)
	if r := f.runDue(t); r != (domain.InventoryRunResult{}) {
		t.Fatalf("disabled config ran: %+v", r)
	}
	// Re-enabled within the period: continues on schedule, no immediate report.
	if err := f.update(t.Context(), id, invSpec()); err != nil {
		t.Fatal(err)
	}
	if st := f.state(t, id); !st.NextRun.Equal(t0.Add(24 * time.Hour)) {
		t.Errorf("NextRun after early re-enable = %v, want %v", st.NextRun, t0.Add(24*time.Hour))
	}
	if r := f.runDue(t); r.Reports != 0 {
		t.Errorf("early re-enable generated a report: %+v", r)
	}
	// Disabled for longer than the period: due at the next sweep.
	if err := f.update(t.Context(), id, off, "spec.enabled"); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(48 * time.Hour)
	if err := f.update(t.Context(), id, invSpec()); err != nil {
		t.Fatal(err)
	}
	if st := f.state(t, id); !st.NextRun.Equal(f.clk.Now()) {
		t.Errorf("NextRun after late re-enable = %v, want now %v", st.NextRun, f.clk.Now())
	}
}

func TestInventoryEnableNeverRun(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	off := invSpec()
	off.Enabled = false
	id := f.create(t, "n", off).GetMetadata().GetId()
	f.clk.Advance(time.Hour)
	if err := f.update(t.Context(), id, invSpec()); err != nil {
		t.Fatal(err)
	}
	if st := f.state(t, id); !st.NextRun.Equal(f.clk.Now()) {
		t.Errorf("NextRun = %v, want now", st.NextRun)
	}
	if r := f.runDue(t); r.Reports != 1 {
		t.Errorf("sweep = %+v", r)
	}
}

func TestInventoryScheduleChange(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	id := f.create(t, "s", invSpec()).GetMetadata().GetId()
	f.runDue(t)
	f.clk.Advance(time.Hour)
	weekly := invSpec()
	weekly.Schedule = storagepb.InventorySpec_WEEKLY
	ctx := invAs(t, "user-9")
	if err := f.update(ctx, id, weekly); err != nil {
		t.Fatal(err)
	}
	st := f.state(t, id)
	if !st.NextRun.Equal(t0.Add(7*24*time.Hour)) || st.CreatedBy != "user-9" {
		t.Errorf("state = %+v, want NextRun last+7d and CreatedBy user-9", st)
	}
}

func TestInventoryRunFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		werr   error
		setup  func(context.Context, *invFixture)
		want   storagepb.InventoryStatus_Error
		writes int
	}{
		{name: "destination deleted", setup: func(ctx context.Context, f *invFixture) { _ = f.buckets.Delete(ctx, invDst) }, want: storagepb.InventoryStatus_DESTINATION_BUCKET_NOT_FOUND},
		{name: "destination soft-deleted", setup: func(ctx context.Context, f *invFixture) {
			b, _ := f.buckets.Get(ctx, invDst)
			b.Status.State = storagepb.BucketStatus_SCHEDULED_FOR_DELETION
			_ = f.buckets.Update(ctx, b)
		}, want: storagepb.InventoryStatus_DESTINATION_BUCKET_NOT_FOUND},
		{name: "writer quota", werr: &errs.Error{Kind: errs.KindQuotaExceeded}, want: storagepb.InventoryStatus_QUOTA_EXCEEDED, writes: 1},
		{name: "writer not found", werr: errs.NotFound("dst"), want: storagepb.InventoryStatus_DESTINATION_BUCKET_NOT_FOUND, writes: 1},
		{name: "writer denied", werr: errs.PermissionDenied("dst"), want: storagepb.InventoryStatus_PERMISSION_DENIED, writes: 1},
		{name: "writer other keeps status", werr: errors.New("boom"), want: storagepb.InventoryStatus_NO_ERROR, writes: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newInvFixture(t)
			id := f.create(t, "f", invSpec()).GetMetadata().GetId()
			f.writer.err = tc.werr
			if tc.setup != nil {
				tc.setup(t.Context(), f)
			}
			if r := f.runDue(t); r.Failures != 1 || r.Reports != 0 {
				t.Fatalf("sweep = %+v, want one failure", r)
			}
			got, _ := f.svc.Get(t.Context(), id)
			if got.GetStatus().GetErrorCode() != tc.want || got.GetStatus().GetLastSuccess() != nil || got.GetMetadata().GetResourceVersion() != 1 {
				t.Errorf("status = %v (version %d), want %v", got.GetStatus(), got.GetMetadata().GetResourceVersion(), tc.want)
			}
			if f.writer.count() != tc.writes {
				t.Errorf("writes = %d, want %d", f.writer.count(), tc.writes)
			}
			if st := f.state(t, id); !st.LastRun.Equal(t0) || !st.NextRun.Equal(t0.Add(domain.InventoryRetryInterval)) {
				t.Errorf("state = %+v, want retry in %v", st, domain.InventoryRetryInterval)
			}
		})
	}
}

func TestInventoryRecoversAfterFailure(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	id := f.create(t, "rec", invSpec()).GetMetadata().GetId()
	f.writer.err = &errs.Error{Kind: errs.KindQuotaExceeded}
	f.runDue(t)
	f.writer.err = nil
	f.clk.Advance(domain.InventoryRetryInterval)
	if r := f.runDue(t); r.Reports != 1 {
		t.Fatalf("retry = %+v", r)
	}
	got, _ := f.svc.Get(t.Context(), id)
	if got.GetStatus().GetErrorCode() != storagepb.InventoryStatus_NO_ERROR || got.GetStatus().GetLastSuccess() == nil {
		t.Errorf("status after recovery = %v", got.GetStatus())
	}
}

func TestInventoryRunPermissionOfCreator(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mode    authz.Mode
		want    storagepb.InventoryStatus_Error
		reports int
	}{
		{name: "enforce", mode: authz.ModeEnforce, want: storagepb.InventoryStatus_PERMISSION_DENIED},
		{name: "audit logs and writes", mode: authz.ModeAudit, reports: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			allow := map[string]bool{"user-1@" + invDst: true}
			f := newInvFixture(t, func(d *domain.InventoryDeps) { d.Access, d.Mode = invChecker{allow: allow}, authz.ModeEnforce })
			id := mustInv(t)(f.tryCreate(invAs(t, "user-1"), "c", invSpec())).GetMetadata().GetId()
			// The creator loses the grant; a fresh service with the mode under test runs the sweep.
			svc, err := domain.NewInventoryService(domain.InventoryDeps{
				Repo: f.repo, Buckets: f.buckets, Writer: f.writer, Ops: inlineOps{}, IDs: &seqIDs{}, Clock: f.clk,
				Access: invChecker{allow: map[string]bool{}}, Mode: tc.mode,
			})
			if err != nil {
				t.Fatal(err)
			}
			r, err := svc.RunDue(t.Context())
			if err != nil || r.Reports != tc.reports {
				t.Fatalf("RunDue = %+v, %v", r, err)
			}
			got, _ := f.svc.Get(t.Context(), id)
			if got.GetStatus().GetErrorCode() != tc.want {
				t.Errorf("error_code = %v, want %v", got.GetStatus().GetErrorCode(), tc.want)
			}
		})
	}
}

func TestInventorySweepOrphans(t *testing.T) {
	t.Parallel()
	f := newInvFixture(t)
	f.create(t, "keep", invSpec())
	op, err := f.svc.Create(t.Context(), domain.CreateInventoryCmd{Metadata: &commonpb.ResourceMetadata{ParentId: invDst, Name: "orphan"}, Spec: invSpec()})
	if err != nil || op.Err != nil {
		t.Fatal(err, op)
	}
	if err := f.buckets.Delete(t.Context(), invDst); err != nil {
		t.Fatal(err)
	}
	n, err := f.svc.SweepOrphans(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("SweepOrphans = %d, %v, want 1", n, err)
	}
	if _, err := f.svc.Get(t.Context(), op.ResourceID); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("orphan still there: %v", err)
	}
	if page, _ := f.svc.List(t.Context(), invSrc, 0, ""); len(page.Items) != 1 {
		t.Errorf("remaining = %d, want 1", len(page.Items))
	}
}

func TestNewInventoryServiceRequiresDeps(t *testing.T) {
	t.Parallel()
	if _, err := domain.NewInventoryService(domain.InventoryDeps{}); err == nil {
		t.Error("NewInventoryService with no deps succeeded")
	}
}

func mustInv(t *testing.T) func(*storagepb.Inventory, error) *storagepb.Inventory {
	t.Helper()
	return func(inv *storagepb.Inventory, err error) *storagepb.Inventory {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return inv
	}
}
