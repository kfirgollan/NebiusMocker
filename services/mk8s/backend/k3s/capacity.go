package k3s

import (
	"fmt"
	"math"
)

// hostLoad is what the Docker host runs right now, as admission sees it.
type hostLoad struct {
	// CPUs and Memory are the host's totals (docker info NCPU, MemTotal).
	CPUs   int
	Memory int64
	// Used is the memory the host's running containers use, with this
	// backend's own containers counted at least at their estimate (a k3s
	// agent that just started has not grown to its working set yet).
	Used int64
	// Agents is the number of k3s agents of every cluster on the host,
	// including ones admitted but not created yet.
	Agents int
}

// capacityLimits are the admission settings (pkg/config MK8sK3sConfig).
type capacityLimits struct {
	AgentMemory   int64
	AgentCPU      float64
	MemoryReserve int64
	// MaxAgents caps the agents on the host; 0 means no cap.
	MaxAgents int
}

// admitAgent decides whether the host fits one more agent. It returns ""
// when it does, else why not, naming the limit (shown in the node group's
// status as the reason a node is not ready).
func admitAgent(l capacityLimits, h hostLoad) string {
	if l.MaxAgents > 0 && h.Agents >= l.MaxAgents {
		return fmt.Sprintf("Docker host capacity: %d of %d k3s agents are running (NEBIUS_MOCKER_MK8S_K3S_MAX_AGENTS)",
			h.Agents, l.MaxAgents)
	}
	if l.AgentCPU > 0 {
		maxByCPU := int(math.Floor(float64(h.CPUs) / l.AgentCPU))
		if h.Agents >= maxByCPU {
			return fmt.Sprintf("Docker host capacity: %d CPUs fit %d k3s agents at %.2g CPU each (NEBIUS_MOCKER_MK8S_K3S_AGENT_CPU), %d are running",
				h.CPUs, maxByCPU, l.AgentCPU, h.Agents)
		}
	}
	free := h.Memory - h.Used - l.MemoryReserve
	if free < l.AgentMemory {
		return fmt.Sprintf("Docker host capacity: %s of %s memory is free after a %s reserve, a k3s agent needs %s (NEBIUS_MOCKER_MK8S_K3S_AGENT_MEMORY)",
			bytesString(max(free, 0)), bytesString(h.Memory), bytesString(l.MemoryReserve), bytesString(l.AgentMemory))
	}
	return ""
}

// bytesString formats n in MiB or GiB.
func bytesString(n int64) string {
	const mib, gib = 1 << 20, 1 << 30
	if n >= gib {
		return fmt.Sprintf("%.1f GiB", float64(n)/gib)
	}
	return fmt.Sprintf("%d MiB", n/mib)
}
