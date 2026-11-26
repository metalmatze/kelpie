package bind

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/host"
	"github.com/metalmatze/kelpie/internal/state"
)

// Binder resolves an event's subject to the workspace that owns the work.
type Binder struct {
	state  state.State
	branch bool
	// Branches are the providers' branch patterns, of those whose branch
	// binding is on. A subject one of them is about binds by it alone, not
	// by binders.branch, which is for PRs.
	Branches []core.BranchPattern
	// Parent names the workspace that holds workspace id, like an Orca
	// agent pane's worktree, from the ID alone (see host.Parent). Nil when
	// no host nests its workspaces.
	Parent func(id string) (string, bool)
	now    func() time.Time

	mu         sync.Mutex
	workspaces map[string]host.Workspace
	// awaited are the running hosts that have not listed their workspaces
	// yet. Until one does, a workspace of it that is not known is not
	// closed either: the host may still be starting, like the Orca app
	// after a login.
	awaited map[string]bool
}

func New(st state.State, branch bool) *Binder {
	return &Binder{state: st, branch: branch, now: time.Now, workspaces: map[string]host.Workspace{}, awaited: map[string]bool{}}
}

// Await takes every host the daemon runs, before any of them lists. A host
// left out counts as listed, so its unknown workspaces count as closed.
func (b *Binder) Await(hosts ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, h := range hosts {
		b.awaited[h] = true
	}
}

// SetWorkspaces replaces the known workspaces of one host and ends that
// host's bindings whose workspace is gone, except that those of a child
// whose parent is listed move to the parent (see fallBack). Call it only
// after a successful host poll, or a transient CLI failure unbinds
// everything. Other hosts' bindings stay even when their workspaces are not
// known yet: that host may be down or not listed yet at startup.
func (b *Binder) SetWorkspaces(hostName string, list []host.Workspace) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.awaited, hostName)
	for id, ws := range b.workspaces {
		if ws.Host == hostName {
			delete(b.workspaces, id)
		}
	}
	for _, ws := range list {
		b.workspaces[ws.ID] = ws
	}
	b.fallBack(hostName)
	for _, bd := range b.state.AllBindings() {
		if _, ok := b.workspaces[bd.Workspace]; !ok && host.HostOf(bd.Workspace) == hostName {
			slog.Info("unbind: workspace closed", "subject", bd.Subject, "workspace", bd.Workspace)
			b.state.DeleteBinding(bd.Subject, bd.Workspace)
		}
	}
}

// Upsert adds or replaces one workspace and returns the one it replaced.
func (b *Binder) Upsert(ws host.Workspace) (prev host.Workspace, existed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	prev, existed = b.workspaces[ws.ID]
	b.workspaces[ws.ID] = ws
	return prev, existed
}

// Remove forgets a closed workspace and ends its bindings, except that
// those of a child whose parent is listed move to the parent, as in
// SetWorkspaces. Other workspaces watching the same subjects keep theirs.
func (b *Binder) Remove(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.workspaces, id)
	b.fallBackFrom(id)
	for _, bd := range b.state.AllBindings() {
		if bd.Workspace == id {
			slog.Info("unbind: workspace closed", "subject", bd.Subject, "workspace", id)
			b.state.DeleteBinding(bd.Subject, id)
		}
	}
}

// fallBack moves the bindings of the gone children of hostName to their
// parent, when the parent is listed, instead of ending them. It goes by
// what is bound, not by what the host listed last: a child can be gone
// since before the daemon started. A subject that a listed sibling with the
// same parent watches ends with the child instead: on the parent too, every
// update would arrive twice. Called with b.mu held.
func (b *Binder) fallBack(hostName string) {
	gone := map[string]bool{}
	for _, bd := range b.state.AllBindings() {
		if _, ok := b.workspaces[bd.Workspace]; !ok && host.HostOf(bd.Workspace) == hostName {
			gone[bd.Workspace] = true
		}
	}
	for _, id := range slices.Sorted(maps.Keys(gone)) {
		b.fallBackFrom(id)
	}
}

// fallBackFrom moves the bindings of gone workspace id to its listed
// parent, if it has one, but ends those a listed sibling with the same
// parent watches. Called with b.mu held.
func (b *Binder) fallBackFrom(id string) {
	to, ok := b.heir(id)
	if !ok {
		return
	}
	for _, bd := range b.state.AllBindings() {
		if bd.Workspace != id {
			continue
		}
		if sibling, ok := b.siblingWatching(bd.Subject, id, to.ID); ok {
			b.state.DeleteBinding(bd.Subject, id)
			slog.Info("watch ends with its pane: a sibling watches it", "subject", bd.Subject, "from", id, "sibling", sibling)
			continue
		}
		moved := bd
		moved.Workspace = to.ID
		b.state.SetBinding(moved)
		b.state.DeleteBinding(bd.Subject, id)
		slog.Info("watch falls back", "subject", bd.Subject, "from", id, "to", to.ID)
	}
}

// siblingWatching is a listed workspace other than id, with parent
// parent, that watches s. Called with b.mu held.
func (b *Binder) siblingWatching(s core.Subject, id, parent string) (string, bool) {
	for _, bd := range b.state.Bindings(s) {
		if _, ok := b.workspaces[bd.Workspace]; !ok || bd.Workspace == id {
			continue
		}
		if p, _ := b.parent(bd.Workspace); p == parent {
			return bd.Workspace, true
		}
	}
	return "", false
}

// Heir is the listed parent of workspace id when id itself is not listed:
// a child that is gone, or one its host does not list, like an Orca
// terminal without a tab. It takes over what id would hold.
func (b *Binder) Heir(id string) (host.Workspace, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.heir(id)
}

// heir is Heir with b.mu held.
func (b *Binder) heir(id string) (host.Workspace, bool) {
	if _, ok := b.workspaces[id]; ok {
		return host.Workspace{}, false
	}
	to, ok := b.parent(id)
	if !ok {
		return host.Workspace{}, false
	}
	ws, ok := b.workspaces[to]
	return ws, ok
}

// Move re-keys a workspace whose ID changed, bindings included. Without it a
// moved herdr pane looks closed and loses its watches.
func (b *Binder) Move(from string, ws host.Workspace) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.workspaces, from)
	b.workspaces[ws.ID] = ws
	if from == ws.ID {
		return
	}
	for _, bd := range b.state.AllBindings() {
		if bd.Workspace == from {
			moved := bd
			moved.Workspace = ws.ID
			b.state.SetBinding(moved)
			b.state.DeleteBinding(bd.Subject, from)
		}
	}
}

// Unlisted reports whether the host of workspace id runs but has not
// listed its workspaces yet, so whether it exists is not known.
func (b *Binder) Unlisted(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.awaited[host.HostOf(id)]
}

func (b *Binder) Workspace(id string) (host.Workspace, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ws, ok := b.workspaces[id]
	return ws, ok
}

func (b *Binder) Workspaces() []host.Workspace {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.SortedFunc(maps.Values(b.workspaces), func(a, c host.Workspace) int {
		return strings.Compare(a.ID, c.ID)
	})
}

// Target is one workspace an event goes to. New reports a binding made just
// now, which the first delivered message mentions so the agent knows why it
// hears about this subject.
type Target struct {
	Workspace host.Workspace
	Binding   core.Binding
	New       bool
}

// Resolve returns every workspace that watches ev's subject. Each gets its
// own delivery.
func (b *Binder) Resolve(ev core.Event) []Target {
	b.mu.Lock()
	defer b.mu.Unlock()

	var targets []Target
	for _, bd := range b.state.Bindings(ev.Subject) {
		ws, alive := b.workspaces[bd.Workspace]
		// A branch binding follows the branch: after a checkout of another
		// branch the workspace no longer owns this PR.
		if alive && (bd.Via != "branch" || b.followsBranch(ws, ev)) {
			targets = append(targets, Target{Workspace: ws, Binding: bd})
			continue
		}
		if !alive && b.awaited[host.HostOf(bd.Workspace)] {
			targets = append(targets, Target{Workspace: unlisted(bd.Workspace), Binding: bd})
			continue
		}
		b.state.DeleteBinding(ev.Subject, bd.Workspace)
	}
	patterns := b.patterns(ev.Subject)
	if len(targets) > 0 || len(patterns) == 0 && !b.branch {
		return targets
	}

	var candidates []host.Workspace
	for _, ws := range b.workspaces {
		// A child, like an Orca agent pane, shares its parent's branch.
		// Branch binding picks the parent, which delivers to its best
		// agent; with the children as candidates too it would be ambiguous.
		if _, ok := b.parent(ws.ID); ok {
			continue
		}
		if len(patterns) > 0 && namedByBranch(patterns, ev.Subject, ws.Branch) || len(patterns) == 0 && matchesBranch(ws, ev) {
			candidates = append(candidates, ws)
		}
	}
	// A worktree often has a shell pane next to its agent. Two agents on
	// one branch stay ambiguous: either could own the work. So does a
	// candidate whose host cannot tell, as it may run the other agent.
	blind := slices.ContainsFunc(candidates, func(ws host.Workspace) bool { return !host.ReportsAgent(ws.Host) })
	if agents := slices.DeleteFunc(slices.Clone(candidates), func(ws host.Workspace) bool { return !ws.Agent }); len(agents) == 1 && !blind {
		candidates = agents
	}
	switch len(candidates) {
	case 1:
		bd := core.Binding{Subject: ev.Subject, Workspace: candidates[0].ID, Via: "branch", Since: b.now()}
		b.state.SetBinding(bd)
		return []Target{{Workspace: candidates[0], Binding: bd, New: true}}
	case 0:
	default:
		// TODO: confirmation queue. Until then, guessing would wake the wrong agent.
		ids := make([]string, len(candidates))
		for i, c := range candidates {
			ids[i] = c.ID
		}
		slices.Sort(ids)
		slog.Warn("ambiguous branch binding, not delivering", "subject", ev.Subject, "workspaces", ids)
	}
	return nil
}

func (b *Binder) Bind(bd core.Binding) { b.state.SetBinding(bd) }

// Unbind ends one workspace's binding of s.
func (b *Binder) Unbind(s core.Subject, workspace string) { b.state.DeleteBinding(s, workspace) }

// Watch binds s to workspace id on request and returns the workspace the
// binding is on. It keeps s bound to a parent or to its children, never
// both: both bound, the parent's best agent, most likely that child's,
// would get every update twice. So a parent leaves s to a listed child that
// watches it, and a child takes over its parent's binding, a branch binding
// or a watch made before the child was a workspace, and keeps its time.
// Watching again keeps the original time.
func (b *Binder) Watch(s core.Subject, id string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if children := b.childrenWatching(s, id); len(children) > 0 {
		slog.Info("watch stays with the pane", "subject", s, "workspace", id, "pane", children[0])
		id = children[0]
	}
	from, _ := b.parent(id)
	since := b.now()
	if old, ok := b.state.Binding(s, from); ok {
		since = old.Since
	}
	if _, ok := b.state.Binding(s, id); !ok {
		b.state.SetBinding(core.Binding{Subject: s, Workspace: id, Via: "explicit", Since: since})
	}
	// After the SetBinding: ending the last binding of a subject forgets
	// its snapshots.
	if from != "" && from != id {
		if _, ok := b.state.Binding(s, from); ok {
			b.state.DeleteBinding(s, from)
			slog.Info("watch moved to pane", "subject", s, "from", from, "to", id)
		}
	}
	return id
}

// Unwatch ends the binding of s that a request from workspace id means and
// returns the workspace it was on: id's own. Else, from a child, its
// parent's: a watch made before the child was a workspace, or one that fell
// back. Else, from a parent, like a shell tab in an Orca worktree, the one
// listed child that watches s, which a watch from the parent names as the
// one the updates go to. With several such children it ends none.
func (b *Binder) Unwatch(s core.Subject, id string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	target, err := b.unwatchTarget(s, id)
	if err != nil {
		return "", err
	}
	b.state.DeleteBinding(s, target)
	return target, nil
}

// unwatchTarget is the workspace whose binding Unwatch ends. Called with
// b.mu held.
func (b *Binder) unwatchTarget(s core.Subject, id string) (string, error) {
	if _, ok := b.state.Binding(s, id); ok {
		return id, nil
	}
	if from, ok := b.parent(id); ok {
		if _, ok := b.state.Binding(s, from); ok {
			return from, nil
		}
	}
	children := b.childrenWatching(s, id)
	switch len(children) {
	case 0:
		return "", fmt.Errorf("pane %s is not watching %s", id, s)
	case 1:
		return children[0], nil
	}
	return "", fmt.Errorf("panes %s watch %s; end one with --workspace", strings.Join(children, ", "), s)
}

// childrenWatching are the listed children of workspace id that watch s.
// Called with b.mu held.
func (b *Binder) childrenWatching(s core.Subject, id string) []string {
	var out []string
	for _, bd := range b.state.Bindings(s) {
		if p, ok := b.parent(bd.Workspace); !ok || p != id {
			continue
		}
		if _, ok := b.workspaces[bd.Workspace]; ok {
			out = append(out, bd.Workspace)
		}
	}
	return out
}

// parent is b.Parent, or none without it.
func (b *Binder) parent(id string) (string, bool) {
	if b.Parent == nil {
		return "", false
	}
	return b.Parent(id)
}

func (b *Binder) Binding(s core.Subject, workspace string) (core.Binding, bool) {
	return b.state.Binding(s, workspace)
}

// Bindings lists the workspaces watching s.
func (b *Binder) Bindings(s core.Subject) []core.Binding { return b.state.Bindings(s) }

func (b *Binder) AllBindings() []core.Binding { return b.state.AllBindings() }

// followsBranch tells whether ws is still on the branch that bound ev's
// subject to it.
func (b *Binder) followsBranch(ws host.Workspace, ev core.Event) bool {
	if patterns := b.patterns(ev.Subject); len(patterns) > 0 {
		return namedByBranch(patterns, ev.Subject, ws.Branch)
	}
	return matchesBranch(ws, ev)
}

// patterns are the branch patterns about s. Called with b.mu held.
func (b *Binder) patterns(s core.Subject) []core.BranchPattern {
	var ps []core.BranchPattern
	for _, p := range b.Branches {
		if p.About(s) {
			ps = append(ps, p)
		}
	}
	return ps
}

func namedByBranch(patterns []core.BranchPattern, s core.Subject, branch string) bool {
	return slices.ContainsFunc(patterns, func(p core.BranchPattern) bool {
		_, names := p.Match(s, branch)
		return names
	})
}

// matchesBranch binds a PR by its head branch, in one of the workspace's
// repos, fork remotes included.
func matchesBranch(ws host.Workspace, ev core.Event) bool {
	branch := ev.Bind["branch"]
	if branch == "" || ws.Branch != branch {
		return false
	}
	for _, r := range ws.Repos {
		if strings.EqualFold(r, ev.Bind["repo"]) || strings.EqualFold(r, ev.Bind["head_repo"]) {
			return true
		}
	}
	return false
}

// unlisted stands in for a workspace whose host has not listed it yet. The
// router queues for it and delivers once the host knows it.
func unlisted(id string) host.Workspace {
	return host.Workspace{Host: host.HostOf(id), ID: id}
}
