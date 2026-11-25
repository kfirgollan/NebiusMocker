package audit

import (
	"errors"
	"strings"
	"testing"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

func sampleFields() *Fields {
	return &Fields{
		Action:         "DELETE",
		Type:           "ai.nebius.compute.computeinstance.delete",
		Status:         "DONE",
		Service:        "IAM",
		SubjectName:    "example@nebius.com",
		ResourceID:     "computeinstance-e00abc",
		ResourceName:   "my-test-vm",
		ResourceType:   "computeinstance",
		HierarchyIDs:   []string{"tenant-e00x", "container-e0t"},
		HierarchyNames: []string{"acme", "prod"},
	}
}

func TestParseFilterMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		filter string
		want   bool
	}{
		{"empty", "", true},
		{"whitespace only", " \t\n ", true},
		{"eq single", "action='DELETE'", true},
		{"eq single miss", "action='CREATE'", false},
		{"eq case sensitive", "action='delete'", false},
		{"ne single", "action!='CREATE'", true},
		{"ne single miss", "action!='DELETE'", false},
		{"contains single", "resource.metadata.name:'test'", true},
		{"contains single miss", "resource.metadata.name:'prod'", false},
		{"eq multi any", "resource.hierarchy.id='container-e0t'", true},
		{"eq multi miss", "resource.hierarchy.id='nope'", false},
		{"ne multi none equal", "resource.hierarchy.id!='nope'", true},
		{"ne multi one equal", "resource.hierarchy.id!='tenant-e00x'", false},
		{"contains multi", "resource.hierarchy.name:'ro'", true},
		{"contains multi miss", "resource.hierarchy.name:'zzz'", false},
		{"regex single", `regex(resource.metadata.name, '^.*test.*$')`, true},
		{"regex doc alias", `regex(resource.name, '^.*prod.*$')`, false},
		{"regex unanchored", "regex(resource.name, 'test')", true},
		{"regex anchored miss", "regex(resource.name, '^test')", false},
		{"regex multi", "regex(resource.hierarchy.name, '^pro')", true},
		{"regex spaces", "regex ( resource.name ,\"vm$\" )", true},
		{"AND chain", "service.name = 'IAM' AND resource.hierarchy.id:'container-e0t' AND regex(resource.metadata.name, '^.*test.*$')", true},
		{"AND chain miss", "service.name='IAM' AND action='CREATE'", false},
		{"lowercase and", "action='DELETE' and status='DONE'", true},
		{"mixed case And", "action='DELETE' And status='DONE'", true},
		{"AND adjacent quotes", "action='DELETE'AND status='DONE'", true},
		{"double quotes", `action="DELETE"`, true},
		{"escaped quote", `resource.name='it\'s'`, false},
		{"escaped backslash", `resource.name='a\\b'`, false},
		{"escaped other char", `action='DEL\ETE'`, true},
		{"bareword", "action=DELETE", true},
		{"bareword with punctuation", "authentication.subject.name=example@nebius.com", true},
		{"bareword AND bareword", "action=DELETE AND status=DONE", true},
		{"no spaces", "action='DELETE'AND status!=ERROR", true},
		{"newline separated", "action='DELETE'\nAND\nstatus='DONE'", true},
		{"leading trailing ws", "  action='DELETE'  ", true},
		{"spaces around op", "action   =   'DELETE'", true},
		{
			"doc example subject",
			"authentication.subject.name='example@nebius.com' AND action='DELETE' AND resource.metadata.type='computeinstance'",
			true,
		},
		{"doc example hierarchy", "resource.hierarchy.name:'x'", false},
		{"doc example type", "type='ai.nebius.compute.computeinstance.delete'", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, err := ParseFilter(tt.filter)
			if err != nil {
				t.Fatalf("ParseFilter(%q): %v", tt.filter, err)
			}
			if got := f.Match(sampleFields()); got != tt.want {
				t.Errorf("Match(%q) = %v, want %v", tt.filter, got, tt.want)
			}
		})
	}
}

func TestParseFilterEscapes(t *testing.T) {
	t.Parallel()
	e := &Fields{ResourceName: `it's a\b`}
	for _, s := range []string{`resource.name='it\'s a\\b'`, `resource.name="it's a\\b"`} {
		f, err := ParseFilter(s)
		if err != nil {
			t.Fatalf("ParseFilter(%q): %v", s, err)
		}
		if !f.Match(e) {
			t.Errorf("Match(%q) = false, want true", s)
		}
	}
}

func TestZeroFilterMatchesAll(t *testing.T) {
	t.Parallel()
	var f Filter
	if !f.Match(&Fields{}) || !f.Match(sampleFields()) {
		t.Error("zero Filter must match everything")
	}
}

func TestParseFilterErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		filter string
		want   string
	}{
		{"unknown field", "foo='x'", `position 1: unknown field "foo"; valid fields: action, authentication.`},
		{"unknown regex field", "regex(nope, 'x')", `position 7: unknown field "nope"`},
		{"missing operator", "action x", `position 8: expected operator (=, !=, :) after "action", found "x"`},
		{"operator at end", "action", `position 7: expected operator (=, !=, :) after "action", found end of filter`},
		{"lone bang", "action ! 'x'", `position 8: expected operator`},
		{"unterminated string", "action='x", "position 8: unterminated string"},
		{"unterminated after escape", `action='x\`, "position 8: unterminated string"},
		{"missing value", "action=", "position 8: unexpected end of filter, expected value"},
		{"bad value char", "action=!", `position 8: expected value, found "!"`},
		{"missing AND", "action='x' y", `position 12: expected AND, found "y"`},
		{"glued AND", "action=''ANDaction=''", `position 10: expected AND, found "ANDaction=''"`},
		{"OR", "action='a' OR action='b'", "position 12: only AND is supported"},
		{"NOT", "action='a' NOT action='b'", "position 12: only AND is supported"},
		{"trailing AND", "action='a' AND", "position 15: expected field name, found end of filter"},
		{"leading AND", "AND action='a'", "position 1: unknown field"},
		{"parens", "(action='a')", `position 1: expected field name, found "(action='a')"`},
		{"trailing dot", "resource.='a'", `position 10: expected field name, found "='a'"`},
		{"invalid regex", "regex(action, '(')", "position 15: invalid regex"},
		{"regex missing comma", "regex(action 'x')", `position 14: expected ",", found "'x')"`},
		{"regex missing paren", "regex(action, 'x'", `position 18: expected ")", found end of filter`},
		{"regex unquoted pattern", "regex(action, x)", "position 15: expected quoted regex pattern"},
		{"regex no path", "regex(", "position 7: expected field name"},
		{"unicode position", "action='é' x", `position 12: expected AND, found "x"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseFilter(tt.filter)
			if err == nil {
				t.Fatalf("ParseFilter(%q) succeeded, want error", tt.filter)
			}
			if k := errs.KindOf(err); k != errs.KindBadRequest {
				t.Fatalf("kind = %v, want bad_request", k)
			}
			var e *errs.Error
			if !asErr(err, &e) || len(e.Violations) != 1 || e.Violations[0].Field != "filter" {
				t.Fatalf("want one violation on field filter, got %#v", err)
			}
			if msg := e.Violations[0].Message; !strings.Contains(msg, tt.want) {
				t.Errorf("message = %q, want it to contain %q", msg, tt.want)
			}
		})
	}
}

func TestUnknownFieldListsSortedPaths(t *testing.T) {
	t.Parallel()
	_, err := ParseFilter("bogus='x'")
	var e *errs.Error
	if !asErr(err, &e) {
		t.Fatalf("err = %v", err)
	}
	_, list, ok := strings.Cut(e.Violations[0].Message, "valid fields: ")
	if !ok {
		t.Fatalf("no field list in %q", e.Violations[0].Message)
	}
	got := strings.Split(list, ", ")
	if len(got) != len(fields) {
		t.Errorf("listed %d fields, want %d", len(got), len(fields))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("fields not sorted: %q before %q", got[i-1], got[i])
		}
	}
}

func FuzzParseFilter(f *testing.F) {
	seeds := []string{
		"",
		" ",
		"authentication.subject.name='example@nebius.com' AND action='DELETE' AND resource.metadata.type='computeinstance'",
		"resource.hierarchy.name:'x'",
		"type='ai.nebius.compute.computeinstance.delete'",
		"service.name = 'iam' AND resource.hierarchy.id:'container-e0t' AND regex(resource.metadata.name, '^.*test.*$')",
		`regex(resource.name, '^.*prod.*$')`,
		"action=DELETE and status!=DONE",
		`action='it\'s'`,
		"action='x",
		"action 'x'",
		"action='a' OR action='b'",
		"regex(action, '(')",
		"(action='a')",
		"action='a' AND",
		"regex(",
		"\x00\xff",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	populated := sampleFields()
	f.Fuzz(func(t *testing.T, s string) {
		got, err := ParseFilter(s)
		again, err2 := ParseFilter(s)
		if (err == nil) != (err2 == nil) || (err != nil && err.Error() != err2.Error()) {
			t.Fatalf("non-deterministic parse of %q: %v vs %v", s, err, err2)
		}
		if err != nil {
			if errs.KindOf(err) != errs.KindBadRequest {
				t.Fatalf("error kind = %v for %q", errs.KindOf(err), s)
			}
			return
		}
		r1 := got.Match(&Fields{})
		r2 := got.Match(populated)
		if again.Match(&Fields{}) != r1 || again.Match(populated) != r2 {
			t.Fatalf("non-deterministic match of %q", s)
		}
	})
}

func asErr(err error, target **errs.Error) bool {
	e, ok := errors.AsType[*errs.Error](err)
	if ok {
		*target = e
	}
	return ok
}
