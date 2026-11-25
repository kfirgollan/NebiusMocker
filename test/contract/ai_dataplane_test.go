package contract

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"
)

// dataPlaneResult is the outcome of one data-plane call.
type dataPlaneResult struct {
	status int
	body   map[string]any
}

// callDataPlane sends a request with an optional Authorization header.
func callDataPlane(t *testing.T, method, url, authorization string) dataPlaneResult {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	res := dataPlaneResult{status: resp.StatusCode}
	if err := json.Unmarshal(raw, &res.body); err != nil {
		t.Fatalf("body %q is not JSON: %v", raw, err)
	}
	return res
}

func TestAIDataPlane(t *testing.T) {
	t.Parallel()
	env := startAI(t, nil)
	base := env.mod.DataPlaneURL()
	parent := env.inst.ProjectID

	id := aiCreate(t, env.svc, parent, "secured", nil)
	open := aiCreate(t, env.svc, parent, "open", func(s *aipb.EndpointSpec) { s.AuthToken = "" })
	stopped := aiCreate(t, env.svc, parent, "stopped", nil)
	op, err := env.svc.Stop(t.Context(), &aipb.StopEndpointRequest{Id: stopped})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Stop wait: %v", err)
	}

	t.Run("correct token", func(t *testing.T) {
		t.Parallel()
		res := callDataPlane(t, http.MethodPost, base+"/"+id+"/v1/chat/completions", "Bearer "+aiToken)
		if res.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %v)", res.status, res.body)
		}
		for k, want := range map[string]any{
			"message": "endpoint was called", "endpoint_id": id, "method": "POST", "path": "/v1/chat/completions",
		} {
			if res.body[k] != want {
				t.Errorf("body[%q] = %v, want %v", k, res.body[k], want)
			}
		}
	})
	t.Run("missing token", func(t *testing.T) {
		t.Parallel()
		if res := callDataPlane(t, http.MethodGet, base+"/"+id+"/x", ""); res.status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", res.status)
		}
	})
	t.Run("wrong token", func(t *testing.T) {
		t.Parallel()
		if res := callDataPlane(t, http.MethodGet, base+"/"+id+"/x", "Bearer nope"); res.status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", res.status)
		}
	})
	t.Run("stopped endpoint", func(t *testing.T) {
		t.Parallel()
		if res := callDataPlane(t, http.MethodGet, base+"/"+stopped+"/x", "Bearer "+aiToken); res.status != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", res.status)
		}
	})
	t.Run("unknown endpoint", func(t *testing.T) {
		t.Parallel()
		if res := callDataPlane(t, http.MethodGet, base+"/aiendpoint-nope/x", ""); res.status != http.StatusNotFound {
			t.Errorf("status = %d, want 404", res.status)
		}
	})
	t.Run("no authentication configured", func(t *testing.T) {
		t.Parallel()
		if res := callDataPlane(t, http.MethodGet, base+"/"+open+"/x", ""); res.status != http.StatusOK {
			t.Errorf("status = %d, want 200", res.status)
		}
	})
}

func TestAIDataPlaneTwoPorts(t *testing.T) {
	t.Parallel()
	env := startAI(t, nil)
	id := aiCreate(t, env.svc, env.inst.ProjectID, "two", func(s *aipb.EndpointSpec) {
		s.AuthToken = ""
		s.Ports = []*aipb.EndpointSpec_Port{
			{ContainerPort: 80, Protocol: aipb.EndpointSpec_Port_HTTP},
			{ContainerPort: 8080, Protocol: aipb.EndpointSpec_Port_HTTP},
		}
	})
	urls := aiGet(t, env.svc, id).GetStatus().GetPublicEndpoints()
	if len(urls) != 2 {
		t.Fatalf("public_endpoints = %v, want 2", urls)
	}
	for i, port := range []int{80, 8080} {
		want := fmt.Sprintf("%s/%s:%d", env.mod.DataPlaneURL(), id, port)
		if urls[i] != want {
			t.Errorf("public_endpoints[%d] = %q, want %q", i, urls[i], want)
		}
		res := callDataPlane(t, http.MethodGet, urls[i]+"/ping", "")
		if res.status != http.StatusOK || res.body["port"] != float64(port) {
			t.Errorf("GET %s: status %d, body %v; want 200 with port %d", urls[i], res.status, res.body, port)
		}
	}
	if res := callDataPlane(t, http.MethodGet, fmt.Sprintf("%s/%s:9999/ping", env.mod.DataPlaneURL(), id), ""); res.status != http.StatusNotFound {
		t.Errorf("unknown port: status %d, want 404", res.status)
	}
}
