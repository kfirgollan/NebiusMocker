//go:build e2e_storage

package storage

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

// trxDig digs a path out of decoded CLI JSON and renders the leaf as text
// (the CLI prints int64 as a JSON string, enums by name).
func trxDig(m map[string]any, path ...string) string {
	var cur any = m
	for _, k := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[k]
	}
	if cur == nil {
		return ""
	}
	return fmt.Sprint(cur)
}

func trxWriteJSON(t *testing.T, dir, name string, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	writeFile(t, path, string(b))
	return path
}

// trxCLIWait polls "nebius storage transfer get <id>" until cond holds,
// calling tick (which may advance the mocker clock) before each check.
func trxCLIWait(t *testing.T, cli *nebiuscli.CLI, what, id string, tick func(), cond func(st map[string]any) bool) map[string]any {
	t.Helper()
	var got map[string]any
	poll(t, what, tick, func() bool {
		got = nbJSON(t, cli, "storage", "transfer", "get", id)
		st, _ := got["status"].(map[string]any)
		return cond(st)
	})
	return got
}

// TestTransferCLIFlow runs the documented transfer flow
// (docs.nebius.com/object-storage/transfer/quickstart) with the nebius CLI and
// the AWS CLI against the mocker, then the update/stop/resume/delete commands.
func TestTransferCLIFlow(t *testing.T) {
	e := trxStart(t)
	cli := newNebiusCLI(t, e.env)
	aws := e.awsCLI(t, e.keyID, e.secret)
	dir := t.TempDir()

	srcName, dstName := unique("trx-cli-src"), unique("trx-cli-dst")
	nb(t, cli, "storage", "bucket", "create", "--name", srcName)
	nb(t, cli, "storage", "bucket", "create", "--name", dstName)
	obj := filepath.Join(dir, "o.txt")
	writeFile(t, obj, "transfer cli object\n")
	for _, k := range []string{"logs/one.txt", "logs/two.txt", "other/three.txt"} {
		aw(t, aws, "s3", "cp", obj, "s3://"+srcName+"/"+k)
	}

	nebiusSide := func(bucket, keyID, secret string) map[string]any {
		return map[string]any{"region": "eu-north1", "bucket_name": bucket, "access_key": map[string]any{"access_key_id": keyID, "secret_access_key": secret}}
	}
	transferJSON := func(name, srcSecret string, extra map[string]any) map[string]any {
		spec := map[string]any{
			"source":                        map[string]any{"nebius": nebiusSide(srcName, e.keyID, srcSecret), "prefix": "logs/"},
			"destination":                   map[string]any{"nebius": nebiusSide(dstName, e.keyID, e.secret), "prefix": "copied/"},
			"overwrite_strategy":            "IF_NEWER",
			"enable_deletes_in_destination": false,
			"touch_unmanaged":               false,
			"limiters":                      map[string]any{"requests_per_second": 1000, "bandwidth_bytes_per_second": 1073741824},
		}
		for k, v := range extra {
			spec[k] = v
		}
		return map[string]any{"metadata": map[string]any{"name": name, "parent_id": e.inst.ProjectID}, "spec": spec}
	}

	// --- the documented quickstart: one iteration from a transfer.json ---
	file := trxWriteJSON(t, dir, "transfer.json", transferJSON("cli-once", e.secret, map[string]any{"after_one_iteration": map[string]any{}}))
	nb(t, cli, "storage", "transfer", "create", "--parent-id", e.inst.ProjectID, "--file", file)

	// The docs list without --parent-id: the profile's parent-id is used.
	listed := nbJSON(t, cli, "storage", "transfer", "list")
	items, _ := listed["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("transfer list = %v, want one item", listed)
	}
	onceID := trxDig(items[0].(map[string]any), "metadata", "id")
	if !strings.HasPrefix(onceID, "storagetransfer-") {
		t.Fatalf("transfer id = %q", onceID)
	}
	got := trxCLIWait(t, cli, "the one-time transfer to stop", onceID, nil, func(st map[string]any) bool {
		return trxDig(st, "state") == "STOPPED" && trxDig(st, "last_iteration", "state") == "COMPLETED"
	})
	if trxDig(got, "status", "last_iteration", "objects_transferred_count") != "2" {
		t.Errorf("last_iteration = %v, want 2 objects transferred", got["status"])
	}
	ls := aw(t, aws, "s3", "ls", "s3://"+dstName+"/copied/", "--recursive")
	mustContain(t, "aws s3 ls destination", ls, "copied/one.txt", "copied/two.txt")
	if strings.Contains(ls, "three.txt") {
		t.Errorf("aws s3 ls destination = %s, want only logs/ objects", ls)
	}
	if b := aw(t, aws, "s3", "cp", "s3://"+dstName+"/copied/two.txt", "-"); strings.TrimSpace(b) != "transfer cli object" {
		t.Errorf("aws s3 cp of copied/two.txt = %q", b)
	}

	// get: no secret in any output form.
	for _, args := range [][]string{
		{"storage", "transfer", "get", onceID},
		{"storage", "transfer", "get", onceID, "--format", "json"},
		{"storage", "transfer", "get-by-name", "--parent-id", e.inst.ProjectID, "--name", "cli-once"},
		{"storage", "transfer", "list"},
	} {
		text := nb(t, cli, args...)
		if strings.Contains(text, e.secret) {
			t.Errorf("nebius %s printed the secret access key", strings.Join(args, " "))
		}
		mustContain(t, strings.Join(args, " "), text, e.keyID)
	}
	hist := nbJSON(t, cli, "storage", "transfer", "get-iteration-history", "--transfer-id", onceID)
	its, _ := hist["iterations"].([]any)
	if len(its) != 1 || trxDig(its[0].(map[string]any), "state") != "COMPLETED" || trxDig(its[0].(map[string]any), "objects_transferred_count") != "2" {
		t.Errorf("get-iteration-history = %v, want one COMPLETED iteration with 2 objects", hist)
	}

	// --- a replicating transfer that starts with a wrong source secret ---
	file = trxWriteJSON(t, dir, "transfer-bad.json", transferJSON("cli-repl", "wrong-secret", map[string]any{
		"infinite": map[string]any{}, "inter_iteration_interval": "60s",
	}))
	nb(t, cli, "storage", "transfer", "create", "--parent-id", e.inst.ProjectID, "--file", file)
	replID := trxDig(nbJSON(t, cli, "storage", "transfer", "get-by-name", "--parent-id", e.inst.ProjectID, "--name", "cli-repl"), "metadata", "id")
	got = trxCLIWait(t, cli, "the transfer with a wrong secret to fail", replID, nil, func(st map[string]any) bool {
		return trxDig(st, "state") == "FAILED"
	})
	if trxDig(got, "status", "error", "origin") != "SOURCE" || trxDig(got, "status", "error", "code") == "" {
		t.Errorf("failed transfer status = %v, want error origin SOURCE with a code", got["status"])
	}

	// The documented credentials update, then resume.
	nb(t, cli, "storage", "transfer", "update", replID,
		"--source-nebius-access-key-access-key-id", e.keyID,
		"--source-nebius-access-key-secret-access-key", e.secret)
	nb(t, cli, "storage", "transfer", "resume", replID)
	trxCLIWait(t, cli, "the resumed transfer to complete iteration 2", replID, nil, func(st map[string]any) bool {
		return trxDig(st, "state") == "ACTIVE" && trxDig(st, "last_iteration", "sequence_number") == "2" && trxDig(st, "last_iteration", "state") == "COMPLETED"
	})
	mustContain(t, "after resume", aw(t, aws, "s3", "ls", "s3://"+dstName+"/copied/", "--recursive"), "copied/one.txt", "copied/two.txt")

	// A new source object is replicated by the next iteration (the mocker clock moves 1 minute).
	aw(t, aws, "s3", "cp", obj, "s3://"+srcName+"/logs/three.txt")
	polls := 0
	trxCLIWait(t, cli, "iteration 3", replID, func() {
		polls++
		if polls%10 == 1 {
			e.clk.Advance(time.Minute)
		}
	}, func(st map[string]any) bool {
		return trxDig(st, "last_iteration", "sequence_number") == "3" && trxDig(st, "last_iteration", "state") == "COMPLETED"
	})
	mustContain(t, "after iteration 3", aw(t, aws, "s3", "ls", "s3://"+dstName+"/copied/", "--recursive"), "copied/three.txt")

	// stop, resume, delete.
	nb(t, cli, "storage", "transfer", "stop", replID)
	trxCLIWait(t, cli, "the transfer to stop", replID, nil, func(st map[string]any) bool { return trxDig(st, "state") == "STOPPED" })
	nb(t, cli, "storage", "transfer", "resume", replID)
	trxCLIWait(t, cli, "the transfer to be active again", replID, nil, func(st map[string]any) bool {
		return trxDig(st, "state") == "ACTIVE" && trxDig(st, "last_iteration", "sequence_number") == "4" && trxDig(st, "last_iteration", "state") == "COMPLETED"
	})
	hist = nbJSON(t, cli, "storage", "transfer", "get-iteration-history", "--transfer-id", replID)
	if its, _ = hist["iterations"].([]any); len(its) != 4 || trxDig(its[0].(map[string]any), "sequence_number") != "4" {
		t.Errorf("get-iteration-history = %v, want 4 iterations, newest (4) first", hist)
	}
	nb(t, cli, "storage", "transfer", "delete", replID)
	if out := nbFails(t, cli, "storage", "transfer", "get", replID); !strings.Contains(strings.ToLower(out), "not") {
		t.Errorf("get after delete: %s, want a not-found error", out)
	}
	nb(t, cli, "storage", "transfer", "delete", onceID)
	if items, _ := nbJSON(t, cli, "storage", "transfer", "list")["items"].([]any); len(items) != 0 {
		t.Errorf("transfer list after deleting both = %v, want none", items)
	}

	// --- the deprecated v1alpha1 command group ---
	ext := rustfsfixture.StartExternal(t)
	extS3 := trxExtClient(ext)
	extSrc := trxExtBucket(t, extS3, "cli-alpha")
	trxText(t, extS3, extSrc, "in/x.txt", "from external")
	alphaCreds := func(id, secret string) map[string]any {
		return map[string]any{"access_key": map[string]any{"access_key_id": id, "secret_access_key": secret}}
	}
	file = trxWriteJSON(t, dir, "transfer-alpha.json", map[string]any{
		"metadata": map[string]any{"name": "cli-alpha", "parent_id": e.inst.ProjectID},
		"spec": map[string]any{
			"source": map[string]any{
				"endpoint": ext.Endpoint, "bucket_name": extSrc, "region": "us-east-1", "prefix": "in/",
				"credentials": alphaCreds(ext.AccessKey, ext.SecretKey),
			},
			"destination":         map[string]any{"bucket_name": dstName, "prefix": "alpha/", "credentials": alphaCreds(e.keyID, e.secret)},
			"after_one_iteration": map[string]any{},
			"overwrite_strategy":  "IF_NEWER",
		},
	})
	nb(t, cli, "storage", "v1alpha1", "transfer", "create", "--parent-id", e.inst.ProjectID, "--file", file)
	alphaList := nbJSON(t, cli, "storage", "v1alpha1", "transfer", "list")
	items, _ = alphaList["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("v1alpha1 transfer list = %v, want one item", alphaList)
	}
	alphaID := trxDig(items[0].(map[string]any), "metadata", "id")
	poll(t, "the v1alpha1 transfer to stop", nil, func() bool {
		g := nbJSON(t, cli, "storage", "v1alpha1", "transfer", "get", alphaID)
		return trxDig(g, "status", "state") == "STOPPED" && trxDig(g, "status", "last_iteration", "state") == "COMPLETED"
	})
	g := nbJSON(t, cli, "storage", "v1alpha1", "transfer", "get", alphaID)
	if trxDig(g, "status", "last_iteration", "objects_migrated_count") != "1" {
		t.Errorf("v1alpha1 get = %v, want 1 object migrated", g["status"])
	}
	if text := nb(t, cli, "storage", "v1alpha1", "transfer", "get", alphaID); strings.Contains(text, ext.SecretKey) || strings.Contains(text, e.secret) {
		t.Errorf("nebius storage v1alpha1 transfer get printed a secret:\n%s", text)
	}
	mustContain(t, "aws s3 ls v1alpha1 destination", aw(t, aws, "s3", "ls", "s3://"+dstName+"/alpha/", "--recursive"), "alpha/x.txt")
	// The v1 command group sees it too ("all existing transfers remain accessible").
	if v1 := nbJSON(t, cli, "storage", "transfer", "get", alphaID); trxDig(v1, "metadata", "name") != "cli-alpha" {
		t.Errorf("v1 get of the v1alpha1 transfer = %v", v1)
	}
}
