package crashwatch

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
)

func ev(id, action string, at int64, attrs ...string) dockerhost.Event {
	a := map[string]string{"name": "n-" + id}
	for i := 0; i+1 < len(attrs); i += 2 {
		a[attrs[i]] = attrs[i+1]
	}
	return dockerhost.Event{ID: id, Action: action, TimeNano: at, Attributes: a}
}

func TestCrashes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		events []dockerhost.Event
		want   []crash
	}{
		{name: "docker stop", events: []dockerhost.Event{
			ev("a", "start", 1), ev("a", "kill", 2, "signal", "15"), ev("a", "die", 3, "exitCode", "143"), ev("a", "stop", 4),
		}},
		{name: "docker rm -f", events: []dockerhost.Event{
			ev("a", "start", 1), ev("a", "kill", 2, "signal", "9"), ev("a", "die", 3, "exitCode", "137"), ev("a", "destroy", 4),
		}},
		{name: "one-shot job exits cleanly", events: []dockerhost.Event{
			ev("a", "start", 1), ev("a", "exec_die", 2, "exitCode", "1"), ev("a", "die", 3, "exitCode", "0"),
		}},
		{name: "crash restarted by the policy", events: []dockerhost.Event{
			ev("a", "start", 1), ev("a", "die", 3, "exitCode", "1"), ev("a", "start", 4),
		}, want: []crash{{id: "a", name: "n-a", at: 3, why: "exited on its own with code 1"}}},
		{name: "a stop before a restart does not excuse a later crash", events: []dockerhost.Event{
			ev("a", "start", 1), ev("a", "kill", 2), ev("a", "die", 3, "exitCode", "143"),
			ev("a", "start", 5), ev("a", "die", 6, "exitCode", "3"),
		}, want: []crash{{id: "a", name: "n-a", at: 6, why: "exited on its own with code 3"}}},
		{name: "OOM kill", events: []dockerhost.Event{
			ev("a", "start", 1), ev("a", "oom", 2), ev("a", "die", 3, "exitCode", "137"),
		}, want: []crash{{id: "a", name: "n-a", at: 3, why: "was OOM-killed (exit code 137)"}}},
		{name: "out of order delivery is sorted", events: []dockerhost.Event{
			ev("a", "die", 3, "exitCode", "137"), ev("a", "start", 1), ev("a", "kill", 2),
			ev("b", "start", 1), ev("b", "die", 2, "exitCode", "2"),
		}, want: []crash{{id: "b", name: "n-b", at: 2, why: "exited on its own with code 2"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tt.want, crashes(tt.events), cmp.AllowUnexported(crash{})); diff != "" {
				t.Errorf("crashes (-want +got):\n%s", diff)
			}
		})
	}
}
