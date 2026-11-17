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

	prURL    = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/pull/(\d+)`)
	issueURL = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/issues/(\d+)`)
	// Any page of a run: the run itself, /job/<id> or /attempts/<n>.
	runURL   = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/actions/runs/(\d+)`)
	numShort = regexp.MustCompile(`^([\w.-]+)/([\w.-]+)#(\d+)$`)

	// A single discussion has no subject of its own yet. Without this ownerURL
	// would take its URL for the repo and start a repo-wide watch the person
	// never asked for.
	discussionURL = regexp.MustCompile(`^https?://github\.com/([\w.-]+)/([\w.-]+)/discussions/\d+`)

	releasesURL = regexp.MustCompile(`^https?://github\.com/([\w.-]+)/([\w.-]+)/releases(?:[/?#].*)?$`)

	ownerURL   = regexp.MustCompile(`^https?://github\.com/([\w.-]+)(?:/([\w.-]+))?(?:[/?#].*)?$`)
	ownerShort = regexp.MustCompile(`^([\w.-]+)(?:/([\w.-]+))?$`)

	// linear.app/<workspace>/issue/ENG-123/<slug>; the slug follows the title
	// and may be missing or stale, the identifier never is.
	linearURL = regexp.MustCompile(`^https?://linear\.app/[^/]+/issue/([A-Za-z][A-Za-z0-9]*-\d+)(?:[/?#].*)?$`)
	linearID  = regexp.MustCompile(`^(?i:linear:)?([A-Za-z][A-Za-z0-9]*-\d+)$`)
)

// IsSubject tells a subject from a URL or a provider's own ID.
func IsSubject(s string) bool { return subject.MatchString(s) }

// ParseSubject accepts a subject as kelpie status lists it, of any provider,
// a Linear issue URL, or what GitHubSubject accepts, owner/repo#123 taken as
// a PR. A bare ENG-123 stays GitHub input, an org there.
func ParseSubject(s string) (Subject, error) {
	s = strings.TrimSpace(s)
	if linearURL.MatchString(s) || strings.HasPrefix(strings.ToLower(s), "linear:") {
		return LinearSubject(s)
	}
	if subject.MatchString(s) {
		return Subject(s), nil
	}
	return GitHubSubject(s, nil)
}

// IsPR tells whether number is a pull request rather than an issue of
// owner/name. GitHub numbers both in one sequence, so only GitHub knows.
type IsPR func(owner, name, number string) (bool, error)

// GitHubSubject accepts what a person or agent would paste about GitHub. A
// pull request URL (any sub-page, like /files or #discussion_r1) is the PR:
// github:owner/repo#123. An issue URL is the issue: github-issue:owner/repo#7.
// A workflow run URL (any of its pages) is the run:
// github-run:owner/repo/42043979918. Any releases page of a repo is its
// releases: github-release:owner/repo. owner/repo#123 is whichever isPR says, a
// PR when isPR is nil. A repo (owner/repo), an org, or a github.com URL of
// either is its notifications: github-inbox:owner/repo or github-inbox:owner.
// Other repo pages (/actions, /tree/...) still mean the repo; a single
// discussion is an error.
//
// Issues get a prefix of their own rather than sharing github: with PRs:
// github-pr runs for every github: subject, and PR watches saved as github:
// must keep working.
func GitHubSubject(s string, isPR IsPR) (Subject, error) {
	s = strings.TrimSpace(s)
	if m := prURL.FindStringSubmatch(s); m != nil {
		return Subject(fmt.Sprintf("github:%s/%s#%s", m[1], m[2], m[3])), nil
	}
	if m := issueURL.FindStringSubmatch(s); m != nil {
		return Subject(fmt.Sprintf("github-issue:%s/%s#%s", m[1], m[2], m[3])), nil
	}
	if m := runURL.FindStringSubmatch(s); m != nil {
		return Subject(fmt.Sprintf("github-run:%s/%s/%s", m[1], m[2], m[3])), nil
	}
	if m := releasesURL.FindStringSubmatch(s); m != nil {
		return Subject(fmt.Sprintf("github-release:%s/%s", m[1], m[2])), nil
	}
	if m := numShort.FindStringSubmatch(s); m != nil {
		pr := true
		if isPR != nil {
			var err error
			if pr, err = isPR(m[1], m[2], m[3]); err != nil {
				return "", fmt.Errorf("is %s a pull request or an issue? %w", s, err)
			}
		}
		if pr {
			return Subject(fmt.Sprintf("github:%s/%s#%s", m[1], m[2], m[3])), nil
		}
		return Subject(fmt.Sprintf("github-issue:%s/%s#%s", m[1], m[2], m[3])), nil
	}
	if m := discussionURL.FindStringSubmatch(s); m != nil {
		return "", fmt.Errorf("kelpie can't watch a single discussion yet; %q covers the repo's notifications, discussions included", m[1]+"/"+m[2])
	}
	m := ownerURL.FindStringSubmatch(s)
	if m == nil {
		m = ownerShort.FindStringSubmatch(s)
	}
	if m == nil {
		return "", fmt.Errorf("cannot parse %q: want a pull request, issue, workflow run or releases URL, owner/repo#number, owner/repo or an org", s)
	}
	if m[2] == "" {
		return Subject("github-inbox:" + m[1]), nil
	}
	return Subject("github-inbox:" + m[1] + "/" + m[2]), nil
}

// LinearSubject accepts an issue identifier (ENG-123), its subject
// (linear:ENG-123) or its URL. Linear matches identifiers in any case, but
// the subject is a map key for watches and snapshots, so it is upper case.
func LinearSubject(s string) (Subject, error) {
	s = strings.TrimSpace(s)
	m := linearURL.FindStringSubmatch(s)
	if m == nil {
		m = linearID.FindStringSubmatch(s)
	}
	if m == nil {
		return "", fmt.Errorf("cannot parse %q: want a Linear issue ID like ENG-123 or its URL", s)
	}
	return Subject("linear:" + strings.ToUpper(m[1])), nil
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
