package domain

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	fieldmaskpb "github.com/nebius/gosdk/proto/fieldmask/protobuf"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// Bucket inventories (https://docs.nebius.com/object-storage/buckets/inventory):
// a configuration lives under its SOURCE bucket and makes the mocker write a
// periodic report of the bucket's objects into a destination bucket. The
// exact time of a report is not guaranteed: the module sweep (every mocker
// minute) writes the reports that are due.

const (
	inventoryNameMax   = 64
	inventoryPrefixMax = 256
	dailyPeriod        = 24 * time.Hour
	weeklyPeriod       = 7 * dailyPeriod
)

// inventoryName allows what is safe inside a report object key.
var inventoryName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// InventoryDeps are the dependencies of an InventoryService.
type InventoryDeps struct {
	Repo    InventoryRepository
	Buckets BucketRepository
	Writer  ReportWriter
	Ops     Operations
	IDs     IDGenerator
	Clock   clock.Clock
	// Access and Mode authorize writing into a destination bucket other than
	// the source (PermObjectWrite); a nil Access skips the check.
	Access AccessChecker
	Mode   authz.Mode
	// Log receives audit-mode warnings and report failures; nil discards them.
	Log *slog.Logger
}

// InventoryService implements nebius.storage.v1.InventoryService and the
// report scheduler.
type InventoryService struct {
	repo    InventoryRepository
	buckets BucketRepository
	writer  ReportWriter
	ops     Operations
	ids     IDGenerator
	clk     clock.Clock
	access  AccessChecker
	mode    authz.Mode
	log     *slog.Logger
}

// NewInventoryService wires an InventoryService.
func NewInventoryService(d InventoryDeps) (*InventoryService, error) {
	if d.Repo == nil || d.Buckets == nil || d.Writer == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("storage: inventory service dependencies must not be nil")
	}
	return &InventoryService{
		repo: d.Repo, buckets: d.Buckets, writer: d.Writer, ops: d.Ops, ids: d.IDs, clk: d.Clock,
		access: d.Access, mode: d.Mode, log: cmp.Or(d.Log, slog.New(slog.DiscardHandler)),
	}, nil
}

// CreateInventoryCmd is the input of Create.
type CreateInventoryCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *storagepb.InventorySpec
	Request  proto.Message
}

// UpdateInventoryCmd is the input of Update (full replace guarded by a reset mask).
type UpdateInventoryCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *storagepb.InventorySpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// InventoryRunResult summarizes one scheduler sweep.
type InventoryRunResult struct {
	// Reports is how many reports were written, Failures how many attempts
	// failed (their status carries the error code and they retry later).
	Reports, Failures int
}

// Get returns an inventory by ID.
func (s *InventoryService) Get(ctx context.Context, id string) (*storagepb.Inventory, error) {
	return s.repo.Get(ctx, id)
}

// GetByName returns the inventory named name of a source bucket.
func (s *InventoryService) GetByName(ctx context.Context, bucketID, name string) (*storagepb.Inventory, error) {
	return s.repo.GetByName(ctx, bucketID, name)
}

// List returns one page of a bucket's inventories in creation order.
func (s *InventoryService) List(ctx context.Context, bucketID string, pageSize int64, pageToken string) (InventoryPage, error) {
	if pageSize < 0 || pageSize > maxPageSize {
		return InventoryPage{}, errs.BadRequest(errs.Violation{Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxPageSize)})
	}
	if _, err := s.buckets.Get(ctx, bucketID); err != nil {
		return InventoryPage{}, err
	}
	return s.repo.List(ctx, bucketID, cmp.Or(int(pageSize), defaultPageSize), pageToken)
}

// Create validates the request and stores the configuration; the first
// report is written by the next sweep.
func (s *InventoryService) Create(ctx context.Context, cmd CreateInventoryCmd) (*operations.Operation, error) {
	by := reqctx.PrincipalFrom(ctx).ID
	inv, err := s.newInventory(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  inv.GetMetadata().GetId(),
		Description: "Create inventory",
		CreatedBy:   by,
		Request:     cmd.Request,
		// The record is stored synchronously: the repository enforces name
		// uniqueness and the per-bucket limit atomically, so parallel creates
		// (Terraform count) get their error from the Create call itself.
		Prepare: func(ctx context.Context) error {
			if err := s.checkRoom(ctx, inv); err != nil {
				return err
			}
			return s.repo.Create(ctx, inv, InventoryState{NextRun: s.clk.Now(), CreatedBy: by})
		},
		Work: func(context.Context) error { return nil },
	})
}

// Update applies a full-replace update with reset-mask semantics. Reports
// generated after the change use the new settings.
func (s *InventoryService) Update(ctx context.Context, cmd UpdateInventoryCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	by := reqctx.PrincipalFrom(ctx).ID
	var (
		updated *storagepb.Inventory
		state   InventoryState
	)
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Update inventory",
		CreatedBy:   by,
		Request:     cmd.Request,
		Prepare: func(ctx context.Context) error {
			var err error
			updated, state, err = s.prepareUpdate(ctx, cmd, by)
			return err
		},
		Work: func(ctx context.Context) error {
			if err := s.repo.Update(ctx, updated); err != nil {
				return err
			}
			return s.repo.SetState(ctx, id, state)
		},
	})
}

// Delete removes a configuration; reports already delivered stay.
func (s *InventoryService) Delete(ctx context.Context, id string, req proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete inventory",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     req,
		Prepare: func(ctx context.Context) error {
			_, err := s.repo.Get(ctx, id)
			return err
		},
		Work: func(ctx context.Context) error { return s.repo.Delete(ctx, id) },
	})
}

func (s *InventoryService) newInventory(ctx context.Context, cmd CreateInventoryCmd) (*storagepb.Inventory, error) {
	md := cmd.Metadata
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	if err := s.checkSource(ctx, md.GetParentId()); err != nil {
		return nil, err
	}
	if err := validateInventoryName(md.GetName()); err != nil {
		return nil, err
	}
	if cmd.Spec == nil {
		return nil, errs.BadRequest(errs.Violation{Field: "spec", Message: "required"})
	}
	spec := proto.CloneOf(cmd.Spec)
	if err := s.validateInventorySpec(ctx, md.GetParentId(), nil, spec); err != nil {
		return nil, err
	}
	id, err := s.ids.New(KindInventory)
	if err != nil {
		return nil, fmt.Errorf("new inventory id: %w", err)
	}
	now := timestamppb.New(s.clk.Now())
	return &storagepb.Inventory{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: md.GetParentId(), Name: md.GetName(), Labels: maps.Clone(md.GetLabels()),
			ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec:   spec,
		Status: &storagepb.InventoryStatus{},
	}, nil
}

// checkSource requires an ACTIVE regular source bucket.
func (s *InventoryService) checkSource(ctx context.Context, bucketID string) error {
	if bucketID == "" {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if kindOf(bucketID) != KindBucket {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "must be a bucket ID"})
	}
	b, err := s.buckets.Get(ctx, bucketID)
	if err != nil {
		return err
	}
	if st := b.GetStatus().GetState(); st != storagepb.BucketStatus_ACTIVE {
		return errs.BadState(bucketID, "bucket is "+st.String()+", inventories can only be added to ACTIVE buckets")
	}
	if IsFilesystemBucket(b) {
		return errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "inventories of filesystem buckets are not supported"})
	}
	return nil
}

func validateInventoryName(name string) error {
	bad := func(msg string) error { return errs.BadRequest(errs.Violation{Field: "metadata.name", Message: msg}) }
	switch {
	case name == "":
		return bad("required")
	case len(name) > inventoryNameMax:
		return bad(fmt.Sprintf("must be at most %d characters", inventoryNameMax))
	case !inventoryName.MatchString(name):
		return bad("may contain only letters, digits, '.', '_' and '-'")
	}
	return nil
}

// validateInventorySpec checks the spec's fields and its destination. The
// destination of an update (before != nil) is only re-checked when it
// changed, so a configuration whose destination was deleted can still be
// disabled. The caller needs PermObjectWrite on a usable destination other
// than the source.
func (s *InventoryService) validateInventorySpec(ctx context.Context, sourceID string, before, spec *storagepb.InventorySpec) error {
	var vs []errs.Violation
	add := func(field, msg string) { vs = append(vs, errs.Violation{Field: field, Message: msg}) }
	if len(spec.GetDestinationPrefix()) > inventoryPrefixMax {
		add("spec.destination_prefix", fmt.Sprintf("must be at most %d characters", inventoryPrefixMax))
	}
	if spec.GetOutputFormat() == storagepb.InventorySpec_OUTPUT_FORMAT_UNSPECIFIED {
		add("spec.output_format", "required")
	}
	if spec.GetSchedule() == storagepb.InventorySpec_SCHEDULE_UNSPECIFIED {
		add("spec.schedule", "required")
	}
	seen := map[storagepb.InventorySpec_OptionalField]bool{}
	for i, f := range spec.GetOptionalFields() {
		field := fmt.Sprintf("spec.optional_fields[%d]", i)
		switch {
		case f == storagepb.InventorySpec_OPTIONAL_FIELD_UNSPECIFIED:
			add(field, "must not be OPTIONAL_FIELD_UNSPECIFIED")
		case seen[f]:
			add(field, "duplicate of an earlier entry: "+f.String())
		}
		seen[f] = true
	}
	dest := spec.GetDestinationBucketId()
	problem := s.destinationProblem(ctx, dest)
	if problem != "" && (before == nil || before.GetDestinationBucketId() != dest) {
		add("spec.destination_bucket_id", problem)
	}
	if len(vs) > 0 {
		return errs.BadRequest(vs...)
	}
	if problem != "" {
		return nil
	}
	return s.authorizeDestination(ctx, reqctx.PrincipalFrom(ctx).ID, sourceID, dest)
}

// destinationProblem explains why id is not a usable destination, or "".
func (s *InventoryService) destinationProblem(ctx context.Context, id string) string {
	if id == "" {
		return "required"
	}
	if kindOf(id) != KindBucket {
		return "must be a bucket ID"
	}
	b, err := s.buckets.Get(ctx, id)
	switch {
	case errs.IsKind(err, errs.KindNotFound):
		return "bucket " + id + " not found"
	case err != nil:
		return "bucket " + id + " cannot be read: " + err.Error()
	case b.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE:
		return "bucket " + id + " is " + b.GetStatus().GetState().String() + ", reports can only be written to ACTIVE buckets"
	case IsFilesystemBucket(b):
		return "reports cannot be written to a filesystem bucket"
	}
	return ""
}

// authorizeDestination checks that principalID may write objects into a
// destination bucket other than the source, per the authorization mode.
func (s *InventoryService) authorizeDestination(ctx context.Context, principalID, sourceID, destID string) error {
	if destID == sourceID || s.access == nil || s.mode == authz.ModeOff {
		return nil
	}
	d, err := s.access.Check(ctx, principalID, PermObjectWrite, destID)
	if err != nil {
		return fmt.Errorf("authorize %s on %s: %w", PermObjectWrite, destID, err)
	}
	if d.Allowed {
		return nil
	}
	if s.mode == authz.ModeEnforce {
		return &errs.Error{Kind: errs.KindPermissionDenied, ResourceID: destID, Message: string(PermObjectWrite) + " denied on the destination bucket: " + d.Reason}
	}
	s.log.WarnContext(ctx, "authz denied", "method", "storage.inventory", "principal", principalID,
		"permission", string(PermObjectWrite), "target", destID, "enforced", false, "reason", d.Reason)
	return nil
}

// checkRoom enforces name uniqueness and the per-bucket limit.
func (s *InventoryService) checkRoom(ctx context.Context, inv *storagepb.Inventory) error {
	md := inv.GetMetadata()
	if existing, err := s.repo.GetByName(ctx, md.GetParentId(), md.GetName()); err == nil {
		return errs.AlreadyExists(existing.GetMetadata().GetId(), "an inventory with this name already exists in the bucket")
	} else if !errs.IsKind(err, errs.KindNotFound) {
		return err
	}
	n, err := s.repo.Count(ctx, md.GetParentId())
	if err != nil {
		return err
	}
	if n >= MaxInventoriesPerBucket {
		return &errs.Error{Kind: errs.KindQuotaExceeded, ResourceID: md.GetParentId(), Message: fmt.Sprintf(
			"a bucket can have at most %d inventory configurations", MaxInventoriesPerBucket)}
	}
	return nil
}

func (s *InventoryService) prepareUpdate(ctx context.Context, cmd UpdateInventoryCmd, by string) (*storagepb.Inventory, InventoryState, error) {
	stored, err := s.repo.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return nil, InventoryState{}, err
	}
	state, err := s.repo.State(ctx, stored.GetMetadata().GetId())
	if err != nil {
		return nil, InventoryState{}, err
	}
	updated, err := patchInventory(stored, cmd)
	if err != nil {
		return nil, InventoryState{}, err
	}
	if err := s.validateInventorySpec(ctx, stored.GetMetadata().GetParentId(), stored.GetSpec(), updated.GetSpec()); err != nil {
		return nil, InventoryState{}, err
	}
	if !proto.Equal(updated, stored) {
		updated.Metadata.ResourceVersion++
		updated.Metadata.UpdatedAt = timestamppb.New(s.clk.Now())
	}
	return updated, s.nextState(state, stored.GetSpec(), updated.GetSpec(), by), nil
}

// patchInventory computes the updated inventory and enforces the update rules.
func patchInventory(stored *storagepb.Inventory, cmd UpdateInventoryCmd) (*storagepb.Inventory, error) {
	md, smd := cmd.Metadata, stored.GetMetadata()
	if p := md.GetParentId(); p != "" && p != smd.GetParentId() {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "immutable: an inventory cannot move to another bucket"})
	}
	if n := md.GetName(); n != "" && n != smd.GetName() {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "immutable: inventory names cannot be changed"})
	}
	if v := md.GetResourceVersion(); v != 0 && v != smd.GetResourceVersion() {
		return nil, errs.Conflict(smd.GetId(), fmt.Sprintf("resource_version %d does not match current version %d", v, smd.GetResourceVersion()))
	}
	resetMask := cmd.ResetMask
	if resetMask == nil {
		resetMask = mask.New()
	}
	updated := proto.CloneOf(stored)
	patch := &storagepb.Inventory{Metadata: &commonpb.ResourceMetadata{Labels: md.GetLabels()}, Spec: cmd.Spec}
	if err := fieldmaskpb.PatchWithResetMask(updated, patch, resetMask); err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: "X-ResetMask", Message: err.Error()})
	}
	labels := updated.GetMetadata().GetLabels()
	updated.Metadata = proto.CloneOf(smd)
	updated.Metadata.Labels = labels
	if updated.GetSpec() == nil {
		updated.Spec = &storagepb.InventorySpec{}
	}
	updated.Status = proto.CloneOf(stored.GetStatus())
	return updated, nil
}

// nextState computes the scheduler state after an update: the updater
// becomes the report principal, a changed schedule counts from the last
// report, and enabling a disabled configuration waits for its next slot.
func (s *InventoryService) nextState(st InventoryState, before, after *storagepb.InventorySpec, by string) InventoryState {
	now := s.clk.Now()
	st.CreatedBy = by
	if before.GetSchedule() != after.GetSchedule() && !st.LastRun.IsZero() {
		st.NextRun = st.LastRun.Add(inventoryPeriod(after.GetSchedule()))
	}
	if !before.GetEnabled() && after.GetEnabled() {
		st.NextRun = now
		if next := st.LastRun.Add(inventoryPeriod(after.GetSchedule())); !st.LastRun.IsZero() && next.After(now) {
			st.NextRun = next
		}
	}
	return st
}

// inventoryPeriod is the time between two reports of a schedule.
func inventoryPeriod(sc storagepb.InventorySpec_Schedule) time.Duration {
	if sc == storagepb.InventorySpec_WEEKLY {
		return weeklyPeriod
	}
	return dailyPeriod
}

// RunDue writes the reports that are due: every enabled inventory whose
// next run has come. Failures are recorded in the inventory status and
// retried after InventoryRetryInterval; they do not stop the sweep.
func (s *InventoryService) RunDue(ctx context.Context) (InventoryRunResult, error) {
	all, err := s.repo.ListAll(ctx)
	if err != nil {
		return InventoryRunResult{}, fmt.Errorf("list inventories: %w", err)
	}
	var (
		res  InventoryRunResult
		errl []error
	)
	for _, inv := range all {
		if err := ctx.Err(); err != nil {
			return res, errors.Join(append(errl, err)...)
		}
		if !inv.GetSpec().GetEnabled() {
			continue
		}
		outcome, err := s.runIfDue(ctx, inv)
		if err != nil {
			errl = append(errl, fmt.Errorf("inventory %s: %w", inv.GetMetadata().GetId(), err))
		}
		switch outcome {
		case runWritten:
			res.Reports++
		case runFailed:
			res.Failures++
		case runSkipped:
		}
	}
	return res, errors.Join(errl...)
}

type runOutcome int

const (
	runSkipped runOutcome = iota
	runWritten
	runFailed
)

// runIfDue runs one inventory when it is due.
func (s *InventoryService) runIfDue(ctx context.Context, inv *storagepb.Inventory) (runOutcome, error) {
	id := inv.GetMetadata().GetId()
	st, err := s.repo.State(ctx, id)
	if errs.IsKind(err, errs.KindNotFound) {
		return runSkipped, nil // deleted meanwhile
	} else if err != nil {
		return runSkipped, err
	}
	now := s.clk.Now()
	if st.NextRun.After(now) {
		return runSkipped, nil
	}
	src, err := s.buckets.Get(ctx, inv.GetMetadata().GetParentId())
	if errs.IsKind(err, errs.KindNotFound) || (err == nil && src.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE) {
		return runSkipped, nil // soft-deleted or purged sources are skipped silently
	}
	if err != nil {
		return runSkipped, err
	}
	code, rep, werr := s.generate(ctx, inv, src, st, now)
	if werr != nil {
		s.log.WarnContext(ctx, "inventory report failed", "inventory", id, "error", werr)
	}
	if code != nil && *code == storagepb.InventoryStatus_NO_ERROR {
		s.log.InfoContext(ctx, "inventory report written", "inventory", id, "manifest", rep.ManifestKey, "objects", rep.Objects)
		return runWritten, s.finish(ctx, id, st, now, now.Add(inventoryPeriod(inv.GetSpec().GetSchedule())), code)
	}
	return runFailed, s.finish(ctx, id, st, now, now.Add(InventoryRetryInterval), code)
}

// generate writes one report. It returns the status code to record: NO_ERROR
// on success, the failure's code when it maps to one, nil to keep the status.
// The error is the cause of an unsuccessful write, for logging.
func (s *InventoryService) generate(ctx context.Context, inv *storagepb.Inventory, src *storagepb.Bucket, st InventoryState, now time.Time) (*storagepb.InventoryStatus_Error, InventoryReport, error) {
	codeOf := func(c storagepb.InventoryStatus_Error) *storagepb.InventoryStatus_Error { return &c }
	destID := inv.GetSpec().GetDestinationBucketId()
	dst, err := s.buckets.Get(ctx, destID)
	if errs.IsKind(err, errs.KindNotFound) || (err == nil && dst.GetStatus().GetState() != storagepb.BucketStatus_ACTIVE) {
		return codeOf(storagepb.InventoryStatus_DESTINATION_BUCKET_NOT_FOUND), InventoryReport{}, nil
	}
	if err != nil {
		return nil, InventoryReport{}, err
	}
	if err := s.authorizeDestination(ctx, st.CreatedBy, src.GetMetadata().GetId(), destID); errs.IsKind(err, errs.KindPermissionDenied) {
		return codeOf(storagepb.InventoryStatus_PERMISSION_DENIED), InventoryReport{}, nil
	} else if err != nil {
		return nil, InventoryReport{}, err
	}
	rep, err := s.writer.Write(ctx, ReportSource{Inventory: inv, Source: src, Destination: dst, Now: now})
	switch {
	case err == nil:
		return codeOf(storagepb.InventoryStatus_NO_ERROR), rep, nil
	case errs.IsKind(err, errs.KindQuotaExceeded):
		return codeOf(storagepb.InventoryStatus_QUOTA_EXCEEDED), InventoryReport{}, err
	case errs.IsKind(err, errs.KindNotFound):
		return codeOf(storagepb.InventoryStatus_DESTINATION_BUCKET_NOT_FOUND), InventoryReport{}, err
	case errs.IsKind(err, errs.KindPermissionDenied):
		return codeOf(storagepb.InventoryStatus_PERMISSION_DENIED), InventoryReport{}, err
	}
	return nil, InventoryReport{}, err
}

// finish records the outcome of a run: the status code (nil keeps it;
// NO_ERROR also records last_success) and the next scheduled run.
func (s *InventoryService) finish(ctx context.Context, id string, st InventoryState, now, next time.Time, code *storagepb.InventoryStatus_Error) error {
	if err := s.recordStatus(ctx, id, now, code); err != nil {
		return err
	}
	st.LastRun, st.NextRun = now, next
	if err := s.repo.SetState(ctx, id, st); err != nil && !errs.IsKind(err, errs.KindNotFound) {
		return err
	}
	return nil
}

// recordStatus writes the status of a run; it never changes resource_version.
func (s *InventoryService) recordStatus(ctx context.Context, id string, now time.Time, code *storagepb.InventoryStatus_Error) error {
	if code == nil {
		return nil
	}
	inv, err := s.repo.Get(ctx, id)
	if errs.IsKind(err, errs.KindNotFound) {
		return nil // deleted meanwhile
	}
	if err != nil {
		return err
	}
	status := &storagepb.InventoryStatus{LastSuccess: inv.GetStatus().GetLastSuccess(), ErrorCode: *code}
	if *code == storagepb.InventoryStatus_NO_ERROR {
		status.LastSuccess = timestamppb.New(now)
	}
	inv.Status = status
	return s.repo.Update(ctx, inv)
}

// SweepOrphans deletes the inventories of purged source buckets and returns
// how many it removed.
func (s *InventoryService) SweepOrphans(ctx context.Context) (int, error) {
	all, err := s.repo.ListAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("list inventories: %w", err)
	}
	var (
		n    int
		errl []error
	)
	for _, inv := range all {
		_, err := s.buckets.Get(ctx, inv.GetMetadata().GetParentId())
		if !errs.IsKind(err, errs.KindNotFound) {
			if err != nil {
				errl = append(errl, err)
			}
			continue
		}
		if err := s.repo.Delete(ctx, inv.GetMetadata().GetId()); err != nil && !errs.IsKind(err, errs.KindNotFound) {
			errl = append(errl, err)
			continue
		}
		n++
	}
	return n, errors.Join(errl...)
}
