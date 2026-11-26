package dockerhost

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ExecSpec is a command to run inside a running container.
type ExecSpec struct {
	Cmd        []string
	User       string   // "" runs as the container's user
	Env        []string // "K=V"
	WorkingDir string
}

// ExecResult is a finished command. A non-zero ExitCode is not a Go error;
// the caller decides what it means.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Exec runs e in the running container id and waits for it to finish.
func (c *Client) Exec(ctx context.Context, id string, e ExecSpec) (ExecResult, error) {
	body, err := jsonBody(map[string]any{
		"AttachStdout": true, "AttachStderr": true, "Tty": false,
		"Cmd": e.Cmd, "User": e.User, "Env": e.Env, "WorkingDir": e.WorkingDir,
	})
	if err != nil {
		return ExecResult{}, err
	}
	var created struct {
		ID string `json:"Id"`
	}
	err = c.doJSON(ctx, request{
		method: http.MethodPost, path: "/containers/" + url.PathEscape(id) + "/exec",
		body: body, contentType: "application/json",
	}, &created)
	if err != nil {
		return ExecResult{}, err
	}
	startBody, err := jsonBody(map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return ExecResult{}, err
	}
	var res ExecResult
	err = c.do(ctx, request{
		method: http.MethodPost, path: "/exec/" + url.PathEscape(created.ID) + "/start",
		body: startBody, contentType: "application/json",
	}, func(r io.Reader) error {
		var stdout, stderr bytes.Buffer
		err := demux(io.LimitReader(r, maxBody), &stdout, &stderr)
		res.Stdout, res.Stderr = stdout.String(), stderr.String()
		return err
	})
	if err != nil {
		return res, err
	}
	var inspect struct {
		ExitCode int `json:"ExitCode"`
	}
	err = c.doJSON(ctx, request{method: http.MethodGet, path: "/exec/" + url.PathEscape(created.ID) + "/json"}, &inspect)
	res.ExitCode = inspect.ExitCode
	return res, err
}

// demux splits Docker's multiplexed (non-TTY) stream into stdout (stream 1)
// and stderr (stream 2).
func demux(r io.Reader, stdout, stderr io.Writer) error {
	var header [8]byte
	for {
		_, err := io.ReadFull(r, header[:])
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("docker: exec frame header: %w", err)
		}
		dst := io.Discard
		switch header[0] {
		case 1:
			dst = stdout
		case 2:
			dst = stderr
		}
		if _, err := io.CopyN(dst, r, int64(binary.BigEndian.Uint32(header[4:]))); err != nil {
			return fmt.Errorf("docker: exec frame payload: %w", err)
		}
	}
}
