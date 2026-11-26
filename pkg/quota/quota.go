// Package quota is the catalog of the Nebius quotas the mocker enforces: their
// names, units, the service that owns them and their default limits.
//
// Names are the identifiers Nebius uses in QuotaAllowance.metadata.name (for
// example "compute.disk.size.network-ssd"). Service areas charge them through
// peer.QuotaGate and report usage through peer.QuotaUsageSource; the quotas
// module serves them as QuotaAllowance resources. A quota is in the catalog
// only when an area charges it, so every allowance a user can set has effect.
package quota

import (
	"maps"
	"slices"
	"strings"
)

// Units, as in QuotaAllowance.status.unit.
const (
	UnitCount = "count"
	UnitByte  = "byte"
)

// Compute quotas.
const (
	ComputeInstanceCount            = "compute.instance.count"
	ComputeInstancePreemptibleCount = "compute.instance.preemptible.count"
	ComputeInstanceNonGPUVCPU       = "compute.instance.non-gpu.vcpu"
	ComputeGPUClusterCount          = "compute.gpucluster.count"
	ComputeDiskCount                = "compute.disk.count"
	ComputeDiskSnapshotCount        = "compute.disk.snapshot.count"
	ComputeDiskSnapshotSize         = "compute.disk.snapshot.size"
	ComputeFilesystemCount          = "compute.filesystem.count"

	computeInstanceGPUPrefix = "compute.instance.gpu."
	computeDiskSizePrefix    = "compute.disk.size."
	computeFSSizePrefix      = "compute.filesystem.size."
)

// VPC and DNS quotas.
const (
	VPCNetworkCount       = "vpc.network.count"
	VPCSubnetCount        = "vpc.subnet.count"
	VPCPoolCount          = "vpc.pool.count"
	VPCAllocationCount    = "vpc.allocation.count"
	VPCPublicIPv4Count    = "vpc.ipv4-address.public.count"
	VPCRouteTableCount    = "vpc.routetable.count"
	VPCRouteCount         = "vpc.route.count"
	VPCSecurityGroupCount = "vpc.securitygroup.count"
	VPCSecurityRuleCount  = "vpc.securityrule.count"
	DNSZoneCount          = "dns.zone.count"
)

// Other areas' quotas.
const (
	StorageBucketCount    = "storage.bucket.count"
	StorageTransferCount  = "storage.transfer.count"
	MK8sClusterCount      = "mk8s.cluster.count"
	RegistryCount         = "registry.count"
	MysteryBoxSecretCount = "mysterybox.secret.count" //nolint:gosec // G101: a quota name, not a credential
	KMSSymmetricKeyCount  = "kms.symmetrickey.count"
	KMSAsymmetricKeyCount = "kms.asymmetrickey.count"
)

// GPUFamilies are the accelerator families of the GPU quotas listed by
// default ("compute.instance.gpu.<family>"). Any other family a platform
// uses is still enforced with the GPU default (see Catalog.Lookup).
var GPUFamilies = []string{"b200", "b300", "gb200", "gb300", "h100", "h200", "l40s", "rtx6000"}

// ComputeInstanceGPU is the GPU quota of a compute platform: GPU quotas are
// keyed by accelerator family, so "gpu-h200-sxm" consumes
// "compute.instance.gpu.h200". It returns "" for a non-GPU platform.
func ComputeInstanceGPU(platform string) string {
	rest, ok := strings.CutPrefix(platform, "gpu-")
	if !ok {
		return ""
	}
	family, _, _ := strings.Cut(rest, "-")
	if family == "" {
		return ""
	}
	return computeInstanceGPUPrefix + family
}

// ComputeDiskSize is the size quota of a disk type given by its enum name,
// e.g. "NETWORK_SSD_IO_M3" → "compute.disk.size.network-ssd-io-m3".
func ComputeDiskSize(diskType string) string { return computeDiskSizePrefix + typeSuffix(diskType) }

// ComputeFilesystemSize is the size quota of a filesystem type given by its
// enum name, e.g. "NETWORK_SSD" → "compute.filesystem.size.network-ssd".
func ComputeFilesystemSize(fsType string) string { return computeFSSizePrefix + typeSuffix(fsType) }

func typeSuffix(enumName string) string {
	return strings.ReplaceAll(strings.ToLower(enumName), "_", "-")
}

// Entry is one quota of the catalog.
type Entry struct {
	Name string
	// Service is the owning area as in QuotaAllowance.status.service.
	Service            string
	ServiceDescription string
	Description        string
	Unit               string
	// Default is the limit of every tenant and project allowance that no
	// one has set.
	Default uint64
}

// Default limits: far above the documented default tier, so local
// development and tests are not limited until someone lowers an allowance.
const (
	defaultCount = 1000
	defaultVCPU  = 100_000
	defaultGPU   = 4096
	pib          = 1 << 50
	defaultBytes = 1 * pib
)

const (
	svcCompute    = "compute"
	svcVPC        = "vpc"
	svcDNS        = "dns"
	svcStorage    = "storage"
	svcMK8s       = "mk8s"
	svcRegistry   = "registry"
	svcMysteryBox = "mysterybox"
	svcKMS        = "kms"
)

var serviceDescriptions = map[string]string{
	svcCompute:    "Compute",
	svcVPC:        "Virtual Private Cloud",
	svcDNS:        "DNS",
	svcStorage:    "Object Storage",
	svcMK8s:       "Managed Kubernetes®",
	svcRegistry:   "Container Registry",
	svcMysteryBox: "SecretStash",
	svcKMS:        "Key Management Service",
}

// diskTypes and fsTypes are the size-quota suffixes of compute disk and
// filesystem types (DiskSpec.DiskType / FilesystemSpec.FilesystemType).
var (
	diskTypes = []string{"NETWORK_SSD", "NETWORK_SSD_NON_REPLICATED", "NETWORK_SSD_IO_M3", "NETWORK_HDD"}
	fsTypes   = []string{"NETWORK_SSD", "NETWORK_HDD", "WEKA", "VAST"}
)

func defaultEntries() []Entry {
	c := func(name, desc string) Entry {
		return Entry{Name: name, Service: svcCompute, Description: desc, Unit: UnitCount, Default: defaultCount}
	}
	cb := func(name, desc string) Entry {
		return Entry{Name: name, Service: svcCompute, Description: desc, Unit: UnitByte, Default: defaultBytes}
	}
	es := []Entry{
		c(ComputeInstanceCount, "Regular VMs"),
		c(ComputeInstancePreemptibleCount, "Preemptible VMs"),
		{Name: ComputeInstanceNonGPUVCPU, Service: svcCompute, Description: "Total vCPUs for non-GPU VMs", Unit: UnitCount, Default: defaultVCPU},
		c(ComputeGPUClusterCount, "Number of GPU clusters (InfiniBand)"),
		c(ComputeDiskCount, "Disks"),
		c(ComputeDiskSnapshotCount, "Total number of disk snapshots"),
		cb(ComputeDiskSnapshotSize, "Total storage capacity of all disk snapshots"),
		c(ComputeFilesystemCount, "Shared filesystems"),
	}
	for _, f := range GPUFamilies {
		es = append(es, gpuEntry(computeInstanceGPUPrefix+f))
	}
	for _, t := range diskTypes {
		es = append(es, cb(ComputeDiskSize(t), "Total disk capacity ("+typeSuffix(t)+")"))
	}
	for _, t := range fsTypes {
		es = append(es, cb(ComputeFilesystemSize(t), "Total shared filesystem capacity ("+typeSuffix(t)+")"))
	}
	n := func(svc, name, desc string) Entry {
		return Entry{Name: name, Service: svc, Description: desc, Unit: UnitCount, Default: defaultCount}
	}
	return append(es,
		n(svcVPC, VPCNetworkCount, "Networks"),
		n(svcVPC, VPCSubnetCount, "Subnets"),
		n(svcVPC, VPCPoolCount, "Pools"),
		n(svcVPC, VPCAllocationCount, "Allocations"),
		n(svcVPC, VPCPublicIPv4Count, "Public IPv4 addresses"),
		n(svcVPC, VPCRouteTableCount, "Route tables"),
		n(svcVPC, VPCRouteCount, "Routes"),
		n(svcVPC, VPCSecurityGroupCount, "Security groups"),
		n(svcVPC, VPCSecurityRuleCount, "Security rules"),
		n(svcDNS, DNSZoneCount, "DNS zones"),
		n(svcStorage, StorageBucketCount, "Buckets"),
		n(svcStorage, StorageTransferCount, "Transfers"),
		n(svcMK8s, MK8sClusterCount, "Clusters per region"),
		n(svcRegistry, RegistryCount, "Registries"),
		n(svcMysteryBox, MysteryBoxSecretCount, "Secrets"),
		n(svcKMS, KMSSymmetricKeyCount, "Symmetric keys"),
		n(svcKMS, KMSAsymmetricKeyCount, "Asymmetric keys"),
	)
}

func gpuEntry(name string) Entry {
	family := strings.TrimPrefix(name, computeInstanceGPUPrefix)
	return Entry{
		Name: name, Service: svcCompute, Unit: UnitCount, Default: defaultGPU,
		Description: "Total NVIDIA " + strings.ToUpper(family) + " GPUs for regular VMs without reservations",
	}
}

// Catalog is an immutable set of quota entries.
type Catalog struct {
	entries map[string]Entry
	names   []string // sorted
}

// DefaultCatalog returns the built-in catalog with limits overridden by
// limits (quota name → default limit; unknown names are an error).
func DefaultCatalog(limits map[string]uint64) (*Catalog, error) {
	c := &Catalog{entries: map[string]Entry{}}
	for _, e := range defaultEntries() {
		e.ServiceDescription = serviceDescriptions[e.Service]
		c.entries[e.Name] = e
	}
	for name, v := range limits {
		e, ok := c.Lookup(name)
		if !ok {
			return nil, &UnknownQuotaError{Name: name}
		}
		e.Default = v
		c.entries[name] = e
	}
	c.names = slices.Sorted(maps.Keys(c.entries))
	return c, nil
}

// UnknownQuotaError reports a quota name that is not in the catalog.
type UnknownQuotaError struct{ Name string }

func (e *UnknownQuotaError) Error() string { return "unknown quota " + `"` + e.Name + `"` }

// Lookup returns the entry of a quota name. GPU quotas of families not listed
// in GPUFamilies (a custom platform catalog) resolve to a GPU entry with the
// GPU default, so every GPU platform is enforced.
func (c *Catalog) Lookup(name string) (Entry, bool) {
	if e, ok := c.entries[name]; ok {
		return e, true
	}
	if family, ok := strings.CutPrefix(name, computeInstanceGPUPrefix); ok && family != "" && !strings.Contains(family, ".") {
		e := gpuEntry(name)
		e.ServiceDescription = serviceDescriptions[svcCompute]
		return e, true
	}
	return Entry{}, false
}

// Entries returns the listed entries sorted by name.
func (c *Catalog) Entries() []Entry {
	out := make([]Entry, 0, len(c.names))
	for _, n := range c.names {
		out = append(out, c.entries[n])
	}
	return out
}
