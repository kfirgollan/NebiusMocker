package dockerhost_test

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// newClient serves h on a TCP httptest server and returns a client for it.
func newClient(t *testing.T, h http.HandlerFunc) *dockerhost.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := dockerhost.New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	return c
}

func jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestUnixSocket(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.43/_ping" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, "OK")
	})}}
	srv.Start()
	t.Cleanup(srv.Close)
	c, err := dockerhost.New("unix://" + sock)
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("Ping = %v", err)
	}
	if c.Host() != "unix://"+sock {
		t.Errorf("Host = %q", c.Host())
	}
}

func TestNewRejectsBadHost(t *testing.T) {
	t.Parallel()
	for _, h := range []string{"http://x", "tcp://", "npipe:////./pipe/docker"} {
		if _, err := dockerhost.New(h); err == nil {
			t.Errorf("New(%q) = nil error, want error", h)
		}
	}
}

func TestUnavailable(t *testing.T) {
	t.Parallel()
	c, err := dockerhost.New("unix://" + filepath.Join(t.TempDir(), "none.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(t.Context()); !errors.Is(err, dockerhost.ErrUnavailable) {
		t.Fatalf("Ping err = %v, want ErrUnavailable", err)
	}
}

func TestErrorMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status  int
		want    error
		wantMsg string
	}{
		{http.StatusNotFound, dockerhost.ErrNotFound, "No such container"},
		{http.StatusConflict, dockerhost.ErrConflict, "No such container"},
		{http.StatusInternalServerError, nil, "500: No such container"},
	}
	for _, tt := range tests {
		c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			jsonReply(w, tt.status, map[string]string{"message": "No such container"})
		})
		_, err := c.InspectContainer(t.Context(), "x")
		if err == nil || !strings.Contains(err.Error(), tt.wantMsg) || !strings.Contains(err.Error(), "GET /containers/x/json") {
			t.Errorf("status %d err = %v, want message %q", tt.status, err, tt.wantMsg)
		}
		if tt.want != nil && !errors.Is(err, tt.want) {
			t.Errorf("status %d err = %v, want Is %v", tt.status, err, tt.want)
		}
	}
}

func TestInfo(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.43/info" {
			t.Errorf("path = %q", r.URL.Path)
		}
		jsonReply(w, 200, map[string]any{
			"NCPU": 4, "MemTotal": 8192, "OperatingSystem": "OS", "Architecture": "aarch64", "ServerVersion": "27.0",
			"SystemTime": "2026-11-26T13:08:06.123456789Z",
		})
	})
	got, err := c.Info(t.Context())
	want := dockerhost.Info{
		NCPU: 4, MemTotal: 8192, OperatingSystem: "OS", Architecture: "aarch64", ServerVersion: "27.0",
		SystemTime: time.Date(2026, 11, 26, 13, 8, 6, 123456789, time.UTC),
	}
	if err != nil || got != want {
		t.Fatalf("Info = %+v, %v; want %+v", got, err, want)
	}
}

func TestEnsureImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		ref       string
		present   bool
		stream    string
		wantQuery string
		wantErr   string
	}{
		{name: "present", ref: "alpine:3.20", present: true},
		{name: "tag", ref: "rancher/k3s:v1.30", stream: `{"status":"Pulling"}` + "\n" + `{"status":"Done"}` + "\n", wantQuery: "fromImage=rancher%2Fk3s&tag=v1.30"},
		{name: "default tag", ref: "alpine", stream: `{"status":"ok"}`, wantQuery: "fromImage=alpine&tag=latest"},
		{name: "registry port", ref: "localhost:5000/a/b", stream: `{}`, wantQuery: "fromImage=localhost%3A5000%2Fa%2Fb&tag=latest"},
		{name: "digest", ref: "a/b@sha256:abc", stream: `{}`, wantQuery: "fromImage=a%2Fb&tag=sha256%3Aabc"},
		{name: "stream error", ref: "x:1", stream: `{"status":"p"}` + "\n" + `{"errorDetail":{"message":"denied"},"error":"denied"}` + "\n", wantQuery: "fromImage=x&tag=1", wantErr: "denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var pulled string
			c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
					if want := "/v1.43/images/" + tt.ref + "/json"; r.URL.Path != want {
						t.Errorf("inspect path = %q, want %q", r.URL.Path, want)
					}
					if tt.present {
						jsonReply(w, 200, map[string]string{})
						return
					}
					jsonReply(w, 404, map[string]string{"message": "no such image"})
				case r.Method == http.MethodPost && r.URL.Path == "/v1.43/images/create":
					pulled = r.URL.RawQuery
					_, _ = io.WriteString(w, tt.stream)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
				}
			})
			err := c.EnsureImage(t.Context(), tt.ref)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("EnsureImage err = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("EnsureImage = %v", err)
			}
			if pulled != tt.wantQuery {
				t.Errorf("pull query = %q, want %q", pulled, tt.wantQuery)
			}
		})
	}
}

func TestNetworks(t *testing.T) {
	t.Parallel()
	var created map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.43/networks/net1":
			jsonReply(w, 404, map[string]string{"message": "nope"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1.43/networks/create":
			_ = json.NewDecoder(r.Body).Decode(&created)
			jsonReply(w, 201, map[string]string{"Id": "nid"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1.43/networks/have":
			jsonReply(w, 200, map[string]string{"Id": "existing"})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1.43/networks/gone":
			jsonReply(w, 404, map[string]string{"message": "nope"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1.43/networks":
			if got := r.URL.Query().Get("filters"); got != `{"label":["a=b","k"]}` {
				t.Errorf("filters = %q", got)
			}
			jsonReply(w, 200, []map[string]any{{"Id": "i", "Name": "n", "Labels": map[string]string{"a": "b"}}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	})
	id, err := c.EnsureNetwork(t.Context(), "net1", map[string]string{"a": "b"})
	if err != nil || id != "nid" {
		t.Fatalf("EnsureNetwork = %q, %v", id, err)
	}
	if created["Name"] != "net1" || created["Driver"] != "bridge" {
		t.Errorf("create body = %v", created)
	}
	if id, err := c.EnsureNetwork(t.Context(), "have", nil); err != nil || id != "existing" {
		t.Errorf("EnsureNetwork existing = %q, %v", id, err)
	}
	if err := c.RemoveNetwork(t.Context(), "gone"); err != nil {
		t.Errorf("RemoveNetwork 404 = %v, want nil", err)
	}
	ns, err := c.ListNetworks(t.Context(), map[string]string{"a": "b", "k": ""})
	if err != nil || len(ns) != 1 || ns[0].Name != "n" || ns[0].Labels["a"] != "b" {
		t.Errorf("ListNetworks = %+v, %v", ns, err)
	}
}

func TestVolumes(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1.43/volumes/create":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["Name"] != "v1" {
				t.Errorf("create body = %v", body)
			}
			jsonReply(w, 201, body)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1.43/volumes/v1":
			if r.URL.Query().Get("force") != "true" {
				t.Errorf("query = %q", r.URL.RawQuery)
			}
			jsonReply(w, 404, map[string]string{"message": "gone"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1.43/volumes":
			jsonReply(w, 200, map[string]any{"Volumes": []map[string]string{{"Name": "v1"}, {"Name": "v2"}}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	})
	if err := c.EnsureVolume(t.Context(), "v1", map[string]string{"a": "b"}); err != nil {
		t.Fatalf("EnsureVolume = %v", err)
	}
	if err := c.RemoveVolume(t.Context(), "v1"); err != nil {
		t.Errorf("RemoveVolume 404 = %v, want nil", err)
	}
	names, err := c.ListVolumes(t.Context(), map[string]string{"a": "b"})
	if err != nil || len(names) != 2 || names[1] != "v2" {
		t.Errorf("ListVolumes = %v, %v", names, err)
	}
}

func readTar(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		data, _ := io.ReadAll(tr)
		if h.Typeflag == tar.TypeDir {
			out[h.Name] = "<dir>"
			continue
		}
		out[h.Name] = string(data)
	}
}

func TestCreateContainerWireFormat(t *testing.T) {
	t.Parallel()
	var create map[string]any
	var name string
	var archive map[string]string
	var order []string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1.43/containers/create":
			name = r.URL.Query().Get("name")
			_ = json.NewDecoder(r.Body).Decode(&create)
			jsonReply(w, 201, map[string]string{"Id": "cid"})
		case "/v1.43/containers/cid/archive":
			if r.URL.Query().Get("path") != "/" || r.Header.Get("Content-Type") != "application/x-tar" {
				t.Errorf("archive request: %s %v", r.URL.RawQuery, r.Header)
			}
			archive = readTar(t, r.Body)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	})
	id, err := c.CreateContainer(t.Context(), dockerhost.ContainerSpec{
		Name: "k3s", Image: "rancher/k3s:v1", Cmd: []string{"server"}, Env: []string{"A=B"},
		Labels: map[string]string{"l": "v"}, Privileged: true, Tmpfs: map[string]string{"/run": ""},
		Network: "net", Aliases: []string{"api"}, Ports: []dockerhost.PortBinding{{ContainerPort: "6443/tcp", HostIP: "127.0.0.1"}},
		ExtraHosts: []string{"host.docker.internal:host-gateway"}, Memory: 1 << 30, Binds: []string{"vol:/var/lib/rancher/k3s"},
		Files: []dockerhost.File{{Path: "/etc/rancher/k3s/config.yaml", Mode: 0o600, Content: []byte("a: b")}},
	})
	if err != nil || id != "cid" {
		t.Fatalf("CreateContainer = %q, %v", id, err)
	}
	if name != "k3s" {
		t.Errorf("name = %q", name)
	}
	raw, _ := json.Marshal(create)
	for _, want := range []string{
		`"Image":"rancher/k3s:v1"`, `"Cmd":["server"]`, `"Env":["A=B"]`, `"Labels":{"l":"v"}`,
		`"ExposedPorts":{"6443/tcp":{}}`, `"Privileged":true`, `"Tmpfs":{"/run":""}`,
		`"PortBindings":{"6443/tcp":[{"HostIp":"127.0.0.1","HostPort":""}]}`,
		`"ExtraHosts":["host.docker.internal:host-gateway"]`, `"Memory":1073741824`, `"NetworkMode":"net"`,
		`"Binds":["vol:/var/lib/rancher/k3s"]`, `"EndpointsConfig":{"net":{"Aliases":["api"]}}`,
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("create body missing %s\n%s", want, raw)
		}
	}
	wantTar := map[string]string{"etc/": "<dir>", "etc/rancher/": "<dir>", "etc/rancher/k3s/": "<dir>", "etc/rancher/k3s/config.yaml": "a: b"}
	if len(archive) != len(wantTar) {
		t.Errorf("archive = %v, want %v", archive, wantTar)
	}
	for k, v := range wantTar {
		if archive[k] != v {
			t.Errorf("archive[%q] = %q, want %q", k, archive[k], v)
		}
	}
	if len(order) != 2 || order[0] != "POST /v1.43/containers/create" {
		t.Errorf("call order = %v", order)
	}
}

func TestCreateContainerConflictAndCopyFailureCleanup(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, 409, map[string]string{"message": "name in use"})
	})
	if _, err := c.CreateContainer(t.Context(), dockerhost.ContainerSpec{Name: "x"}); !errors.Is(err, dockerhost.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}

	var removed bool
	c = newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/create"):
			jsonReply(w, 201, map[string]string{"Id": "cid"})
		case r.Method == http.MethodPut:
			jsonReply(w, 500, map[string]string{"message": "disk full"})
		case r.Method == http.MethodDelete:
			removed = true
		}
	})
	_, err := c.CreateContainer(t.Context(), dockerhost.ContainerSpec{Name: "x", Files: []dockerhost.File{{Path: "/a", Content: []byte("1")}}})
	if err == nil || !strings.Contains(err.Error(), "disk full") || !removed {
		t.Errorf("err = %v, removed = %v; want copy error and cleanup", err, removed)
	}
}

func TestCopyToRejectsRelativePath(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(http.ResponseWriter, *http.Request) { t.Error("no request expected") })
	if err := c.CopyTo(t.Context(), "id", []dockerhost.File{{Path: "rel"}}); err == nil {
		t.Error("CopyTo relative path = nil error")
	}
}

func TestStartAndRemove(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1.43/containers/a/start":
			w.WriteHeader(http.StatusNotModified)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.43/containers/b/start":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1.43/containers/a":
			if q := r.URL.Query(); q.Get("force") != "true" || q.Get("v") != "true" {
				t.Errorf("remove query = %q", r.URL.RawQuery)
			}
			jsonReply(w, 404, map[string]string{"message": "no"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	})
	for _, id := range []string{"a", "b"} {
		if err := c.StartContainer(t.Context(), id); err != nil {
			t.Errorf("StartContainer(%s) = %v", id, err)
		}
	}
	if err := c.RemoveContainer(t.Context(), "a"); err != nil {
		t.Errorf("RemoveContainer 404 = %v, want nil", err)
	}
}

func TestInspectAndList(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.43/containers/k3s/json":
			jsonReply(w, 200, map[string]any{
				"Id": "cid", "Name": "/k3s", "RestartCount": 2,
				"Config": map[string]any{"Image": "rancher/k3s", "Labels": map[string]string{"a": "b"}},
				"State":  map[string]any{"Running": true, "Status": "running", "ExitCode": 0, "OOMKilled": true},
				"NetworkSettings": map[string]any{
					"Ports":    map[string]any{"6443/tcp": []map[string]string{{"HostIp": "0.0.0.0", "HostPort": "32768"}}, "80/tcp": nil},
					"Networks": map[string]any{"net": map[string]string{"IPAddress": "172.18.0.2"}},
				},
			})
		case "/v1.43/containers/json":
			q := r.URL.Query()
			if q.Get("all") != "true" || q.Get("filters") != `{"label":["a=b"]}` {
				t.Errorf("list query = %q", r.URL.RawQuery)
			}
			jsonReply(w, 200, []map[string]any{{"Id": "cid", "Names": []string{"/k3s"}, "Image": "i", "Labels": map[string]string{"a": "b"}, "State": "exited"}})
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	})
	got, err := c.InspectContainer(t.Context(), "k3s")
	if err != nil {
		t.Fatalf("InspectContainer = %v", err)
	}
	if got.ID != "cid" || got.Name != "k3s" || got.Image != "rancher/k3s" || !got.Running || got.Status != "running" ||
		got.Ports["6443/tcp"][0].HostPort != "32768" || got.Networks["net"] != "172.18.0.2" || got.Labels["a"] != "b" ||
		got.RestartCount != 2 || !got.OOMKilled {
		t.Errorf("InspectContainer = %+v", got)
	}
	list, err := c.ListContainers(t.Context(), map[string]string{"a": "b"})
	if err != nil || len(list) != 1 || list[0].Name != "k3s" || list[0].Running || list[0].Status != "exited" {
		t.Errorf("ListContainers = %+v, %v", list, err)
	}
}

func TestReadFile(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") == "/missing" {
			jsonReply(w, 404, map[string]string{"message": "Could not find the file"})
			return
		}
		if r.URL.Query().Get("path") != "/etc/kubeconfig" || r.URL.Path != "/v1.43/containers/cid/archive" {
			t.Errorf("unexpected %s", r.URL)
		}
		tw := tar.NewWriter(w)
		_ = tw.WriteHeader(&tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755})
		_ = tw.WriteHeader(&tar.Header{Name: "d/kubeconfig", Typeflag: tar.TypeReg, Mode: 0o600, Size: 5})
		_, _ = tw.Write([]byte("hello"))
		_ = tw.Close()
	})
	got, err := c.ReadFile(t.Context(), "cid", "/etc/kubeconfig")
	if err != nil || string(got) != "hello" {
		t.Errorf("ReadFile = %q, %v", got, err)
	}
	if _, err := c.ReadFile(t.Context(), "cid", "/missing"); !errors.Is(err, dockerhost.ErrNotFound) {
		t.Errorf("ReadFile missing err = %v, want ErrNotFound", err)
	}
}

func TestMemoryUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		stats map[string]any
		want  int64
	}{
		{"with inactive_file", map[string]any{"usage": 1000, "stats": map[string]any{"inactive_file": 300}}, 700},
		{"no stats", map[string]any{"usage": 1000}, 1000},
	}
	for _, tt := range tests {
		c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if r.URL.Path != "/v1.43/containers/cid/stats" || q.Get("stream") != "false" || q.Get("one-shot") != "true" {
				t.Errorf("unexpected %s", r.URL)
			}
			jsonReply(w, 200, map[string]any{"memory_stats": tt.stats})
		})
		if got, err := c.MemoryUsage(t.Context(), "cid"); err != nil || got != tt.want {
			t.Errorf("%s: MemoryUsage = %d, %v; want %d", tt.name, got, err, tt.want)
		}
	}
}

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestLogs(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/v1.43/containers/cid/logs" || q.Get("stdout") != "1" || q.Get("stderr") != "1" || q.Get("tail") != "50" {
			t.Errorf("unexpected %s", r.URL)
		}
		_, _ = w.Write(frame(1, "out line\n"))
		_, _ = w.Write(frame(2, "err line\n"))
	})
	got, err := c.Logs(t.Context(), "cid", 50)
	if err != nil || got != "out line\nerr line\n" {
		t.Errorf("Logs = %q, %v", got, err)
	}

	c = newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tail") != "all" {
			t.Errorf("tail = %q, want all", r.URL.Query().Get("tail"))
		}
		_, _ = w.Write(frame(1, "truncated")[:12]) // header plus 4 of 9 payload bytes
	})
	if _, err := c.Logs(t.Context(), "cid", 0); err == nil {
		t.Error("Logs with truncated frame = nil error")
	}
}
