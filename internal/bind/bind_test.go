package bind

import (
	"slices"
	"testing"

	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/host"
	"github.com/metalmatze/kelpie/internal/state"
)

func TestBranchBinding(t *testing.T) {
	st := state.NewMemory()
	b := New(st, true)
	ws := host.Workspace{Host: "herdr", ID: "w1:p1", Repos: []string{"metalmatze/api", "Acme/API"}, Branch: "fix-auth"}
	b.SetWorkspaces("herdr", []host.Workspace{ws, {Host: "herdr", ID: "w2:p1", Repos: []string{"acme/api"}, Branch: "main"}})

	pr := core.Event{Subject: "github:acme/api#421", Bind: map[string]string{"repo": "acme/api", "head_repo": "metalmatze/api", "branch": "fix-auth"}}
	got := b.Resolve(pr)
	if len(got) != 1 || got[0].Workspace.ID != "w1:p1" || !got[0].New {
		t.Fatalf("first resolve: %+v", got)
	}
	if got := b.Resolve(pr); len(got) != 1 || got[0].New {
		t.Fatalf("second resolve should reuse the binding: %+v", got)
	}

	// Checking out another branch moves the PR away from the workspace.
	ws.Branch = "other"
	b.SetWorkspaces("herdr", []host.Workspace{ws})
	if got := b.Resolve(pr); got != nil {
		t.Fatal("bound after branch switch")
	}

	// Two agents on the same branch: don't guess.
	b.SetWorkspaces("herdr", []host.Workspace{
		{Host: "herdr", ID: "w1:p1", Repos: []string{"acme/api"}, Branch: "fix-auth"},
		{Host: "herdr", ID: "w3:p1", Repos: []string{"acme/api"}, Branch: "fix-auth"},
	})
	if got := b.Resolve(pr); got != nil {
		t.Fatal("ambiguous match bound")
	}

	// Closing the workspace ends the binding.
	b.SetWorkspaces("herdr", []host.Workspace{{Host: "herdr", ID: "w1:p1", Repos: []string{"acme/api"}, Branch: "fix-auth"}})
	b.Resolve(pr)
	b.SetWorkspaces("herdr", nil)
	if got := st.Bindings(pr.Subject); got != nil {
		t.Fatal("binding survived its workspace")
	}
}

// Several workspaces watch one subject: each is a target, and closing,
// moving or unbinding one leaves the others alone.
func TestSeveralWorkspaces(t *testing.T) {
	st := state.NewMemory()
	b := New(st, false)
	ws := func(id string) host.Workspace { return host.Workspace{Host: "herdr", ID: id} }
	b.SetWorkspaces("herdr", []host.Workspace{ws("w1:p1"), ws("w2:p1"), ws("w3:p1")})
	inbox := core.Subject("github-inbox:acme/api")
	for _, id := range []string{"w1:p1", "w2:p1", "w3:p1"} {
		b.Bind(core.Binding{Subject: inbox, Workspace: id, Via: "explicit"})
	}
	b.Bind(core.Binding{Subject: "github:acme/api#421", Workspace: "w1:p1", Via: "explicit"})

	targets := func() []string {
		var ids []string
		for _, t := range b.Resolve(core.Event{Subject: inbox}) {
			ids = append(ids, t.Workspace.ID)
		}
		return ids
	}
	if got := targets(); !slices.Equal(got, []string{"w1:p1", "w2:p1", "w3:p1"}) {
		t.Fatalf("targets %v", got)
	}

	b.Remove("w1:p1")
	b.Unbind(inbox, "w2:p1")
	b.Move("w3:p1", ws("w4:p1"))
	if got := targets(); !slices.Equal(got, []string{"w4:p1"}) {
		t.Fatalf("after close, unbind and move: targets %v", got)
	}
	if got := b.AllBindings(); len(got) != 1 || got[0].Workspace != "w4:p1" {
		t.Fatalf("bindings %+v", got)
	}
}

func TestGitHubRepo(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:metalmatze/kelpie.git":     "metalmatze/kelpie",
		"https://github.com/dash0hq/dash0":         "dash0hq/dash0",
		"https://github.com/dash0hq/dash0.git/":    "dash0hq/dash0",
		"ssh://git@github.com/parca-dev/parca.git": "parca-dev/parca",
		"git@gitlab.com:foo/bar.git":               "",
	} {
		got, _ := host.GitHubRepo(url)
		if got != want {
			t.Errorf("GitHubRepo(%q) = %q, want %q", url, got, want)
		}
	}
}

// A host that has not listed its workspaces yet, like the Orca app still
// starting at login, keeps its bindings; once it lists, a missing workspace
// is closed.
func TestAwaitedHostKeepsBindings(t *testing.T) {
	st := state.NewMemory()
	b := New(st, false)
	b.Await("herdr", "orca")
	const ws = "orca:repo::/src/api"
	b.Bind(core.Binding{Subject: "github:acme/api#1", Workspace: ws, Via: "explicit"})
	b.SetWorkspaces("herdr", []host.Workspace{{Host: "herdr", ID: "w1:p1"}})

	got := b.Resolve(core.Event{Subject: "github:acme/api#1"})
	if len(got) != 1 || got[0].Workspace.ID != ws || !b.Unlisted(ws) {
		t.Fatalf("before orca listed: %+v", got)
	}
	b.SetWorkspaces("orca", nil)
	if got := b.Resolve(core.Event{Subject: "github:acme/api#1"}); got != nil || b.Unlisted(ws) {
		t.Fatalf("after orca listed without it: %+v", got)
	}
	if _, ok := b.Binding("github:acme/api#1", ws); ok {
		t.Fatal("binding of a closed workspace kept")
	}
}

// A pane whose ID changes keeps its watches and what kelpie saw of them.
func TestMoveKeepsSnapshots(t *testing.T) {
	st := state.NewMemory()
	b := New(st, false)
	b.SetWorkspaces("herdr", []host.Workspace{{Host: "herdr", ID: "w1:p1"}})
	sub := core.Subject("github-release:acme/api")
	b.Bind(core.Binding{Subject: sub, Workspace: "w1:p1", Via: "explicit"})
	st.SetSnapshot("github-release", sub, map[string]any{"latest": "2026-11-01T00:00:00Z"})

	b.Move("w1:p1", host.Workspace{Host: "herdr", ID: "w1:p2"})
	if _, ok := st.Binding(sub, "w1:p2"); !ok {
		t.Fatal("binding not moved")
	}
	if _, ok := st.Snapshot("github-release", sub); !ok {
		t.Fatal("snapshot forgotten by a move")
	}
}

// A move that keeps the pane ID keeps its watches.
func TestMoveToSameID(t *testing.T) {
	st := state.NewMemory()
	b := New(st, false)
	b.SetWorkspaces("herdr", []host.Workspace{{Host: "herdr", ID: "w1:p1"}})
	sub := core.Subject("github-release:acme/api")
	b.Bind(core.Binding{Subject: sub, Workspace: "w1:p1", Via: "explicit"})

	b.Move("w1:p1", host.Workspace{Host: "herdr", ID: "w1:p1"})
	if _, ok := st.Binding(sub, "w1:p1"); !ok {
		t.Fatal("binding lost by a move to the same ID")
	}
}
