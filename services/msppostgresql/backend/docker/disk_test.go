package docker

import (
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
)

// diskHandler answers du with kib and the database listing with listing;
// everything else succeeds.
func diskHandler(kib, listing string, ran *[]string) func(string, dockerhost.ExecSpec) dockerhost.ExecResult {
	return func(_ string, e dockerhost.ExecSpec) dockerhost.ExecResult {
		last := e.Cmd[len(e.Cmd)-1]
		switch {
		case strings.Contains(strings.Join(e.Cmd, " "), "du -sk"):
			return dockerhost.ExecResult{Stdout: kib + "\n"}
		case strings.HasPrefix(last, "select d.datname"):
			return dockerhost.ExecResult{Stdout: listing}
		case strings.Contains(last, "ALTER DATABASE"):
			*ran = append(*ran, last)
		case last == "select pg_is_in_recovery()":
			return dockerhost.ExecResult{Stdout: "f\n"}
		}
		return dockerhost.ExecResult{}
	}
}

func TestCheckDiskEnforcement(t *testing.T) {
	t.Parallel()
	const limit = 100 << 20
	tests := []struct {
		name     string
		kib      string
		listing  string
		wantFull bool
		wantSQL  []string // substrings of the one enforcing statement, none when empty
	}{
		{name: "below the limit, unmarked", kib: "1024", listing: "appdb|false\npostgres|false\n"},
		{
			name: "above the limit marks every database and ends sessions", kib: "204800", listing: "appdb|false\npostgres|false\n", wantFull: true,
			wantSQL: []string{`ALTER DATABASE "appdb" SET default_transaction_read_only = on;`, `ALTER DATABASE "postgres" SET default_transaction_read_only = on;`, "pg_terminate_backend"},
		},
		{name: "at the limit is full", kib: "102400", listing: "appdb|true\n", wantFull: true},
		{name: "already marked is left alone", kib: "204800", listing: "appdb|true\npostgres|true\n", wantFull: true},
		{
			name: "a new database is marked", kib: "204800", listing: "appdb|true\nnewdb|false\n", wantFull: true,
			wantSQL: []string{`ALTER DATABASE "newdb" SET default_transaction_read_only = on;`, "pg_terminate_backend"},
		},
		{
			name: "below the limit resets the marks", kib: "1024", listing: "appdb|true\npostgres|true\n",
			wantSQL: []string{`ALTER DATABASE "appdb" RESET default_transaction_read_only;`, `ALTER DATABASE "postgres" RESET default_transaction_read_only;`, "pg_terminate_backend"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.ensure(testCluster("pg1"))
			var ran []string
			h.f.SetExecHandler(diskHandler(tt.kib, tt.listing, &ran))
			usage, err := h.b.CheckDisk(t.Context(), "pg1", limit)
			if err != nil {
				t.Fatalf("CheckDisk = %v", err)
			}
			if usage.Full != tt.wantFull || usage.LimitBytes != limit {
				t.Errorf("usage = %+v, want full %v", usage, tt.wantFull)
			}
			if want := int64(0); usage.UsedBytes == want {
				t.Errorf("used bytes not measured: %+v", usage)
			}
			if len(tt.wantSQL) == 0 {
				if len(ran) != 0 {
					t.Errorf("unexpected enforcement: %v", ran)
				}
				return
			}
			if len(ran) != 1 {
				t.Fatalf("enforcing statements = %v, want one", ran)
			}
			for _, want := range tt.wantSQL {
				if !strings.Contains(ran[0], want) {
					t.Errorf("statement lacks %q:\n%s", want, ran[0])
				}
			}
			if !strings.Contains(ran[0], "nmock_pooler") || !strings.Contains(ran[0], "nmock_replicator") {
				t.Errorf("sessions of the pooler and replication logins must be spared:\n%s", ran[0])
			}
		})
	}
}

func TestCheckDiskMeasuresThePrimaryDataDirectory(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ensure(testCluster("pg1"))
	var ran []string
	h.f.SetExecHandler(diskHandler("2048", "appdb|false\n", &ran))
	usage, err := h.b.CheckDisk(t.Context(), "pg1", 1<<30)
	if err != nil || usage.UsedBytes != 2048<<10 || usage.Full {
		t.Fatalf("CheckDisk = %+v, %v", usage, err)
	}
	var du bool
	for _, c := range h.f.Execs() {
		if strings.Contains(strings.Join(c.Spec.Cmd, " "), `du -sk "$PGDATA"`) {
			du = true
			if c.Container != n("pg1-0") {
				t.Errorf("du ran in %s, want the primary", c.Container)
			}
		}
	}
	if !du {
		t.Error("the data directory was not measured")
	}
}

func TestCheckDiskOfAnUnknownOrStoppedCluster(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if u, err := h.b.CheckDisk(t.Context(), "nope", 100); err != nil || u.Full || u.UsedBytes != 0 {
		t.Errorf("unknown cluster: %+v, %v", u, err)
	}
	h.ensure(testCluster("pg1"))
	if err := h.b.StopCluster(t.Context(), "pg1"); err != nil {
		t.Fatal(err)
	}
	if u, err := h.b.CheckDisk(t.Context(), "pg1", 100); err != nil || u.Full {
		t.Errorf("stopped cluster: %+v, %v", u, err)
	}
}

func TestPSQLSwitchesReadOnlyOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ensure(testCluster("pg1"))
	var found bool
	for _, c := range h.f.Execs() {
		j := strings.Join(c.Spec.Cmd, "\x00")
		if strings.HasPrefix(j, "psql") && strings.Contains(j, "-c\x00SET default_transaction_read_only = off\x00-c\x00") {
			found = true
		}
	}
	if !found {
		t.Error("the backend's psql calls must not be refused by a full disk's read-only default")
	}
}
