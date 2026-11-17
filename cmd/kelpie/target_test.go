package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alecthomas/kong"

	"github.com/metalmatze/kelpie/internal/config"
	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/provider"
)

// linearProvider is a config registering the real kelpie-linear, built once
// per test binary run.
func linearProvider(t *testing.T) string {
	t.Helper()
	return "providers:\n  linear: {command: ['" + buildLinear(t) + "']}\n"
}

var linearBin string

func buildLinear(t *testing.T) string {
	t.Helper()
	if linearBin != "" {
		return linearBin
	}
	dir, err := os.MkdirTemp("", "kelpie-linear-test")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "kelpie-linear")
	if out, err := exec.Command("go", "build", "-o", bin, "../kelpie-linear").CombinedOutput(); err != nil {
		t.Fatalf("go build kelpie-linear: %v\n%s", err, out)
	}
	linearBin = bin
	return bin
}

func TestMain(m *testing.M) {
	code := m.Run()
	if linearBin != "" {
		os.RemoveAll(filepath.Dir(linearBin))
	}
	os.Exit(code)
}

// acme claims the same bare IDs as linear, as Jira would.
func acmeProvider(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "describe.yaml")
	if err := os.WriteFile(path, []byte("kelpie: 1\nname: acme\ntargets: ['^[A-Z]+-\\d+$']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return "  acme: {command: [sh, -c, \"cat '" + path + "'\"]}\n"
}

func TestResolve(t *testing.T) {
	cfg, err := config.Parse(t.Context(), []byte(linearProvider(t)))
	if err != nil {
		t.Fatal(err)
	}
	github := func(s string) (core.Subject, error) { return core.GitHubSubject(s, nil) }
	for _, tc := range []struct {
		args []string
		want []core.Subject
		err  string
	}{
		{args: []string{"linear", "eng-123"}, want: []core.Subject{"linear:ENG-123"}},
		{args: []string{"linear", "ENG-1", "https://linear.app/acme/issue/ENG-2/x"}, want: []core.Subject{"linear:ENG-1", "linear:ENG-2"}},
		{args: []string{"https://linear.app/acme/issue/eng-123/fix-auth"}, want: []core.Subject{"linear:ENG-123"}},
		{args: []string{"ENG-123"}, want: []core.Subject{"linear:ENG-123"}},
		{args: []string{"linear:eng-123"}, want: []core.Subject{"linear:ENG-123"}},
		{args: []string{"https://github.com/acme/api/pull/7"}, want: []core.Subject{"github:acme/api#7"}},
		{args: []string{"github-inbox:acme"}, want: []core.Subject{"github-inbox:acme"}},
		{args: []string{"linear", "acme/api#7"}, err: "want a Linear issue ID"},
		{args: []string{"jira", "ENG-1"}, err: `no provider "jira"; providers: github (built in), linear`},
	} {
		got, err := resolve(t.Context(), cfg, tc.args, github)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%v: got %v, %v; want error %q", tc.args, got, err, tc.err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%v: got %v, %v; want %v", tc.args, got, err, tc.want)
		}
	}

	// Two providers claiming a bare ID: refused, with the way out. A URL
	// carries its host and still goes through.
	cfg, err = config.Parse(t.Context(), []byte(linearProvider(t)+acmeProvider(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(t.Context(), cfg, []string{"ENG-123"}, github); err == nil || !strings.Contains(err.Error(), "ENG-123 could be acme or linear; name the provider") {
		t.Errorf("ambiguous: %v", err)
	}
	if got, err := resolve(t.Context(), cfg, []string{"https://linear.app/acme/issue/ENG-1"}, github); err != nil || !slices.Equal(got, []core.Subject{"linear:ENG-1"}) {
		t.Errorf("URL: %v %v", got, err)
	}
	if got, err := subjectOf(t.Context(), cfg, "https://linear.app/acme/issue/ENG-1"); err != nil || got != "linear:ENG-1" {
		t.Errorf("--subject: %v %v", got, err)
	}
	if got, err := subjectOf(t.Context(), cfg, "acme/api#7"); err != nil || got != "github:acme/api#7" {
		t.Errorf("--subject of GitHub: %v %v", got, err)
	}

	// Without the provider, its link says so instead of failing as GitHub input.
	plain, err := config.Parse(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(t.Context(), plain, []string{"https://linear.app/acme/issue/ENG-1"}, github); err == nil || !strings.Contains(err.Error(), "no provider claims https://linear.app/acme/issue/ENG-1; no provider is registered") {
		t.Errorf("unclaimed link: %v", err)
	}
	if _, err := resolve(t.Context(), plain, []string{"linear", "ENG-1"}, github); err == nil || !strings.Contains(err.Error(), `no provider "linear"`) {
		t.Errorf("unregistered provider: %v", err)
	}
}

// The generic watch and unwatch sit beside the built-in github ones.
func TestWatchCommands(t *testing.T) {
	for args, want := range map[string]string{
		"watch github acme/api#1":                "watch github",
		"watch github":                           "watch github",
		"watch linear ENG-1 --workspace w1":      "watch target <provider target>",
		"watch https://linear.app/a/issue/ENG-1": "watch target <provider target>",
		"unwatch linear:ENG-1":                   "unwatch subject <subject or provider target>",
		"unwatch linear ENG-1":                   "unwatch subject <subject or provider target>",
		"unwatch github acme/api#1":              "unwatch github",
	} {
		var cli CLI
		p, err := kong.New(&cli, kong.Vars{"config": "x"})
		if err != nil {
			t.Fatal(err)
		}
		kctx, err := p.Parse(strings.Fields(args))
		if err != nil {
			t.Errorf("%s: %v", args, err)
			continue
		}
		if got := strings.TrimSuffix(kctx.Command(), " <target>"); got != want {
			t.Errorf("%s: command %q, want %q", args, got, want)
		}
		if args == "watch linear ENG-1 --workspace w1" && (!slices.Equal(cli.Watch.Target.Target, []string{"linear", "ENG-1"}) || cli.Watch.Target.Workspace != "w1") {
			t.Errorf("%s: parsed %+v", args, cli.Watch.Target)
		}
	}
}

// verify runs through kong with a context; an empty stdin fails before any
// network call.
func TestLinearVerifyWithoutKey(t *testing.T) {
	_, err := provider.Verify(t.Context(), []string{buildLinear(t)}, []string{"verify"}, "")
	if err == nil || !strings.Contains(err.Error(), "kelpie-linear verify: no API key on stdin") {
		t.Fatalf("got %v", err)
	}
}
