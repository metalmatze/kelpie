package core

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	prURL   = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/pull/(\d+)`)
	prShort = regexp.MustCompile(`^(?:github:)?([\w.-]+)/([\w.-]+)#(\d+)$`)
)

// ParseSubject accepts what a person or agent would paste: a PR URL (any
// sub-page, like /files or #discussion_r1), owner/repo#123, or a subject.
func ParseSubject(s string) (Subject, error) {
	s = strings.TrimSpace(s)
	if m := prURL.FindStringSubmatch(s); m != nil {
		return Subject(fmt.Sprintf("github:%s/%s#%s", m[1], m[2], m[3])), nil
	}
	if m := prShort.FindStringSubmatch(s); m != nil {
		return Subject(fmt.Sprintf("github:%s/%s#%s", m[1], m[2], m[3])), nil
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return "", fmt.Errorf("%s is not a GitHub pull request URL", s)
	}
	return "", fmt.Errorf("cannot parse %q: want a PR URL or owner/repo#number", s)
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
		// Lazy, so in "{owner}/{name}" the owner stops at the first slash.
		fmt.Fprintf(&b, "(?P<%s>.+?)", p[m[2]:m[3]])
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
