package main

import (
	"regexp"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]string{
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
		"github:acme/api#1":           "",
		"":                            "",
	} {
		got, err := parse(in, nil)
		if got != want || (err == nil) != (want != "") {
			t.Errorf("parse(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// owner/repo#N is whatever GitHub says the number is; URLs say it themselves.
func TestParseAsksGitHub(t *testing.T) {
	issue7 := func(owner, name, number string) (bool, error) { return number != "7", nil }
	for in, want := range map[string]string{
		"acme/api#7":                           "github-issue:acme/api#7",
		"acme/api#421":                         "github:acme/api#421",
		"https://github.com/acme/api/pull/7":   "github:acme/api#7",
		"https://github.com/acme/api/issues/8": "github-issue:acme/api#8",
	} {
		if got, err := parse(in, issue7); got != want || err != nil {
			t.Errorf("parse(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// kelpie routes a target given without a provider name by the targets in
// describe.yaml, and then runs subject: whatever they claim must parse, or a
// ctrl-clicked link reaches this binary only to be refused. They claim less
// than parse takes: an org alone needs kelpie watch github <org>, since any
// provider's ID could be a bare word.
func TestTargetsParse(t *testing.T) {
	var d struct {
		Targets []string `yaml:"targets"`
	}
	if err := yaml.Unmarshal(describe, &d); err != nil {
		t.Fatal(err)
	}
	claims := func(in string) bool {
		for _, p := range d.Targets {
			if regexp.MustCompile(p).MatchString(in) {
				return true
			}
		}
		return false
	}
	for in, claimed := range map[string]bool{
		"https://github.com/acme/api/pull/421/files":  true,
		"https://github.com/acme/api/issues/7":        true,
		"https://github.com/acme/api/actions/runs/42": true,
		"https://github.com/acme/api/releases":        true,
		"https://github.com/acme":                     true,
		"acme/api#7":                                  true,
		"acme/api":                                    true,
		"acme":                                        false,
		"ENG-123":                                     false,
		"linear:ENG-123":                              false,
		"https://linear.app/acme/issue/ENG-123":       false,
		"https://gitlab.com/acme/api":                 false,
	} {
		if got := claims(in); got != claimed {
			t.Errorf("targets claim %q: %v, want %v", in, got, claimed)
		}
		if _, err := parse(in, nil); claimed && err != nil {
			t.Errorf("targets claim %q, but parse refuses it: %v", in, err)
		}
	}
}
