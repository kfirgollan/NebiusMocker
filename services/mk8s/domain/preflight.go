package domain

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// errStop ends a dry-run evaluation whose remaining checks depend on a
// failed one (the diagnostic is already recorded).
var errStop = errors.New("mk8s: preflight stopped")

// checker runs the node group validations. A strict checker (Create,
// Update) fails on the first problem. A dry-run checker (PreflightCheck)
// records problems as diagnostics and carries on, dropping those about
// fields whose values are unknown.
// diagnosticKinds are the errors about the request itself, which a dry run
// reports as diagnostics.
var diagnosticKinds = []errs.Kind{errs.KindBadRequest, errs.KindNotFound, errs.KindBadState, errs.KindAlreadyExists, errs.KindConflict}

type checker struct {
	dry     bool
	unknown *mask.Mask // nil: every value is known
	diags   []*commonpb.PreflightCheckDiagnostic
}

// check returns err when strict. In a dry run it records err and returns
// nil, unless err is not about the request (a storage failure).
func (c *checker) check(err error) error {
	if err == nil || !c.dry {
		return err
	}
	if !slices.Contains(diagnosticKinds, errs.KindOf(err)) {
		return err
	}
	c.record(err)
	return nil
}

// stop is check for a problem the next checks cannot do without: a dry run
// returns errStop after recording it.
func (c *checker) stop(err error) error {
	if err == nil {
		return nil
	}
	if e := c.check(err); e != nil {
		return e
	}
	return errStop
}

func (c *checker) record(err error) {
	diag := func(path, summary string) {
		if path != "" && c.isUnknown(path) {
			return
		}
		c.diags = append(c.diags, &commonpb.PreflightCheckDiagnostic{
			Severity: commonpb.PreflightCheckDiagnostic_ERROR, Summary: summary, Path: path,
		})
	}
	e, ok := errors.AsType[*errs.Error](err)
	if !ok || len(e.Violations) == 0 {
		diag("", oneLine(err.Error()))
		return
	}
	for _, v := range e.Violations {
		diag(v.Field, oneLine(v.Field+": "+v.Message))
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// isUnknown reports whether the value at path (or a parent) is unknown.
func (c *checker) isUnknown(path string) bool {
	return c.unknown != nil && c.unknown.IsPathUnknown(fieldPath(path))
}

// anyUnknown reports whether path or anything under it is unknown.
func (c *checker) anyUnknown(path string) bool {
	if c.unknown == nil {
		return false
	}
	fp := fieldPath(path)
	if c.unknown.IsPathUnknown(fp) {
		return true
	}
	sub, err := c.unknown.GetSubMaskByPath(fp)
	return err == nil && sub != nil && !sub.IsEmpty()
}

var indexRE = regexp.MustCompile(`\[(\d+)\]`)

// fieldPath converts a violation field ("spec.template.network_interfaces[0].subnet_id")
// to a mask field path.
func fieldPath(path string) mask.FieldPath {
	path = indexRE.ReplaceAllString(path, ".$1")
	parts := strings.Split(path, ".")
	fp := make(mask.FieldPath, 0, len(parts))
	for _, p := range parts {
		fp = append(fp, mask.FieldKey(p))
	}
	return fp
}

// PreflightCmd is the input of PreflightCheckNodeGroup.
type PreflightCmd struct {
	Context  *commonpb.PreflightCheckContext
	Metadata *commonpb.ResourceMetadata
	Spec     *mk8spb.NodeGroupSpec
	// ResetMask is the request's X-ResetMask: an UPDATE is a patch, as
	// the Update it previews (the CLI sends only the changed fields).
	ResetMask *mask.Mask
}

// PreflightResult is what PreflightCheckNodeGroup found.
type PreflightResult struct {
	Diagnostics []*commonpb.PreflightCheckDiagnostic
	// PathsRequireRecreate lists the immutable paths an UPDATE changes, as
	// a Nebius mask ("" when none).
	PathsRequireRecreate string
}

// PreflightCheckNodeGroup evaluates a node group create, recreate or
// update without changing anything: validation problems come back as
// diagnostics, never as an error.
func (s *Service) PreflightCheckNodeGroup(ctx context.Context, cmd PreflightCmd) (PreflightResult, error) {
	chk := &checker{dry: true}
	if u := cmd.Context.GetUnknownPaths(); u != "" {
		m, err := mask.Parse(u)
		if err != nil {
			return PreflightResult{}, errs.BadRequest(errs.Violation{Field: "context.unknown_paths", Message: err.Error()})
		}
		chk.unknown = m
	}
	md := cmd.Metadata
	if md == nil {
		md = &commonpb.ResourceMetadata{}
	}
	spec := cmd.Spec
	if spec == nil {
		spec = &mk8spb.NodeGroupSpec{}
	}
	var res PreflightResult
	var err error
	switch action(cmd.Context.GetAction(), md) {
	case commonpb.PreflightCheckContext_DELETE:
		return res, nil
	case commonpb.PreflightCheckContext_UPDATE:
		res.PathsRequireRecreate, err = s.preflightUpdate(ctx, md, spec, cmd.ResetMask, chk)
	case commonpb.PreflightCheckContext_CREATE, commonpb.PreflightCheckContext_RECREATE, commonpb.PreflightCheckContext_UNSPECIFIED:
		err = s.preflightCreate(ctx, md, spec, chk)
	}
	if err != nil && !errors.Is(err, errStop) {
		return PreflightResult{}, err
	}
	res.Diagnostics = chk.diags
	return res, nil
}

// action resolves UNSPECIFIED: UPDATE when the metadata has an ID.
func action(a commonpb.PreflightCheckContext_Action, md *commonpb.ResourceMetadata) commonpb.PreflightCheckContext_Action {
	if a != commonpb.PreflightCheckContext_UNSPECIFIED {
		return a
	}
	if md.GetId() != "" {
		return commonpb.PreflightCheckContext_UPDATE
	}
	return commonpb.PreflightCheckContext_CREATE
}

// preflightCreate evaluates a create, or a recreate of metadata.id (which
// is deleted first, so its name is free).
func (s *Service) preflightCreate(ctx context.Context, md *commonpb.ResourceMetadata, spec *mk8spb.NodeGroupSpec, chk *checker) error {
	md = proto.CloneOf(md)
	replaces := md.GetId()
	md.Id, md.ResourceVersion = "", 0
	_, err := s.planCreate(ctx, md, spec, replaces, chk)
	return err
}

// preflightUpdate evaluates an update. When it changes an immutable field,
// the paths are returned and the request is evaluated as a recreate.
func (s *Service) preflightUpdate(ctx context.Context, md *commonpb.ResourceMetadata, spec *mk8spb.NodeGroupSpec, resetMask *mask.Mask, chk *checker) (string, error) {
	stored, err := s.d.NodeGroups.Get(ctx, md.GetId())
	if err := chk.stop(err); err != nil {
		return "", err
	}
	paths := recreatePaths(stored, md, spec, chk)
	if len(paths) > 0 {
		if md.GetParentId() == "" {
			md = proto.CloneOf(md)
			md.ParentId = stored.GetMetadata().GetParentId()
		}
		return strings.Join(paths, ","), s.preflightCreate(ctx, md, spec, chk)
	}
	_, err = s.planUpdate(ctx, md, spec, resetMask, chk)
	return "", err
}

// recreatePaths are the known IMMUTABLE paths whose proposed value differs
// from the stored one.
func recreatePaths(stored *mk8spb.NodeGroup, md *commonpb.ResourceMetadata, spec *mk8spb.NodeGroupSpec, chk *checker) []string {
	var out []string
	differs := func(path, prev, next string) {
		if next != "" && next != prev && !chk.isUnknown(path) {
			out = append(out, path)
		}
	}
	differs("metadata.parent_id", stored.GetMetadata().GetParentId(), md.GetParentId())
	differs("spec.template.nvlink.nvl_instance_group_id",
		stored.GetSpec().GetTemplate().GetNvlink().GetNvlInstanceGroupId(), spec.GetTemplate().GetNvlink().GetNvlInstanceGroupId())
	return out
}
