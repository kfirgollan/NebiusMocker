// Package harbor is the registry backend over a Harbor server
// (https://goharbor.io), reached through its REST API v2.0 with admin
// credentials. Each Nebius registry is one Harbor project; image data never
// passes through this package (docker talks to Harbor through the mocker's
// docker front, see services/registry/docker).
package harbor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/registry/domain"
)

// pageSize is Harbor's maximum page size for list APIs.
const pageSize = 100

// Config locates Harbor.
type Config struct {
	// URL is Harbor's base URL, e.g. "http://127.0.0.1:8080".
	URL      string
	Username string
	Password string
	// HTTPClient is optional; it defaults to a client with a 30s timeout.
	HTTPClient *http.Client
}

// Client implements domain.Backend over Harbor's API.
type Client struct {
	base *url.URL
	user string
	pass string
	http *http.Client
}

var _ domain.Backend = (*Client)(nil)

// New validates cfg and returns a Client. It does not contact Harbor.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.URL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("harbor: invalid URL %q", cfg.URL)
	}
	if cfg.Username == "" {
		return nil, errors.New("harbor: username is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: u, user: cfg.Username, pass: cfg.Password, http: hc}, nil
}

// URL is Harbor's base URL.
func (c *Client) URL() *url.URL { u := *c.base; return &u }

// Username and Password are the admin credentials the docker front forwards with.
func (c *Client) Username() string { return c.user }

// Password returns the admin password.
func (c *Client) Password() string { return c.pass }

// EnsureNamespace creates a private Harbor project; an existing one is fine.
func (c *Client) EnsureNamespace(ctx context.Context, namespace string) error {
	body := map[string]any{"project_name": namespace, "metadata": map[string]string{"public": "false"}}
	err := c.do(ctx, http.MethodPost, "/api/v2.0/projects", nil, body, nil)
	if errs.IsKind(err, errs.KindAlreadyExists) {
		return nil
	}
	return err
}

// DeleteNamespace deletes every repository of the project, then the project.
func (c *Client) DeleteNamespace(ctx context.Context, namespace string) error {
	repos, err := c.repositories(ctx, namespace)
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, r := range repos {
		if err := c.do(ctx, http.MethodDelete, repoPath(namespace, r), nil, nil, nil); err != nil && !errs.IsKind(err, errs.KindNotFound) {
			return fmt.Errorf("delete repository %s/%s: %w", namespace, r, err)
		}
	}
	err = c.do(ctx, http.MethodDelete, "/api/v2.0/projects/"+url.PathEscape(namespace), nil, nil, nil)
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	return err
}

type artifactJSON struct {
	Digest            string    `json:"digest"`
	MediaType         string    `json:"media_type"`
	ManifestMediaType string    `json:"manifest_media_type"`
	Size              int64     `json:"size"`
	PushTime          time.Time `json:"push_time"`
	Tags              []struct {
		Name string `json:"name"`
	} `json:"tags"`
	References []json.RawMessage `json:"references"`
}

// ListImages lists every artifact of every repository in the project.
func (c *Client) ListImages(ctx context.Context, namespace string) ([]domain.Image, error) {
	repos, err := c.repositories(ctx, namespace)
	if err != nil {
		return nil, err
	}
	var out []domain.Image
	for _, r := range repos {
		arts, err := listAll[artifactJSON](ctx, c, repoPath(namespace, r)+"/artifacts", url.Values{"with_tag": {"true"}})
		if err != nil {
			return nil, fmt.Errorf("artifacts of %s/%s: %w", namespace, r, err)
		}
		for i := range arts {
			out = append(out, toImage(r, &arts[i]))
		}
	}
	return out, nil
}

func toImage(repository string, a *artifactJSON) domain.Image {
	img := domain.Image{
		Repository: repository,
		Digest:     a.Digest,
		MediaType:  a.ManifestMediaType,
		Size:       a.Size,
		PushedAt:   a.PushTime.UTC(),
		List:       len(a.References) > 0 || isIndex(a.ManifestMediaType),
	}
	if img.MediaType == "" {
		img.MediaType = a.MediaType
	}
	for _, t := range a.Tags {
		img.Tags = append(img.Tags, t.Name)
	}
	return img
}

func isIndex(mediaType string) bool {
	return mediaType == "application/vnd.oci.image.index.v1+json" ||
		mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
}

// DeleteImage deletes one artifact (manifest and tags) by digest.
func (c *Client) DeleteImage(ctx context.Context, namespace, repository, digest string) error {
	return c.do(ctx, http.MethodDelete, repoPath(namespace, repository)+"/artifacts/"+url.PathEscape(digest), nil, nil, nil)
}

// repositories returns the project's repository names without the project prefix.
func (c *Client) repositories(ctx context.Context, namespace string) ([]string, error) {
	// Harbor answers an empty list for a missing project here, so check first.
	if err := c.do(ctx, http.MethodHead, "/api/v2.0/projects", url.Values{"project_name": {namespace}}, nil, nil); err != nil {
		return nil, err
	}
	type repoJSON struct {
		Name string `json:"name"`
	}
	repos, err := listAll[repoJSON](ctx, c, "/api/v2.0/projects/"+url.PathEscape(namespace)+"/repositories", nil)
	if err != nil {
		return nil, fmt.Errorf("repositories of %s: %w", namespace, err)
	}
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, strings.TrimPrefix(r.Name, namespace+"/"))
	}
	return out, nil
}

// repoPath is the API path of a repository. Harbor requires slashes inside
// repository names to be double-encoded ("a/b" → "a%252Fb").
func repoPath(namespace, repository string) string {
	return "/api/v2.0/projects/" + url.PathEscape(namespace) + "/repositories/" +
		url.PathEscape(url.PathEscape(repository))
}

// listAll pages through a Harbor list API.
func listAll[T any](ctx context.Context, c *Client, path string, query url.Values) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("page", strconv.Itoa(page))
		q.Set("page_size", strconv.Itoa(pageSize))
		var items []T
		if err := c.do(ctx, http.MethodGet, path, q, nil, &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < pageSize {
			return all, nil
		}
	}
}

// do sends one API request and decodes a JSON response into out (when set).
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) (err error) {
	// path is already escaped (Harbor needs double-encoded repository names),
	// so concatenate instead of letting net/url re-escape it.
	target := c.base.String() + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var rd io.Reader = http.NoBody
	if body != nil {
		b, merr := json.Marshal(body)
		if merr != nil {
			return fmt.Errorf("harbor: encode request: %w", merr)
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return fmt.Errorf("harbor: build request: %w", err)
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Is-Resource-Name", "true")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return errs.Internal("harbor unreachable: " + err.Error())
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512)) // best-effort: error detail only
		return statusError(resp.StatusCode, method+" "+path, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("harbor: decode %s %s: %w", method, path, err)
	}
	return nil
}

func statusError(code int, op, detail string) error {
	switch code {
	case http.StatusNotFound:
		return errs.NotFound(op)
	case http.StatusConflict:
		return errs.AlreadyExists("", "harbor: "+op+": "+detail)
	case http.StatusPreconditionFailed:
		return errs.BadState("", "harbor: "+op+": "+detail)
	default:
		return errs.Internal(fmt.Sprintf("harbor: %s: HTTP %d: %s", op, code, detail))
	}
}
