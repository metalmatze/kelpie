package main

import (
	"regexp"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]string{
		"ENG-123":        "linear:ENG-123",
		" eng-123 ":      "linear:ENG-123",
		"linear:ENG-123": "linear:ENG-123",
		"Linear:eng-7":   "linear:ENG-7",
		"https://linear.app/acme/issue/ENG-123/fix-auth-on-login": "linear:ENG-123",
		"https://linear.app/acme/issue/ENG-123":                   "linear:ENG-123",
		"https://linear.app/acme/issue/ENG-123#comment-1a2b":      "linear:ENG-123",
		"https://linear.app/acme/issue/eng-123/":                  "linear:ENG-123",
		"https://linear.app/acme/project/kelpie-1a2b3c":           "",
		"https://linear.app/acme/issue/ENG-123x":                  "",
		"ENG":                                                     "",
		"ENG-":                                                    "",
		"123":                                                     "",
		"acme/api#7":                                              "",
		"":                                                        "",
		"inbox":                                                   "linear-inbox:me",
		"linear-inbox:me":                                         "linear-inbox:me",
		"https://linear.app/acme/inbox":                           "linear-inbox:me",
		"https://linear.app/acme/inbox/ENG-1":                     "linear-inbox:me",
		"https://linear.app/inbox":                                "",
	} {
		got, err := parse(in)
		if got != want || (err == nil) != (want != "") {
			t.Errorf("parse(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// kelpie routes a target given without a provider name by the targets in
// describe.yaml, and then runs subject: the two must accept the same inputs,
// or a ctrl-clicked link reaches this binary only to be refused.
func TestTargetsMatchParse(t *testing.T) {
	var d struct {
		Targets []string `yaml:"targets"`
	}
	if err := yaml.Unmarshal(describe, &d); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		"ENG-123", "eng-123", "linear:ENG-123", "Linear:eng-7",
		"https://linear.app/acme/issue/ENG-123/fix-auth-on-login",
		"https://linear.app/acme/issue/ENG-123#comment-1a2b",
		"https://linear.app/acme/project/kelpie-1a2b3c", "https://linear.app/acme/issue/ENG-123x",
		"ENG", "123", "acme/api#7", "https://github.com/acme/api/pull/1",
		"linear-inbox:me", "https://linear.app/acme/inbox",
	} {
		claimed := false
		for _, p := range d.Targets {
			if regexp.MustCompile(p).MatchString(in) {
				claimed = true
			}
		}
		_, err := parse(in)
		if claimed != (err == nil) {
			t.Errorf("%q: claimed by targets %v, parsed %v", in, claimed, err == nil)
		}
	}
}
