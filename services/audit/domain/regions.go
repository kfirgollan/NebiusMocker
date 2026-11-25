package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// regionSet is the regions the mocker emulates. The mocker serves one control
// plane, so every recorded event belongs to the first (default) region.
type regionSet struct{ names []string }

func newRegionSet(names []string) (regionSet, error) {
	if len(names) == 0 {
		return regionSet{}, errors.New("audit: at least one region is required")
	}
	return regionSet{names: slices.Clone(names)}, nil
}

// check fails with a BadRequest on field unless region is a known region.
func (r regionSet) check(field, region string) error {
	if region == "" {
		return errs.BadRequest(errs.Violation{Field: field, Message: "required"})
	}
	if !slices.Contains(r.names, region) {
		return errs.BadRequest(errs.Violation{
			Field:   field,
			Message: fmt.Sprintf("unknown region %s; known: %s", region, strings.Join(r.names, ", ")),
		})
	}
	return nil
}

// holdsEvents reports whether region is the one that holds the recorded events.
func (r regionSet) holdsEvents(region string) bool { return region == r.names[0] }

// dataPlaneUnsupported is the message of a rejected DATA_PLANE event type.
const dataPlaneUnsupported = "DATA_PLANE (Object Storage data-plane) events are not recorded by NebiusMocker yet; use CONTROL_PLANE"
