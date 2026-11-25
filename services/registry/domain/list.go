package domain

import (
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// ListQuery is the input of the List methods (ListRegistriesRequest and
// ListArtifactsRequest share this shape).
type ListQuery struct {
	ParentID  string
	PageSize  int64
	PageToken string
	// Filter must be empty: see checkFilter.
	Filter string
}

// checkFilter rejects filters instead of silently ignoring them. The registry
// protos (ListRegistriesRequest.filter, ListArtifactsRequest.filter) and the
// CLI reference define no filter syntax, and the Compute proto marks the same
// field as "not implemented, filtering could be done on client side", so there
// is no documented behavior to emulate.
func checkFilter(filter string) error {
	if filter == "" {
		return nil
	}
	return errs.BadRequest(errs.Violation{
		Field:   "filter",
		Message: fmt.Sprintf("filter %q is not supported: the Nebius API documents no filter syntax; filter on the client side", filter),
	})
}
