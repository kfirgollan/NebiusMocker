package s3

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/go-cmp/cmp"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// repTracker records the reads the front reports.
type repTracker struct {
	mu   sync.Mutex
	seen []string
}

func (r *repTracker) Read(_ context.Context, bucketID, key string) {
	r.mu.Lock()
	r.seen = append(r.seen, bucketID+"/"+key)
	r.mu.Unlock()
}

func (r *repTracker) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func TestFrontReportsSuccessfulGetObject(t *testing.T) {
	t.Parallel()
	gate, err := domain.NewS3Gate(domain.GateDeps{
		Buckets: bucketsFake{items: []*storagepb.Bucket{bucket("storagebucket-tracked", "tracked", storagepb.BucketStatus_ACTIVE)}},
		Keys:    auditKeysFake{}, Checker: checkerFake{}, Hierarchy: hierarchyFake{}, Mode: authz.ModeEnforce,
	})
	if err != nil {
		t.Fatal(err)
	}
	front := NewServer(Config{Domain: "s3.test", Region: "eu-north1"}, gate, &opsFake{}, &errorUpstream{}, clock.Real{}, slog.New(slog.DiscardHandler))
	tr := &repTracker{}
	front.TrackReads(tr)
	srv := httptest.NewServer(front)
	t.Cleanup(srv.Close)
	c := client(srv.URL, testKeyID, testSecret)
	ctx := t.Context()

	if _, err := c.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("tracked"), Key: aws.String("a b.txt"), Body: strings.NewReader("x")}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	out, err := c.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("tracked"), Key: aws.String("a b.txt")})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	_, _ = io.Copy(io.Discard, out.Body)
	_ = out.Body.Close()
	// None of these is a successful GetObject.
	if _, err := c.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("tracked"), Key: aws.String("missing")}); err == nil {
		t.Fatal("GetObject(missing) succeeded")
	}
	if _, err := c.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String("tracked"), Key: aws.String("a b.txt")}); err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if _, err := c.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String("tracked")}); err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}

	if diff := cmp.Diff([]string{"storagebucket-tracked/a b.txt"}, tr.all()); diff != "" {
		t.Fatalf("tracked reads (-want +got):\n%s", diff)
	}
}
