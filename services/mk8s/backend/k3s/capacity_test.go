package k3s

import (
	"strings"
	"testing"
)

func TestAdmitAgent(t *testing.T) {
	t.Parallel()
	const mi, gi = int64(1 << 20), int64(1 << 30)
	limits := capacityLimits{AgentMemory: 384 * mi, AgentCPU: 0.5, MemoryReserve: gi}
	tests := []struct {
		name       string
		limits     func(capacityLimits) capacityLimits
		load       hostLoad
		wantReason string // substring; "" admits
	}{
		{name: "fits", load: hostLoad{CPUs: 8, Memory: 16 * gi, Used: 2 * gi, Agents: 3}},
		{
			name:   "max agents reached",
			limits: func(l capacityLimits) capacityLimits { l.MaxAgents = 2; return l },
			load:   hostLoad{CPUs: 8, Memory: 16 * gi, Agents: 2}, wantReason: "NEBIUS_MOCKER_MK8S_K3S_MAX_AGENTS",
		},
		{
			name:   "below max agents",
			limits: func(l capacityLimits) capacityLimits { l.MaxAgents = 2; return l },
			load:   hostLoad{CPUs: 8, Memory: 16 * gi, Agents: 1},
		},
		{name: "no agent cap by default", load: hostLoad{CPUs: 64, Memory: 256 * gi, Agents: 50}},
		{name: "cpu cap reached", load: hostLoad{CPUs: 4, Memory: 16 * gi, Agents: 8}, wantReason: "NEBIUS_MOCKER_MK8S_K3S_AGENT_CPU"},
		{name: "cpu cap one below", load: hostLoad{CPUs: 4, Memory: 16 * gi, Agents: 7}},
		{
			name:   "no cpu limit",
			limits: func(l capacityLimits) capacityLimits { l.AgentCPU = 0; return l },
			load:   hostLoad{CPUs: 1, Memory: 16 * gi, Agents: 40},
		},
		{
			name: "memory exhausted", load: hostLoad{CPUs: 8, Memory: 4 * gi, Used: 3 * gi},
			wantReason: "NEBIUS_MOCKER_MK8S_K3S_AGENT_MEMORY",
		},
		{name: "memory exactly one agent free", load: hostLoad{CPUs: 8, Memory: 2*gi + 384*mi, Used: gi}},
		{
			name: "memory one byte short", load: hostLoad{CPUs: 8, Memory: 2*gi + 384*mi - 1, Used: gi},
			wantReason: "NEBIUS_MOCKER_MK8S_K3S_AGENT_MEMORY",
		},
		{
			name: "used above total", load: hostLoad{CPUs: 8, Memory: 2 * gi, Used: 4 * gi},
			wantReason: "0 MiB of 2.0 GiB",
		},
		{
			name:   "max agents is reported before cpu",
			limits: func(l capacityLimits) capacityLimits { l.MaxAgents = 1; return l },
			load:   hostLoad{CPUs: 1, Memory: gi, Agents: 5}, wantReason: "NEBIUS_MOCKER_MK8S_K3S_MAX_AGENTS",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := limits
			if tt.limits != nil {
				l = tt.limits(l)
			}
			got := admitAgent(l, tt.load)
			if tt.wantReason == "" {
				if got != "" {
					t.Fatalf("admitAgent(%+v, %+v) = %q, want admitted", l, tt.load, got)
				}
				return
			}
			if !strings.Contains(got, tt.wantReason) {
				t.Fatalf("admitAgent(%+v, %+v) = %q, want it to contain %q", l, tt.load, got, tt.wantReason)
			}
		})
	}
}

func TestBytesString(t *testing.T) {
	t.Parallel()
	for n, want := range map[int64]string{0: "0 MiB", 384 << 20: "384 MiB", 1 << 30: "1.0 GiB", 3 << 29: "1.5 GiB"} {
		if got := bytesString(n); got != want {
			t.Errorf("bytesString(%d) = %q, want %q", n, got, want)
		}
	}
}
