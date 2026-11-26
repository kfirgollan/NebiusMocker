package s3

import (
	"context"
	"net/http"

	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// ReadTracker is told about every successful object read of a regular bucket
// (satisfied by *domain.AccessTracker). It feeds the LAST_ACCESS inventory
// field and never fails a request.
type ReadTracker interface {
	Read(ctx context.Context, bucketID, key string)
}

// TrackReads makes the front report successful GetObject calls to t. Call it
// before Start.
func (s *Server) TrackReads(t ReadTracker) { s.reads = t }

// trackRead reports one finished request when it was a successful (2xx)
// GetObject of a regular, non-filesystem bucket.
func (s *Server) trackRead(r *http.Request, t target, sr *statusRecorder) {
	if s.reads == nil || t.Bucket == "" || t.Key == "" || sr.status < http.StatusOK || sr.status >= http.StatusMultipleChoices {
		return
	}
	op, err := classify(r, t)
	if err != nil || op.action != domain.ActGetObject {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	b, err := s.gate.Bucket(ctx, t.Bucket)
	if err != nil || domain.IsFilesystemBucket(b) {
		return
	}
	s.reads.Read(ctx, b.GetMetadata().GetId(), t.Key)
}
