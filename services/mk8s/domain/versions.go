package domain

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Version suffixes (cluster.proto ControlPlaneStatus.version,
// node_group.proto NodeGroupStatus.version).
const (
	// cpInfraSuffix is the control-plane infrastructure version the mock
	// runs: "<major>.<minor>.<patch>-nebius-cp.1".
	cpInfraSuffix = "-nebius-cp.1"
	nodeSuffix    = "-nebius-node."
	// latestNodeImage is the newest node image version of every minor.
	latestNodeImage = 1
	// maxNodeSkew is how many minors a node group may lag behind its
	// cluster (docs.nebius.com/kubernetes/versions).
	maxNodeSkew = 2
	// defaultOS is the node OS when spec.template.os is empty.
	defaultOS = "ubuntu24.04"
)

var (
	minorRE       = regexp.MustCompile(`^(\d+)\.(\d+)$`)
	nodeVersionRE = regexp.MustCompile(`^(\d+\.\d+)(?:-nebius-node\.([1-9]\d*))?$`)
	statusMinorRE = regexp.MustCompile(`^(\d+\.\d+)\.(\d+)`)
)

// minor is a Kubernetes "<major>.<minor>" version.
type minor struct{ major, minor int }

func parseMinor(v string) (minor, bool) {
	m := minorRE.FindStringSubmatch(v)
	if m == nil {
		return minor{}, false
	}
	major, err1 := strconv.Atoi(m[1])
	mn, err2 := strconv.Atoi(m[2])
	return minor{major, mn}, err1 == nil && err2 == nil
}

func (m minor) String() string { return fmt.Sprintf("%d.%d", m.major, m.minor) }

// behind reports how many minors m is behind of newer (negative: ahead), or
// false when the majors differ.
func (m minor) behind(newer minor) (int, bool) {
	return newer.minor - m.minor, m.major == newer.major
}

// catalog is the control-plane version catalog (NEBIUS_MOCKER_MK8S_VERSIONS),
// oldest first.
type catalog struct{ versions []string }

func newCatalog(versions []string) (catalog, error) {
	if len(versions) == 0 {
		return catalog{}, errors.New("mk8s: the version catalog is empty")
	}
	for _, v := range versions {
		if _, ok := parseMinor(v); !ok {
			return catalog{}, fmt.Errorf("mk8s: catalog version %q is not <major>.<minor>", v)
		}
	}
	return catalog{versions: slices.Clone(versions)}, nil
}

func (c catalog) has(v string) bool { return slices.Contains(c.versions, v) }

// newest is the default version of new clusters.
func (c catalog) newest() string { return c.versions[len(c.versions)-1] }

// requireCatalog fails with a BadRequest on field when v is not in the catalog.
func (c catalog) require(field, v string) error {
	if c.has(v) {
		return nil
	}
	return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf(
		"Kubernetes version %q is not supported; supported versions: %s", v, strings.Join(c.versions, ", "))})
}

// nodeVersion is a parsed NodeGroupSpec.version.
type nodeVersion struct {
	minor string
	// image is the node image version, 0 when the spec omits it.
	image int
}

func parseNodeVersion(v string) (nodeVersion, bool) {
	m := nodeVersionRE.FindStringSubmatch(v)
	if m == nil {
		return nodeVersion{}, false
	}
	nv := nodeVersion{minor: m[1]}
	if m[2] != "" {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return nodeVersion{}, false
		}
		nv.image = n
	}
	return nv, true
}

// clusterMinor is the cluster's effective "<major>.<minor>": the spec's, or
// the running version's when the spec leaves it to the service, or the
// catalog default for a cluster that never ran.
func (c catalog) clusterMinor(cl *mk8spb.Cluster) string {
	if v := cl.GetSpec().GetControlPlane().GetVersion(); v != "" {
		return v
	}
	if m := statusMinorRE.FindStringSubmatch(cl.GetStatus().GetControlPlane().GetVersion()); m != nil {
		return m[1]
	}
	return c.newest()
}

// clusterPatch is the running "<major>.<minor>.<patch>" of the cluster, or
// "" before it ran.
func clusterPatch(cl *mk8spb.Cluster) string {
	v := cl.GetStatus().GetControlPlane().GetVersion()
	if m := statusMinorRE.FindString(v); m != "" {
		return m
	}
	return ""
}

// nodeStatusVersion is NodeGroupStatus.version for a node group running
// minor with node image: the cluster's patch when on the same minor.
func nodeStatusVersion(cl *mk8spb.Cluster, nodeMinor string, image int) string {
	patch := nodeMinor + ".0"
	if p := clusterPatch(cl); strings.HasPrefix(p, nodeMinor+".") {
		patch = p
	}
	return fmt.Sprintf("%s%s%d", patch, nodeSuffix, image)
}

// statusNode parses a NodeGroupStatus.version back into its minor and
// image ("" and 0 when the node group never ran).
func statusNode(v string) nodeVersion {
	m := statusMinorRE.FindStringSubmatch(v)
	if m == nil {
		return nodeVersion{}
	}
	nv := nodeVersion{minor: m[1]}
	if i := strings.LastIndex(v, nodeSuffix); i >= 0 {
		nv.image, _ = strconv.Atoi(v[i+len(nodeSuffix):]) // written by nodeStatusVersion
	}
	return nv
}

// checkNodeVersion validates a node group's minor against its cluster's:
// in the catalog, not newer than the cluster, at most maxNodeSkew behind.
func (c catalog) checkNodeVersion(nv nodeVersion, clusterMinor string) error {
	const field = "spec.version"
	if err := c.require(field, nv.minor); err != nil {
		return err
	}
	if nv.image > latestNodeImage {
		return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf(
			"node image version %d does not exist; the latest is %d", nv.image, latestNodeImage)})
	}
	nm, _ := parseMinor(nv.minor) // in the catalog, so valid
	cm, ok := parseMinor(clusterMinor)
	if !ok {
		return nil
	}
	lag, sameMajor := nm.behind(cm)
	switch {
	case !sameMajor || lag < 0:
		return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf(
			"node group version %s must not be newer than the cluster version %s", nv.minor, clusterMinor)})
	case lag > maxNodeSkew:
		return errs.BadRequest(errs.Violation{Field: field, Message: fmt.Sprintf(
			"node group version %s is more than %d minor versions behind the cluster version %s", nv.minor, maxNodeSkew, clusterMinor)})
	}
	return nil
}

// checkClusterUpgrade allows keeping the version or raising it by one minor.
func checkClusterUpgrade(current, next string) error {
	if next == current {
		return nil
	}
	cm, ok1 := parseMinor(current)
	nm, ok2 := parseMinor(next)
	if lag, same := cm.behind(nm); ok1 && ok2 && same && lag == 1 {
		return nil
	}
	return errs.BadRequest(errs.Violation{Field: "spec.control_plane.version", Message: fmt.Sprintf(
		"the cluster runs %s: it can only be upgraded by one minor version at a time, and never downgraded", current)})
}

// Platforms of the node compatibility matrix.
var (
	cpuPlatforms = []string{"cpu-d3", "cpu-e2"}
	gpuPlatforms = []string{
		"gpu-h100-sxm", "gpu-h200-sxm", "gpu-l40s-a", "gpu-b200-sxm", "gpu-b200-sxm-a", "gpu-b300-sxm", "gpu-rtx6000",
	}
	blackwellPlatforms = []string{"gpu-b200-sxm", "gpu-b200-sxm-a", "gpu-b300-sxm", "gpu-rtx6000"}
	hopperPlatforms    = []string{"gpu-h100-sxm", "gpu-h200-sxm", "gpu-l40s-a"}
)

// compatItem is one node image: OS, GPU drivers preset and the platforms
// it runs on. Every catalog minor has the same images.
type compatItem struct {
	os, driversPreset string
	platforms         []string
}

var compatItems = []compatItem{
	{os: "ubuntu24.04", platforms: slices.Concat(cpuPlatforms, gpuPlatforms)},
	{os: "ubuntu22.04", platforms: slices.Concat(cpuPlatforms, gpuPlatforms)},
	{os: "ubuntu24.04", driversPreset: "cuda12", platforms: gpuPlatforms},
	{os: "ubuntu24.04", driversPreset: "cuda12.8", platforms: blackwellPlatforms},
	{os: "ubuntu22.04", driversPreset: "cuda12", platforms: hopperPlatforms},
}

// CompatibilityMatrix answers GetCompatibilityMatrix: the node images of the
// node group versions a cluster on clusterVersion may run (that version and
// up to maxNodeSkew minors before it, within the catalog), optionally only
// those for platform.
func (s *Service) CompatibilityMatrix(clusterVersion, platform string) (*mk8spb.NodeGroupCompatibilityMatrix, error) {
	if err := s.versions.require("cluster_kubernetes_version", clusterVersion); err != nil {
		return nil, err
	}
	cm, _ := parseMinor(clusterVersion)
	out := &mk8spb.NodeGroupCompatibilityMatrix{}
	for _, v := range s.versions.versions {
		vm, _ := parseMinor(v)
		if lag, same := vm.behind(cm); !same || lag < 0 || lag > maxNodeSkew {
			continue
		}
		ver := &mk8spb.NodeGroupCompatibilityVersion{KubernetesVersion: v}
		for _, it := range compatItems {
			if platform != "" && !slices.Contains(it.platforms, platform) {
				continue
			}
			ver.Items = append(ver.Items, &mk8spb.NodeGroupCompatibilityVersionItem{
				Os: it.os, DriversPreset: it.driversPreset, CompatiblePlatforms: slices.Clone(it.platforms),
			})
		}
		out.Versions = append(out.Versions, ver)
	}
	return out, nil
}

// checkImage requires a node image for the template's OS, drivers preset
// and platform, and returns its compute image family.
func checkImage(t *mk8spb.NodeTemplate) (string, error) {
	os, preset, platform := t.GetOs(), t.GetGpuSettings().GetDriversPreset(), t.GetResources().GetPlatform()
	if os == "" {
		os = defaultOS
	}
	var osKnown, platformKnown bool
	for _, it := range compatItems {
		if !slices.Contains(it.platforms, platform) {
			continue
		}
		platformKnown = true
		if it.os != os {
			continue
		}
		osKnown = true
		if it.driversPreset == preset {
			return imageFamily(os, preset), nil
		}
	}
	switch {
	case !platformKnown:
		return "", errs.BadRequest(errs.Violation{Field: "spec.template.resources.platform", Message: fmt.Sprintf(
			"platform %q has no Managed Kubernetes node images; see GetCompatibilityMatrix", platform)})
	case !osKnown:
		return "", errs.BadRequest(errs.Violation{Field: "spec.template.os", Message: fmt.Sprintf(
			"OS %q is not available on platform %s; see GetCompatibilityMatrix", os, platform)})
	default:
		return "", errs.BadRequest(errs.Violation{Field: "spec.template.gpu_settings.drivers_preset", Message: fmt.Sprintf(
			"drivers preset %q is not available for %s on platform %s; see GetCompatibilityMatrix", preset, os, platform)})
	}
}

// imageFamily is the public compute image family of a node image. The
// compute catalog has no separate CUDA 12.8 family: its ubuntu24.04-cuda12
// family ships CUDA 12.8.
func imageFamily(os, driversPreset string) string {
	switch driversPreset {
	case "":
		return os + "-driverless"
	case "cuda12.8":
		return "ubuntu24.04-cuda12"
	default:
		return os + "-" + driversPreset
	}
}
