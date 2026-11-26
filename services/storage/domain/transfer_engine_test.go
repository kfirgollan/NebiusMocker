package domain_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

var engT0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type engObject struct {
	meta domain.ObjectMeta
	body []byte
}

// engEndpoint is a map-backed domain.Endpoint that records calls.
type engEndpoint struct {
	clk *clock.Manual

	mu      sync.Mutex
	objects map[string]engObject
	fail    map[string]error // by op: list, head, get, put, replace, delete
	calls   map[string]int
	onPut   func(key string)
}

func engNewEndpoint(clk *clock.Manual) *engEndpoint {
	return &engEndpoint{clk: clk, objects: map[string]engObject{}, fail: map[string]error{}, calls: map[string]int{}}
}

func (f *engEndpoint) record(op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[op]++
	return f.fail[op]
}

func (f *engEndpoint) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

// seed stores an object at the current clock time.
func (f *engEndpoint) seed(key, body string, attrs domain.ObjectMeta) domain.ObjectMeta {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.store(key, []byte(body), attrs)
}

func (f *engEndpoint) store(key string, body []byte, attrs domain.ObjectMeta) domain.ObjectMeta {
	sum := md5.Sum(body)
	attrs.Key, attrs.Size, attrs.ETag = key, int64(len(body)), hex.EncodeToString(sum[:])
	attrs.LastModified = f.clk.Now()
	attrs.Metadata = maps.Clone(attrs.Metadata)
	f.objects[key] = engObject{meta: attrs, body: body}
	return attrs
}

func (f *engEndpoint) object(key string) (engObject, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	return o, ok
}

func (f *engEndpoint) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.objects))
}

func (f *engEndpoint) List(_ context.Context, prefix string) iter.Seq2[domain.ObjectMeta, error] {
	return func(yield func(domain.ObjectMeta, error) bool) {
		if err := f.record("list"); err != nil {
			yield(domain.ObjectMeta{}, err)
			return
		}
		for _, k := range f.keys() {
			o, ok := f.object(k)
			if !ok || !strings.HasPrefix(k, prefix) {
				continue
			}
			if !yield(o.meta, nil) {
				return
			}
		}
	}
}

func (f *engEndpoint) Head(_ context.Context, key string) (domain.ObjectMeta, error) {
	if err := f.record("head"); err != nil {
		return domain.ObjectMeta{}, err
	}
	o, ok := f.object(key)
	if !ok {
		return domain.ObjectMeta{}, errs.NotFound(key)
	}
	return o.meta, nil
}

func (f *engEndpoint) Get(_ context.Context, key string) (io.ReadCloser, domain.ObjectMeta, error) {
	if err := f.record("get"); err != nil {
		return nil, domain.ObjectMeta{}, err
	}
	o, ok := f.object(key)
	if !ok {
		return nil, domain.ObjectMeta{}, errs.NotFound(key)
	}
	return io.NopCloser(bytes.NewReader(o.body)), o.meta, nil
}

func (f *engEndpoint) Put(_ context.Context, key string, body io.Reader, meta domain.ObjectMeta) (string, error) {
	if err := f.record("put"); err != nil {
		return "", err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	if int64(len(data)) != meta.Size {
		return "", fmt.Errorf("put %s: read %d bytes, want %d", key, len(data), meta.Size)
	}
	f.mu.Lock()
	stored := f.store(key, data, meta)
	hook := f.onPut
	f.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	return stored.ETag, nil
}

func (f *engEndpoint) ReplaceMetadata(_ context.Context, key string, meta domain.ObjectMeta) (string, error) {
	if err := f.record("replace"); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return "", errs.NotFound(key)
	}
	return f.store(key, o.body, meta).ETag, nil
}

func (f *engEndpoint) Delete(_ context.Context, key string) error {
	if err := f.record("delete"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

// engEndpoints opens endpoints by bucket name.
type engEndpoints struct {
	sides   map[string]*engEndpoint
	openErr map[string]error
}

func (f *engEndpoints) Open(_ context.Context, side domain.Side) (domain.Endpoint, error) {
	if err := f.openErr[side.Bucket]; err != nil {
		return nil, err
	}
	ep, ok := f.sides[side.Bucket]
	if !ok {
		return nil, &domain.EndpointError{Code: "NoSuchBucket", Message: side.Bucket}
	}
	return ep, nil
}

// engManaged is a map-backed domain.ManagedObjects.
type engManaged struct {
	mu   sync.Mutex
	etag map[string]string
	fail error
}

func engKey(dst, key string) string { return dst + "\x00" + key }

func (m *engManaged) Managed(_ context.Context, dst, key, etag string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return false, m.fail
	}
	got, ok := m.etag[engKey(dst, key)]
	return ok && got == etag, nil
}

func (m *engManaged) MarkManaged(_ context.Context, dst, key, etag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.etag[engKey(dst, key)] = etag
	return nil
}

func (m *engManaged) Forget(_ context.Context, dst, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.etag, engKey(dst, key))
	return nil
}

func (m *engManaged) has(dst, key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.etag[engKey(dst, key)]
	return ok
}

type engEnv struct {
	clk      *clock.Manual
	src, dst *engEndpoint
	eps      *engEndpoints
	managed  *engManaged
	engine   *domain.TransferEngine
	plan     domain.Plan
}

func engSetup() *engEnv {
	clk := clock.NewManual(engT0)
	env := &engEnv{clk: clk, src: engNewEndpoint(clk), dst: engNewEndpoint(clk), managed: &engManaged{etag: map[string]string{}}}
	env.eps = &engEndpoints{sides: map[string]*engEndpoint{"src": env.src, "dst": env.dst}, openErr: map[string]error{}}
	env.engine = domain.NewTransferEngine(env.eps, env.managed, clk, nil)
	env.plan = domain.Plan{
		Source:                  domain.Side{Kind: domain.ProviderS3Compatible, Endpoint: "http://src.example", Bucket: "src"},
		Destination:             domain.Side{Kind: domain.ProviderNebius, Region: "eu-north1", Bucket: "dst"},
		Overwrite:               domain.OverwriteIfNewer,
		RequestsPerSecond:       domain.DefaultTransferRequestsPerSecond,
		BandwidthBytesPerSecond: domain.DefaultTransferBandwidth,
	}
	return env
}

func (env *engEnv) dstID() string { return env.plan.Destination.Identity() }

func (env *engEnv) run(t *testing.T, watermark time.Time) (it domain.Iteration, next time.Time, snaps []domain.Iteration) {
	t.Helper()
	it, next = env.engine.Run(t.Context(), env.plan, 1, watermark, func(s domain.Iteration) { snaps = append(snaps, s) })
	return it, next, snaps
}

func engWantCounts(t *testing.T, it domain.Iteration, discovered, transferred, skipped, deleted int64) {
	t.Helper()
	got := [4]int64{it.Discovered, it.Transferred, it.Skipped, it.Deleted}
	want := [4]int64{discovered, transferred, skipped, deleted}
	if got != want {
		t.Fatalf("counts (discovered, transferred, skipped, deleted) = %v, want %v (iteration %+v)", got, want, it)
	}
}

func engWantCompleted(t *testing.T, it domain.Iteration) {
	t.Helper()
	if it.State != domain.IterationCompleted || it.Error != nil {
		t.Fatalf("iteration = state %d error %+v, want completed", it.State, it.Error)
	}
}

func TestEngineFirstCopyMapsPrefixesAndMetadata(t *testing.T) {
	env := engSetup()
	env.plan.Source.Prefix, env.plan.Destination.Prefix = "in/", "out/"
	env.src.seed("in/a.txt", "hello", domain.ObjectMeta{
		ContentType: "text/plain", CacheControl: "max-age=60", ContentDisposition: "inline",
		ContentEncoding: "identity", ContentLanguage: "en", Metadata: map[string]string{"owner": "kfir"},
	})
	env.clk.Advance(time.Minute)
	last := env.src.seed("in/b/c", "world!", domain.ObjectMeta{})
	env.src.seed("other/x", "outside", domain.ObjectMeta{})
	env.clk.Advance(3 * time.Second)

	it, wm, snaps := env.run(t, time.Time{})

	engWantCompleted(t, it)
	engWantCounts(t, it, 2, 2, 0, 0)
	if it.TransferredBytes != 11 || it.Seq != 1 || !it.Start.Equal(engT0.Add(time.Minute+3*time.Second)) {
		t.Fatalf("iteration = %+v", it)
	}
	if !wm.Equal(last.LastModified) {
		t.Fatalf("watermark = %v, want %v", wm, last.LastModified)
	}
	if got := env.dst.keys(); !slices.Equal(got, []string{"out/a.txt", "out/b/c"}) {
		t.Fatalf("destination keys = %v", got)
	}
	a, _ := env.dst.object("out/a.txt")
	if string(a.body) != "hello" || a.meta.ContentType != "text/plain" || a.meta.CacheControl != "max-age=60" ||
		a.meta.ContentDisposition != "inline" || a.meta.ContentEncoding != "identity" ||
		a.meta.ContentLanguage != "en" || a.meta.Metadata["owner"] != "kfir" {
		t.Fatalf("copied object = %+v body %q", a.meta, a.body)
	}
	if !env.managed.has(env.dstID(), "out/a.txt") || !env.managed.has(env.dstID(), "out/b/c") {
		t.Fatalf("copied objects not marked managed: %v", env.managed.etag)
	}
	if len(snaps) != 2 || snaps[1].Transferred != 2 || snaps[1].State != domain.IterationInProgress {
		t.Fatalf("progress snapshots = %+v", snaps)
	}
}

func TestEngineOverwriteStrategies(t *testing.T) {
	tests := []struct {
		name           string
		overwrite      domain.Overwrite
		srcNewer       bool
		managed        bool
		touchUnmanaged bool
		wantOverwrite  bool
	}{
		{name: "never skips newer managed", overwrite: domain.OverwriteNever, srcNewer: true, managed: true},
		{name: "if newer overwrites newer managed", overwrite: domain.OverwriteIfNewer, srcNewer: true, managed: true, wantOverwrite: true},
		{name: "if newer skips newer unmanaged", overwrite: domain.OverwriteIfNewer, srcNewer: true},
		{name: "if newer touch unmanaged overwrites", overwrite: domain.OverwriteIfNewer, srcNewer: true, touchUnmanaged: true, wantOverwrite: true},
		{name: "if newer skips older managed", overwrite: domain.OverwriteIfNewer, managed: true},
		{name: "if newer skips older touch unmanaged", overwrite: domain.OverwriteIfNewer, touchUnmanaged: true},
		{name: "always overwrites older unmanaged", overwrite: domain.OverwriteAlways, wantOverwrite: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := engSetup()
			env.plan.Overwrite, env.plan.TouchUnmanaged = tc.overwrite, tc.touchUnmanaged
			first, second := env.dst, env.src
			if !tc.srcNewer {
				first, second = env.src, env.dst
			}
			first.seed("k", first.label(env), domain.ObjectMeta{})
			env.clk.Advance(time.Hour)
			second.seed("k", second.label(env), domain.ObjectMeta{})
			if tc.managed {
				d, _ := env.dst.object("k")
				env.managed.etag[engKey(env.dstID(), "k")] = d.meta.ETag
			}

			it, _, _ := env.run(t, time.Time{})

			engWantCompleted(t, it)
			got, _ := env.dst.object("k")
			if overwritten := string(got.body) == "source"; overwritten != tc.wantOverwrite {
				t.Fatalf("destination body = %q, want overwritten=%v", got.body, tc.wantOverwrite)
			}
			if tc.wantOverwrite {
				engWantCounts(t, it, 1, 1, 0, 0)
				if !env.managed.has(env.dstID(), "k") {
					t.Fatal("overwritten object not marked managed")
				}
				return
			}
			engWantCounts(t, it, 1, 0, 1, 0)
			if env.dst.count("put") != 0 {
				t.Fatal("skipped object was written")
			}
		})
	}
}

// label returns the body the side writes in TestEngineOverwriteStrategies.
func (f *engEndpoint) label(env *engEnv) string {
	if f == env.src {
		return "source"
	}
	return "destination"
}

func TestEngineIdenticalDataReplacesMetadataOnly(t *testing.T) {
	tests := []struct {
		name        string
		dstType     string
		wantReplace bool
	}{
		{name: "different metadata", dstType: "application/octet-stream", wantReplace: true},
		{name: "same metadata", dstType: "text/plain"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := engSetup()
			env.plan.Overwrite = domain.OverwriteAlways
			env.dst.seed("k", "same", domain.ObjectMeta{ContentType: tc.dstType})
			env.src.seed("k", "same", domain.ObjectMeta{ContentType: "text/plain", Metadata: map[string]string{"a": "b"}})
			if !tc.wantReplace {
				env.dst.seed("k", "same", domain.ObjectMeta{ContentType: "text/plain", Metadata: map[string]string{"a": "b"}})
			}

			it, _, _ := env.run(t, time.Time{})

			engWantCompleted(t, it)
			if env.dst.count("put") != 0 || env.src.count("get") != 0 || it.TransferredBytes != 0 {
				t.Fatalf("data re-transferred: puts %d gets %d bytes %d", env.dst.count("put"), env.src.count("get"), it.TransferredBytes)
			}
			if !tc.wantReplace {
				engWantCounts(t, it, 1, 0, 1, 0)
				return
			}
			engWantCounts(t, it, 1, 1, 0, 0)
			got, _ := env.dst.object("k")
			if env.dst.count("replace") != 1 || got.meta.ContentType != "text/plain" || got.meta.Metadata["a"] != "b" {
				t.Fatalf("metadata not replaced: %+v", got.meta)
			}
			if !env.managed.has(env.dstID(), "k") {
				t.Fatal("object not marked managed after metadata replace")
			}
		})
	}
}

func TestEngineDeletesInDestination(t *testing.T) {
	tests := []struct {
		name           string
		touchUnmanaged bool
		wantKeys       []string
		wantDeleted    int64
	}{
		{name: "managed only", wantKeys: []string{"out/a", "out/unmanaged", "outside/x"}, wantDeleted: 1},
		{name: "touch unmanaged", touchUnmanaged: true, wantKeys: []string{"out/a", "outside/x"}, wantDeleted: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := engSetup()
			env.plan.Source.Prefix, env.plan.Destination.Prefix = "in/", "out/"
			env.plan.DeleteInDestination, env.plan.TouchUnmanaged = true, tc.touchUnmanaged
			env.src.seed("in/a", "a", domain.ObjectMeta{})
			m := env.dst.seed("out/managed", "m", domain.ObjectMeta{})
			env.managed.etag[engKey(env.dstID(), "out/managed")] = m.ETag
			env.dst.seed("out/unmanaged", "u", domain.ObjectMeta{})
			env.dst.seed("outside/x", "x", domain.ObjectMeta{})

			it, _, snaps := env.run(t, time.Time{})

			engWantCompleted(t, it)
			engWantCounts(t, it, 1, 1, 0, tc.wantDeleted)
			if got := env.dst.keys(); !slices.Equal(got, tc.wantKeys) {
				t.Fatalf("destination keys = %v, want %v", got, tc.wantKeys)
			}
			if env.managed.has(env.dstID(), "out/managed") {
				t.Fatal("deleted object still recorded as managed")
			}
			if last := snaps[len(snaps)-1]; last.Deleted != tc.wantDeleted || len(snaps) != 1+int(tc.wantDeleted) {
				t.Fatalf("progress snapshots = %+v", snaps)
			}
		})
	}
}

func TestEngineWatermarkSkipsOldObjects(t *testing.T) {
	env := engSetup()
	env.plan.DeleteInDestination = true
	env.src.seed("old", "old", domain.ObjectMeta{})
	env.dst.seed("old", "stale copy", domain.ObjectMeta{})
	env.clk.Advance(time.Hour)
	watermark := env.clk.Now()
	env.clk.Advance(time.Minute)
	fresh := env.src.seed("new", "new", domain.ObjectMeta{})

	it, wm, _ := env.run(t, watermark)

	engWantCompleted(t, it)
	engWantCounts(t, it, 2, 1, 1, 0)
	if !wm.Equal(fresh.LastModified) {
		t.Fatalf("watermark = %v, want %v", wm, fresh.LastModified)
	}
	if got := env.dst.keys(); !slices.Equal(got, []string{"new", "old"}) {
		t.Fatalf("destination keys = %v (a watermark-skipped source key must not be deleted)", got)
	}
	if env.dst.count("head") != 1 {
		t.Fatalf("destination heads = %d, want 1 (skipped objects are not examined)", env.dst.count("head"))
	}

	// An iteration that lists nothing newer keeps the watermark.
	_, wm2, _ := env.run(t, env.clk.Now())
	if !wm2.Equal(env.clk.Now()) {
		t.Fatalf("watermark moved back to %v", wm2)
	}
}

func TestEngineFailures(t *testing.T) {
	denied := &domain.EndpointError{Code: "AccessDenied", Message: "access denied"}
	tests := []struct {
		name    string
		prepare func(env *engEnv)
		want    domain.TransferError
	}{
		{
			name:    "source open",
			prepare: func(env *engEnv) { env.eps.openErr["src"] = denied },
			want:    domain.TransferError{Origin: domain.OriginSource, Code: "AccessDenied", Message: "access denied"},
		},
		{
			name:    "destination open",
			prepare: func(env *engEnv) { delete(env.eps.sides, "dst") },
			want:    domain.TransferError{Origin: domain.OriginDestination, Code: "NoSuchBucket", Message: "dst"},
		},
		{
			name:    "source list",
			prepare: func(env *engEnv) { env.src.fail["list"] = denied },
			want:    domain.TransferError{Origin: domain.OriginSource, Code: "AccessDenied", Message: "access denied"},
		},
		{
			name:    "source get",
			prepare: func(env *engEnv) { env.src.fail["get"] = fmt.Errorf("dial: %w", denied) },
			want:    domain.TransferError{Origin: domain.OriginSource, Code: "AccessDenied", Message: "access denied"},
		},
		{
			name:    "destination put",
			prepare: func(env *engEnv) { env.dst.fail["put"] = &domain.EndpointError{Code: "QuotaExceeded", Message: "full"} },
			want:    domain.TransferError{Origin: domain.OriginDestination, Code: "QuotaExceeded", Message: "full"},
		},
		{
			name:    "destination head non-endpoint error",
			prepare: func(env *engEnv) { env.dst.fail["head"] = errors.New("connection reset") },
			want:    domain.TransferError{Origin: domain.OriginDestination, Code: "InternalError", Message: "connection reset"},
		},
		{
			name:    "managed store",
			prepare: func(env *engEnv) { env.managed.fail = errors.New("store down") },
			want:    domain.TransferError{Origin: domain.OriginUnspecified, Code: "InternalError", Message: "store down"},
		},
		{
			name: "empty destination key",
			prepare: func(env *engEnv) {
				env.plan.Source.Prefix = "obj"
				env.src.fail["get"] = errors.New("must not be read")
			},
			want: domain.TransferError{
				Origin: domain.OriginDestination, Code: "InvalidObjectName",
				Message: `source object "obj" maps to invalid destination key ""`,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := engSetup()
			env.src.seed("obj", "data", domain.ObjectMeta{})
			tc.prepare(env)
			wm := engT0.Add(-time.Hour)

			it, gotWM, _ := env.run(t, wm)

			if it.State != domain.IterationFailed || it.Error == nil || *it.Error != tc.want {
				t.Fatalf("iteration = state %d error %+v, want failed with %+v", it.State, it.Error, tc.want)
			}
			if !gotWM.Equal(wm) || it.End.IsZero() {
				t.Fatalf("watermark %v end %v after failure", gotWM, it.End)
			}
		})
	}
}

func TestEngineRejectsTooLongDestinationKey(t *testing.T) {
	env := engSetup()
	env.plan.Destination.Prefix = strings.Repeat("p", 1020)
	env.src.seed("abcde", "x", domain.ObjectMeta{})

	it, _, _ := env.run(t, time.Time{})

	if it.State != domain.IterationFailed || it.Error.Code != "InvalidObjectName" || it.Error.Origin != domain.OriginDestination {
		t.Fatalf("iteration = %+v error %+v", it, it.Error)
	}
}

func TestEngineCancelInterrupts(t *testing.T) {
	env := engSetup()
	for _, k := range []string{"a", "b", "c"} {
		env.src.seed(k, k, domain.ObjectMeta{})
	}
	ctx, cancel := context.WithCancel(t.Context())
	env.dst.onPut = func(string) { cancel() }
	wm := engT0.Add(-time.Hour)

	it, gotWM := env.engine.Run(ctx, env.plan, 3, wm, nil)

	if it.State != domain.IterationInterrupted || it.Error != nil || it.Seq != 3 {
		t.Fatalf("iteration = %+v, want interrupted without error", it)
	}
	if !gotWM.Equal(wm) || env.dst.count("put") != 1 || it.Transferred != 1 {
		t.Fatalf("after cancel: watermark %v puts %d iteration %+v", gotWM, env.dst.count("put"), it)
	}
}

// engStart runs the engine in a goroutine of the synctest bubble.
func engStart(ctx context.Context, t *testing.T, env *engEnv) <-chan domain.Iteration {
	t.Helper()
	done := make(chan domain.Iteration, 1)
	go func() {
		it, _ := env.engine.Run(ctx, env.plan, 1, time.Time{}, nil)
		done <- it
	}()
	return done
}

func engFinished(done <-chan domain.Iteration) (domain.Iteration, bool) {
	select {
	case it := <-done:
		return it, true
	default:
		return domain.Iteration{}, false
	}
}

func TestEngineRequestLimiterWaitsOnClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		env := engSetup()
		env.plan.RequestsPerSecond = 2
		for i := range 5 {
			env.src.seed(fmt.Sprintf("k%d", i), "x", domain.ObjectMeta{})
		}
		done := engStart(t.Context(), t, env)

		// 6 source requests (1 list page + 5 gets) against a bucket of 2 at 2/s.
		for _, step := range []struct {
			advance  time.Duration
			wantGets int
		}{
			{0, 1}, {499 * time.Millisecond, 1}, {time.Millisecond, 2}, {time.Second, 4}, {500 * time.Millisecond, 5},
		} {
			env.clk.Advance(step.advance)
			synctest.Wait()
			if got := env.src.count("get"); got != step.wantGets {
				t.Fatalf("after +%v: source gets = %d, want %d", step.advance, got, step.wantGets)
			}
		}
		it, ok := engFinished(done)
		if !ok {
			t.Fatal("iteration not finished after the limiter refilled")
		}
		engWantCompleted(t, it)
		engWantCounts(t, it, 5, 5, 0, 0)
		if env.dst.count("head") != 5 {
			t.Fatalf("destination heads = %d (destination requests are not limited)", env.dst.count("head"))
		}
	})
}

func TestEngineBandwidthLimiterWaitsOnClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		env := engSetup()
		env.plan.BandwidthBytesPerSecond = 10
		env.src.seed("big", strings.Repeat("b", 25), domain.ObjectMeta{})
		done := engStart(t.Context(), t, env)

		// 25 bytes at 10 B/s from a full bucket: 10 now, 10 after 1s, 5 after 0.5s more.
		for _, advance := range []time.Duration{0, time.Second, 499 * time.Millisecond} {
			env.clk.Advance(advance)
			synctest.Wait()
			if _, ok := engFinished(done); ok || env.dst.count("put") != 1 {
				t.Fatalf("after +%v: transfer finished before the bandwidth budget allowed it", advance)
			}
		}
		env.clk.Advance(time.Millisecond)
		synctest.Wait()
		it, ok := engFinished(done)
		if !ok {
			t.Fatal("iteration not finished after the bandwidth bucket refilled")
		}
		engWantCompleted(t, it)
		if it.TransferredBytes != 25 || it.ThroughputBytes != 25 {
			t.Fatalf("bytes %d throughput %d, want 25 and 25 (1.5s rounds to 1s)", it.TransferredBytes, it.ThroughputBytes)
		}
		got, _ := env.dst.object("big")
		if len(got.body) != 25 {
			t.Fatalf("destination body has %d bytes", len(got.body))
		}
	})
}

func TestEngineCancelWhileLimitedInterrupts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		env := engSetup()
		env.plan.RequestsPerSecond = 1
		env.src.seed("a", "a", domain.ObjectMeta{})
		ctx, cancel := context.WithCancel(t.Context())
		done := engStart(ctx, t, env)
		synctest.Wait()
		if _, ok := engFinished(done); ok {
			t.Fatal("iteration finished without waiting for a request token")
		}
		cancel()
		it := <-done
		if it.State != domain.IterationInterrupted || it.Error != nil || env.src.count("get") != 0 {
			t.Fatalf("iteration = %+v, gets %d; want interrupted before the get", it, env.src.count("get"))
		}
	})
}

func TestEngineManyPagesChargeListRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		env := engSetup()
		env.plan.RequestsPerSecond = domain.TransferListPage + 1
		env.plan.DeleteInDestination = true
		for i := range domain.TransferListPage {
			env.src.seed(fmt.Sprintf("k%04d", i), "x", domain.ObjectMeta{})
		}
		// 1 list + 1000 gets + 1 token before the second page = 1002 > 1001.
		done := engStart(t.Context(), t, env)
		synctest.Wait()
		if _, ok := engFinished(done); ok {
			t.Fatal("second list page was not charged")
		}
		env.clk.Advance(time.Second)
		synctest.Wait()
		it, ok := engFinished(done)
		if !ok {
			t.Fatal("iteration did not finish")
		}
		engWantCompleted(t, it)
		engWantCounts(t, it, domain.TransferListPage, domain.TransferListPage, 0, 0)
	})
}

func TestEngineIterationEmpty(t *testing.T) {
	tests := []struct {
		it   domain.Iteration
		want bool
	}{
		{domain.Iteration{Discovered: 3, Skipped: 3}, true},
		{domain.Iteration{Transferred: 1}, false},
		{domain.Iteration{Deleted: 1}, false},
	}
	for _, tc := range tests {
		if got := tc.it.Empty(); got != tc.want {
			t.Errorf("%+v.Empty() = %v, want %v", tc.it, got, tc.want)
		}
	}

	env := engSetup()
	env.src.seed("a", "a", domain.ObjectMeta{})
	env.plan.Overwrite = domain.OverwriteNever
	first, _, _ := env.run(t, time.Time{})
	second, _, _ := env.run(t, time.Time{})
	if first.Empty() || !second.Empty() {
		t.Fatalf("first %+v second %+v", first, second)
	}
}
