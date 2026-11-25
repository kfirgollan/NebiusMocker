package domain

import (
	"fmt"
	"hash/fnv"
	"slices"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

type state = aipb.EndpointStatus_State

// verb is a lifecycle action.
type verb string

const (
	verbStart   verb = "start"
	verbStop    verb = "stop"
	verbRestart verb = "restart"
	verbDelete  verb = "delete"
)

// transition is one row of the lifecycle table: the states a verb is allowed
// from, the state shown while its operation runs and the state it ends in.
type transition struct {
	from    []state
	during  state
	settled state
}

// lifecycle is the endpoint state machine. Create is PROVISIONING → RUNNING.
// Delete ends by removing the endpoint, so it has no settled state.
var lifecycle = map[verb]transition{
	verbStart: {
		from:    []state{aipb.EndpointStatus_STOPPED, aipb.EndpointStatus_ERROR},
		during:  aipb.EndpointStatus_STARTING,
		settled: aipb.EndpointStatus_RUNNING,
	},
	verbStop: {
		from:    []state{aipb.EndpointStatus_RUNNING, aipb.EndpointStatus_ERROR},
		during:  aipb.EndpointStatus_STOPPING,
		settled: aipb.EndpointStatus_STOPPED,
	},
	verbRestart: {
		from:    []state{aipb.EndpointStatus_RUNNING, aipb.EndpointStatus_ERROR},
		during:  aipb.EndpointStatus_STARTING,
		settled: aipb.EndpointStatus_RUNNING,
	},
	verbDelete: {
		from: []state{
			aipb.EndpointStatus_PROVISIONING, aipb.EndpointStatus_STARTING, aipb.EndpointStatus_IMAGE_PULLING,
			aipb.EndpointStatus_RUNNING, aipb.EndpointStatus_STOPPING, aipb.EndpointStatus_STOPPED,
			aipb.EndpointStatus_ERROR,
		},
		during: aipb.EndpointStatus_DELETING,
	},
}

// checkTransition returns errs.BadState if v is not allowed from cur.
func checkTransition(id string, v verb, cur state) (transition, error) {
	t := lifecycle[v]
	if !slices.Contains(t.from, cur) {
		return t, errs.BadState(id, fmt.Sprintf("cannot %s an endpoint in state %s", v, cur))
	}
	return t, nil
}

// stateDetails are the short codes shown in status.state_details.
var stateDetails = map[state][2]string{
	aipb.EndpointStatus_PROVISIONING: {"Provisioning", "Creating the endpoint's resources."},
	aipb.EndpointStatus_STARTING:     {"Starting", "Starting the endpoint's workload."},
	aipb.EndpointStatus_RUNNING:      {"Running", "The endpoint is running (NebiusMocker stub: no container is run)."},
	aipb.EndpointStatus_STOPPING:     {"Stopping", "Stopping the endpoint."},
	aipb.EndpointStatus_STOPPED:      {"Stopped", "The endpoint is stopped."},
	aipb.EndpointStatus_DELETING:     {"Deleting", "Deleting the endpoint."},
}

// setState moves e to st. Entering RUNNING creates a fresh synthetic
// instance (a stop releases the VM, a restart creates a new one); STOPPED
// releases it. instanceID is used only when entering RUNNING.
func setState(e *aipb.Endpoint, st state, instanceID string) {
	status := e.GetStatus()
	status.State = st
	d := stateDetails[st]
	status.StateDetails = &aipb.EndpointStateDetails{Code: d[0], Message: d[1]}
	switch st {
	case aipb.EndpointStatus_RUNNING:
		status.Instances = []*aipb.EndpointInstanceStatus{syntheticInstance(e.GetSpec(), instanceID)}
	case aipb.EndpointStatus_STOPPED:
		status.Instances = nil
	case aipb.EndpointStatus_STATE_UNSPECIFIED, aipb.EndpointStatus_PROVISIONING, aipb.EndpointStatus_STARTING,
		aipb.EndpointStatus_IMAGE_PULLING, aipb.EndpointStatus_STOPPING, aipb.EndpointStatus_DELETING,
		aipb.EndpointStatus_ERROR:
		for _, in := range status.GetInstances() {
			in.State = instanceState[st]
		}
	}
}

var instanceState = map[state]aipb.EndpointInstanceStatus_State{
	aipb.EndpointStatus_PROVISIONING: aipb.EndpointInstanceStatus_PROVISIONING,
	aipb.EndpointStatus_STARTING:     aipb.EndpointInstanceStatus_STARTING,
	aipb.EndpointStatus_RUNNING:      aipb.EndpointInstanceStatus_RUNNING,
	aipb.EndpointStatus_STOPPING:     aipb.EndpointInstanceStatus_STOPPING,
	aipb.EndpointStatus_STOPPED:      aipb.EndpointInstanceStatus_STOPPED,
	aipb.EndpointStatus_DELETING:     aipb.EndpointInstanceStatus_DELETING,
}

// syntheticInstance describes the VM a real endpoint would run on. Its
// addresses are deterministic per instance ID; nothing is created.
func syntheticInstance(spec *aipb.EndpointSpec, instanceID string) *aipb.EndpointInstanceStatus {
	h := fnv.New32a()
	_, _ = h.Write([]byte(instanceID)) // hash.Hash writes never fail
	n := h.Sum32()
	in := &aipb.EndpointInstanceStatus{
		State:                aipb.EndpointInstanceStatus_RUNNING,
		ComputeInstanceId:    instanceID,
		ComputeInstanceState: computepb.InstanceStatus_RUNNING,
		PrivateIp:            fmt.Sprintf("10.0.%d.%d", (n>>8)%256, n%254+1),
	}
	// A public IP is assigned for public_ip, and for SSH access (docs).
	if spec.GetPublicIp() || len(spec.GetSshAuthorizedKeys()) > 0 {
		in.PublicIp = fmt.Sprintf("198.51.100.%d", (n>>16)%254+1)
	}
	return in
}
