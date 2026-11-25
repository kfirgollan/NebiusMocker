//go:build e2e && !e2e_storage

package storage

import (
	"os"
	"testing"

	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
)

// TestMain removes the private Terraform plugin cache (see tfcache). Under
// e2e_storage, objects_test.go's TestMain also removes the RustFS container.
func TestMain(m *testing.M) { os.Exit(tfcache.Main(m)) }
