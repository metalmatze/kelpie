package main

import (
	"strings"
	"testing"

	"github.com/metalmatze/kelpie/internal/config"
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
