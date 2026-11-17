package core

import "testing"

func TestGitHubSubject(t *testing.T) {
	for in, want := range map[string]Subject{
		"https://github.com/acme/api/pull/421/files":             "github:acme/api#421",
		"https://github.com/acme/api/pull/421":                   "github:acme/api#421",
		"acme/api#421":                                           "github:acme/api#421",
		"acme/api":                                               "github-inbox:acme/api",
		"acme":                                                   "github-inbox:acme",
		" acme/api.go ":                                          "github-inbox:acme/api.go",
		"https://github.com/acme/api":                            "github-inbox:acme/api",
		"https://github.com/acme/api/":                           "github-inbox:acme/api",
		"https://github.com/acme/api/issues":                     "github-inbox:acme/api",
		"https://github.com/acme/api/issues/7":                   "github-issue:acme/api#7",
		"https://github.com/acme/api/issues/7#issuecomment-1":    "github-issue:acme/api#7",
		"https://github.com/acme/api/discussions/7":              "",
		"https://github.com/acme/api/actions/runs/42":            "github-run:acme/api/42",
		"https://github.com/acme/api/actions/runs/42/job/7":      "github-run:acme/api/42",
		"https://github.com/acme/api/actions/runs/42/attempts/2": "github-run:acme/api/42",
		"https://github.com/acme/api/actions":                    "github-inbox:acme/api",
		"https://github.com/acme/api/releases":                   "github-release:acme/api",
		"https://github.com/acme/api/releases/":                  "github-release:acme/api",
		"https://github.com/acme/api/releases/tag/v1.2.0":        "github-release:acme/api",
		"https://github.com/acme/api/releases/latest":            "github-release:acme/api",
		"https://github.com/acme/api/releases?page=2":            "github-release:acme/api",
		"https://github.com/acme/api/releasesx":                  "github-inbox:acme/api",
		"https://github.com/acme":                                "github-inbox:acme",
		"https://github.com/acme?tab=repositories":               "github-inbox:acme",
		"acme/api/x":                  "",
		"https://gitlab.com/acme/api": "",
		"":                            "",
	} {
		got, err := GitHubSubject(in, nil)
		if got != want || (err == nil) != (want != "") {
			t.Errorf("GitHubSubject(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// owner/repo#N is whatever GitHub says the number is; URLs say it themselves.
func TestGitHubSubjectAsksGitHub(t *testing.T) {
	issue7 := func(owner, name, number string) (bool, error) { return number != "7", nil }
	for in, want := range map[string]Subject{
		"acme/api#7":                           "github-issue:acme/api#7",
		"acme/api#421":                         "github:acme/api#421",
		"https://github.com/acme/api/pull/7":   "github:acme/api#7",
		"https://github.com/acme/api/issues/8": "github-issue:acme/api#8",
	} {
		if got, err := GitHubSubject(in, issue7); got != want || err != nil {
			t.Errorf("GitHubSubject(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// Subjects of any provider pass through; anything else is GitHub input.
// Other providers' targets never get here: kelpie routes them first.
func TestParseSubject(t *testing.T) {
	for in, want := range map[string]Subject{
		"github:acme/api#421":                            "github:acme/api#421",
		"github-inbox:acme":                              "github-inbox:acme",
		"github-issue:acme/api#7":                        "github-issue:acme/api#7",
		"github-release:acme/api":                        "github-release:acme/api",
		"https://github.com/acme/api/issues/7":           "github-issue:acme/api#7",
		" slack:C0123ABCD/1761811234.567890 ":            "slack:C0123ABCD/1761811234.567890",
		"linear:ENG-123":                                 "linear:ENG-123",
		"https://linear.app/acme/issue/ENG-123/fix-auth": "",
		"https://github.com/acme/api/pull/421":           "github:acme/api#421",
		"acme/api":                                       "github-inbox:acme/api",
		"https://acme.slack.com/archives/C0":             "",
	} {
		got, err := ParseSubject(in)
		if got != want || (err == nil) != (want != "") {
			t.Errorf("ParseSubject(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// The org pattern must not also match repo subjects, or a repo watch would
// start the org's loop with owner "acme/api".
func TestPatternStopsAtSlash(t *testing.T) {
	org, err := NewPattern("github-inbox:{owner}")
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := org.Match("github-inbox:acme/api"); ok {
		t.Fatalf("org pattern matched a repo subject: %v", f)
	}
	if f, ok := org.Match("github-inbox:acme"); !ok || f["owner"] != "acme" {
		t.Fatalf("org pattern: %v %v", f, ok)
	}
	rel, err := NewPattern("github-release:{owner}/{name}")
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := rel.Match("github-release:acme"); ok {
		t.Fatalf("release pattern matched an owner alone: %v", f)
	}
	pr, err := NewPattern("github:{owner}/{name}#{number}")
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := pr.Match("github:acme/api.go#421"); !ok || f["owner"] != "acme" || f["name"] != "api.go" || f["number"] != "421" {
		t.Fatalf("pr pattern: %v %v", f, ok)
	}
}

func TestIsSubject(t *testing.T) {
	for in, want := range map[string]bool{
		"linear:ENG-1": true, "github-inbox:acme": true, "github:acme/api#1": true,
		"https://linear.app/acme/issue/ENG-1": false, "ENG-1": false, "acme/api": false, "": false,
	} {
		if got := IsSubject(in); got != want {
			t.Errorf("IsSubject(%q) = %v, want %v", in, got, want)
		}
	}
}

// A branch pattern names a subject as a whole word in any case, as Linear's
// branch names do, and is about only the subjects of its pattern.
func TestBranchPattern(t *testing.T) {
	p, err := NewBranchPattern("linear:{id}", `(?i)(?:^|[^a-z0-9]){id}(?:$|[^0-9])`)
	if err != nil {
		t.Fatal(err)
	}
	for branch, want := range map[string]bool{
		"eng-123": true, "ENG-123-x": true, "u/eng-123": true, "feature_eng-123_x": true,
		"eng-1234": false, "xeng-123": false, "eng-12": false, "": false,
	} {
		if applies, names := p.Match("linear:ENG-123", branch); !applies || names != want {
			t.Errorf("branch %q names linear:ENG-123: %v, want %v", branch, names, want)
		}
	}
	// A value is quoted: ENG.1 must not match ENGx1.
	if _, names := p.Match("linear:ENG.1", "engx1"); names {
		t.Error("value not quoted")
	}
	if applies, _ := p.Match("github:acme/api#1", "eng-123"); applies || p.About("github:acme/api#1") || !p.About("linear:ENG-1") {
		t.Error("a linear pattern is about a github subject")
	}
	// A {2} that is no placeholder of the subject stays a quantifier.
	q, err := NewBranchPattern("x:{id}", `^a{2}-{id}$`)
	if err != nil {
		t.Fatal(err)
	}
	if _, names := q.Match("x:7", "aa-7"); !names {
		t.Error("quantifier replaced")
	}
	for _, bad := range [][2]string{{"x:{id}", `({id}`}, {"x:fixed", `x`}, {"x:{id", `x`}} {
		if _, err := NewBranchPattern(bad[0], bad[1]); err == nil {
			t.Errorf("NewBranchPattern(%q, %q) accepted", bad[0], bad[1])
		}
	}
}
