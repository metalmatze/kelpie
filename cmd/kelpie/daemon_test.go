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
	cfg, err := config.Parse(t.Context(), []byte(linearProvider(t)))
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
