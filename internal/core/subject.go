package core

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// A subject is a provider prefix and an id, like github:org/api#421 or
	// slack:C0123ABCD/1761811234.567890. Not a URL: https: is followed by /.
	subject = regexp.MustCompile(`^[a-z][a-z0-9-]*:[^/\s]\S*$`)

	prURL   = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/pull/(\d+)`)
	prShort = regexp.MustCompile(`^([\w.-]+)/([\w.-]+)#(\d+)$`)

	ownerURL   = regexp.MustCompile(`^https?://github\.com/([\w.-]+)(?:/([\w.-]+))?(?:[/?#].*)?$`)
	ownerShort = regexp.MustCompile(`^([\w.-]+)(?:/([\w.-]+))?$`)
)

// ParseSubject accepts a subject as kelpie status lists it, of any provider,
// or what GitHubSubject accepts.
func ParseSubject(s string) (Subject, error) {
	s = strings.TrimSpace(s)
	if subject.MatchString(s) {
		return Subject(s), nil
	}
	return GitHubSubject(s)
}

// GitHubSubject accepts what a person or agent would paste about GitHub. A
// pull request URL (any sub-page, like /files or #discussion_r1) or
// owner/repo#123 is the PR: github:owner/repo#123. A repo (owner/repo), an
// org, or a github.com URL of either is its notifications:
// github-inbox:owner/repo or github-inbox:owner.
func GitHubSubject(s string) (Subject, error) {
	s = strings.TrimSpace(s)
	m := prURL.FindStringSubmatch(s)
	if m == nil {
		m = prShort.FindStringSubmatch(s)
	}
	if m != nil {
		return Subject(fmt.Sprintf("github:%s/%s#%s", m[1], m[2], m[3])), nil
	}
	m = ownerURL.FindStringSubmatch(s)
	if m == nil {
		m = ownerShort.FindStringSubmatch(s)
	}
	if m == nil {
		return "", fmt.Errorf("cannot parse %q: want a pull request URL, owner/repo#number, owner/repo or an org", s)
	}
	if m[2] == "" {
		return Subject("github-inbox:" + m[1]), nil
	}
	return Subject("github-inbox:" + m[1] + "/" + m[2]), nil
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
