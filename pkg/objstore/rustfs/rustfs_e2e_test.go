//go:build e2e_storage

package rustfs_test

import (
	"os"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/objstore/objstoretest"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

func TestMain(m *testing.M) { os.Exit(rustfsfixture.Main(m)) }

// TestConformance runs the shared objstore suite against a real RustFS container.
func TestConformance(t *testing.T) {
	srv := rustfsfixture.Start(t)
	objstoretest.Run(t, srv.Client(t), "conformance-bucket")
}
