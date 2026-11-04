package core

import "testing"

func TestGitHubSubject(t *testing.T) {
	for in, want := range map[string]Subject{
		"https://github.com/acme/api/pull/421/files": "github:acme/api#421",
		"https://github.com/acme/api/pull/421":       "github:acme/api#421",
		"acme/api#421":                               "github:acme/api#421",
		"acme/api":                                   "github-inbox:acme/api",
		"acme":                                       "github-inbox:acme",
		" acme/api.go ":                              "github-inbox:acme/api.go",
		"https://github.com/acme/api":                "github-inbox:acme/api",
		"https://github.com/acme/api/":               "github-inbox:acme/api",
		"https://github.com/acme/api/issues":         "github-inbox:acme/api",
		"https://github.com/acme/api/issues/7":       "",
		"https://github.com/acme/api/discussions/7":  "",
		"https://github.com/acme":                    "github-inbox:acme",
		"https://github.com/acme?tab=repositories":   "github-inbox:acme",
		"acme/api/x":                                 "",
		"https://gitlab.com/acme/api":                "",
		"":                                           "",
	} {
		got, err := GitHubSubject(in)
		if got != want || (err == nil) != (want != "") {
			t.Errorf("GitHubSubject(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// Subjects of any provider pass through; anything else is GitHub input.
func TestParseSubject(t *testing.T) {
	for in, want := range map[string]Subject{
		"github:acme/api#421":                  "github:acme/api#421",
		"github-inbox:acme":                    "github-inbox:acme",
		" slack:C0123ABCD/1761811234.567890 ":  "slack:C0123ABCD/1761811234.567890",
		"linear:ENG-123":                       "linear:ENG-123",
		"https://github.com/acme/api/pull/421": "github:acme/api#421",
		"acme/api":                             "github-inbox:acme/api",
		"https://acme.slack.com/archives/C0":   "",
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
	pr, err := NewPattern("github:{owner}/{name}#{number}")
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := pr.Match("github:acme/api.go#421"); !ok || f["owner"] != "acme" || f["name"] != "api.go" || f["number"] != "421" {
		t.Fatalf("pr pattern: %v %v", f, ok)
	}
}
