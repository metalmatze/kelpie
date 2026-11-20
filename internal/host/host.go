package host

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
)

type Workspace struct {
	Host   string   // herdr | orca | hc | external host name
	ID     string   // unique across hosts, see HostOf
	Repos  []string // owner/name of every GitHub remote, so fork PRs match too
	Branch string
	Path   string
	// Agent breaks a tie between workspaces on one branch, so a shell pane
	// next to the agent does not silence branch binding. It means something
	// only where ReportsAgent(Host).
	Agent bool
}

var (
	regMu    sync.RWMutex
	prefixed = []string{"orca", "hc"}

	// agentHosts set Workspace.Agent. Anywhere else, like hc, whose terminals say
	// nothing about what runs in them, a false Agent is not known rather than a
	// shell, and must not let another host's agent win the branch. A new host is
	// left out until it reports agents, so it keeps ambiguity instead.
	agentHosts = []string{"herdr", "orca"}
)

// Register records a prefixed host name and whether it reports Workspace.Agent.
// Built-in hosts (herdr, orca, hc) are pre-registered; external hosts call this
// from config so HostOf and ReportsAgent recognize their workspace IDs.
func Register(name string, reportsAgent bool) {
	if name == "" || name == "herdr" {
		return
	}
	regMu.Lock()
	defer regMu.Unlock()
	if !slices.Contains(prefixed, name) {
		prefixed = append(prefixed, name)
	}
	if reportsAgent {
		if !slices.Contains(agentHosts, name) {
			agentHosts = append(agentHosts, name)
		}
	} else {
		agentHosts = slices.DeleteFunc(agentHosts, func(h string) bool { return h == name })
	}
}

func ReportsAgent(host string) bool {
	regMu.RLock()
	defer regMu.RUnlock()
	return slices.Contains(agentHosts, host)
}

// HostOf names the host a workspace ID belongs to. IDs are unique across
// hosts so bindings, reminders and delivery queues can key on the ID alone,
// and so a host that failed to list its workspaces does not have its
// bindings ended by another host's resync. herdr's pane IDs (w3:p1) stay
// bare because watches.json and reminders.json stored them before there was
// a second host; every other host prefixes its IDs with its name.
func HostOf(id string) string {
	if name, _, ok := strings.Cut(id, ":"); ok {
		regMu.RLock()
		known := slices.Contains(prefixed, name)
		regMu.RUnlock()
		if known {
			return name
		}
	}
	return "herdr"
}

type AgentState string

const (
	Idle    AgentState = "idle"
	Working AgentState = "working"
	Blocked AgentState = "blocked"
	Unknown AgentState = "unknown"
)

// ErrBlocked means the agent is waiting at an approval or question; the
// delivery stays queued and is retried later.
var ErrBlocked = errors.New("agent is blocked")

// ErrBusy means the agent is working and its host cannot hand it text
// without mixing it into the running turn; the delivery waits until the
// agent is idle.
var ErrBusy = errors.New("agent is busy")

// ErrUnconfirmed means the text was submitted but the agent showed no
// activity afterwards. The text may well be in its terminal, so it is not
// sent again: a second copy would land in the same input.
var ErrUnconfirmed = errors.New("agent showed no activity after the prompt")

type Host interface {
	Name() string
	Workspaces(ctx context.Context) ([]Workspace, error)
	// Watch reports workspace changes until ctx ends: pushed if the host can,
	// else by polling Workspaces and reporting each result as a Resync.
	Watch(ctx context.Context, ev Events) error
	State(ctx context.Context, ws Workspace) (AgentState, error)
	Deliver(ctx context.Context, ws Workspace, text string) error
	// Badge shows what the workspace watches as a short label, like herdr's
	// sidebar token $kelpie. A zero Badge removes it.
	Badge(ctx context.Context, ws Workspace, b Badge) error
	// Notify shows the person a notification. errors.ErrUnsupported lets
	// another host show it instead.
	Notify(ctx context.Context, n Notification) error
}

type Notification struct {
	Title, Body string
	// Workspace is the one the notification is about, so a host can point
	// at it.
	Workspace Workspace
	// NeedsYou is a request for the person's attention, not just news.
	NeedsYou bool
}

// Badge is what one workspace watches.
type Badge struct {
	Subjects  []string // one label per watched subject, sorted
	Reminders int
}

func (b Badge) String() string {
	parts := b.Subjects
	if b.Reminders > 0 {
		parts = append(slices.Clip(parts), fmt.Sprintf("⏰%d", b.Reminders))
	}
	return strings.Join(parts, " · ")
}

// Events receives what a host's Watch observes.
type Events interface {
	// Resync replaces every workspace of the host, after a (re)connect or a poll.
	Resync(host string, list []Workspace)
	// Upsert reports a workspace that opened or changed (cwd, branch).
	Upsert(ws Workspace)
	// Remove reports a closed workspace.
	Remove(host, id string)
	// Move reports a workspace that got a new ID, like a herdr pane moved
	// into another workspace; bindings follow it.
	Move(host, from string, ws Workspace)
	// AgentReady reports that the agent in a workspace can take input now,
	// so a delivery queued while it was busy or blocked can go out.
	AgentReady(host, id string)
}

// GitInfo reads the branch and GitHub remotes of the checkout at path. A path
// that is not a git checkout returns empty values, not an error: an agent in
// ~/.dotfiles is still a workspace, it just never binds by branch.
func GitInfo(ctx context.Context, path string) (branch string, repos []string) {
	out, err := exec.CommandContext(ctx, "git", "-C", path, "symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err == nil {
		branch = strings.TrimSpace(string(out))
	}
	out, err = exec.CommandContext(ctx, "git", "-C", path, "remote", "-v").Output()
	if err != nil {
		return branch, nil
	}
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if r, ok := GitHubRepo(fields[1]); ok && !contains(repos, r) {
			repos = append(repos, r)
		}
	}
	return branch, repos
}

// GitHubRepo extracts owner/name from git@github.com:o/r.git,
// https://github.com/o/r(.git) and ssh://git@github.com/o/r.git.
func GitHubRepo(url string) (string, bool) {
	var rest string
	for _, prefix := range []string{"git@github.com:", "https://github.com/", "ssh://git@github.com/", "http://github.com/"} {
		if r, ok := strings.CutPrefix(url, prefix); ok {
			rest = r
			break
		}
	}
	rest = strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git")
	owner, name, ok := strings.Cut(rest, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return owner + "/" + name, true
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
