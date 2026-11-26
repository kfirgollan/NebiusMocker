package dockerhost

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// EnsureVolume creates a labelled named volume; an existing one is fine.
func (c *Client) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	body, err := jsonBody(map[string]any{"Name": name, "Labels": labels})
	if err != nil {
		return err
	}
	return c.do(ctx, request{
		method: http.MethodPost, path: "/volumes/create", body: body, contentType: "application/json",
	}, nil)
}

// RemoveVolume force-removes a volume; a missing one is not an error.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	err := c.do(ctx, request{
		method: http.MethodDelete, path: "/volumes/" + url.PathEscape(name),
		query: url.Values{"force": {"true"}},
	}, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ListVolumes returns the names of volumes carrying all the given labels.
func (c *Client) ListVolumes(ctx context.Context, labels map[string]string) ([]string, error) {
	filters, err := labelFilters(labels)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Volumes []struct {
			Name string `json:"Name"`
		} `json:"Volumes"`
	}
	err = c.doJSON(ctx, request{method: http.MethodGet, path: "/volumes", query: url.Values{"filters": {filters}}}, &raw)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(raw.Volumes))
	for _, v := range raw.Volumes {
		names = append(names, v.Name)
	}
	return names, nil
}
