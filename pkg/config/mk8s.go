package config

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// MK8s backends (NEBIUS_MOCKER_MK8S_BACKEND).
const (
	// MK8sBackendMock runs no Kubernetes: clusters get endpoints nothing
	// listens on and every node is reported ready.
	MK8sBackendMock = "mock"
	// MK8sBackendK3s runs one k3s cluster per mk8s cluster in Docker.
	MK8sBackendK3s = "k3s"
)

// MK8sConfig configures Managed Kubernetes (NEBIUS_MOCKER_MK8S_*).
type MK8sConfig struct {
	// Backend is the Kubernetes data plane: "mock" (default) or "k3s"
	// (NEBIUS_MOCKER_MK8S_BACKEND).
	Backend string
	// Versions is the control-plane version catalog, "<major>.<minor>",
	// oldest first; the newest is the default for new clusters
	// (NEBIUS_MOCKER_MK8S_VERSIONS, comma-separated).
	Versions []string
}

// defaultMK8s follows docs.nebius.com/kubernetes/versions: 1.36 is used for
// new clusters, 1.35 and 1.34 are supported.
func defaultMK8s() MK8sConfig {
	return MK8sConfig{Backend: MK8sBackendMock, Versions: []string{"1.34", "1.35", "1.36"}}
}

func mk8sParsers(m *MK8sConfig) []struct {
	name  string
	apply func(string) error
} {
	return []struct {
		name  string
		apply func(string) error
	}{
		{"MK8S_BACKEND", func(s string) error { return parseMK8sBackend(s, &m.Backend) }},
		{"MK8S_VERSIONS", func(s string) error { return parseMK8sVersions(s, &m.Versions) }},
	}
}

func parseMK8sBackend(s string, out *string) error {
	if s != MK8sBackendMock && s != MK8sBackendK3s {
		return fmt.Errorf("want %q or %q, got %q", MK8sBackendMock, MK8sBackendK3s, s)
	}
	*out = s
	return nil
}

var mk8sVersionRE = regexp.MustCompile(`^([1-9])\.(\d{2})$`)

func parseMK8sVersions(s string, out *[]string) error {
	var vs []string
	for v := range strings.SplitSeq(s, ",") {
		v = strings.TrimSpace(v)
		if !mk8sVersionRE.MatchString(v) {
			return fmt.Errorf("want comma-separated <major>.<minor> versions such as 1.35, got %q", v)
		}
		if slices.Contains(vs, v) {
			return fmt.Errorf("version %s listed twice", v)
		}
		vs = append(vs, v)
	}
	slices.SortFunc(vs, compareMinor)
	*out = vs
	return nil
}

// compareMinor orders validated "<major>.<minor>" versions numerically.
func compareMinor(a, b string) int {
	am, an := splitMinor(a)
	bm, bn := splitMinor(b)
	if am != bm {
		return am - bm
	}
	return an - bn
}

func splitMinor(v string) (major, minor int) {
	m := mk8sVersionRE.FindStringSubmatch(v)
	major, _ = strconv.Atoi(m[1]) // digits by mk8sVersionRE
	minor, _ = strconv.Atoi(m[2])
	return major, minor
}
