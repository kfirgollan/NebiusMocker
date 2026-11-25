package audit

import (
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
)

// DefaultMaxEvents is the Log capacity used when none is configured. At a few
// KB per event it keeps the log in the tens of MB.
const DefaultMaxEvents = 50_000

// Entry is one stored event. Entries are immutable once appended.
type Entry struct {
	// Seq increases by one per appended event, starting at 1. It is the
	// paging cursor: it survives eviction of older events.
	Seq  uint64
	Time time.Time
	// Type is CONTROL_PLANE (management RPCs) or DATA_PLANE (Object
	// Storage object access). AuditEvent has no such field; List and
	// exports select on it.
	Type   auditv2pb.EventType
	Fields Fields
	data   []byte // deterministic proto encoding of the event
}

// Event decodes the stored event.
func (e *Entry) Event() (*auditv2pb.AuditEvent, error) {
	ev := &auditv2pb.AuditEvent{}
	if err := proto.Unmarshal(e.data, ev); err != nil {
		return nil, fmt.Errorf("decode audit event %d: %w", e.Seq, err)
	}
	return ev, nil
}

// Log is a bounded, in-memory, append-only event log. When full, the oldest
// event is evicted. Events are kept encoded, with their filterable Fields
// extracted, so a query scans small structs and decodes only matches.
//
// Log is the in-memory implementation of the event store; consumers depend
// on their own narrow ports (Append for writers, Since for readers), so a
// persistent implementation can replace it later.
type Log struct {
	mu   sync.RWMutex
	buf  []*Entry // ring; buf[head] is the oldest entry when len == cap
	head int
	size int
	next uint64 // Seq of the next appended entry
}

// NewLog returns an empty log holding at most capacity events.
func NewLog(capacity int) (*Log, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("audit: log capacity must be positive, got %d", capacity)
	}
	return &Log{buf: make([]*Entry, capacity), next: 1}, nil
}

// Append encodes and stores ev as an event of type typ (CONTROL_PLANE or
// DATA_PLANE), evicting the oldest event if the log is full.
func (l *Log) Append(ev *auditv2pb.AuditEvent, typ auditv2pb.EventType) (*Entry, error) {
	if typ != auditv2pb.EventType_CONTROL_PLANE && typ != auditv2pb.EventType_DATA_PLANE {
		return nil, fmt.Errorf("audit: event type must be CONTROL_PLANE or DATA_PLANE, got %s", typ)
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("encode audit event: %w", err)
	}
	if ev.GetTime() == nil {
		return nil, errors.New("audit: event has no time")
	}
	e := &Entry{Time: ev.GetTime().AsTime(), Type: typ, Fields: FieldsOf(ev), data: data}
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Seq = l.next
	l.next++
	idx := (l.head + l.size) % len(l.buf)
	l.buf[idx] = e
	if l.size < len(l.buf) {
		l.size++
	} else {
		l.head = (l.head + 1) % len(l.buf)
	}
	return e, nil
}

// Len returns the number of stored events.
func (l *Log) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.size
}

// Since yields stored entries with Seq > after, oldest first. It iterates a
// snapshot, so the caller may run slow code (or Append) while ranging.
func (l *Log) Since(after uint64) iter.Seq[*Entry] {
	snap := l.snapshot(after)
	return func(yield func(*Entry) bool) {
		for _, e := range snap {
			if !yield(e) {
				return
			}
		}
	}
}

// snapshot copies the entry pointers newer than after, in order.
func (l *Log) snapshot(after uint64) []*Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]*Entry, 0, l.size)
	for i := range l.size {
		if e := l.buf[(l.head+i)%len(l.buf)]; e.Seq > after {
			out = append(out, e)
		}
	}
	return out
}
