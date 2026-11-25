//go:build e2e_storage

package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/test/internal/awscli"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
)

// nb runs a nebius CLI command and returns its trimmed stdout.
func nb(t *testing.T, cli *nebiuscli.CLI, args ...string) string {
	t.Helper()
	res, err := cli.Run(t.Context(), args...)
	if err != nil {
		t.Fatalf("nebius %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, res.Stdout, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

// nbFails runs a nebius CLI command that must fail and returns stdout+stderr.
func nbFails(t *testing.T, cli *nebiuscli.CLI, args ...string) string {
	t.Helper()
	res, err := cli.Run(t.Context(), args...)
	if err == nil {
		t.Fatalf("nebius %s succeeded, want an error; stdout: %s", strings.Join(args, " "), res.Stdout)
	}
	return res.Stdout + res.Stderr
}

// nbJSON runs a nebius command with --format json and decodes the output
// (what the docs pipe through "jq").
func nbJSON(t *testing.T, cli *nebiuscli.CLI, args ...string) map[string]any {
	t.Helper()
	var m map[string]any
	cli.JSON(t, &m, args...)
	return m
}

// jq digs a dotted path (as in "jq -r '.status.secret'") out of decoded JSON.
func jq(t *testing.T, m map[string]any, path string) string {
	t.Helper()
	var cur any = m
	for _, k := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("jq .%s: %v is not an object in %v", path, cur, m)
		}
		cur = obj[k]
	}
	s, ok := cur.(string)
	if !ok || s == "" {
		t.Fatalf("jq -r .%s = %v, want a non-empty string in %v", path, cur, m)
	}
	return s
}

// aw runs an aws CLI command and returns stdout.
func aw(t *testing.T, c *awscli.CLI, args ...string) string {
	t.Helper()
	out, err := c.Run(t.Context(), args...)
	if err != nil {
		t.Fatalf("%v\nstdout: %s", err, out)
	}
	return out
}

// awFails runs an aws CLI command that must fail and returns its error text.
func awFails(t *testing.T, c *awscli.CLI, args ...string) string {
	t.Helper()
	out, err := c.Run(t.Context(), args...)
	if err == nil {
		t.Fatalf("aws %s succeeded, want an error; stdout: %s", strings.Join(args, " "), out)
	}
	return err.Error()
}

// awLsEmpty asserts that "aws s3 ls <uri> --recursive --summarize" finds
// nothing (the real CLI exits 1 when nothing matches).
func awLsEmpty(t *testing.T, c *awscli.CLI, uri string) {
	t.Helper()
	out, err := c.Run(t.Context(), "s3", "ls", uri, "--recursive", "--summarize")
	if err == nil || !strings.Contains(out, "Total Objects: 0") {
		t.Errorf("aws s3 ls %s = %q, %v; want exit 1 with Total Objects: 0", uri, out, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustContain(t *testing.T, what, got string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(got, s) {
			t.Errorf("%s: missing %q in:\n%s", what, s, got)
		}
	}
}

func newNebiusCLI(t *testing.T, e *env) *nebiuscli.CLI {
	t.Helper()
	return nebiuscli.New(t, nebiuscli.Profile{
		Addr: e.inst.Addr, Token: e.inst.Token, ParentID: e.inst.ProjectID, TenantID: e.inst.TenantID,
	})
}

// awsKeyFor follows the docs' access-key steps for service account saID.
func awsKeyFor(t *testing.T, cli *nebiuscli.CLI, saID string) (awsKeyID, secret string) {
	t.Helper()
	keyID := jq(t, nbJSON(t, cli, "iam", "v2", "access-key", "create",
		"--account-service-account-id", saID, "--description", "AWS CLI"), "metadata.id")
	got := nbJSON(t, cli, "iam", "v2", "access-key", "get", "--id", keyID)
	return jq(t, got, "status.aws_access_key_id"), jq(t, got, "status.secret")
}

func (e *env) awsCLI(t *testing.T, keyID, secret string) *awscli.CLI {
	t.Helper()
	c := awscli.New(t)
	c.Configure(t, keyID, secret, "eu-north1", "http://"+e.s3Addr)
	return c
}

// TestCLIQuickstart runs the Object Storage quickstart and the documented aws
// CLI forms literally.
func TestCLIQuickstart(t *testing.T) {
	e := start(t)
	cli := newNebiusCLI(t, e)

	saID := nb(t, cli, "iam", "service-account", "create", "--name", "object-storage-sa", "--format", "jsonpath={.metadata.id}")
	projectID := nb(t, cli, "config", "get", "parent-id")
	if projectID != e.inst.ProjectID {
		t.Fatalf("config get parent-id = %q, want %q", projectID, e.inst.ProjectID)
	}
	tenantID := nb(t, cli, "iam", "project", "get", projectID, "--format", "jsonpath={.metadata.parent_id}")
	if tenantID != e.inst.TenantID {
		t.Fatalf("project parent_id = %q, want %q", tenantID, e.inst.TenantID)
	}
	editors := nb(t, cli, "iam", "group", "get-by-name", "--name", "editors", "--parent-id", tenantID, "--format", "jsonpath={.metadata.id}")
	nb(t, cli, "iam", "group-membership", "create", "--parent-id", editors, "--member-id", saID)

	keyID, secret := awsKeyFor(t, cli, saID)
	aws := e.awsCLI(t, keyID, secret)

	b := unique("quickstart-bucket")
	aw(t, aws, "s3", "mb", "s3://"+b)
	dir := t.TempDir()
	testTxt := filepath.Join(dir, "test.txt")
	writeFile(t, testTxt, "Hello world!\n")
	aw(t, aws, "s3", "cp", testTxt, "s3://"+b+"/test.txt")
	mustContain(t, "aws s3 ls bucket", aw(t, aws, "s3", "ls", "s3://"+b), "test.txt")
	mustContain(t, "aws s3 ls", aw(t, aws, "s3", "ls"), b)

	// The bucket created over S3 is visible in the Nebius API.
	got := nbJSON(t, cli, "storage", "bucket", "get-by-name", "--name", b)
	if st := jq(t, got, "status.state"); st != "ACTIVE" {
		t.Errorf("bucket state = %s, want ACTIVE", st)
	}
	bucketID := jq(t, got, "metadata.id")
	if id := nb(t, cli, "storage", "bucket", "get-by-name", "--name", b, "--format", "jsonpath={.metadata.id}"); id != bucketID {
		t.Errorf("get-by-name jsonpath id = %q, want %q", id, bucketID)
	}

	// Download.
	back := filepath.Join(dir, "back.txt")
	aw(t, aws, "s3", "cp", "s3://"+b+"/test.txt", back)
	if data, err := os.ReadFile(back); err != nil || string(data) != "Hello world!\n" {
		t.Errorf("downloaded = %q, %v; want Hello world!", data, err)
	}
	if out := aw(t, aws, "s3", "cp", "s3://"+b+"/test.txt", "-"); out != "Hello world!\n" {
		t.Errorf("cp to stdout = %q", out)
	}

	// cp --recursive and sync into prefixes.
	src := filepath.Join(dir, "src")
	writeFile(t, filepath.Join(src, "a.txt"), "a")
	writeFile(t, filepath.Join(src, "sub", "b.txt"), "bb")
	aw(t, aws, "s3", "cp", "--recursive", src, "s3://"+b+"/copied/")
	syncDir := filepath.Join(dir, "sync")
	writeFile(t, filepath.Join(syncDir, "c.txt"), "ccc")
	writeFile(t, filepath.Join(syncDir, "d", "e.txt"), "dddd")
	aw(t, aws, "s3", "sync", syncDir+"/", "s3://"+b+"/synced/")
	if out := aw(t, aws, "s3", "sync", syncDir+"/", "s3://"+b+"/synced/"); strings.Contains(out, "upload:") {
		t.Errorf("second sync uploaded again: %s", out)
	}
	out := aw(t, aws, "s3", "ls", "s3://"+b+"/copied", "--recursive", "--human-readable", "--summarize")
	mustContain(t, "ls copied", out, "copied/a.txt", "copied/sub/b.txt", "Total Objects: 2")
	out = aw(t, aws, "s3", "ls", "s3://"+b+"/synced", "--recursive", "--human-readable", "--summarize")
	mustContain(t, "ls synced", out, "synced/c.txt", "synced/d/e.txt", "Total Objects: 2")

	// mv --recursive into a second bucket created with the Nebius CLI, with a
	// lifecycle rule set through the Nebius API: S3 reads the same spec.
	dst := unique("quickstart-dst")
	nb(t, cli, "storage", "bucket", "create", "--name", dst,
		"--lifecycle-configuration-rules", `[{"id":"from-nebius","status":"enabled","filter":{"prefix":"tmp/"},"expiration":{"days":3}}]`)
	var fromNebius struct {
		Rules []struct {
			ID         string
			Status     string
			Filter     struct{ Prefix string }
			Expiration struct{ Days int }
		}
	}
	if err := json.Unmarshal([]byte(aw(t, aws, "s3api", "get-bucket-lifecycle-configuration", "--bucket", dst)), &fromNebius); err != nil ||
		len(fromNebius.Rules) != 1 || fromNebius.Rules[0].ID != "from-nebius" || fromNebius.Rules[0].Status != "Enabled" ||
		fromNebius.Rules[0].Filter.Prefix != "tmp/" || fromNebius.Rules[0].Expiration.Days != 3 {
		t.Errorf("get-bucket-lifecycle-configuration of a Nebius-configured bucket = %+v, %v", fromNebius, err)
	}
	aw(t, aws, "s3", "mv", "s3://"+b+"/copied/", "s3://"+dst+"/copied/", "--recursive")
	mustContain(t, "ls dst", aw(t, aws, "s3", "ls", "s3://"+dst+"/copied", "--recursive", "--summarize"), "Total Objects: 2")
	awLsEmpty(t, aws, "s3://"+b+"/copied")

	// rm one key, then rm --recursive a prefix.
	aw(t, aws, "s3", "rm", "s3://"+b+"/test.txt")
	awLsEmpty(t, aws, "s3://"+b+"/test.txt")
	aw(t, aws, "s3", "rm", "s3://"+b+"/synced", "--recursive")
	awLsEmpty(t, aws, "s3://"+b+"/synced")

	// Lifecycle through the S3 API shows up in the Nebius API.
	lc := filepath.Join(dir, "lifecycle.json")
	writeFile(t, lc, `{"Rules":[{"ID":"expire-5","Status":"Enabled","Filter":{},"Expiration":{"Days":5}}]}`)
	aw(t, aws, "s3api", "put-bucket-lifecycle-configuration", "--bucket", b, "--lifecycle-configuration", "file://"+lc)
	var s3lc struct {
		Rules []struct {
			ID         string
			Status     string
			Expiration struct{ Days int }
		}
	}
	if err := json.Unmarshal([]byte(aw(t, aws, "s3api", "get-bucket-lifecycle-configuration", "--bucket", b)), &s3lc); err != nil ||
		len(s3lc.Rules) != 1 || s3lc.Rules[0].ID != "expire-5" || s3lc.Rules[0].Status != "Enabled" || s3lc.Rules[0].Expiration.Days != 5 {
		t.Errorf("get-bucket-lifecycle-configuration = %+v, %v", s3lc, err)
	}
	spec, _ := nbJSON(t, cli, "storage", "bucket", "get", "--id", bucketID)["spec"].(map[string]any)
	lcSpec, _ := spec["lifecycle_configuration"].(map[string]any)
	rules, _ := lcSpec["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("nebius spec.lifecycle_configuration = %v, want one rule expire-5", spec["lifecycle_configuration"])
	}
	if r, _ := rules[0].(map[string]any); r["id"] != "expire-5" {
		t.Errorf("nebius lifecycle rule = %v, want id expire-5", r)
	}

	// rb on a non-empty bucket fails; rb --force empties and removes it.
	aw(t, aws, "s3", "cp", testTxt, "s3://"+b+"/keep.txt")
	mustContain(t, "rb non-empty", awFails(t, aws, "s3", "rb", "s3://"+b), "BucketNotEmpty")
	aw(t, aws, "s3", "rb", "s3://"+b, "--force")
	if st := jq(t, nbJSON(t, cli, "storage", "bucket", "get", "--id", bucketID), "status.state"); st != "SCHEDULED_FOR_DELETION" {
		t.Errorf("state after rb --force = %s, want SCHEDULED_FOR_DELETION", st)
	}
	if out := aw(t, aws, "s3", "ls"); strings.Contains(out, b) {
		t.Errorf("aws s3 ls still lists deleted bucket: %s", out)
	}
}

// TestCLIBucketLifecycle runs the documented "nebius storage bucket" commands.
func TestCLIBucketLifecycle(t *testing.T) {
	e := start(t)
	cli := newNebiusCLI(t, e)

	name := unique("cli-bucket")
	nb(t, cli, "storage", "bucket", "create", "--name", name,
		"--max-size-bytes", "1073741824",
		"--versioning-policy", "enabled",
		"--cors-rules", `[{"allowed_methods":["GET"],"allowed_origins":["https://example.com"],"max_age_seconds":3600}]`,
		"--lifecycle-configuration-rules", `[{"id":"r1","status":"enabled","filter":{"prefix":"expiring/"},"expiration":{"days":5}},`+
			`{"id":"r2","status":"enabled","noncurrent_version_expiration":{"noncurrent_days":5,"newer_noncurrent_versions":3}},`+
			`{"id":"r3","status":"enabled","abort_incomplete_multipart_upload":{"days_after_initiation":7}}]`,
		"--bucket-policy-rules", `[{"paths":["public/*"],"roles":["storage.viewer"],"anonymous":{}}]`)

	id := nb(t, cli, "storage", "bucket", "get-by-name", "--name", name, "--format", "jsonpath={.metadata.id}")
	got := nbJSON(t, cli, "storage", "bucket", "get", "--id", id)
	raw, _ := json.Marshal(got)
	mustContain(t, "bucket get", string(raw),
		`"max_size_bytes":"1073741824"`, `"versioning_policy":"ENABLED"`, "https://example.com", `"max_age_seconds":3600`,
		`"id":"r1"`, `"prefix":"expiring/"`, `"days":5`, `"noncurrent_days":5`, `"newer_noncurrent_versions":3`,
		`"days_after_initiation":7`, "public/*", "storage.viewer")
	if st := jq(t, got, "status.state"); st != "ACTIVE" {
		t.Errorf("state = %s, want ACTIVE", st)
	}
	status, _ := got["status"].(map[string]any)
	if status["anonymous_access_enabled"] != true {
		t.Errorf("status.anonymous_access_enabled = %v, want true; status = %v", status["anonymous_access_enabled"], status)
	}
	if pid := nb(t, cli, "storage", "bucket", "get", id, "--format", "jsonpath={.metadata.parent_id}"); pid != e.inst.ProjectID {
		t.Errorf("get <id> parent_id = %q, want %q", pid, e.inst.ProjectID)
	}

	// update is a patch by default: other fields stay.
	nb(t, cli, "storage", "bucket", "update", "--id", id, "--max-size-bytes", "268435456000")
	after, _ := json.Marshal(nbJSON(t, cli, "storage", "bucket", "get", "--id", id))
	mustContain(t, "after update", string(after), `"max_size_bytes":"268435456000"`, "https://example.com", `"id":"r1"`,
		"public/*", `"versioning_policy":"ENABLED"`)

	// Invalid settings name the field.
	msg := nbFails(t, cli, "storage", "bucket", "update", "--id", id, "--default-storage-class", "intelligent")
	mustContain(t, "invalid default_storage_class", strings.ToLower(msg), "storage_class")
	msg = nbFails(t, cli, "storage", "bucket", "update", "--id", id, "--object-audit-logging", "all")
	mustContain(t, "invalid object_audit_logging", strings.ToLower(msg), "audit")

	// delete -> SCHEDULED_FOR_DELETION, undelete -> ACTIVE.
	nb(t, cli, "storage", "bucket", "delete", "--id", id)
	if st := jq(t, nbJSON(t, cli, "storage", "bucket", "get", "--id", id), "status.state"); st != "SCHEDULED_FOR_DELETION" {
		t.Errorf("state after delete = %s", st)
	}
	nb(t, cli, "storage", "bucket", "undelete", "--id", id)
	if st := jq(t, nbJSON(t, cli, "storage", "bucket", "get", "--id", id), "status.state"); st != "ACTIVE" {
		t.Errorf("state after undelete = %s", st)
	}

	// delete --ttl 0s removes the bucket right away.
	nb(t, cli, "storage", "bucket", "delete", "--id", id, "--ttl", "0s")
	mustContain(t, "get after ttl 0s", strings.ToLower(nbFails(t, cli, "storage", "bucket", "get", "--id", id)), "not found")

	// delete then purge.
	other := unique("cli-purge")
	nb(t, cli, "storage", "bucket", "create", "--name", other)
	otherID := nb(t, cli, "storage", "bucket", "get-by-name", "--name", other, "--format", "jsonpath={.metadata.id}")
	nb(t, cli, "storage", "bucket", "delete", "--id", otherID)
	nb(t, cli, "storage", "bucket", "purge", "--id", otherID)
	mustContain(t, "get after purge", strings.ToLower(nbFails(t, cli, "storage", "bucket", "get", "--id", otherID)), "not found")
}

// TestCLIBucketPolicyAndCORS grants a service account with no IAM permit
// access through a bucket policy rule for its group, and anonymous access
// through an anonymous rule.
func TestCLIBucketPolicyAndCORS(t *testing.T) {
	e := start(t)
	cli := newNebiusCLI(t, e)

	// An editor writes the objects; a second account has no IAM permit at all.
	editorSA := nb(t, cli, "iam", "service-account", "create", "--name", unique("editor-sa"), "--format", "jsonpath={.metadata.id}")
	editors := nb(t, cli, "iam", "group", "get-by-name", "--name", "editors", "--parent-id", e.inst.TenantID, "--format", "jsonpath={.metadata.id}")
	nb(t, cli, "iam", "group-membership", "create", "--parent-id", editors, "--member-id", editorSA)
	editorKey, editorSecret := awsKeyFor(t, cli, editorSA)
	editor := e.awsCLI(t, editorKey, editorSecret)

	saID := nb(t, cli, "iam", "service-account", "create", "--name", unique("policy-sa"), "--format", "jsonpath={.metadata.id}")
	groupID := nb(t, cli, "iam", "group", "create", "--name", unique("policy-group"), "--parent-id", e.inst.TenantID, "--format", "jsonpath={.metadata.id}")
	nb(t, cli, "iam", "group-membership", "create", "--parent-id", groupID, "--member-id", saID)
	keyID, secret := awsKeyFor(t, cli, saID)
	aws := e.awsCLI(t, keyID, secret)

	b := unique("policy-bucket")
	nb(t, cli, "storage", "bucket", "create", "--name", b)
	bucketID := nb(t, cli, "storage", "bucket", "get-by-name", "--name", b, "--format", "jsonpath={.metadata.id}")

	file := filepath.Join(t.TempDir(), "x.txt")
	writeFile(t, file, "payload")

	// No permit and no rule: denied.
	mustContain(t, "no permit", awFails(t, aws, "s3", "cp", file, "s3://"+b+"/allowed/x.txt"), "AccessDenied")

	rules := `[{"paths":["allowed/*"],"roles":["storage.editor"],"group_id":"` + groupID + `"},` +
		`{"paths":["public/*"],"roles":["storage.viewer"],"anonymous":{}}]`
	nb(t, cli, "storage", "bucket", "update", "--id", bucketID, "--bucket-policy-rules", rules)
	nb(t, cli, "storage", "bucket", "update", "--id", bucketID, "--cors-rules",
		`[{"allowed_methods":["GET"],"allowed_origins":["*"]}]`)

	aw(t, aws, "s3", "cp", file, "s3://"+b+"/allowed/x.txt")
	mustContain(t, "outside allowed prefix", awFails(t, aws, "s3", "cp", file, "s3://"+b+"/other/x.txt"), "AccessDenied")

	// Anonymous: the public prefix is readable without signing, nothing else.
	aw(t, editor, "s3", "cp", file, "s3://"+b+"/public/x")
	aw(t, editor, "s3", "cp", file, "s3://"+b+"/private/x")
	if out := aw(t, aws, "s3", "cp", "s3://"+b+"/public/x", "-", "--no-sign-request"); out != "payload" {
		t.Errorf("anonymous read of public/x = %q", out)
	}
	mustContain(t, "anonymous private", awFails(t, aws, "s3", "cp", "s3://"+b+"/private/x", "-", "--no-sign-request"), "403")
	mustContain(t, "anonymous write", awFails(t, aws, "s3", "cp", file, "s3://"+b+"/public/y", "--no-sign-request"), "AccessDenied")

	// The update kept the policy and added CORS.
	raw, _ := json.Marshal(nbJSON(t, cli, "storage", "bucket", "get", "--id", bucketID))
	mustContain(t, "bucket get", string(raw), groupID, "public/*", "allowed_origins")
}
