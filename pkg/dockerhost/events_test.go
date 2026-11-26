package dockerhost_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
)

func TestContainerEventsWindow(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/v1.43/events" || q.Get("since") != "1700000000.000000005" || q.Get("until") != "1700000060.500000000" ||
			q.Get("filters") != `{"label":["owner=me"],"type":["container"]}` {
			t.Errorf("request = %s", r.URL)
		}
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]any{"Type": "container", "Action": "kill", "Actor": map[string]any{"ID": "c1", "Attributes": map[string]string{"signal": "15"}}, "timeNano": 1})
		_ = enc.Encode(map[string]any{"Type": "container", "Action": "die", "Actor": map[string]any{"ID": "c1", "Attributes": map[string]string{"exitCode": "137"}}, "timeNano": 2})
	})
	var got []dockerhost.Event
	err := c.ContainerEvents(t.Context(), map[string]string{"owner": "me"},
		time.Unix(1700000000, 5), time.Unix(1700000060, 500000000), func(e dockerhost.Event) { got = append(got, e) })
	if err != nil {
		t.Fatalf("ContainerEvents = %v", err)
	}
	if len(got) != 2 || got[0].Action != "kill" || got[0].Attributes["signal"] != "15" ||
		got[1].Action != "die" || got[1].ID != "c1" || got[1].Attributes["exitCode"] != "137" || got[1].TimeNano != 2 {
		t.Errorf("events = %+v", got)
	}
}

func TestContainerEventsStreamEndsWithContext(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("until") || r.URL.Query().Has("since") {
			t.Errorf("request = %s", r.URL)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Action": "start", "Actor": map[string]any{"ID": "c1"}})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(t.Context())
	err := c.ContainerEvents(ctx, nil, time.Time{}, time.Time{}, func(e dockerhost.Event) {
		if e.Action == "start" {
			cancel()
		}
	})
	if err != nil {
		t.Fatalf("ContainerEvents after cancel = %v, want nil", err)
	}
}
