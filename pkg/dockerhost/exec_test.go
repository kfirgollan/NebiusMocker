package dockerhost_test

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
)

func TestExec(t *testing.T) {
	t.Parallel()
	var created map[string]any
	var started map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1.43/containers/pg/exec":
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Errorf("decode create: %v", err)
			}
			jsonReply(w, 201, map[string]string{"Id": "eid"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1.43/exec/eid/start":
			if err := json.NewDecoder(r.Body).Decode(&started); err != nil {
				t.Errorf("decode start: %v", err)
			}
			var buf bytes.Buffer
			buf.Write(frame(1, "out1 "))
			buf.Write(frame(2, "err1"))
			buf.Write(frame(1, "out2"))
			_, _ = w.Write(buf.Bytes())
		case r.Method == http.MethodGet && r.URL.Path == "/v1.43/exec/eid/json":
			jsonReply(w, 200, map[string]any{"ExitCode": 3, "Running": false})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	})
	got, err := c.Exec(t.Context(), "pg", dockerhost.ExecSpec{
		Cmd: []string{"psql", "-c", "select 1"}, User: "postgres", Env: []string{"A=b"}, WorkingDir: "/tmp",
	})
	if err != nil {
		t.Fatalf("Exec = %v", err)
	}
	want := dockerhost.ExecResult{ExitCode: 3, Stdout: "out1 out2", Stderr: "err1"}
	if got != want {
		t.Errorf("Exec = %+v, want %+v", got, want)
	}
	if created["AttachStdout"] != true || created["AttachStderr"] != true || created["Tty"] != false ||
		created["User"] != "postgres" || created["WorkingDir"] != "/tmp" {
		t.Errorf("exec create payload = %v", created)
	}
	if cmd, _ := created["Cmd"].([]any); len(cmd) != 3 || cmd[0] != "psql" {
		t.Errorf("Cmd = %v", created["Cmd"])
	}
	if started["Detach"] != false || started["Tty"] != false {
		t.Errorf("exec start payload = %v", started)
	}
}

func TestExecErrors(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, 409, map[string]string{"message": "container is not running"})
	})
	if _, err := c.Exec(t.Context(), "pg", dockerhost.ExecSpec{Cmd: []string{"true"}}); !errors.Is(err, dockerhost.ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}
}

func TestStopContainer(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Query().Get("t") != "15" {
			t.Errorf("query = %q, want t=15", r.URL.RawQuery)
		}
		switch r.URL.Path {
		case "/v1.43/containers/a/stop":
			w.WriteHeader(http.StatusNoContent)
		case "/v1.43/containers/b/stop":
			w.WriteHeader(http.StatusNotModified)
		default:
			jsonReply(w, 404, map[string]string{"message": "no such container"})
		}
	})
	for _, id := range []string{"a", "b"} {
		if err := c.StopContainer(t.Context(), id, 15*time.Second); err != nil {
			t.Errorf("StopContainer(%s) = %v", id, err)
		}
	}
	if err := c.StopContainer(t.Context(), "gone", 15*time.Second); !errors.Is(err, dockerhost.ErrNotFound) {
		t.Errorf("StopContainer(gone) = %v, want ErrNotFound", err)
	}
}

func TestImageExists(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.43/images/have:1/json" {
			jsonReply(w, 200, map[string]string{"Id": "x"})
			return
		}
		jsonReply(w, 404, map[string]string{"message": "no such image"})
	})
	for ref, want := range map[string]bool{"have:1": true, "missing:1": false} {
		got, err := c.ImageExists(t.Context(), ref)
		if err != nil || got != want {
			t.Errorf("ImageExists(%s) = %v, %v; want %v", ref, got, err, want)
		}
	}
	bad := newClient(t, func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, 500, map[string]string{"message": "boom"}) })
	if _, err := bad.ImageExists(t.Context(), "x"); err == nil {
		t.Error("ImageExists on a 500 = nil error")
	}
}

func TestBuildImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		stream  string
		wantErr string
	}{
		{name: "ok", stream: `{"stream":"Step 1/2"}` + "\n" + `{"aux":{"ID":"sha256:x"}}` + "\n"},
		{name: "error line", stream: `{"stream":"x"}` + "\n" + `{"error":"returned a non-zero code: 1","errorDetail":{"message":"returned a non-zero code: 1"}}` + "\n", wantErr: "non-zero code"},
		{name: "detail only", stream: `{"errorDetail":{"message":"bad base"}}` + "\n", wantErr: "bad base"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var query map[string][]string
			var files map[string]string
			c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1.43/build" {
					t.Errorf("unexpected %s %s", r.Method, r.URL)
				}
				if ct := r.Header.Get("Content-Type"); ct != "application/x-tar" {
					t.Errorf("Content-Type = %q", ct)
				}
				query = r.URL.Query()
				files = map[string]string{}
				tr := tar.NewReader(r.Body)
				for {
					h, err := tr.Next()
					if err != nil {
						break
					}
					b, _ := io.ReadAll(tr)
					files[h.Name] = string(b)
				}
				_, _ = io.WriteString(w, tt.stream)
			})
			err := c.BuildImage(t.Context(), "img:1", "FROM scratch\n", map[string]string{"k": "v"})
			if tt.wantErr == "" && err != nil {
				t.Fatalf("BuildImage = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("BuildImage err = %v, want %q", err, tt.wantErr)
			}
			if query["t"][0] != "img:1" || query["rm"][0] != "1" || query["labels"][0] != `{"k":"v"}` {
				t.Errorf("query = %v", query)
			}
			if len(files) != 1 || files["Dockerfile"] != "FROM scratch\n" {
				t.Errorf("context files = %v, want only Dockerfile", files)
			}
		})
	}
}

func TestCreateContainerNewFields(t *testing.T) {
	t.Parallel()
	var payload map[string]any
	var tarOwner [2]int
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/create"):
			_ = json.NewDecoder(r.Body).Decode(&payload)
			jsonReply(w, 201, map[string]string{"Id": "cid"})
		case r.Method == http.MethodPut:
			tr := tar.NewReader(r.Body)
			for {
				h, err := tr.Next()
				if err != nil {
					break
				}
				if h.Typeflag == tar.TypeReg {
					tarOwner = [2]int{h.Uid, h.Gid}
				}
			}
		}
	})
	_, err := c.CreateContainer(t.Context(), dockerhost.ContainerSpec{
		Name: "x", Entrypoint: []string{"sh", "-c"}, User: "70", NanoCPUs: 2e9,
		Files: []dockerhost.File{{Path: "/k", Content: []byte("1"), UID: 70, GID: 71}},
	})
	if err != nil {
		t.Fatal(err)
	}
	host, _ := payload["HostConfig"].(map[string]any)
	if payload["User"] != "70" || host["NanoCpus"] != float64(2e9) {
		t.Errorf("payload = %v", payload)
	}
	if ep, _ := payload["Entrypoint"].([]any); len(ep) != 2 || ep[0] != "sh" {
		t.Errorf("Entrypoint = %v", payload["Entrypoint"])
	}
	if tarOwner != [2]int{70, 71} {
		t.Errorf("tar owner = %v, want 70:71", tarOwner)
	}
}
