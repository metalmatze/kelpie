package bind

import (
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
	now    func() time.Time

	mu         sync.Mutex
	workspaces map[string]host.Workspace
}

func New(st state.State, branch bool) *Binder {
	return &Binder{state: st, branch: branch, now: time.Now, workspaces: map[string]host.Workspace{}}
}

// SetWorkspaces replaces the known workspaces of one host and ends bindings
// whose workspace is gone. Call it only after a successful host poll, or a
// transient CLI failure unbinds everything.
func (b *Binder) SetWorkspaces(hostName string, list []host.Workspace) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ws := range b.workspaces {
		if ws.Host == hostName {
			delete(b.workspaces, id)
		}
	}
	for _, ws := range list {
		b.workspaces[ws.ID] = ws
	}
	for _, bd := range b.state.AllBindings() {
		if _, ok := b.workspaces[bd.Workspace]; !ok {
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

// Remove forgets a closed workspace and ends its bindings. Other workspaces
// watching the same subjects keep theirs.
func (b *Binder) Remove(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.workspaces, id)
	for _, bd := range b.state.AllBindings() {
		if bd.Workspace == id {
			slog.Info("unbind: workspace closed", "subject", bd.Subject, "workspace", id)
			b.state.DeleteBinding(bd.Subject, id)
		}
	}
}

// Move re-keys a workspace whose ID changed, bindings included. Without it a
// moved herdr pane looks closed and loses its watches.
func (b *Binder) Move(from string, ws host.Workspace) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.workspaces, from)
	b.workspaces[ws.ID] = ws
	for _, bd := range b.state.AllBindings() {
		if bd.Workspace == from {
			b.state.DeleteBinding(bd.Subject, from)
			bd.Workspace = ws.ID
			b.state.SetBinding(bd)
		}
	}
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
		if alive && (bd.Via != "branch" || matchesBranch(ws, ev)) {
			targets = append(targets, Target{Workspace: ws, Binding: bd})
			continue
		}
		b.state.DeleteBinding(ev.Subject, bd.Workspace)
	}
	if len(targets) > 0 || !b.branch {
		return targets
	}

	var candidates []host.Workspace
	for _, ws := range b.workspaces {
		if matchesBranch(ws, ev) {
			candidates = append(candidates, ws)
		}
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

func (b *Binder) Binding(s core.Subject, workspace string) (core.Binding, bool) {
	return b.state.Binding(s, workspace)
}

// Bindings lists the workspaces watching s.
func (b *Binder) Bindings(s core.Subject) []core.Binding { return b.state.Bindings(s) }

func (b *Binder) AllBindings() []core.Binding { return b.state.AllBindings() }

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
