package dockerhost

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// File is a file placed into a container.
type File struct {
	Path    string // absolute, in the container
	Mode    int64
	Content []byte
}

// CopyTo writes files into the container as one tar extracted at "/".
func (c *Client) CopyTo(ctx context.Context, id string, files []File) error {
	if len(files) == 0 {
		return nil
	}
	archive, err := buildTar(files)
	if err != nil {
		return err
	}
	return c.do(ctx, request{
		method:      http.MethodPut,
		path:        "/containers/" + url.PathEscape(id) + "/archive",
		query:       url.Values{"path": {"/"}},
		body:        bytes.NewReader(archive),
		contentType: "application/x-tar",
	}, nil)
}

// writeParentDirs emits directory entries for every parent of name not yet in seen.
func writeParentDirs(tw *tar.Writer, name string, seen map[string]bool) error {
	var dirs []string
	for d := path.Dir(name); d != "." && d != "/"; d = path.Dir(d) {
		dirs = append(dirs, d)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if seen[dirs[i]] {
			continue
		}
		seen[dirs[i]] = true
		hdr := &tar.Header{Typeflag: tar.TypeDir, Name: dirs[i] + "/", Mode: 0o755}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("docker: tar dir %s: %w", dirs[i], err)
		}
	}
	return nil
}

func buildTar(files []File) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	seen := map[string]bool{}
	for _, f := range files {
		if !strings.HasPrefix(f.Path, "/") {
			return nil, fmt.Errorf("docker: file path %q must be absolute", f.Path)
		}
		name := strings.TrimPrefix(path.Clean(f.Path), "/")
		if err := writeParentDirs(tw, name, seen); err != nil {
			return nil, err
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(f.Content))}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("docker: tar header %s: %w", name, err)
		}
		if _, err := tw.Write(f.Content); err != nil {
			return nil, fmt.Errorf("docker: tar write %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("docker: tar close: %w", err)
	}
	return buf.Bytes(), nil
}

// ReadFile returns the first regular file of the archive Docker returns for filePath.
func (c *Client) ReadFile(ctx context.Context, id, filePath string) ([]byte, error) {
	var out []byte
	err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/containers/" + url.PathEscape(id) + "/archive",
		query:  url.Values{"path": {filePath}},
	}, func(body io.Reader) error {
		tr := tar.NewReader(io.LimitReader(body, maxBody))
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("docker: %s in container %s: no regular file in archive: %w", filePath, id, ErrNotFound)
			}
			if err != nil {
				return fmt.Errorf("docker: read archive of %s: %w", filePath, err)
			}
			if hdr.Typeflag != tar.TypeReg {
				continue
			}
			out, err = io.ReadAll(tr)
			if err != nil {
				return fmt.Errorf("docker: read %s: %w", filePath, err)
			}
			return nil
		}
	})
	return out, err
}
