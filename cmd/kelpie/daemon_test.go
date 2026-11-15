package main

import (
	"strings"
	"testing"

	"github.com/metalmatze/kelpie/internal/bind"
	"github.com/metalmatze/kelpie/internal/config"
	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/diff"
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
	cfg, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{runners: map[string]*runner{}}
	if err := d.buildRunners(cfg.Sources, state.NewMemory()); err != nil {
		t.Fatal(err)
	}
	for name, r := range d.runners {
		if want := name == "linear"; r.d.ForgetMissing != want {
			t.Errorf("built-in %s: ForgetMissing %v, want %v", name, r.d.ForgetMissing, want)
		}
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
