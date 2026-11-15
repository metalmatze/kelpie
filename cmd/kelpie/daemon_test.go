package main

import (
	"testing"

	"github.com/metalmatze/kelpie/internal/bind"
	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/diff"
	"github.com/metalmatze/kelpie/internal/state"
)

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
