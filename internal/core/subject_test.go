package core

import "testing"

// Subjects of any provider pass through. URLs and IDs are the providers' to
// parse, which kelpie has done before a subject gets here.
func TestParseSubject(t *testing.T) {
	for in, want := range map[string]Subject{
		"github:acme/api#421":                            "github:acme/api#421",
		"github-inbox:acme":                              "github-inbox:acme",
		"github-issue:acme/api#7":                        "github-issue:acme/api#7",
		"github-release:acme/api":                        "github-release:acme/api",
		" slack:C0123ABCD/1761811234.567890 ":            "slack:C0123ABCD/1761811234.567890",
		"linear:ENG-123":                                 "linear:ENG-123",
		"https://github.com/acme/api/pull/421":           "",
		"https://linear.app/acme/issue/ENG-123/fix-auth": "",
		"acme/api": "",
		"":         "",
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
