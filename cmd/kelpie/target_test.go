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
	"github.com/metalmatze/kelpie/internal/host/external"
	"github.com/metalmatze/kelpie/internal/provider"
)

// providers is a config registering the real kelpie-github and
// kelpie-linear, built once per test binary run.
func providers(t *testing.T) string {
	t.Helper()
	return "providers:\n  github: {command: ['" + build(t, "kelpie-github") + "']}\n  linear: {command: ['" + build(t, "kelpie-linear") + "']}\n"
}

var built = map[string]string{}

func build(t *testing.T, name string) string {
	t.Helper()
	if bin, ok := built[name]; ok {
		return bin
	}
	dir, err := os.MkdirTemp("", name+"-test")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, name)
	if out, err := exec.Command("go", "build", "-o", bin, "../"+name).CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", name, err, out)
	}
	built[name] = bin
	return bin
}

func TestMain(m *testing.M) {
	code := m.Run()
	for _, bin := range built {
		os.RemoveAll(filepath.Dir(bin))
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
	cfg, err := config.Parse(t.Context(), []byte(providers(t)))
	if err != nil {
		t.Fatal(err)
	}
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
		{args: []string{"github", "https://github.com/acme/api/issues/7", "acme/api"}, want: []core.Subject{"github-issue:acme/api#7", "github-inbox:acme/api"}},
		{args: []string{"acme/api"}, want: []core.Subject{"github-inbox:acme/api"}},
		{args: []string{"github-inbox:acme"}, want: []core.Subject{"github-inbox:acme"}},
		{args: []string{"https://github.com/acme/api/discussions/3"}, want: []core.Subject{"github-discussion:acme/api#3"}},
		{args: []string{"acme"}, err: "no provider claims acme; name its provider, as in kelpie watch <provider> acme"},
		{args: []string{"linear", "acme/api#7"}, err: "want a Linear issue ID"},
		{args: []string{"jira", "ENG-1"}, err: `no provider "jira"; providers: github, linear`},
	} {
		got, err := resolve(t.Context(), cfg, tc.args)
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
	cfg, err = config.Parse(t.Context(), []byte(providers(t)+acmeProvider(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(t.Context(), cfg, []string{"ENG-123"}); err == nil || !strings.Contains(err.Error(), "ENG-123 could be acme or linear; name the provider") {
		t.Errorf("ambiguous: %v", err)
	}
	if got, err := resolve(t.Context(), cfg, []string{"https://linear.app/acme/issue/ENG-1"}); err != nil || !slices.Equal(got, []core.Subject{"linear:ENG-1"}) {
		t.Errorf("URL: %v %v", got, err)
	}
	if got, err := subjectOf(t.Context(), cfg, "https://linear.app/acme/issue/ENG-1"); err != nil || got != "linear:ENG-1" {
		t.Errorf("--subject: %v %v", got, err)
	}
	if got, err := subjectOf(t.Context(), cfg, "https://github.com/acme/api/pull/7"); err != nil || got != "github:acme/api#7" {
		t.Errorf("--subject of GitHub: %v %v", got, err)
	}

	// Without the provider, its link says so.
	plain, err := config.Parse(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(t.Context(), plain, []string{"https://linear.app/acme/issue/ENG-1"}); err == nil || !strings.Contains(err.Error(), "no provider claims https://linear.app/acme/issue/ENG-1;") || !strings.Contains(err.Error(), "no provider loaded") {
		t.Errorf("unclaimed link: %v", err)
	}
	if _, err := resolve(t.Context(), plain, []string{"linear", "ENG-1"}); err == nil || !strings.Contains(err.Error(), `no provider "linear"`) {
		t.Errorf("unregistered provider: %v", err)
	}
}

// github is a provider like any other: kelpie watch github <target>, as the
// skill has always written it, now reaches it through the generic watch.
func TestWatchCommands(t *testing.T) {
	for args, want := range map[string]string{
		"watch github acme/api#1":                "watch <provider target>",
		"watch github":                           "watch <provider target>",
		"watch linear ENG-1 --workspace w1":      "watch <provider target>",
		"watch https://linear.app/a/issue/ENG-1": "watch <provider target>",
		"unwatch linear:ENG-1":                   "unwatch subject <subject or provider target>",
		"unwatch linear ENG-1":                   "unwatch subject <subject or provider target>",
		"unwatch github acme/api#1":              "unwatch subject <subject or provider target>",
		"unwatch subject github:acme/api#1":      "unwatch subject <subject or provider target>",
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
		if got := kctx.Command(); got != want {
			t.Errorf("%s: command %q, want %q", args, got, want)
		}
		if args == "watch linear ENG-1 --workspace w1" && (!slices.Equal(cli.Watch.Target, []string{"linear", "ENG-1"}) || cli.Watch.Workspace != "w1") {
			t.Errorf("%s: parsed %+v", args, cli.Watch)
		}
	}
}

// verify runs through kong with a context; an empty stdin fails before any
// network call.
func TestLinearVerifyWithoutKey(t *testing.T) {
	_, err := provider.Who(t.Context(), []string{build(t, "kelpie-linear")}, []string{"verify"}, "")
	if err == nil || !strings.Contains(err.Error(), "kelpie-linear verify: no API key on stdin") {
		t.Fatalf("got %v", err)
	}
}

func TestCallerWorkspaceExternalHost(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("HC_TERMINAL_ID", "")
	t.Setenv("ORCA_WORKTREE_ID", "")
	t.Setenv("ORCA_AGENT_SESSION_ID", "")
	t.Setenv("TERM_PROGRAM", "")

	ext := []config.DescribedHost{{
		Description: external.Description{
			Name:        "custom",
			TermProgram: "customterm",
			CallerEnv:   []string{"TEST_CUSTOM_WS"},
		},
		Command: []string{"sh", "-c", `[ "$1" = caller ] && printf '%s\n' "$TEST_CUSTOM_WS"`, "custom"},
	}}

	// Outside every host: error names configured external hosts.
	t.Setenv("TEST_CUSTOM_WS", "")
	if _, err := callerWorkspace(t.Context(), "", fixedExternalHosts(ext)); err == nil || !strings.Contains(err.Error(), "external host (custom)") {
		t.Fatalf("outside all hosts: %v", err)
	}

	// Explicit --workspace always wins.
	if got, err := callerWorkspace(t.Context(), "custom:manual", fixedExternalHosts(ext)); err != nil || got != "custom:manual" {
		t.Fatalf("explicit flag: %q %v", got, err)
	}

	// External host alone matches when its caller env is set.
	t.Setenv("TEST_CUSTOM_WS", "s7")
	if got, err := callerWorkspace(t.Context(), "", fixedExternalHosts(ext)); err != nil || got != "custom:s7" {
		t.Fatalf("external caller: %q %v", got, err)
	}

	// When nested inside herdr, TERM_PROGRAM picks the innermost host.
	t.Setenv("HERDR_PANE_ID", "w1:p2")
	t.Setenv("TERM_PROGRAM", "customterm")
	if got, err := callerWorkspace(t.Context(), "", fixedExternalHosts(ext)); err != nil || got != "custom:s7" {
		t.Fatalf("TERM_PROGRAM=customterm: %q %v", got, err)
	}
	t.Setenv("TERM_PROGRAM", "herdr")
	if got, err := callerWorkspace(t.Context(), "", fixedExternalHosts(ext)); err != nil || got != "w1:p2" {
		t.Fatalf("TERM_PROGRAM=herdr: %q %v", got, err)
	}
}

// Commands that do not otherwise read config (kelpie remind, kelpie unwatch
// <subject>) must keep working in built-in hosts when the config file is broken.
func TestCallerWorkspaceBrokenConfig(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("HC_TERMINAL_ID", "")
	t.Setenv("ORCA_WORKTREE_ID", "")
	t.Setenv("ORCA_AGENT_SESSION_ID", "")
	t.Setenv("TERM_PROGRAM", "")

	broken := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(broken, []byte("not: [valid: yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &Globals{Config: broken}

	// Matching TERM_PROGRAM or empty TERM_PROGRAM in a built-in host skips config
	// loading altogether.
	called := false
	failIfCalled := func() ([]config.DescribedHost, error) {
		called = true
		return nil, os.ErrInvalid
	}
	t.Setenv("HERDR_PANE_ID", "w1:p2")
	t.Setenv("TERM_PROGRAM", "herdr")
	if got, err := callerWorkspace(t.Context(), "", failIfCalled); err != nil || got != "w1:p2" || called {
		t.Fatalf("herdr TERM_PROGRAM=herdr: got %q %v called=%v", got, err, called)
	}
	t.Setenv("TERM_PROGRAM", "")
	if got, err := callerWorkspace(t.Context(), "", failIfCalled); err != nil || got != "w1:p2" || called {
		t.Fatalf("herdr TERM_PROGRAM='': got %q %v called=%v", got, err, called)
	}

	// Non-matching TERM_PROGRAM (like hc inside ghostty) still falls back to the
	// built-in host when the config file fails to parse.
	t.Setenv("TERM_PROGRAM", "ghostty")
	if got, err := callerWorkspace(t.Context(), "", loadExternalHosts(t.Context(), g)); err != nil || got != "w1:p2" {
		t.Fatalf("herdr with broken config: got %q %v", got, err)
	}
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("HC_TERMINAL_ID", "t7")
	if got, err := callerWorkspace(t.Context(), "", loadExternalHosts(t.Context(), g)); err != nil || got != "hc:t7" {
		t.Fatalf("hc with broken config: got %q %v", got, err)
	}

	// Outside every built-in host, the broken config error surfaces.
	t.Setenv("HC_TERMINAL_ID", "")
	if _, err := callerWorkspace(t.Context(), "", loadExternalHosts(t.Context(), g)); err == nil || !strings.Contains(err.Error(), broken) {
		t.Fatalf("outside built-in hosts with broken config: got %v", err)
	}
}
