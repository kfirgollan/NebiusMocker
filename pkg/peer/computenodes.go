package peer

import "context"

// ComputeNodesKey is the capability provided by the compute module: the
// compute instances that are the nodes of mk8s node groups.
var ComputeNodesKey = NewKey[ComputeNodes]("compute.managed-nodes")

// ComputeNodes creates and deletes the compute instances behind mk8s node
// groups. As in Nebius, each node is an ordinary compute instance in the
// cluster's project, visible to InstanceService Get/List, but managed by its
// node group: users cannot update, stop, start or delete it (InstanceService
// refuses with errs.BadState naming the node group), only the node group
// changes it.
//
// A node instance is created RUNNING with exactly the spec the template
// implies (InstanceService invariants hold: spec echoes, status is
// synthesized): resources, a managed boot disk from ImageFamily, one NIC
// with a private address (and a public one when PublicIP) bound through
// peer.NetworkInterfaces, the service account, GPU cluster membership and
// filesystem attachments.
//
// It is an internal, privileged call: no IAM checks; the caller authorized
// its own request. Every method is safe for concurrent use.
type ComputeNodes interface {
	// CheckTemplate validates a node template in a project as
	// InstanceService.Create would (platform and preset in the project's
	// region, boot disk size and type, subnet in the project, service
	// account, GPU cluster, filesystems), without creating anything, and
	// returns what the preset provides. Violations are errs.BadRequest with
	// fields named relative to the mk8s NodeTemplate, e.g.
	// "resources.preset", "boot_disk.size_bytes",
	// "network_interfaces[0].subnet_id", "service_account_id",
	// "gpu_cluster.id", "filesystems[1].existing_filesystem.id"; a missing
	// referenced resource is errs.NotFound.
	CheckTemplate(ctx context.Context, projectID string, t NodeTemplate) (NodeResources, error)

	// CreateNode creates one node instance and returns it RUNNING. It is
	// idempotent: when the node group already has a node with n.Name, that
	// node is returned unchanged.
	CreateNode(ctx context.Context, n NodeSpec) (NodeInstance, error)

	// DeleteNode deletes a node instance with its managed boot disk and its
	// addresses. A missing instance is not an error.
	DeleteNode(ctx context.Context, instanceID string) error

	// ListNodes returns the node instances of a node group in creation order.
	ListNodes(ctx context.Context, nodeGroupID string) ([]NodeInstance, error)
}

// NodeTemplate is the compute part of an mk8s NodeTemplate.
type NodeTemplate struct {
	Platform string
	Preset   string
	BootDisk NodeDisk
	// ImageFamily is the public image family of the boot disk (chosen by
	// mk8s from the GPU settings), e.g. "ubuntu24.04-driverless".
	ImageFamily string
	// SubnetID is the NIC subnet (the cluster's subnet when the template
	// sets none).
	SubnetID         string
	PublicIP         bool
	SecurityGroupIDs []string
	ServiceAccountID string
	GpuClusterID     string
	Filesystems      []NodeFilesystem
	// CloudInitUserData is passed to the instance as is.
	CloudInitUserData string
	Preemptible       bool
}

// NodeDisk is a node's boot disk.
type NodeDisk struct {
	SizeBytes      int64
	BlockSizeBytes int64
	// Type is a compute DiskSpec type name: "NETWORK_SSD", "NETWORK_HDD",
	// "NETWORK_SSD_IO_M3" or "NETWORK_SSD_NON_REPLICATED".
	Type string
	// Encrypted asks for a disk with DISK_ENCRYPTION_MANAGED.
	Encrypted bool
}

// NodeFilesystem is a filesystem attached to every node.
type NodeFilesystem struct {
	ID       string
	MountTag string
	ReadOnly bool
}

// NodeResources is what a node's preset provides.
type NodeResources struct {
	GPUs   int
	VCPUs  int
	Memory int64 // GiB
}

// NodeSpec is one node instance to create.
type NodeSpec struct {
	ProjectID string
	// Name is the instance name (and the Kubernetes Node name), unique in
	// the project.
	Name        string
	ClusterID   string
	NodeGroupID string
	// Labels are the instance metadata labels: the template's
	// instance_metadata labels plus the service's own, which win.
	Labels   map[string]string
	Template NodeTemplate
}

// NodeInstance is a node's compute instance.
type NodeInstance struct {
	ID   string
	Name string
	// PrivateIP and PublicIP are the NIC addresses without prefix length
	// ("" when none).
	PrivateIP string
	PublicIP  string
	Resources NodeResources
}
