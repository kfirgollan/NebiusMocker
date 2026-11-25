// Package tfcache gives Terraform e2e suites a provider plugin cache that no
// other process writes to.
//
// A plugin cache shared between processes (other packages, other checkouts)
// is not safe: with no dependency lock file, every `terraform init` downloads
// the provider again and rewrites the cached binary, so a concurrent run
// executes a half-written plugin ("Unrecognized remote plugin message",
// "Required plugins are not installed"). Each test binary therefore gets its
// own cache directory; within it, TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE
// lets later inits reuse the first download.
//
// Usage:
//
//	func TestMain(m *testing.M) { os.Exit(tfcache.Main(m)) }
//	env["TF_PLUGIN_CACHE_DIR"] = tfcache.Dir(t)
//	maps.Copy(env, tfcache.Env(t))
package tfcache

import (
	"os"
	"sync"
	"testing"
)

var (
	once sync.Once
	dir  string
	err  error
)

// Dir returns this test binary's private plugin cache directory.
func Dir(tb testing.TB) string {
	tb.Helper()
	once.Do(func() { dir, err = os.MkdirTemp("", "nebius-mocker-tf-plugins-") })
	if err != nil {
		tb.Fatalf("tfcache: %v", err)
	}
	return dir
}

// Env returns the Terraform environment that uses Dir as the plugin cache.
func Env(tb testing.TB) map[string]string {
	tb.Helper()
	return map[string]string{
		"TF_PLUGIN_CACHE_DIR":                            Dir(tb),
		"TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE": "1",
	}
}

// Main runs the tests and removes the cache afterwards.
func Main(m *testing.M) int {
	code := m.Run()
	if dir != "" {
		_ = os.RemoveAll(dir) // best-effort: a temp dir
	}
	return code
}
