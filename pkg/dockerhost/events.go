package dockerhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Event is one container event of the daemon.
type Event struct {
	// Action is start, kill, die, oom, stop, destroy, exec_create: ..., and so on.
	Action string
	ID     string // the container
	// Attributes are the container's labels plus name and image, and, by
	// action, exitCode (die) or signal (kill).
	Attributes map[string]string
	TimeNano   int64 // daemon time
}

// ContainerEvents hands fn the events of containers carrying every given
// label (an empty value matches on the key alone), oldest first. since and
// until bound them in daemon time; a zero since starts now, and a zero until
// streams until ctx ends, which is then not an error. The daemon replays
// past events from a small buffer, so a past window must be short.
func (c *Client) ContainerEvents(ctx context.Context, labels map[string]string, since, until time.Time, fn func(Event)) error {
	f := map[string][]string{"type": {"container"}}
	for k, v := range labels {
		if v != "" {
			k += "=" + v
		}
		f["label"] = append(f["label"], k)
	}
	filters, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("docker: encode filters: %w", err)
	}
	q := url.Values{"filters": {string(filters)}}
	if !since.IsZero() {
		q.Set("since", unixArg(since))
	}
	if !until.IsZero() {
		q.Set("until", unixArg(until))
	}
	err = c.do(ctx, request{method: http.MethodGet, path: "/events", query: q}, func(body io.Reader) error {
		dec := json.NewDecoder(body)
		for {
			var raw struct {
				Action string `json:"Action"`
				Actor  struct {
					ID         string            `json:"ID"`
					Attributes map[string]string `json:"Attributes"`
				} `json:"Actor"`
				TimeNano int64 `json:"timeNano"`
			}
			if err := dec.Decode(&raw); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return fmt.Errorf("docker: GET /events: decode: %w", err)
			}
			fn(Event{Action: raw.Action, ID: raw.Actor.ID, Attributes: raw.Actor.Attributes, TimeNano: raw.TimeNano})
		}
	})
	if until.IsZero() && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil // a stream ends with ctx
	}
	return err
}

// unixArg is t as the API's "seconds.nanoseconds" timestamp.
func unixArg(t time.Time) string {
	return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond())
}
