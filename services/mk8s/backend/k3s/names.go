package k3s

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

// Labels on every Docker object the backend creates. LabelBackend marks them
// all, so the reaper finds leftovers of a mocker that was killed; the others
// say what an object belongs to.
const (
	LabelBackend   = "nebius-mocker.backend"
	LabelCluster   = "nebius-mocker.cluster"
	LabelNodeGroup = "nebius-mocker.node-group"
	LabelNode      = "nebius-mocker.node"
	LabelInstance  = "nebius-mocker.instance"
	LabelRole      = "nebius-mocker.role"
	// LabelOwner is the mocker that created an object: its webhook URL,
	// which carries the edge port no two live mockers share. Startup and
	// shutdown reap only their own objects, so a second mocker on the same
	// Docker host (a dev instance and a test run) never removes the other's.
	LabelOwner = "nebius-mocker.owner"
	// labelSpec is a hash of what an agent was started with (image, labels,
	// taints, GPUs); a change recreates the agent.
	labelSpec = "nebius-mocker.spec"
)

// backendName is the LabelBackend value and the NEBIUS_MOCKER_MK8S_BACKEND
// value of this backend.
const backendName = "k3s"

// Container roles (LabelRole).
const (
	roleServer = "server"
	roleAgent  = "agent"
)

// apiPort is the k3s API server port inside the server container.
const apiPort = "6443/tcp"

// gpuResource is the extended resource GPU nodes advertise (what the NVIDIA
// device plugin would report on a real GPU node).
const gpuResource = "nvidia.com/gpu"

func networkName(clusterID string) string { return "nebius-mocker-" + clusterID }
func serverName(clusterID string) string  { return "nebius-mocker-" + clusterID + "-server" }
func volumeName(clusterID string) string  { return "nebius-mocker-" + clusterID + "-server" }

func agentName(clusterID, node string) string { return "nebius-mocker-" + clusterID + "-" + node }

func (b *Backend) clusterLabels(clusterID, role string) map[string]string {
	return map[string]string{LabelBackend: backendName, LabelOwner: b.webhookURL, LabelCluster: clusterID, LabelRole: role}
}

// joinToken is the cluster's k3s join token, derived from the backend's
// per-process secret so a recreated server (an upgrade) keeps it.
func joinToken(secret []byte, clusterID string) string {
	return mac(secret, "token/"+clusterID)
}

// nodePassword is the password a k3s agent registers its node with. k3s
// rejects a node name registered before with another password, so a
// recreated agent (an upgrade, a template change) must present the same one.
func nodePassword(secret []byte, clusterID, node string) string {
	return mac(secret, "node/"+clusterID+"/"+node)
}

func mac(secret []byte, s string) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

// agentSpec is what an agent container is started with. specHash covers
// what needs a new agent (image, GPUs); labels and taints are reconciled on
// the Node in place, as a template's metadata and taints change nodes
// without recreating them.
type agentSpec struct {
	Image  string             `json:"image"`
	Labels map[string]string  `json:"labels"`
	Taints []domain.NodeTaint `json:"taints"`
	GPUs   int                `json:"gpus"`
}

func specHash(s agentSpec) string {
	b, _ := json.Marshal(struct {
		Image string `json:"image"`
		GPUs  int    `json:"gpus"`
	}{s.Image, s.GPUs}) //nolint:errchkjson // plain strings, maps and ints always marshal
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// kubeletLabel reports whether the kubelet may set label key on its own
// Node (the NodeRestriction admission plugin keeps kubernetes.io and k8s.io
// keys to an allowed set). Other labels are applied by the backend.
func kubeletLabel(key string) bool {
	prefix, _, ok := strings.Cut(key, "/")
	if !ok {
		return true
	}
	if prefix != "kubernetes.io" && prefix != "k8s.io" &&
		!strings.HasSuffix(prefix, ".kubernetes.io") && !strings.HasSuffix(prefix, ".k8s.io") {
		return true
	}
	if prefix == "kubelet.kubernetes.io" || prefix == "node.kubernetes.io" {
		return true
	}
	return slices.Contains([]string{
		"kubernetes.io/hostname", "kubernetes.io/arch", "kubernetes.io/os",
		"beta.kubernetes.io/instance-type", "node.kubernetes.io/instance-type",
		"failure-domain.beta.kubernetes.io/region", "failure-domain.beta.kubernetes.io/zone",
		"topology.kubernetes.io/region", "topology.kubernetes.io/zone",
	}, key)
}
