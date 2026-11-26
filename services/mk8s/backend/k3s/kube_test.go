package k3s

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseGitVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "v1.35.10+k3s1", want: "1.35.10"},
		{in: "v1.35.10", want: "1.35.10"},
		{in: "1.35.2-nebius-cp.3", want: "1.35.2"},
		{in: "", wantErr: true},
		{in: "v1.35", wantErr: true},
		{in: "v1.35.10.1", wantErr: true},
		{in: "vx.y.z", wantErr: true},
		{in: "v1.35.x+k3s1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseGitVersion(tt.in)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("parseGitVersion(%q) = %q, %v; want %q (error %v)", tt.in, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestParseAdminKubeconfig(t *testing.T) {
	t.Parallel()
	got, err := parseAdminKubeconfig(testKubeconfig())
	if err != nil {
		t.Fatalf("parseAdminKubeconfig: %v", err)
	}
	if string(got.CA) != testCA || string(got.Cert) != "CERT" || string(got.Key) != "KEY" {
		t.Fatalf("parseAdminKubeconfig = %q/%q/%q, want the decoded CA, certificate and key", got.CA, got.Cert, got.Key)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("x"))
	bad := map[string]string{
		"not yaml":     "clusters: [",
		"no clusters":  "apiVersion: v1\nkind: Config\n",
		"bad base64":   "clusters:\n- cluster: {certificate-authority-data: '!!!'}\nusers:\n- user: {client-certificate-data: " + b64 + ", client-key-data: " + b64 + "}\n",
		"empty data":   "clusters:\n- cluster: {}\nusers:\n- user: {client-certificate-data: " + b64 + ", client-key-data: " + b64 + "}\n",
		"missing user": "clusters:\n- cluster: {certificate-authority-data: " + b64 + "}\n",
	}
	for name, in := range bad {
		if _, err := parseAdminKubeconfig([]byte(in)); err == nil {
			t.Errorf("parseAdminKubeconfig(%s) succeeded, want an error", name)
		}
	}
}

func TestKubeletLabel(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"team":                                 true,
		"nebius.ai/gpu":                        true,
		"example.com/x":                        true,
		"kubernetes.io/hostname":               true,
		"kubernetes.io/os":                     true,
		"topology.kubernetes.io/zone":          true,
		"node.kubernetes.io/instance-type":     true,
		"node.kubernetes.io/exclude":           true,
		"kubelet.kubernetes.io/anything":       true,
		"beta.kubernetes.io/instance-type":     true,
		"kubernetes.io/foo":                    false,
		"node-role.kubernetes.io/gpu":          false,
		"beta.kubernetes.io/os":                false,
		"k8s.io/x":                             false,
		"foo.k8s.io/bar":                       false,
		"node-restriction.kubernetes.io/owner": false,
	}
	for key, want := range tests {
		if got := kubeletLabel(key); got != want {
			t.Errorf("kubeletLabel(%q) = %v, want %v", key, got, want)
		}
	}
}

type recordedRequest struct {
	Method, Path, ContentType, Body string
}

// kubeServer is a TLS API server stand-in: it records requests and answers
// with the canned status and body of the path.
type kubeServer struct {
	mu        sync.Mutex
	requests  []recordedRequest
	responses map[string]cannedResponse // "METHOD path"
}

type cannedResponse struct {
	status int
	body   string
}

func newKubeServer(t *testing.T, responses map[string]cannedResponse) (*kubeServer, *restKube) {
	t.Helper()
	ks := &kubeServer{responses: responses}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ks.mu.Lock()
		ks.requests = append(ks.requests, recordedRequest{r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(body)})
		resp, ok := ks.responses[r.Method+" "+r.URL.Path]
		ks.mu.Unlock()
		if !ok {
			resp = cannedResponse{http.StatusOK, "{}"}
		}
		w.WriteHeader(resp.status)
		_, _ = io.WriteString(w, resp.body)
	}))
	t.Cleanup(srv.Close)
	return ks, &restKube{base: srv.URL, http: srv.Client()}
}

func (ks *kubeServer) last(t *testing.T) recordedRequest {
	t.Helper()
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if len(ks.requests) == 0 {
		t.Fatal("the API server got no request")
	}
	return ks.requests[len(ks.requests)-1]
}

func TestRestKubeReady(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		resp    cannedResponse
		wantErr bool
	}{
		{name: "ok", resp: cannedResponse{http.StatusOK, "ok"}},
		{name: "not ok", resp: cannedResponse{http.StatusOK, "[-]etcd failed"}, wantErr: true},
		{name: "unavailable", resp: cannedResponse{http.StatusServiceUnavailable, "starting"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, k := newKubeServer(t, map[string]cannedResponse{"GET /readyz": tt.resp})
			if err := k.Ready(t.Context()); (err != nil) != tt.wantErr {
				t.Fatalf("Ready() = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

func TestRestKubeVersionAndNodes(t *testing.T) {
	t.Parallel()
	_, k := newKubeServer(t, map[string]cannedResponse{
		"GET /version": {http.StatusOK, `{"gitVersion":"v1.35.10+k3s1"}`},
		"GET /api/v1/nodes": {http.StatusOK, `{"items":[
			{"metadata":{"name":"n1","labels":{"a":"b"}},"status":{"capacity":{"nvidia.com/gpu":"8"},
				"conditions":[{"type":"MemoryPressure","status":"False"},{"type":"Ready","status":"True"}]}},
			{"metadata":{"name":"n2"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}`},
	})
	v, err := k.Version(t.Context())
	if err != nil || v != "1.35.10" {
		t.Fatalf("Version() = %q, %v; want 1.35.10", v, err)
	}
	nodes, err := k.Nodes(t.Context())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	want := map[string]kubeNode{
		"n1": {Ready: true, Labels: map[string]string{"a": "b"}, Capacity: map[string]string{"nvidia.com/gpu": "8"}},
		"n2": {},
	}
	if diff := cmp.Diff(want, nodes); diff != "" {
		t.Fatalf("Nodes() mismatch (-want +got):\n%s", diff)
	}
}

func TestRestKubeLabelNode(t *testing.T) {
	t.Parallel()
	ks, k := newKubeServer(t, nil)
	if err := k.LabelNode(t.Context(), "n1", map[string]string{"node-role.kubernetes.io/gpu": "true"}); err != nil {
		t.Fatalf("LabelNode: %v", err)
	}
	got := ks.last(t)
	if got.Method != http.MethodPatch || got.Path != "/api/v1/nodes/n1" || got.ContentType != "application/merge-patch+json" {
		t.Fatalf("LabelNode request = %+v, want a merge patch of /api/v1/nodes/n1", got)
	}
	assertJSON(t, got.Body, `{"metadata":{"labels":{"node-role.kubernetes.io/gpu":"true"}}}`)
}

func TestRestKubeSetExtendedResource(t *testing.T) {
	t.Parallel()
	ks, k := newKubeServer(t, nil)
	if err := k.SetExtendedResource(t.Context(), "n1", "nvidia.com/gpu", 8); err != nil {
		t.Fatalf("SetExtendedResource: %v", err)
	}
	got := ks.last(t)
	if got.Method != http.MethodPatch || got.Path != "/api/v1/nodes/n1/status" || got.ContentType != "application/json-patch+json" {
		t.Fatalf("SetExtendedResource request = %+v, want a JSON patch of /api/v1/nodes/n1/status", got)
	}
	assertJSON(t, got.Body, `[{"op":"add","path":"/status/capacity/nvidia.com~1gpu","value":"8"}]`)
}

func TestRestKubeDeleteNode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "deleted", status: http.StatusOK},
		{name: "already gone", status: http.StatusNotFound},
		{name: "forbidden", status: http.StatusForbidden, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ks, k := newKubeServer(t, map[string]cannedResponse{"DELETE /api/v1/nodes/n1": {tt.status, `{"message":"x"}`}})
			err := k.DeleteNode(t.Context(), "n1")
			if (err != nil) != tt.wantErr {
				t.Fatalf("DeleteNode() = %v, want error %v", err, tt.wantErr)
			}
			if got := ks.last(t); got.Method != http.MethodDelete || got.Path != "/api/v1/nodes/n1" {
				t.Fatalf("DeleteNode request = %+v", got)
			}
		})
	}
}

func TestRestKubeErrorCarriesBody(t *testing.T) {
	t.Parallel()
	_, k := newKubeServer(t, map[string]cannedResponse{"GET /version": {http.StatusInternalServerError, "etcd unavailable"}})
	_, err := k.Version(t.Context())
	if err == nil || !strings.Contains(err.Error(), "etcd unavailable") {
		t.Fatalf("Version() error = %v, want the response body in it", err)
	}
}

func TestNewRestKubeRejectsBadCredentials(t *testing.T) {
	t.Parallel()
	if _, err := newRestKube("https://127.0.0.1:1", adminKubeconfig{CA: []byte("x"), Cert: []byte("c"), Key: []byte("k")}); err == nil {
		t.Fatal("newRestKube with a non-PEM CA succeeded, want an error")
	}
}

func assertJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("body %q is not JSON: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want %q: %v", want, err)
	}
	if diff := cmp.Diff(w, g); diff != "" {
		t.Fatalf("body mismatch (-want +got):\n%s", diff)
	}
}
