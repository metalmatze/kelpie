package core

import (
	"fmt"
	"regexp"
	"strings"
)

// A subject is a provider prefix and an id, like github:org/api#421 or
// slack:C0123ABCD/1761811234.567890. Not a URL: https: is followed by /.
var subject = regexp.MustCompile(`^[a-z][a-z0-9-]*:[^/\s]\S*$`)

// IsSubject tells a subject from a URL or a provider's own ID.
func IsSubject(s string) bool { return subject.MatchString(s) }

// ParseSubject accepts a subject as kelpie status lists it, of any provider.
// URLs and IDs are the providers' to parse: the CLI has their subject
// subcommand turn them into subjects before it asks the daemon.
func ParseSubject(s string) (Subject, error) {
	s = strings.TrimSpace(s)
	if !subject.MatchString(s) {
		return "", fmt.Errorf("%q is no subject like github:acme/api#1; kelpie watch and unwatch have a provider resolve it", s)
	}
	return Subject(s), nil
}

// Pattern matches subjects against "github:{owner}/{name}#{number}" and
// returns the placeholders as fields.
type Pattern struct{ re *regexp.Regexp }

var placeholder = regexp.MustCompile(`\{(\w+)\}`)

func NewPattern(p string) (Pattern, error) {
	var b strings.Builder
	b.WriteString("^")
	last := 0
	for _, m := range placeholder.FindAllStringSubmatchIndex(p, -1) {
		b.WriteString(regexp.QuoteMeta(p[last:m[0]]))
		// Never across a slash, so "github-inbox:{owner}" does not match the
		// repo subject github-inbox:owner/name as well.
		fmt.Fprintf(&b, "(?P<%s>[^/]+?)", p[m[2]:m[3]])
		last = m[1]
	}
	b.WriteString(regexp.QuoteMeta(p[last:]))
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return Pattern{re}, err
}

func (p Pattern) Match(s Subject) (map[string]any, bool) {
	m := p.re.FindStringSubmatch(string(s))
	if m == nil {
		return nil, false
	}
	fields := map[string]any{}
	for i, name := range p.re.SubexpNames() {
		if name != "" {
			fields[name] = m[i]
		}
	}
	return fields, true
}

// BranchPattern binds a provider's subjects to the workspace whose branch
// names them, as user/eng-123-fix-auth names linear:ENG-123. It goes from the
// subject to a regex rather than from the branch to a subject: one branch may
// name several IDs, and the provider's IDs keep their own case rules inside
// the regex instead of kelpie guessing how eng-123 becomes ENG-123.
type BranchPattern struct {
	subject Pattern
	branch  string
}

// NewBranchPattern takes a subject pattern like "linear:{id}" and a branch
// regex in which each of its {placeholders} stands for the subject's value,
// quoted. Other braces, like a {2} quantifier, stay regex.
func NewBranchPattern(subject, branch string) (BranchPattern, error) {
	p, err := NewPattern(subject)
	if err != nil {
		return BranchPattern{}, fmt.Errorf("subject %q: %w", subject, err)
	}
	b := BranchPattern{subject: p, branch: branch}
	fields := map[string]any{}
	for _, n := range p.re.SubexpNames() {
		if n != "" {
			fields[n] = "x"
		}
	}
	if len(fields) == 0 {
		return BranchPattern{}, fmt.Errorf("subject %q has no {placeholder} for the branch to name", subject)
	}
	if _, err := regexp.Compile(b.fill(fields)); err != nil {
		return BranchPattern{}, fmt.Errorf("branch %q: %w", branch, err)
	}
	return b, nil
}

// About tells whether the pattern binds s at all.
func (b BranchPattern) About(s Subject) bool {
	_, ok := b.subject.Match(s)
	return ok
}

// Match tells whether the pattern is about s at all, and if so whether
// branch names it.
func (b BranchPattern) Match(s Subject, branch string) (applies, names bool) {
	fields, ok := b.subject.Match(s)
	if !ok {
		return false, false
	}
	re, err := regexp.Compile(b.fill(fields))
	return true, err == nil && re.MatchString(branch)
}

func (b BranchPattern) fill(fields map[string]any) string {
	return placeholder.ReplaceAllStringFunc(b.branch, func(m string) string {
		v, ok := fields[m[1:len(m)-1]]
		if !ok {
			return m
		}
		return regexp.QuoteMeta(v.(string))
	})
}
