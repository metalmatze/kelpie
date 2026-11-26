package bind

import (
	"slices"
	"strings"
	"testing"
	"time"

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
	// An agent's pane and a shell pane: the agent's. Two agents: still nobody.
	b.SetWorkspaces("herdr", []host.Workspace{
		{Host: "herdr", ID: "w1:p1", Repos: []string{"acme/api"}, Branch: "fix-auth"},
		{Host: "herdr", ID: "w3:p1", Repos: []string{"acme/api"}, Branch: "fix-auth", Agent: true},
	})
	if got := b.Resolve(pr); len(got) != 1 || got[0].Workspace.ID != "w3:p1" || !got[0].New {
		t.Fatalf("agent pane not preferred: %+v", got)
	}
	b.Unbind(pr.Subject, "w3:p1")
	b.SetWorkspaces("herdr", []host.Workspace{
		{Host: "herdr", ID: "w1:p1", Repos: []string{"acme/api"}, Branch: "fix-auth", Agent: true},
		{Host: "herdr", ID: "w3:p1", Repos: []string{"acme/api"}, Branch: "fix-auth", Agent: true},
		{Host: "herdr", ID: "w4:p1", Repos: []string{"acme/api"}, Branch: "fix-auth"},
	})
	if got := b.Resolve(pr); got != nil {
		t.Fatalf("two agents bound: %+v", got)
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

// linearBranch is the branch pattern kelpie-linear describes.
func linearBranch(t *testing.T) core.BranchPattern {
	p, err := core.NewBranchPattern("linear:{id}", `(?i)(?:^|[^a-z0-9]){id}(?:$|[^0-9])`)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A provider's subject binds to the one workspace whose branch names it, in
// any repo, and only while that provider's branch patterns are in use.
func TestProviderBranchBinding(t *testing.T) {
	st := state.NewMemory()
	b := New(st, false)
	b.SetWorkspaces("herdr", []host.Workspace{
		{Host: "herdr", ID: "w1:p1", Branch: "matthias/eng-123-fix-auth"},
		{Host: "herdr", ID: "w2:p1", Branch: "eng-1234-other"},
		{Host: "herdr", ID: "w3:p1", Branch: "main"},
	})
	ev := core.Event{Subject: "linear:ENG-123"}
	if got := b.Resolve(ev); got != nil {
		t.Fatalf("bound without branch patterns: %+v", got)
	}
	b.Branches = []core.BranchPattern{linearBranch(t)}
	if got := b.Resolve(ev); len(got) != 1 || got[0].Workspace.ID != "w1:p1" || !got[0].New || got[0].Binding.Via != "branch" {
		t.Fatalf("resolve: %+v", got)
	}
	// The next event keeps the binding rather than making it again, which
	// would repeat the note on why the workspace hears about it.
	if got := b.Resolve(ev); len(got) != 1 || got[0].Workspace.ID != "w1:p1" || got[0].New {
		t.Fatalf("second resolve should reuse the binding: %+v", got)
	}
	// PRs still need binders.branch.
	if got := b.Resolve(core.Event{Subject: "github:acme/api#1", Bind: map[string]string{"repo": "acme/api", "branch": "main"}}); got != nil {
		t.Fatalf("PR bound by a provider's branch pattern: %+v", got)
	}
	// The binding follows the branch: after a checkout of another branch the
	// workspace no longer gets the issue.
	b.SetWorkspaces("herdr", []host.Workspace{{Host: "herdr", ID: "w1:p1", Branch: "main"}})
	if got := b.Resolve(ev); got != nil {
		t.Fatalf("bound after a checkout of another branch: %+v", got)
	}
	// The same tie-break as for PRs: the agent's pane, not the shell next to it.
	b.SetWorkspaces("herdr", []host.Workspace{
		{Host: "herdr", ID: "w1:p1", Branch: "matthias/eng-123-fix-auth"},
		{Host: "herdr", ID: "w1:p2", Branch: "matthias/eng-123-fix-auth", Agent: true},
	})
	if got := b.Resolve(ev); len(got) != 1 || got[0].Workspace.ID != "w1:p2" {
		t.Fatalf("agent pane not preferred for a provider's subject: %+v", got)
	}
	// hc cannot tell whether its terminal runs an agent, so herdr's agent
	// does not beat it: ambiguous, until one of them leaves the branch.
	b.Unbind(ev.Subject, "w1:p2")
	b.SetWorkspaces("hc", []host.Workspace{{Host: "hc", ID: "hc:t1", Branch: "matthias/eng-123-fix-auth"}})
	if got := b.Resolve(ev); got != nil {
		t.Fatalf("herdr agent won over an hc terminal: %+v", got)
	}
	b.SetWorkspaces("herdr", nil)
	if got := b.Resolve(ev); len(got) != 1 || got[0].Workspace.ID != "hc:t1" {
		t.Fatalf("hc terminal alone on the branch: %+v", got)
	}
	b.SetWorkspaces("hc", nil)
	// binders.branch alone does not bind a provider's subjects.
	b2 := New(state.NewMemory(), true)
	b2.SetWorkspaces("herdr", []host.Workspace{{Host: "herdr", ID: "w1:p1", Branch: "matthias/eng-123-fix-auth"}})
	if got := b2.Resolve(ev); got != nil {
		t.Fatalf("bound by binders.branch without the provider's patterns: %+v", got)
	}
}

// An Orca worktree and the panes of its agents share a branch. A pane is a
// child of the worktree, so a branch binding picks the worktree and not
// nothing.
func TestBranchBindingIgnoresPanes(t *testing.T) {
	b := New(state.NewMemory(), true)
	b.Parent = paneParent
	b.SetWorkspaces("orca", []host.Workspace{
		{Host: "orca", ID: "orca:r::/src", Repos: []string{"acme/api"}, Branch: "fix-auth", Agent: true},
		{Host: "orca", ID: "orca:pane:t1:leaf@r::/src", Repos: []string{"acme/api"}, Branch: "fix-auth"},
		{Host: "orca", ID: "orca:pane:t2:leaf@r::/src", Repos: []string{"acme/api"}, Branch: "fix-auth"},
	})
	pr := core.Event{Subject: "github:acme/api#421", Bind: map[string]string{"repo": "acme/api", "head_repo": "acme/api", "branch": "fix-auth"}}
	if got := b.Resolve(pr); len(got) != 1 || got[0].Workspace.ID != "orca:r::/src" {
		t.Fatalf("Resolve = %+v; want the worktree", got)
	}
}

// With no agent detected in the worktree, its panes, which share its
// branch, are no candidates, so the worktree is the one and the event waits
// there.
func TestBranchBindingWorktreeWithoutAgent(t *testing.T) {
	b := New(state.NewMemory(), true)
	b.Parent = paneParent
	b.SetWorkspaces("orca", []host.Workspace{
		{Host: "orca", ID: "orca:r::/src", Repos: []string{"acme/api"}, Branch: "fix-auth"},
		{Host: "orca", ID: "orca:pane:t1:leaf@r::/src", Repos: []string{"acme/api"}, Branch: "fix-auth"},
		{Host: "orca", ID: "orca:pane:t2:leaf@r::/src", Repos: []string{"acme/api"}, Branch: "fix-auth"},
	})
	pr := core.Event{Subject: "github:acme/api#421", Bind: map[string]string{"repo": "acme/api", "head_repo": "acme/api", "branch": "fix-auth"}}
	if got := b.Resolve(pr); len(got) != 1 || got[0].Workspace.ID != "orca:r::/src" {
		t.Fatalf("Resolve = %+v; want the worktree", got)
	}
}

// paneParent nests workspaces like Orca: the parent of
// "orca:pane:<key>@<worktree>" is "orca:<worktree>".
func paneParent(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, "orca:pane:")
	if !ok {
		return "", false
	}
	_, wt, ok := strings.Cut(rest, "@")
	return "orca:" + wt, ok
}

const (
	paneWT = "orca:r::/src"
	paneA  = "orca:pane:t1:leaf@r::/src"
	paneB  = "orca:pane:t2:leaf@r::/src"
)

// paneBinder knows worktree paneWT and the listed panes of it.
func paneBinder(panes ...string) *Binder {
	b := New(state.NewMemory(), false)
	b.Parent = paneParent
	list := []host.Workspace{{Host: "orca", ID: paneWT, Agent: true}}
	for _, p := range panes {
		list = append(list, host.Workspace{Host: "orca", ID: p})
	}
	b.SetWorkspaces("orca", list)
	return b
}

func workspacesOf(b *Binder, s core.Subject) []string {
	var out []string
	for _, bd := range b.Bindings(s) {
		out = append(out, bd.Workspace)
	}
	slices.Sort(out)
	return out
}

// A pane replaces its worktree's binding and keeps its time; the worktree
// leaves a subject its pane watches to the pane, and binds one no pane
// watches itself.
func TestWatchKeepsParentOrChildren(t *testing.T) {
	b := paneBinder(paneA)
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	b.Bind(core.Binding{Subject: "rel:acme", Workspace: paneWT, Via: "branch", Since: since})

	if got := b.Watch("rel:acme", paneA); got != paneA {
		t.Errorf("Watch from the pane bound %s; want the pane", got)
	}
	if got := workspacesOf(b, "rel:acme"); !slices.Equal(got, []string{paneA}) {
		t.Errorf("watch is on %v; want the pane alone", got)
	}
	if bd, _ := b.Binding("rel:acme", paneA); !bd.Since.Equal(since) || bd.Via != "explicit" {
		t.Errorf("binding = %+v; want explicit since the worktree's %v", bd, since)
	}

	if got := b.Watch("rel:acme", paneWT); got != paneA {
		t.Errorf("Watch from the worktree bound %s; want the pane that watches it", got)
	}
	if got := workspacesOf(b, "rel:acme"); !slices.Equal(got, []string{paneA}) {
		t.Errorf("watch is on %v; want the pane alone", got)
	}

	if got := b.Watch("rel:other", paneWT); got != paneWT {
		t.Errorf("Watch from the worktree bound %s; want the worktree", got)
	}
}

func TestUnwatchFromPaneEndsWorktreeBinding(t *testing.T) {
	b := paneBinder(paneA)
	b.Bind(core.Binding{Subject: "rel:acme", Workspace: paneWT, Via: "explicit", Since: time.Now()})
	if got, err := b.Unwatch("rel:acme", paneA); err != nil || got != paneWT {
		t.Fatalf("Unwatch = %s, %v; want the worktree's binding", got, err)
	}
	if got := workspacesOf(b, "rel:acme"); len(got) != 0 {
		t.Errorf("watch is on %v; want it ended", got)
	}
	if _, err := b.Unwatch("rel:acme", paneA); err == nil {
		t.Error("unwatch with nothing watching succeeded")
	}
}

// Unwatch from the worktree, like one typed in a shell tab, ends the watch
// of the one pane that holds it, the one watch from there named; with two
// panes holding it, it names them and ends neither.
func TestUnwatchFromWorktreeEndsPaneBinding(t *testing.T) {
	b := paneBinder(paneA)
	b.Bind(core.Binding{Subject: "rel:acme", Workspace: paneA, Via: "explicit", Since: time.Now()})
	if got, err := b.Unwatch("rel:acme", paneWT); err != nil || got != paneA {
		t.Fatalf("Unwatch = %s, %v; want the pane's binding", got, err)
	}
	if got := workspacesOf(b, "rel:acme"); len(got) != 0 {
		t.Errorf("watch is on %v; want it ended", got)
	}
	if _, err := b.Unwatch("rel:acme", paneWT); err == nil {
		t.Error("unwatch with nothing watching succeeded")
	}

	b = paneBinder(paneA, paneB)
	for _, p := range []string{paneA, paneB} {
		b.Bind(core.Binding{Subject: "rel:acme", Workspace: p, Via: "explicit", Since: time.Now()})
	}
	_, err := b.Unwatch("rel:acme", paneWT)
	if err == nil || !strings.Contains(err.Error(), paneA) || !strings.Contains(err.Error(), paneB) {
		t.Errorf("unwatch with two panes watching = %v; want an error naming both", err)
	}
	if got := workspacesOf(b, "rel:acme"); !slices.Equal(got, []string{paneA, paneB}) {
		t.Errorf("watch is on %v; want both panes kept", got)
	}
}

// A pane that is gone hands its bindings to its worktree, keeping their
// time; with the worktree gone too they end, and another host's stay.
func TestSetWorkspacesFallsBackToParent(t *testing.T) {
	b := paneBinder(paneA)
	b.SetWorkspaces("herdr", []host.Workspace{{Host: "herdr", ID: "w1:p1"}})
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	b.Bind(core.Binding{Subject: "rel:acme", Workspace: paneA, Via: "explicit", Since: since})
	b.Bind(core.Binding{Subject: "rel:other", Workspace: "w1:p1", Via: "explicit"})

	b.SetWorkspaces("orca", []host.Workspace{{Host: "orca", ID: paneWT}})
	if got := workspacesOf(b, "rel:acme"); !slices.Equal(got, []string{paneWT}) {
		t.Fatalf("watch of the gone pane is on %v; want its worktree", got)
	}
	if bd, _ := b.Binding("rel:acme", paneWT); !bd.Since.Equal(since) {
		t.Errorf("moved binding since %v; want %v", bd.Since, since)
	}

	b.SetWorkspaces("orca", nil)
	if got := workspacesOf(b, "rel:acme"); len(got) != 0 {
		t.Errorf("watch is on %v; want it ended", got)
	}
	if got := workspacesOf(b, "rel:other"); !slices.Equal(got, []string{"w1:p1"}) {
		t.Errorf("other host's watch is on %v; want it untouched", got)
	}
}

// When a pane closes and a sibling pane of its worktree watches the same
// subject, the closed pane's watch ends instead of moving: the worktree
// would deliver every update a second time.
func TestSetWorkspacesFallBackSkipsWatchedSibling(t *testing.T) {
	b := paneBinder(paneA, paneB)
	b.Bind(core.Binding{Subject: "rel:acme", Workspace: paneA, Via: "explicit", Since: time.Now()})
	b.Bind(core.Binding{Subject: "rel:acme", Workspace: paneB, Via: "explicit", Since: time.Now()})
	b.Bind(core.Binding{Subject: "rel:solo", Workspace: paneA, Via: "explicit", Since: time.Now()})

	b.SetWorkspaces("orca", []host.Workspace{{Host: "orca", ID: paneWT}, {Host: "orca", ID: paneB}}) // paneA closed
	if got := workspacesOf(b, "rel:acme"); !slices.Equal(got, []string{paneB}) {
		t.Errorf("shared watch is on %v; want the sibling alone", got)
	}
	if got := workspacesOf(b, "rel:solo"); !slices.Equal(got, []string{paneWT}) {
		t.Errorf("sole watch is on %v; want the worktree", got)
	}
}

// The heir of a pane that is not listed is its listed worktree; a listed
// pane, a pane of an unlisted worktree, and a worktree have none.
func TestHeir(t *testing.T) {
	b := paneBinder(paneA)
	if ws, ok := b.Heir(paneB); !ok || ws.ID != paneWT {
		t.Errorf("Heir(unlisted pane) = %q, %v; want %s", ws.ID, ok, paneWT)
	}
	for _, id := range []string{paneA, "orca:pane:t9:leaf@r::/nowhere", "orca:r::/nowhere", paneWT} {
		if ws, ok := b.Heir(id); ok {
			t.Errorf("Heir(%s) = %s; want none", id, ws.ID)
		}
	}
}

// Without Parent, no workspace nests: a host other than Orca behaves as
// before, and Watch and Unwatch act on the workspace given.
func TestNoParent(t *testing.T) {
	b := New(state.NewMemory(), false)
	b.SetWorkspaces("herdr", []host.Workspace{{Host: "herdr", ID: "w1:p1"}, {Host: "herdr", ID: "w1:p2"}})
	b.Bind(core.Binding{Subject: "rel:acme", Workspace: "w1:p2", Via: "explicit", Since: time.Now()})
	if got := b.Watch("rel:acme", "w1:p1"); got != "w1:p1" {
		t.Errorf("Watch bound %s; want w1:p1", got)
	}
	if got := workspacesOf(b, "rel:acme"); !slices.Equal(got, []string{"w1:p1", "w1:p2"}) {
		t.Errorf("watch is on %v; want both panes", got)
	}
	if _, err := b.Unwatch("rel:other", "w1:p1"); err == nil {
		t.Error("unwatch with nothing watching succeeded")
	}
	b.SetWorkspaces("herdr", []host.Workspace{{Host: "herdr", ID: "w1:p1"}})
	if got := workspacesOf(b, "rel:acme"); !slices.Equal(got, []string{"w1:p1"}) {
		t.Errorf("watch is on %v; want the closed pane's ended", got)
	}
}
