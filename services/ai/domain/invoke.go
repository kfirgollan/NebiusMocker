package domain

import (
	"context"
	"crypto/subtle"
	"strconv"
	"strings"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Call is an authorized call to an endpoint's stub data plane.
type Call struct {
	EndpointID string
	// Port is the container port of the HTTP port being called.
	Port int32
}

// AuthorizeCall decides whether a data-plane request may reach an endpoint.
// target is the first path segment of the request ("<endpoint-id>" or
// "<endpoint-id>:<container_port>") and authorization the raw Authorization
// header ("" when absent).
//
// Errors: errs.NotFound for an unknown endpoint or port; errs.BadState when
// the endpoint is not RUNNING (or its token secret is unreadable);
// errs.Unauthenticated for a missing or wrong bearer token. Endpoints without
// authentication configured are open.
func (s *EndpointService) AuthorizeCall(ctx context.Context, target, authorization string) (Call, error) {
	id, portStr, hasPort := strings.Cut(target, ":")
	if hasPort && portStr == "" {
		return Call{}, errs.NotFound(target)
	}
	e, err := s.repo.Get(ctx, id)
	if err != nil {
		return Call{}, err
	}
	port, err := callPort(e, portStr)
	if err != nil {
		return Call{}, err
	}
	if st := e.GetStatus().GetState(); st != aipb.EndpointStatus_RUNNING {
		return Call{}, errs.BadState(id, "endpoint is not running (state "+st.String()+")")
	}
	token, err := s.authToken(ctx, e.GetSpec())
	if err != nil {
		return Call{}, err
	}
	if token != "" {
		if err := checkBearer(authorization, token); err != nil {
			return Call{}, err
		}
	}
	return Call{EndpointID: id, Port: port}, nil
}

// callPort picks the HTTP port addressed by the request: the given container
// port, or the first HTTP port when portStr is empty.
func callPort(e *aipb.Endpoint, portStr string) (int32, error) {
	ports := httpPorts(e.GetSpec())
	id := e.GetMetadata().GetId()
	if len(ports) == 0 {
		return 0, errs.NotFound(id + " (no HTTP port)")
	}
	if portStr == "" {
		return ports[0].GetContainerPort(), nil
	}
	n, err := strconv.ParseInt(portStr, 10, 32)
	if err != nil {
		return 0, errs.NotFound(id + ":" + portStr)
	}
	for _, p := range ports {
		if int64(p.GetContainerPort()) == n {
			return p.GetContainerPort(), nil
		}
	}
	return 0, errs.NotFound(id + ":" + portStr)
}

func checkBearer(authorization, token string) error {
	scheme, got, ok := strings.Cut(strings.TrimSpace(authorization), " ")
	if authorization == "" || !ok || !strings.EqualFold(scheme, "Bearer") {
		return errs.Unauthenticated("missing bearer token")
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(token)) != 1 {
		return errs.Unauthenticated("invalid token")
	}
	return nil
}

// AuthorizeTCP decides whether a connection to an endpoint's TCP port on the
// TLS stub listener may proceed. TCP ports carry no endpoint authentication
// (Nebius allows auth only with a single HTTP port).
//
// Errors: errs.NotFound for an unknown endpoint or a port that is not one of
// its TCP ports; errs.BadState when the endpoint is not RUNNING.
func (s *EndpointService) AuthorizeTCP(ctx context.Context, endpointID string, port int32) (Call, error) {
	e, err := s.repo.Get(ctx, endpointID)
	if err != nil {
		return Call{}, err
	}
	found := false
	for _, p := range e.GetSpec().GetPorts() {
		if p.GetProtocol() == aipb.EndpointSpec_Port_TCP && p.GetContainerPort() == port {
			found = true
		}
	}
	if !found {
		return Call{}, errs.NotFound(endpointID + ":" + strconv.Itoa(int(port)))
	}
	if st := e.GetStatus().GetState(); st != aipb.EndpointStatus_RUNNING {
		return Call{}, errs.BadState(endpointID, "endpoint is not running (state "+st.String()+")")
	}
	return Call{EndpointID: endpointID, Port: port}, nil
}
