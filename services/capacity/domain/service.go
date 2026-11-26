package domain

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Listing page sizes: allowances allow up to 1000 per page, block groups
// and intervals up to 200 (the protos).
const (
	defaultPageSize       = 100
	maxAllowancePageSize  = 1000
	maxBlockGroupPageSize = 200
)

// Config holds the settings the domain needs from the mocker configuration.
type Config struct {
	// TenantID and ProjectID are the seeded tenant and project; they always
	// exist, even without IAM.
	TenantID  string
	ProjectID string
}

// Deps are the dependencies of a Service. All are required.
type Deps struct {
	Inventory  *Inventory
	Allowances AllowanceRepository
	Usage      UsageRepository
	Hierarchy  Hierarchy
	Ops        Operations
	IDs        IDGenerator
	Clock      clock.Clock
	Config     Config
}

// Service implements the capacity services. mu serializes allowance writes
// and reservations, so the per-(project, block group) checks are atomic with
// the writes they guard.
type Service struct {
	mu         sync.Mutex
	inv        *Inventory
	allowances AllowanceRepository
	usage      UsageRepository
	hierarchy  Hierarchy
	ops        Operations
	ids        IDGenerator
	clk        clock.Clock
	cfg        Config
}

// New wires the capacity domain.
func New(d Deps) (*Service, error) {
	if d.Inventory == nil || d.Allowances == nil || d.Usage == nil || d.Hierarchy == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("capacity: service dependencies must not be nil")
	}
	return &Service{
		inv: d.Inventory, allowances: d.Allowances, usage: d.Usage, hierarchy: d.Hierarchy,
		ops: d.Ops, ids: d.IDs, clk: d.Clock, cfg: d.Config,
	}, nil
}

func pageSize(n int64, maxSize int) (int, error) {
	if n < 0 || n > int64(maxSize) {
		return 0, errs.BadRequest(errs.Violation{Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxSize)})
	}
	if n == 0 {
		return min(defaultPageSize, maxSize), nil
	}
	return int(n), nil
}

// paginate returns one page of items after the offset in pageToken.
func paginate[T any](items []T, size int, pageToken string) (Page[T], error) {
	offset := 0
	if pageToken != "" {
		b, err := base64.RawURLEncoding.DecodeString(pageToken)
		n, perr := strconv.Atoi(string(b))
		if err != nil || perr != nil || n < 0 {
			return Page[T]{}, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
		}
		offset = n
	}
	if offset >= len(items) {
		return Page[T]{Items: []T{}}, nil
	}
	end := min(offset+size, len(items))
	page := Page[T]{Items: items[offset:end]}
	if end < len(items) {
		page.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	}
	return page, nil
}

// listPage validates the paging arguments and returns one page of items.
func listPage[T any](items []T, size int64, maxSize int, pageToken string) (Page[T], error) {
	n, err := pageSize(size, maxSize)
	if err != nil {
		return Page[T]{}, err
	}
	return paginate(items, n, pageToken)
}

// checkTenant requires an existing tenant ID in field.
func (s *Service) checkTenant(ctx context.Context, field, tenantID string) error {
	if !strings.HasPrefix(tenantID, KindTenant+"-") {
		return errs.BadRequest(errs.Violation{Field: field, Message: "must be a tenant ID"})
	}
	if tenantID == s.cfg.TenantID {
		return nil
	}
	if _, err := s.hierarchy.Parent(ctx, tenantID); err != nil {
		return fmt.Errorf("%s %s: %w", field, tenantID, err)
	}
	return nil
}

// projectTenant returns the tenant of an existing project in field.
func (s *Service) projectTenant(ctx context.Context, field, projectID string) (string, error) {
	if !strings.HasPrefix(projectID, KindProject+"-") {
		return "", errs.BadRequest(errs.Violation{Field: field, Message: "must be a project ID"})
	}
	tenant, err := s.hierarchy.Parent(ctx, projectID)
	switch {
	case err == nil:
		return tenant, nil
	case projectID == s.cfg.ProjectID && errs.IsKind(err, errs.KindNotFound):
		return s.cfg.TenantID, nil // the seeded project exists without IAM
	default:
		return "", fmt.Errorf("%s %s: %w", field, projectID, err)
	}
}

// group returns a block group of tenantID, or errs.NotFound (also for a
// group of another tenant, which the caller cannot see).
func (s *Service) group(id, tenantID string) (*BlockGroup, error) {
	g, ok := s.inv.Group(id)
	if !ok || (tenantID != "" && g.TenantID != tenantID) {
		return nil, errs.NotFound(id)
	}
	return g, nil
}
