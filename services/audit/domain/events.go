package domain

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/audit"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

const (
	defaultEventPageSize = 100
	maxEventPageSize     = 500
	eventTokenPrefix     = "v1:"
)

// EventService answers AuditEventService queries.
type EventService struct {
	events    EventSource
	hierarchy Hierarchy
	regions   regionSet
}

// NewEventService wires an EventService. regions are the emulated region
// names; the first holds every recorded event.
func NewEventService(events EventSource, hierarchy Hierarchy, regions []string) (*EventService, error) {
	if events == nil || hierarchy == nil {
		return nil, errors.New("audit: event service dependencies must not be nil")
	}
	rs, err := newRegionSet(regions)
	if err != nil {
		return nil, err
	}
	return &EventService{events: events, hierarchy: hierarchy, regions: rs}, nil
}

// ListEventsQuery is the input of EventService.List.
type ListEventsQuery struct {
	ParentID string
	// Region must be an emulated region; only the default region has events.
	Region string
	// Start is inclusive, End exclusive.
	Start, End time.Time
	PageSize   int64
	PageToken  string
	Filter     string
	EventType  auditv2pb.EventType
}

// List returns one page of the events recorded under ParentID, oldest first.
// The next page token is empty on the last page.
func (s *EventService) List(ctx context.Context, q ListEventsQuery) (events []*auditv2pb.AuditEvent, nextPageToken string, err error) {
	if err := s.regions.check("region", q.Region); err != nil {
		return nil, "", err
	}
	if q.EventType == auditv2pb.EventType_DATA_PLANE {
		return nil, "", errs.BadRequest(errs.Violation{Field: "event_type", Message: dataPlaneUnsupported})
	}
	if q.PageSize < 0 || q.PageSize > maxEventPageSize {
		return nil, "", errs.BadRequest(errs.Violation{
			Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxEventPageSize),
		})
	}
	size := int(q.PageSize)
	if size == 0 {
		size = defaultEventPageSize
	}
	if !q.End.After(q.Start) {
		return nil, "", errs.BadRequest(errs.Violation{Field: "end", Message: "end must be after start"})
	}
	filter, err := audit.ParseFilter(q.Filter)
	if err != nil {
		return nil, "", err
	}
	cursor, err := decodeEventToken(q.PageToken)
	if err != nil {
		return nil, "", err
	}
	if _, err := s.hierarchy.Parent(ctx, q.ParentID); err != nil {
		return nil, "", err
	}
	if !s.regions.holdsEvents(q.Region) {
		return []*auditv2pb.AuditEvent{}, "", nil
	}
	sc := scope{parentID: q.ParentID, from: q.Start, to: q.End, filter: filter}

	var matched []*audit.Entry
	for e := range s.events.Since(cursor) {
		if !sc.match(e) {
			continue
		}
		matched = append(matched, e)
		if len(matched) > size {
			break
		}
	}
	if len(matched) > size {
		matched = matched[:size]
		nextPageToken = encodeEventToken(matched[size-1].Seq)
	}
	events = make([]*auditv2pb.AuditEvent, 0, len(matched))
	for _, e := range matched {
		ev, err := e.Event()
		if err != nil {
			return nil, "", err
		}
		events = append(events, ev)
	}
	return events, nextPageToken, nil
}

// scope selects the entries of one parent in a half-open time range.
type scope struct {
	parentID string
	from, to time.Time
	filter   audit.Filter
}

// match reports whether e is in the time range, under the parent and passes
// the filter. The cheap checks come first.
func (sc *scope) match(e *audit.Entry) bool {
	if e.Time.Before(sc.from) || !e.Time.Before(sc.to) {
		return false
	}
	if e.Fields.ResourceID != sc.parentID && !slices.Contains(e.Fields.HierarchyIDs, sc.parentID) {
		return false
	}
	return sc.filter.Match(&e.Fields)
}

func encodeEventToken(seq uint64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(eventTokenPrefix + strconv.FormatUint(seq, 10)))
}

func decodeEventToken(token string) (uint64, error) {
	if token == "" {
		return 0, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(token); err == nil {
		if rest, ok := strings.CutPrefix(string(b), eventTokenPrefix); ok {
			if seq, perr := strconv.ParseUint(rest, 10, 64); perr == nil {
				return seq, nil
			}
		}
	}
	return 0, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
}
