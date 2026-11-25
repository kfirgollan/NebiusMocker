package memory_test

import (
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/memory"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore/objstoretest"
)

func TestConformance(t *testing.T) {
	t.Parallel()
	objstoretest.Run(t, memory.New(clock.NewManual(time.Unix(1_700_000_000, 0))), "suite-bucket")
}

func TestVersioningRecorded(t *testing.T) {
	t.Parallel()
	s := memory.New(clock.NewManual(time.Unix(0, 0)))
	if err := s.EnsureBucket(t.Context(), "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVersioning(t.Context(), "b", objstore.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVersioning(t.Context(), "b", objstore.VersioningUnset); err != nil {
		t.Fatal(err)
	}
	if v, ok := s.Versioning("b"); !ok || v != objstore.VersioningEnabled {
		t.Fatalf("Versioning(b) = %v, %v; want enabled (unset is a no-op)", v, ok)
	}
}
