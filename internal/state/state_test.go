package state

import (
	"testing"

	"github.com/metalmatze/kelpie/internal/core"
)

// The last watch of a subject ending forgets what every source saw of it, so
// a new watch starts from a fresh baseline. Other watches keep it.
func TestDeleteLastBindingForgetsSnapshots(t *testing.T) {
	m := NewMemory()
	rel, pr := core.Subject("github-release:acme/api"), core.Subject("github:acme/api#1")
	m.SetBinding(core.Binding{Subject: rel, Workspace: "w1:p1"})
	m.SetBinding(core.Binding{Subject: rel, Workspace: "w2:p1"})
	m.SetBinding(core.Binding{Subject: pr, Workspace: "w1:p1"})
	for _, source := range []string{"github-release", "other"} {
		m.SetSnapshot(source, rel, map[string]any{"latest": "2026-11-01T00:00:00Z"})
	}
	m.SetSnapshot("github-pr", pr, map[string]any{"state": "OPEN"})

	m.DeleteBinding(rel, "w1:p1")
	if _, ok := m.Snapshot("github-release", rel); !ok {
		t.Fatal("snapshot forgotten while w2:p1 still watches")
	}
	m.DeleteBinding(rel, "w2:p1")
	for _, source := range []string{"github-release", "other"} {
		if _, ok := m.Snapshot(source, rel); ok {
			t.Errorf("%s snapshot kept after the last watch ended", source)
		}
	}
	if _, ok := m.Snapshot("github-pr", pr); !ok {
		t.Error("another subject's snapshot was forgotten")
	}
}
