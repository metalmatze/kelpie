package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metalmatze/kelpie/internal/bind"
	"github.com/metalmatze/kelpie/internal/config"
	"github.com/metalmatze/kelpie/internal/control"
	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/diff"
	"github.com/metalmatze/kelpie/internal/host"
	"github.com/metalmatze/kelpie/internal/host/external"
	"github.com/metalmatze/kelpie/internal/route"
	"github.com/metalmatze/kelpie/internal/schedule"
	"github.com/metalmatze/kelpie/internal/state"
)

// A list of current state, like DESIGN's prom-local-errors, can miss a
// subject for one poll; forgetting it would make its return silent.
func TestListSourceKeepsSnapshotsByDefault(t *testing.T) {
	d := &daemon{runners: map[string]*runner{}}
	if err := d.buildRunners([]config.Source{{Name: "errors", Adapter: "cli", Run: []string{"true"}, Map: "."}}, state.NewMemory()); err != nil {
		t.Fatal(err)
	}
	if d.runners["errors"].d.ForgetMissing {
		t.Fatal("a list source forgets without forget_missing")
	}
}

func TestForgetMissing(t *testing.T) {
	cfg, err := config.Parse(t.Context(), []byte(providers(t)))
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{runners: map[string]*runner{}}
	if err := d.buildRunners(cfg.Sources, state.NewMemory()); err != nil {
		t.Fatal(err)
	}
	for name, r := range d.runners {
		if want := name == "linear"; r.d.ForgetMissing != want {
			t.Errorf("%s: ForgetMissing %v, want %v", name, r.d.ForgetMissing, want)
		}
	}
	if _, ok := d.runners["linear"]; !ok {
		t.Error("no linear source from the provider")
	}

	for _, tc := range []struct {
		name string
		src  config.Source
		err  string
	}{
		{"for_each", config.Source{Adapter: "cli", ForEach: &config.ForEach{Subjects: "x:{id}"}, ForgetMissing: true}, "forget_missing is for a list source"},
		{"streaming", config.Source{Adapter: "cli-watch", ForgetMissing: true}, "forget_missing needs adapter cli"},
		{"page on for_each", config.Source{Adapter: "cli", ForEach: &config.ForEach{Subjects: "x:{id}"}, Page: 50}, "page is the page size of a list source"},
	} {
		tc.src.Name, tc.src.Run, tc.src.Map = tc.name, []string{"true"}, "."
		d := &daemon{runners: map[string]*runner{}}
		if err := d.buildRunners([]config.Source{tc.src}, state.NewMemory()); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.err)
		}
	}
}

// A watch loop polls once more after its last watch ended. That poll must not
// store a snapshot, or a later watch of the subject would diff against it and
// report what changed while nobody watched.
func TestWatchObserverIgnoresUnwatchedSubject(t *testing.T) {
	st := state.NewMemory()
	df, err := diff.New("github-release", nil, st)
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{binder: bind.New(st, false)}
	sub := core.Subject("github-release:acme/api")
	observe := d.watchObserver(&runner{d: df}, sub)
	obs := []core.Observation{{Subject: sub, Fields: map[string]any{"latest": "2026-11-01T00:00:00Z"}}}

	observe(obs)
	if _, ok := st.Snapshot("github-release", sub); ok {
		t.Fatal("stored a snapshot of a subject nobody watches")
	}

	d.binder.Bind(core.Binding{Subject: sub, Workspace: "w1:p1", Via: "explicit"})
	observe(obs)
	if _, ok := st.Snapshot("github-release", sub); !ok {
		t.Fatal("did not store a snapshot of a watched subject")
	}
}

// Watching a subject again while the loop of its earlier watch still sleeps
// takes the baseline from the watch's own poll. Otherwise nothing would take
// it until that loop's next tick, an hour for releases, and a release
// published in between would be the silent baseline.
func TestRewatchTakesBaselineAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	page, polls := filepath.Join(dir, "page.json"), filepath.Join(dir, "polls")
	publish := func(latest string) {
		if err := os.WriteFile(page, []byte(`{"latest": "`+latest+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	publish("2026-11-01T00:00:00Z")

	st := state.NewMemory()
	d := &daemon{ctx: ctx, runners: map[string]*runner{}, badges: map[core.Subject]string{}, shown: map[string]string{}, binder: bind.New(st, false)}
	var err error
	if d.sched, err = schedule.New(filepath.Join(dir, "reminders.json"), 0, func(core.Reminder, time.Duration) {}); err != nil {
		t.Fatal(err)
	}
	src := config.Source{
		Name: "releases", Adapter: "cli", Every: config.Duration{Duration: time.Hour},
		ForEach:     &config.ForEach{Subjects: "rel:{id}"},
		Run:         []string{"sh", "-c", `cat "$0"; echo >> "$1"`, page, polls},
		Map:         "{fields: .}",
		Transitions: []config.Transition{{Field: "latest", Increased: true, Kind: "release.new"}},
	}
	if err := d.buildRunners([]config.Source{src}, st); err != nil {
		t.Fatal(err)
	}
	d.binder.SetWorkspaces("herdr", []host.Workspace{{Host: "herdr", ID: "w1:p1"}})
	req := control.WatchRequest{Subject: "rel:acme", Workspace: "w1:p1"}

	if _, err := d.Watch(ctx, req); err != nil {
		t.Fatal(err)
	}
	// The watch's poll, then the loop's first; after that the loop sleeps.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, _ := os.ReadFile(polls); len(b) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watch loop did not poll")
		}
	}
	time.Sleep(100 * time.Millisecond) // let that poll reach the observer
	if err := d.Unwatch(ctx, req); err != nil {
		t.Fatal(err)
	}

	publish("2026-11-10T00:00:00Z")
	if _, err := d.Watch(ctx, req); err != nil {
		t.Fatal(err)
	}
	snap, ok := st.Snapshot("releases", "rel:acme")
	if !ok || snap["latest"] != "2026-11-10T00:00:00Z" {
		t.Fatalf("baseline after re-watch: %v %v, want the release published before it", snap, ok)
	}
}

// An external host without local git paths works end to end: Watch binds the
// prefixed workspace, transitions deliver through the binary, and a Resync
// dropping that workspace unbinds only it without touching other hosts.
func TestExternalHostEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dir := t.TempDir()
	wsFile := filepath.Join(dir, "workspaces.json")
	delivered := filepath.Join(dir, "delivered.txt")
	if err := os.WriteFile(wsFile, []byte(`[{"id":"s1","state":"idle"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	host.Register("exttest", false)
	h := external.New("exttest", []string{"sh", "-c", `
case "$1" in
  workspaces) cat "` + wsFile + `" ;;
  deliver)    [ "$2" = s1 ] && cat > "` + delivered + `" ;;
  *)          exit 1 ;;
esac`, "exttest"}, time.Hour, nil)

	st := state.NewMemory()
	b := bind.New(st, false)
	b.Await("exttest", "herdr")
	r := route.New(ctx, config.Delivery{
		Debounce:       config.Duration{Duration: 10 * time.Millisecond},
		UrgentDebounce: config.Duration{Duration: 10 * time.Millisecond},
		Retry:          config.Duration{Duration: 50 * time.Millisecond},
		Prefix:         "[kelpie]",
	}, route.NewRules(config.Rules{}, nil), b, []host.Host{h})

	d := &daemon{
		ctx:      ctx,
		binder:   b,
		router:   r,
		hosts:    []host.Host{h},
		runners:  map[string]*runner{},
		lastSeen: map[string]string{},
		badges:   map[core.Subject]string{},
		shown:    map[string]string{},
	}
	var err error
	if d.sched, err = schedule.New(filepath.Join(dir, "reminders.json"), 0, func(core.Reminder, time.Duration) {}); err != nil {
		t.Fatal(err)
	}

	d.refreshWorkspaces()
	b.SetWorkspaces("herdr", []host.Workspace{{Host: "herdr", ID: "w1:p1"}})
	b.Bind(core.Binding{Subject: "github:acme/api#7", Workspace: "exttest:s1", Via: "explicit"})
	b.Bind(core.Binding{Subject: "github:acme/api#8", Workspace: "w1:p1", Via: "explicit"})

	r.Route(core.Event{
		Subject: "github:acme/api#7",
		Kind:    "checks.failed",
		Summary: "1 failed: test",
		Urgent:  true,
		Fetch:   []string{"gh", "pr", "checks", "7", "-R", "acme/api"},
	})
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if body, err := os.ReadFile(delivered); err == nil && len(body) > 0 {
			if !strings.Contains(string(body), "github:acme/api#7 checks.failed: 1 failed: test") {
				t.Fatalf("unexpected delivery:\n%s", string(body))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("external host did not receive delivery")
		}
	}

	// Closing the external workspace unbinds only exttest:s1, leaving herdr's
	// watch intact.
	if err := os.WriteFile(wsFile, []byte(`[]`), 0o644); err != nil {
		t.Fatal(err)
	}
	d.refreshWorkspaces()
	if _, ok := b.Binding("github:acme/api#7", "exttest:s1"); ok {
		t.Fatal("closed external workspace kept its binding")
	}
	if _, ok := b.Binding("github:acme/api#8", "w1:p1"); !ok {
		t.Fatal("external host resync dropped herdr's binding")
	}
}

// When an enabled external host fails describe at daemon startup, the daemon
// still awaits it so incoming events keep its saved bindings and stay queued
// instead of being dropped. Stuck notifications fall back to another host
// without panicking.
func TestSkippedExternalHostKeepsBindings(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dir := t.TempDir()
	notified := filepath.Join(dir, "notified.json")
	healthyScript := filepath.Join(dir, "healthy.sh")
	script := `#!/bin/sh
case "$1" in
  describe)   printf 'kelpie: 1\nname: healthyext\ncapabilities: [notify]\n' ;;
  workspaces) printf '[]\n' ;;
  notify)     cat > "` + notified + `.tmp" && mv "` + notified + `.tmp" "` + notified + `" ;;
  *)          exit 1 ;;
esac
`
	if err := os.WriteFile(healthyScript, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfgYAML := "hosts:\n" +
		"  herdr: {enabled: false}\n" +
		"  healthyext: {command: ['" + healthyScript + "']}\n" +
		"  brokenext: {command: [sh, -c, 'echo down >&2; exit 1']}\n" +
		"  disabledext: {enabled: false, command: ['true']}\n"
	cfg, err := config.Parse(ctx, []byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Skipped) != 1 || !strings.Contains(cfg.Skipped[0], "host brokenext skipped") {
		t.Fatalf("skipped = %v, want brokenext", cfg.Skipped)
	}

	st := state.NewMemory()
	d := &daemon{
		ctx:      ctx,
		binder:   bind.New(st, false),
		runners:  map[string]*runner{},
		lastSeen: map[string]string{},
		badges:   map[core.Subject]string{},
		shown:    map[string]string{},
	}
	if err := d.initHosts(cfg, false); err != nil {
		t.Fatal(err)
	}
	if !d.binder.Unlisted("brokenext:s1") {
		t.Fatal("skipped enabled external host is not awaited")
	}
	if d.binder.Unlisted("disabledext:s1") {
		t.Fatal("disabled external host should not be awaited")
	}

	d.binder.Bind(core.Binding{Subject: "github:acme/api#10", Workspace: "brokenext:s1", Via: "explicit"})
	d.binder.Bind(core.Binding{Subject: "github:acme/api#11", Workspace: "disabledext:s1", Via: "explicit"})
	// Another host resyncing must leave both registered external hosts alone.
	d.binder.SetWorkspaces("herdr", nil)
	if _, ok := d.binder.Binding("github:acme/api#10", "brokenext:s1"); !ok {
		t.Fatal("herdr resync dropped skipped external host binding")
	}

	d.router = route.New(ctx, config.Delivery{
		Debounce:       config.Duration{Duration: 10 * time.Millisecond},
		UrgentDebounce: config.Duration{Duration: 10 * time.Millisecond},
		Retry:          config.Duration{Duration: 25 * time.Millisecond},
		Notify:         config.Notify{Stuck: config.Duration{Duration: 20 * time.Millisecond}},
		Prefix:         "[kelpie]",
	}, route.NewRules(config.Rules{}, nil), d.binder, d.hosts)

	d.router.Route(core.Event{
		Subject: "github:acme/api#10",
		Kind:    "checks.failed",
		Summary: "1 failed: test",
		Urgent:  true,
	})
	// Wait until the stuck notification reaches healthyext, proving the batch
	// retried while unlisted and fallback notification handled the nil host.
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if body, err := os.ReadFile(notified); err == nil && len(body) > 0 {
			if !strings.Contains(string(body), "brokenext:s1") {
				t.Fatalf("unexpected fallback notification: %s", string(body))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stuck notification did not fall back to healthyext")
		}
	}
	if _, ok := d.binder.Binding("github:acme/api#10", "brokenext:s1"); !ok {
		t.Fatal("event dropped binding of skipped enabled external host")
	}
	if got := d.router.Queued()["brokenext:s1"]; got != 1 {
		t.Fatalf("queued for brokenext:s1 = %d, want 1", got)
	}

	// A disabled external host is not awaited, so an event ends its binding.
	d.router.Route(core.Event{Subject: "github:acme/api#11", Kind: "checks.failed", Summary: "1 failed", Urgent: true})
	if _, ok := d.binder.Binding("github:acme/api#11", "disabledext:s1"); ok {
		t.Fatal("disabled external host kept its binding after an event")
	}
}
