package audit

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Filter is a compiled List filter expression; the zero value (and
// ParseFilter("")) matches everything. Only AND-joined clauses are supported.
type Filter struct {
	clauses []clause
}

type op int

const (
	opEq op = iota
	opNe
	opContains
	opRegex
)

type clause struct {
	field field
	op    op
	value string
	re    *regexp.Regexp
}

// ParseFilter compiles s. Errors are errs.BadRequest violations on "filter"
// whose message names the 1-based character position of the problem.
func ParseFilter(s string) (Filter, error) {
	p := &parser{src: []rune(s)}
	p.skipSpace()
	var f Filter
	if p.eof() {
		return f, nil
	}
	for {
		c, err := p.clause()
		if err != nil {
			return Filter{}, err
		}
		f.clauses = append(f.clauses, c)
		p.skipSpace()
		if p.eof() {
			return f, nil
		}
		if err := p.and(); err != nil {
			return Filter{}, err
		}
		p.skipSpace()
	}
}

// Match reports whether e satisfies every clause.
func (f Filter) Match(e *Fields) bool {
	for i := range f.clauses {
		if !f.clauses[i].match(e) {
			return false
		}
	}
	return true
}

func (c *clause) match(e *Fields) bool {
	var vals []string
	if c.field.many != nil {
		vals = c.field.many(e)
	} else {
		vals = []string{c.field.one(e)}
	}
	hit := slices.ContainsFunc(vals, c.matchOne)
	if c.op == opNe {
		return !hit
	}
	return hit
}

func (c *clause) matchOne(v string) bool {
	switch c.op {
	case opEq, opNe:
		return v == c.value
	case opContains:
		return strings.Contains(v, c.value)
	case opRegex:
		return c.re.MatchString(v)
	}
	return false
}

type parser struct {
	src []rune
	pos int // 0-based index into src
}

func (p *parser) eof() bool { return p.pos >= len(p.src) }

func (p *parser) peek() rune {
	if p.eof() {
		return 0
	}
	return p.src[p.pos]
}

func (p *parser) skipSpace() {
	for !p.eof() && unicode.IsSpace(p.src[p.pos]) {
		p.pos++
	}
}

// fail builds the error for the 0-based index at.
func fail(at int, format string, args ...any) error {
	msg := fmt.Sprintf("position %d: ", at+1) + fmt.Sprintf(format, args...)
	return errs.BadRequest(errs.Violation{Field: "filter", Message: msg})
}

// found describes what is at index at, for "expected X, found Y" messages.
func (p *parser) found(at int) string {
	if at >= len(p.src) {
		return "end of filter"
	}
	end := at
	for end < len(p.src) && end-at < 20 && !unicode.IsSpace(p.src[end]) {
		end++
	}
	if end == at {
		end++
	}
	return strconv.Quote(string(p.src[at:end]))
}

func isIdentStart(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isIdent(r rune) bool { return isIdentStart(r) || (r >= '0' && r <= '9') }

func isBare(r rune) bool {
	return isIdent(r) || strings.ContainsRune(".-@*", r)
}

// and consumes the AND keyword (case-insensitive).
func (p *parser) and() error {
	start := p.pos
	for !p.eof() && isIdent(p.peek()) {
		p.pos++
	}
	word := string(p.src[start:p.pos])
	switch {
	case strings.EqualFold(word, "and"):
		return nil
	case strings.EqualFold(word, "or"), strings.EqualFold(word, "not"):
		return fail(start, "only AND is supported, found %q", word)
	}
	return fail(start, "expected AND, found %s", p.found(start))
}

// path reads ident ("." ident)* without validating it against the field map.
func (p *parser) path() (string, error) {
	start := p.pos
	for {
		if !isIdentStart(p.peek()) {
			return "", fail(p.pos, "expected field name, found %s", p.found(p.pos))
		}
		for isIdent(p.peek()) {
			p.pos++
		}
		if p.peek() != '.' {
			return string(p.src[start:p.pos]), nil
		}
		p.pos++
	}
}

func knownField(path string, at int) (field, error) {
	f, ok := fields[path]
	if ok {
		return f, nil
	}
	valid := make([]string, 0, len(fields))
	for k := range fields {
		valid = append(valid, k)
	}
	slices.Sort(valid)
	return field{}, fail(at, "unknown field %q; valid fields: %s", path, strings.Join(valid, ", "))
}

func (p *parser) clause() (clause, error) {
	start := p.pos
	path, err := p.path()
	if err != nil {
		return clause{}, err
	}
	if path == "regex" {
		save := p.pos
		p.skipSpace()
		if p.peek() == '(' {
			p.pos++
			return p.regexCall()
		}
		p.pos = save
	}
	f, err := knownField(path, start)
	if err != nil {
		return clause{}, err
	}
	o, err := p.operator(path)
	if err != nil {
		return clause{}, err
	}
	v, err := p.value()
	if err != nil {
		return clause{}, err
	}
	return clause{field: f, op: o, value: v}, nil
}

func (p *parser) operator(path string) (op, error) {
	p.skipSpace()
	switch {
	case p.peek() == '=':
		p.pos++
		return opEq, nil
	case p.peek() == ':':
		p.pos++
		return opContains, nil
	case p.peek() == '!' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '=':
		p.pos += 2
		return opNe, nil
	}
	return 0, fail(p.pos, "expected operator (=, !=, :) after %q, found %s", path, p.found(p.pos))
}

func (p *parser) value() (string, error) {
	p.skipSpace()
	if p.eof() {
		return "", fail(p.pos, "unexpected end of filter, expected value")
	}
	if r := p.peek(); r == '\'' || r == '"' {
		return p.str()
	}
	start := p.pos
	for !p.eof() && isBare(p.peek()) {
		p.pos++
	}
	if p.pos == start {
		return "", fail(start, "expected value, found %s", p.found(start))
	}
	return string(p.src[start:p.pos]), nil
}

// str reads a quoted string; a backslash escapes the next character.
func (p *parser) str() (string, error) {
	start := p.pos
	quote := p.src[p.pos]
	p.pos++
	var b strings.Builder
	for !p.eof() {
		r := p.src[p.pos]
		p.pos++
		switch {
		case r == quote:
			return b.String(), nil
		case r == '\\' && !p.eof():
			b.WriteRune(p.src[p.pos])
			p.pos++
		default:
			b.WriteRune(r)
		}
	}
	return "", fail(start, "unterminated string")
}

// expect consumes r after optional whitespace.
func (p *parser) expect(r rune) error {
	p.skipSpace()
	if p.eof() || p.peek() != r {
		return fail(p.pos, "expected %q, found %s", string(r), p.found(p.pos))
	}
	p.pos++
	return nil
}

// regexCall parses the rest of regex( path, 'pattern' ) after the "(".
func (p *parser) regexCall() (clause, error) {
	p.skipSpace()
	start := p.pos
	path, err := p.path()
	if err != nil {
		return clause{}, err
	}
	f, err := knownField(path, start)
	if err != nil {
		return clause{}, err
	}
	if err := p.expect(','); err != nil {
		return clause{}, err
	}
	p.skipSpace()
	if r := p.peek(); p.eof() || (r != '\'' && r != '"') {
		return clause{}, fail(p.pos, "expected quoted regex pattern, found %s", p.found(p.pos))
	}
	patPos := p.pos
	pat, err := p.str()
	if err != nil {
		return clause{}, err
	}
	if err := p.expect(')'); err != nil {
		return clause{}, err
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return clause{}, fail(patPos, "invalid regex: %v", err)
	}
	return clause{field: f, op: opRegex, value: pat, re: re}, nil
}
