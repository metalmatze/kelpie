// Package orca is the Orca host, built on the orca CLI.
package orca

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/metalmatze/kelpie/internal/host"
)

// A workspace is an Orca worktree or one agent pane in it, never a
// terminal: terminal handles are issued per Orca runtime and go stale when
// Orca restarts, while the worktree ID ("<repoId>::<path>") stays. The
// agent's terminal is looked up at delivery time.
//
// A pane workspace, "orca:pane:<paneKey>@<worktreeId>", is one agent's
// pane. It lets a watch made by one of several agents in a worktree reach
// that agent alone, and wait only for its state. The pane key comes
// first because it never holds an "@", while a worktree path may. A pane
// key, unlike a handle, outlives an Orca restart: the terminals run on in
// Orca's own daemon, and the app gives each its old tabId:leafId back when
// it reattaches it. But it reattaches a worktree's terminals only once that
// worktree is opened, and until then lists them as orphaned, with no pane.
// So a watch whose pane is gone falls back to its worktree, its parent (see
// Parent), and stays there when the pane comes back.
const (
	prefix     = "orca:"
	panePrefix = prefix + "pane:"
)

// validPane reports whether key can name a pane workspace.
func validPane(key string) bool { return key != "" && !strings.Contains(key, "@") }

func paneID(worktreeID, paneKey string) string { return panePrefix + paneKey + "@" + worktreeID }

// parseID splits a workspace ID into its worktree ID and pane key, which is
// empty for a worktree.
func parseID(id string) (worktreeID, paneKey string) {
	if rest, ok := strings.CutPrefix(id, panePrefix); ok {
		if key, wt, ok := strings.Cut(rest, "@"); ok {
			return wt, key
		}
	}
	return strings.TrimPrefix(id, prefix), ""
}

func worktreeOf(id string) string {
	wt, _ := parseID(id)
	return wt
}

type Orca struct {
	Bin  string        // empty: $ORCA_CLI_COMMAND, else orca (orca-ide on Linux)
	Poll time.Duration // orca has no event stream, so Watch polls worktree ps
	// Comment shows what a worktree watches as its Orca comment. Off by
	// default: the comment is one field that people and Orca's own skill
	// write too, and kelpie would overwrite it.
	Comment bool
	// CommentsFile keeps the comments kelpie replaced, so that after a
	// crash, or a shutdown while Orca was down, the next run still puts
	// back the person's comment instead of taking its own badge for it.
	CommentsFile string

	// commentMu orders comment writes. Without it, restoring a stale
	// comment could read the comment before a concurrent Badge wrote it
	// and take kelpie's badge for the person's original.
	commentMu sync.Mutex
	mu        sync.Mutex
	repos     map[string]cachedRepos     // by worktree path
	states    map[string]host.AgentState // by workspace, as of the last poll
	comments  map[string]replaced        // by worktree, nil until loaded from CommentsFile
	badges    map[string]host.Badge      // by workspace: a worktree and its panes share one comment
	// seen are the worktree comments this run wrote or checked. The daemon reports
	// every badge again after each resync, for herdr, which forgets them;
	// Orca keeps comments, and polls resync every 15 s.
	seen  map[string]bool
	polls int
	down  bool
}

// replaced is a worktree comment that kelpie's badge replaced.
type replaced struct {
	Original string `json:"original"`
	Written  string `json:"written"` // kelpie's badge, as last written
}

type cachedRepos struct {
	repos []string
	at    time.Time
}

func (o *Orca) Name() string { return "orca" }

// Caller is the workspace the current process runs in: its agent pane when
// it can tell, else its worktree. An Orca terminal names its worktree in
// $ORCA_WORKTREE_ID and its pane in $ORCA_PANE_KEY; the pane is the caller
// only when an agent runs in it (see agentPane). An Orca chat has only
// $ORCA_AGENT_SESSION_ID, so the worktree is the one worktree ps lists that
// session's agent in, which also gives the pane, else the one whose path
// holds the working directory.
func Caller() (string, bool) {
	if id := os.Getenv("ORCA_WORKTREE_ID"); id != "" {
		if key := os.Getenv("ORCA_PANE_KEY"); validPane(key) && (&Orca{}).agentPane(id, key) {
			return paneID(id, key), true
		}
		return prefix + id, true
	}
	session := os.Getenv("ORCA_AGENT_SESSION_ID")
	if session == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := (&Orca{}).list(ctx)
	if err != nil {
		slog.Warn("orca chat: worktree not found", "session", session, "err", err)
		return "", false
	}
	cwd, _ := os.Getwd()
	if w, key, ok := callerWorktree(rows, session, cwd); ok {
		if validPane(key) {
			return paneID(w.WorktreeID, key), true
		}
		return prefix + w.WorktreeID, true
	}
	return "", false
}

// agentPane reports whether pane key of worktree id runs an agent that
// delivery can reach, by the rule delivery itself uses (pickPane). A watch
// from a shell pane, where a person typed kelpie watch, belongs to the
// worktree, as before panes: bound to the shell it would wait for an agent
// there, and take over the worktree's watch of the same subject. When Orca
// cannot tell, the worktree is the safer owner too.
func (o *Orca) agentPane(id, key string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := o.list(ctx)
	if err != nil {
		slog.Warn("orca pane: agent unknown; using the worktree", "pane", key, "err", err)
		return false
	}
	for _, w := range rows {
		if w.WorktreeID == id {
			_, err := o.target(ctx, w, key)
			return err == nil
		}
	}
	return false
}

// callerWorktree also returns the pane key of the agent that matched the
// session, empty when only the directory matched.
func callerWorktree(rows []worktree, session, dir string) (worktree, string, bool) {
	for _, w := range rows {
		for _, a := range w.Agents {
			if s, ok := a.session(); ok && s == session {
				return w, a.PaneKey, true
			}
		}
	}
	var best worktree
	for _, w := range rows {
		rel, err := filepath.Rel(w.Path, dir)
		if err == nil && filepath.IsLocal(rel) && len(w.Path) > len(best.Path) {
			best = w
		}
	}
	return best, "", best.Path != ""
}

type worktree struct {
	WorktreeID string  `json:"worktreeId"`
	Path       string  `json:"path"`
	Branch     string  `json:"branch"`
	IsArchived bool    `json:"isArchived"`
	Comment    string  `json:"comment"`
	Agents     []agent `json:"agents"`
}

type agent struct {
	PaneKey string `json:"paneKey"`
	// State comes from the agent's hooks and stays "waiting" after a
	// question is cancelled with Esc; ProgramStatus follows the terminal
	// and is the one to trust.
	State         string `json:"state"`
	ProgramStatus *struct {
		State string `json:"state"`
	} `json:"programStatus"`
	// MainAgent is the agent's own state, without its children. State
	// counts a chat's background tasks too and stays working while one
	// runs, though the chat is idle and takes a prompt.
	MainAgent *struct {
		State string `json:"state"`
	} `json:"mainAgent"`
	UpdatedAt int64 `json:"updatedAt"` // ms; a chat's last activity
}

// chatPane prefixes the pane key of an agent that runs as an Orca chat (a
// structured session) instead of in a terminal:
// "structured-agent-session-<sessionId>:<leafId>".
const chatPane = "structured-agent-session-"

func (a agent) session() (string, bool) {
	rest, ok := strings.CutPrefix(a.PaneKey, chatPane)
	if !ok {
		return "", false
	}
	id, _, _ := strings.Cut(rest, ":")
	return id, id != ""
}

func (a agent) state() host.AgentState {
	s := a.State
	// Only a working aggregate gives way to the main agent's own state: a
	// background child keeps it working while the chat is idle. A blocked
	// one stays blocked, as a child may wait at a question.
	if s == "working" && a.MainAgent != nil && a.MainAgent.State != "" {
		s = a.MainAgent.State
	}
	if a.ProgramStatus != nil && a.ProgramStatus.State != "" {
		s = a.ProgramStatus.State
	}
	switch s {
	case "idle", "done":
		return host.Idle
	case "working":
		return host.Working
	case "blocked", "waiting":
		return host.Blocked
	}
	return host.Unknown
}

// paneState is the state of one agent, Unknown when no hook reports it.
func (w worktree) paneState(key string) host.AgentState {
	for _, a := range w.Agents {
		if a.PaneKey == key {
			return a.state()
		}
	}
	return host.Unknown
}

// stateOf is the state of a pane, or of the whole worktree for an empty key.
func (w worktree) stateOf(key string) host.AgentState {
	if key == "" {
		return w.state()
	}
	return w.paneState(key)
}

// state of a worktree: one blocked agent is enough to hold a delivery back,
// as the prompt might land in its question.
func (w worktree) state() host.AgentState {
	out := host.Unknown
	for _, a := range w.Agents {
		switch s := a.state(); {
		case s == host.Blocked:
			return s
		case s == host.Working, s == host.Idle && out == host.Unknown:
			out = s
		}
	}
	return out
}

// list returns the worktrees an agent can live in. Archived ones count as
// closed, so their watches end; Orca's floating terminal has no worktree.
func (o *Orca) list(ctx context.Context) ([]worktree, error) {
	var resp struct {
		Worktrees []worktree `json:"worktrees"`
		Truncated bool       `json:"truncated"`
	}
	if err := o.run(ctx, &resp, "worktree", "ps"); err != nil {
		return nil, err
	}
	if resp.Truncated {
		// A missing worktree would look closed and lose its watches.
		return nil, errors.New("orca worktree ps: list truncated")
	}
	return slices.DeleteFunc(resp.Worktrees, func(w worktree) bool { return w.IsArchived || w.Path == "" }), nil
}

// Workspaces are all worktrees, not only those running an agent, so a
// watch survives the agent restarting, and every pane of them, shell or
// agent, so a watch can belong to one agent of several in a worktree, and
// lives as long as its pane. A pane is a child of its worktree (see
// Parent), so an event that binds by branch picks the worktree, whose
// branch the pane shares. A runtime that is not ready fails instead of
// listing nothing, which would end every Orca watch.
func (o *Orca) Workspaces(ctx context.Context) ([]host.Workspace, error) {
	list, _, err := o.workspaces(ctx)
	return list, err
}

// workspaces also returns the worktrees they were made of, which carry the
// agents' state, so a poll needs one worktree ps.
func (o *Orca) workspaces(ctx context.Context) ([]host.Workspace, []worktree, error) {
	var st struct {
		Runtime struct {
			State string `json:"state"`
		} `json:"runtime"`
	}
	if err := o.run(ctx, &st, "status"); err != nil {
		return nil, nil, err
	}
	if st.Runtime.State != "ready" {
		return nil, nil, fmt.Errorf("orca runtime is %s, not ready (is the Orca app running?)", cmp.Or(st.Runtime.State, "unknown"))
	}
	rows, err := o.list(ctx)
	if err != nil {
		return nil, nil, err
	}
	terms, err := o.terminals(ctx)
	if err != nil {
		return nil, nil, err
	}
	var out []host.Workspace
	for _, w := range rows {
		named := slices.ContainsFunc(terms[w.WorktreeID], func(t terminal) bool { return t.AgentIdentity != nil })
		ws := o.workspace(ctx, w, named)
		out = append(out, ws)
		for _, k := range panes(w, terms[w.WorktreeID]) {
			pane := ws
			pane.ID, pane.Agent = paneID(w.WorktreeID, k), false
			out = append(out, pane)
		}
	}
	return out, rows, nil
}

// panes are the pane keys of a worktree: every live terminal, shell or
// agent, and every chat. Like a herdr pane, a pane is a workspace whether
// or not an agent runs in it, so a watch lives as long as its pane and
// survives the agent restarting there; delivery waits for an agent.
func panes(w worktree, terms []terminal) []string {
	var keys []string
	for _, a := range w.Agents {
		if _, ok := a.session(); ok {
			keys = append(keys, a.PaneKey)
		}
	}
	for _, t := range terms {
		keys = append(keys, t.paneKey())
	}
	keys = slices.DeleteFunc(keys, func(k string) bool { return !validPane(k) })
	slices.Sort(keys)
	return slices.Compact(keys)
}

// terminals are the live terminals by worktree. Orphaned ones, left from
// before Orca restarted, have no pane. worktree ps lists only agents that
// report through Orca's hooks, and chats; an agent Orca knows by its process
// or title alone, which pick still delivers to, is only visible here, and
// counting it as no agent would let another host's agent on the branch take
// the binding.
func (o *Orca) terminals(ctx context.Context) (map[string][]terminal, error) {
	var resp struct {
		Terminals []terminal `json:"terminals"`
		Truncated bool       `json:"truncated"`
	}
	if err := o.run(ctx, &resp, "terminal", "list"); err != nil {
		return nil, err
	}
	if resp.Truncated {
		return nil, errors.New("orca terminal list: list truncated")
	}
	live := map[string][]terminal{}
	for _, t := range resp.Terminals {
		if !t.Orphaned {
			live[t.WorktreeID] = append(live[t.WorktreeID], t)
		}
	}
	return live, nil
}

// workspace takes the branch from Orca, which knows it anyway, and the
// remotes from git, cached for minutes: a poll lists every worktree, and
// running git twice for each every 15 s adds up for remotes that hardly
// ever change.
func (o *Orca) workspace(ctx context.Context, w worktree, namedAgent bool) host.Workspace {
	o.mu.Lock()
	c, ok := o.repos[w.Path]
	o.mu.Unlock()
	if !ok || time.Since(c.at) > 5*time.Minute {
		_, repos := host.GitInfo(ctx, w.Path)
		c = cachedRepos{repos: repos, at: time.Now()}
		o.mu.Lock()
		if o.repos == nil {
			o.repos = map[string]cachedRepos{}
		}
		o.repos[w.Path] = c
		o.mu.Unlock()
	}
	return host.Workspace{
		Host:   o.Name(),
		ID:     prefix + w.WorktreeID,
		Repos:  c.repos,
		Branch: strings.TrimPrefix(w.Branch, "refs/heads/"),
		Path:   w.Path,
		Agent:  len(w.Agents) > 0 || namedAgent,
	}
}

// Watch polls, as orca has no event stream. An agent turning idle releases
// a delivery queued while it worked or was blocked.
func (o *Orca) Watch(ctx context.Context, ev host.Events) error {
	for {
		o.poll(ctx, ev)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.Poll):
		}
	}
}

func (o *Orca) poll(ctx context.Context, ev host.Events) {
	list, rows, err := o.workspaces(ctx)
	o.mu.Lock()
	wasDown := o.down
	o.down = err != nil
	o.mu.Unlock()
	if err != nil {
		if !wasDown {
			slog.Warn("orca unavailable; its workspaces keep their watches until it is back", "err", err, "every", o.Poll)
		}
		return
	}
	if wasDown {
		slog.Info("orca is back")
	}
	// Before the resync, which reports every badge again: a pane that is
	// gone must not stay in the worktree's comment.
	o.pruneBadges(ctx, list)
	ev.Resync(o.Name(), list)
	o.ready(rows, ev)
	o.mu.Lock()
	o.polls++
	restore := o.polls == 2
	o.mu.Unlock()
	if restore {
		o.restoreStale(ctx, rows)
	}
}

func (o *Orca) ready(rows []worktree, ev host.Events) {
	now := map[string]host.AgentState{}
	for _, w := range rows {
		now[prefix+w.WorktreeID] = w.state()
		for _, a := range w.Agents {
			if validPane(a.PaneKey) {
				now[paneID(w.WorktreeID, a.PaneKey)] = a.state()
			}
		}
	}
	o.mu.Lock()
	prev := o.states
	o.states = now
	live := map[string]bool{}
	for _, w := range rows {
		live[w.Path] = true
	}
	for p := range o.repos {
		if !live[p] {
			delete(o.repos, p)
		}
	}
	o.mu.Unlock()
	for id, s := range now {
		if s == host.Idle && prev[id] != host.Idle {
			ev.AgentReady(o.Name(), id)
		}
	}
}

// row is the worktree of a workspace, which for a pane is the one it is in.
func (o *Orca) row(ctx context.Context, ws host.Workspace) (worktree, error) {
	rows, err := o.list(ctx)
	if err != nil {
		return worktree{}, err
	}
	id := worktreeOf(ws.ID)
	for _, w := range rows {
		if w.WorktreeID == id {
			return w, nil
		}
	}
	return worktree{}, fmt.Errorf("orca has no worktree %s", id)
}

func (o *Orca) State(ctx context.Context, ws host.Workspace) (host.AgentState, error) {
	w, err := o.row(ctx, ws)
	if err != nil {
		return host.Unknown, err
	}
	_, key := parseID(ws.ID)
	return w.stateOf(key), nil
}

// Parent is the worktree of a pane. A subject is bound to a worktree or to
// its panes, never both; when the pane is gone, its watches and reminders
// move to the worktree instead of ending; branch binding picks the
// worktree. A worktree has none.
func (o *Orca) Parent(id string) (string, bool) {
	wt, key := parseID(id)
	if key == "" {
		return "", false
	}
	return prefix + wt, true
}

type terminal struct {
	Handle        string  `json:"handle"`
	WorktreeID    string  `json:"worktreeId"`
	Orphaned      bool    `json:"orphaned"`
	Writable      bool    `json:"writable"`
	TabID         string  `json:"tabId"`
	LeafID        string  `json:"leafId"`
	AgentIdentity *string `json:"agentIdentity"`
	LastOutputAt  int64   `json:"lastOutputAt"`
}

// paneKey is what worktree ps calls the pane of an agent in this terminal.
func (t terminal) paneKey() string {
	if t.TabID == "" || t.LeafID == "" {
		return ""
	}
	return t.TabID + ":" + t.LeafID
}

// usable terminals are attached to a tab and take input. Orphaned ones are
// left from before Orca restarted.
func (t terminal) usable() bool { return !t.Orphaned && t.Writable }

type target struct {
	handle  string // terminal handle; empty for a chat
	session string // chat session ID; empty for a terminal
	known   bool   // Orca reports the agent's state
	active  int64  // ms of the last output or chat activity
}

// target picks the agent to prompt in a worktree. Its candidates are the
// agent terminals (a worktree also has plain shells, and orphaned
// terminals, left from before Orca restarted, are not attached to any tab)
// and the agents running as chats. An agent Orca reports state for wins
// over one it only recognises by name, which leaves chats and hooked-up
// terminals on par; then the one active last wins, as that is most likely
// the agent the person works with.
//
// For a pane the choice is made already: its own chat, or its own terminal,
// when that is an agent. Never another agent, and never a shell.
func (o *Orca) target(ctx context.Context, w worktree, paneKey string) (target, error) {
	var resp struct {
		Terminals []terminal `json:"terminals"`
	}
	if err := o.run(ctx, &resp, "terminal", "list", "--worktree", "id:"+w.WorktreeID); err != nil {
		return target{}, err
	}
	if paneKey != "" {
		return pickPane(w, paneKey, resp.Terminals)
	}
	return pick(w, resp.Terminals)
}

func pickPane(w worktree, key string, terms []terminal) (target, error) {
	known := false
	for _, a := range w.Agents {
		if a.PaneKey != key {
			continue
		}
		known = true
		if s, ok := a.session(); ok {
			return target{session: s, known: true, active: a.UpdatedAt}, nil
		}
	}
	for _, t := range terms {
		// Only a terminal Orca names an agent in, as in pick: a ps row can
		// outlive the agent, and its terminal is a shell by then.
		if t.paneKey() == key && t.usable() && t.AgentIdentity != nil {
			return target{handle: t.Handle, known: known, active: t.LastOutputAt}, nil
		}
	}
	return target{}, fmt.Errorf("no agent terminal or chat in orca pane %s", key)
}

func pick(w worktree, terms []terminal) (target, error) {
	known := map[string]bool{}
	var cands []target
	for _, a := range w.Agents {
		known[a.PaneKey] = true
		if s, ok := a.session(); ok {
			cands = append(cands, target{session: s, known: true, active: a.UpdatedAt})
		}
	}
	for _, t := range terms {
		if !t.usable() || t.AgentIdentity == nil {
			continue
		}
		cands = append(cands, target{handle: t.Handle, known: known[t.paneKey()], active: t.LastOutputAt})
	}
	if len(cands) == 0 {
		return target{}, errors.New("no agent terminal or chat in this worktree")
	}
	return slices.MaxFunc(cands, func(a, b target) int {
		if a.known != b.known {
			if a.known {
				return 1
			}
			return -1
		}
		return cmp.Compare(a.active, b.active)
	}), nil
}

// Deliver types only into an idle agent. Orca refuses a blocked agent by
// itself (agent_prompt_blocked), but a working one takes the text into its
// input, where it interleaves with the running turn and its pickup cannot
// be confirmed; such a delivery waits for the agent to turn idle. An agent
// Orca has no state for (not hooked up, like a plain shell) gets the TUI
// idle probe instead. A pane waits only for its own agent's state; a
// worktree for all of its agents.
func (o *Orca) Deliver(ctx context.Context, ws host.Workspace, text string) error {
	w, err := o.row(ctx, ws)
	if err != nil {
		return err
	}
	_, key := parseID(ws.ID)
	s := w.stateOf(key)
	switch s {
	case host.Blocked:
		return host.ErrBlocked
	case host.Working:
		return host.ErrBusy
	}
	t, err := o.target(ctx, w, key)
	if err != nil {
		return err
	}
	if t.session != "" {
		return o.prompt(ctx, s, t.session, text)
	}
	if s == host.Unknown {
		err := o.run(ctx, nil, "terminal", "wait", "--terminal", t.handle, "--for", "tui-idle", "--timeout-ms", "1000")
		if code(err) == "timeout" {
			return host.ErrBusy
		} else if err != nil {
			return err
		}
	}
	var resp struct {
		Send struct {
			Prompt struct {
				Stages []string `json:"stages"`
			} `json:"prompt"`
		} `json:"send"`
	}
	// Only failures that come before anything is typed may be retried. Any
	// other (a timeout, a killed or unreadable orca) may have typed the
	// text already, and a retry would type it twice. A stale handle needs
	// no retry here: the router retries, and terminal looks it up again.
	err = o.run(ctx, &resp, "terminal", "send", "--terminal", t.handle, "--text", text, "--enter", "--wait-submit", "10")
	switch c := code(err); {
	case err == nil:
	case c == "agent_prompt_blocked":
		return host.ErrBlocked
	case c == "terminal_handle_stale", c == "selector_not_found", errors.Is(err, errNotStarted):
		return err
	default:
		return fmt.Errorf("%w: %w", host.ErrUnconfirmed, err)
	}
	// turn_started is Orca seeing the agent begin a turn. Agents it has no
	// provider support for only report input_accepted, so their pickup is
	// judged by the agent's state instead.
	if slices.Contains(resp.Send.Prompt.Stages, "turn_started") {
		return nil
	}
	for range 5 {
		if s, err := o.State(ctx, ws); err == nil && (s == host.Working || s == host.Blocked) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("%w: orca reported stages %v", host.ErrUnconfirmed, resp.Send.Prompt.Stages)
}

// prompt hands text to an agent that runs as an Orca chat. A chat has no
// terminal to type into; Orca's way in is the orchestration mailbox of its
// session, which posts each message to the chat as a prompt of its own,
// headed by the sender. Orca delivers into a working chat too, in the
// middle of its turn, so only an idle chat gets one. The mailbox keeps a
// message it accepted and delivers it itself, so an accepted send is a
// delivery. A send fails before anything is stored only when orca did not
// start, names the session or sender unknown, or refuses the sender for
// an agent session; any other failure may have stored the message, and a
// retry would post it twice.
func (o *Orca) prompt(ctx context.Context, s host.AgentState, session, text string) error {
	if s != host.Idle {
		return host.ErrBusy
	}
	var resp struct {
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	err := o.run(ctx, &resp, "orchestration", "send", "--from", "kelpie", "--to", "orca_session_id:"+session,
		"--type", "status", "--subject", "kelpie", "--body", text, "--full")
	switch c := code(err); {
	case err == nil:
		slog.Debug("orca chat prompted", "session", session, "message", resp.Message.ID)
		return nil
	case c == "session_caller_unknown", c == "no_active_sender_terminal", c == "consumer_fenced", errors.Is(err, errNotStarted):
		return err
	default:
		return fmt.Errorf("%w: %w", host.ErrUnconfirmed, err)
	}
}

// Badge replaces the worktree's comment, the line on its card in Orca's
// sidebar, and puts back the comment it replaced once there is nothing to
// show. A comment the person changed meanwhile is theirs and stays. The
// comment belongs to the worktree, so it shows what the worktree and all of
// its panes watch, and a pane's badge is one part of it.
func (o *Orca) Badge(ctx context.Context, ws host.Workspace, b host.Badge) error {
	if !o.Comment {
		return nil
	}
	o.mu.Lock()
	if o.badges == nil {
		o.badges = map[string]host.Badge{}
	}
	if b.String() == "" {
		delete(o.badges, ws.ID)
	} else {
		o.badges[ws.ID] = b
	}
	o.mu.Unlock()
	return o.showComment(ctx, ws)
}

// union is the badge of a worktree and its panes together: their subjects
// once each, and their reminders. Call with o.mu held.
func (o *Orca) union(worktreeID string) host.Badge {
	var u host.Badge
	for id, b := range o.badges {
		if worktreeOf(id) == worktreeID {
			u.Subjects = append(u.Subjects, b.Subjects...)
			u.Reminders += b.Reminders
		}
	}
	slices.Sort(u.Subjects)
	u.Subjects = slices.Compact(u.Subjects)
	return u
}

// showComment writes the comment of the worktree of ws from the badges of
// the worktree and its panes.
func (o *Orca) showComment(ctx context.Context, ws host.Workspace) error {
	wt := worktreeOf(ws.ID)
	id := prefix + wt
	o.mu.Lock()
	o.load()
	text := o.union(wt).String()
	r, saved := o.comments[id]
	same := saved && o.seen[id] && r.Written == text
	o.mu.Unlock()
	if same || !saved && text == "" {
		return nil
	}
	o.commentMu.Lock()
	defer o.commentMu.Unlock()
	w, err := o.row(ctx, ws)
	if err != nil {
		return err
	}
	return o.setComment(ctx, id, w.Comment, text)
}

// pruneBadges forgets the badges of workspaces that are gone, and shows the
// comments of the worktrees they were on without them.
func (o *Orca) pruneBadges(ctx context.Context, list []host.Workspace) {
	live := map[string]bool{}
	for _, ws := range list {
		live[ws.ID] = true
	}
	o.mu.Lock()
	var affected []host.Workspace
	for id := range o.badges {
		if !live[id] {
			delete(o.badges, id)
			affected = append(affected, host.Workspace{Host: o.Name(), ID: id})
		}
	}
	o.mu.Unlock()
	for _, ws := range affected {
		if err := o.showComment(ctx, ws); err != nil {
			slog.Warn("orca comment not updated", "workspace", ws.ID, "err", err)
		}
	}
}

// setComment shows text as the comment of a worktree whose comment is now
// current, or puts back the replaced one when text is empty.
func (o *Orca) setComment(ctx context.Context, id, current, text string) error {
	o.mu.Lock()
	r, saved := o.comments[id]
	o.mu.Unlock()
	if !saved || current != r.Written {
		r.Original = current
	}
	want := text
	if text == "" {
		// Orca ignores --comment "", so an empty original comes back as a
		// single space, which shows as blank.
		want = cmp.Or(r.Original, " ")
	}
	if want != current {
		if err := o.run(ctx, nil, "worktree", "set", "--worktree", "id:"+worktreeOf(id), "--comment", want); err != nil {
			return err
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if text == "" {
		delete(o.comments, id)
		delete(o.seen, id)
	} else {
		r.Written = text
		o.comments[id] = r
		o.seen[id] = true
	}
	o.save()
	return nil
}

// restoreStale puts back the comments a previous run replaced and this
// run shows nothing for. It waits for the second poll: at the first, the
// sources have not observed anything yet, so badges are missing that
// would be written again a moment later.
func (o *Orca) restoreStale(ctx context.Context, rows []worktree) {
	current := map[string]string{}
	for _, w := range rows {
		current[prefix+w.WorktreeID] = w.Comment
	}
	o.mu.Lock()
	o.load()
	var stale []string
	for id := range o.comments {
		if !o.seen[id] {
			stale = append(stale, id)
		}
	}
	o.mu.Unlock()
	for _, id := range stale {
		c, ok := current[id]
		if !ok { // the worktree is gone
			o.mu.Lock()
			delete(o.comments, id)
			o.save()
			o.mu.Unlock()
			continue
		}
		if err := o.restore(ctx, id, c); err != nil {
			slog.Warn("orca comment not restored", "workspace", id, "err", err)
		}
	}
}

// restore puts back one stale comment unless a Badge claimed the worktree
// since the stale list was made, when current is no longer current.
func (o *Orca) restore(ctx context.Context, id, current string) error {
	o.commentMu.Lock()
	defer o.commentMu.Unlock()
	o.mu.Lock()
	claimed := o.seen[id]
	o.mu.Unlock()
	if claimed {
		return nil
	}
	return o.setComment(ctx, id, current, "")
}

// load reads CommentsFile once. Call with o.mu held.
func (o *Orca) load() {
	if o.comments != nil {
		return
	}
	o.comments, o.seen = map[string]replaced{}, map[string]bool{}
	if o.CommentsFile == "" {
		return
	}
	data, err := os.ReadFile(o.CommentsFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("orca comments not read", "file", o.CommentsFile, "err", err)
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &o.comments); err != nil {
			slog.Warn("orca comments not read", "file", o.CommentsFile, "err", err)
		}
	}
}

// save writes CommentsFile and renames it into place, so a crash leaves
// the old file or the new one. Call with o.mu held.
func (o *Orca) save() {
	if o.CommentsFile == "" {
		return
	}
	data, _ := json.MarshalIndent(o.comments, "", "  ")
	tmp := o.CommentsFile + ".tmp"
	err := os.MkdirAll(filepath.Dir(o.CommentsFile), 0o700)
	if err == nil {
		err = os.WriteFile(tmp, data, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, o.CommentsFile)
	}
	if err != nil {
		slog.Warn("orca comments not saved", "file", o.CommentsFile, "err", err)
	}
}

// Notify marks the worktree unread when the person is needed there, the
// sidebar dot Orca uses to ask for attention. Orca has no notification
// with a title and body, so another host shows the text.
func (o *Orca) Notify(ctx context.Context, n host.Notification) error {
	if !n.NeedsYou || n.Workspace.Host != o.Name() {
		return errors.ErrUnsupported
	}
	if err := o.run(ctx, nil, "worktree", "set", "--worktree", "id:"+worktreeOf(n.Workspace.ID), "--unread"); err != nil {
		return err
	}
	return errors.ErrUnsupported
}

// errNotStarted is orca failing to start at all (missing, not executable),
// so a send cannot have typed anything and is safe to retry.
var errNotStarted = errors.New("orca did not start")

type cliError struct {
	Code    string
	Message string
	args    []string
}

func (e *cliError) Error() string {
	return fmt.Sprintf("orca %s: %s: %s", strings.Join(e.args, " "), e.Code, e.Message)
}

func code(err error) string {
	if e, ok := errors.AsType[*cliError](err); ok {
		return e.Code
	}
	return ""
}

func (o *Orca) bin() string {
	if o.Bin != "" {
		return o.Bin
	}
	if b := os.Getenv("ORCA_CLI_COMMAND"); b != "" {
		return b
	}
	if runtime.GOOS == "linux" {
		return "orca-ide" // bare orca is the GNOME screen reader there
	}
	return "orca"
}

// callerVars make orca act as the agent session or terminal they name. A
// daemon started from inside Orca inherits them, and orchestration send
// then refuses --from kelpie (consumer_fenced) or sends as that agent.
var callerVars = []string{"ORCA_AGENT_SESSION_ID", "ORCA_AGENT_SESSION_SPAWN_TOKEN", "ORCA_TERMINAL_HANDLE"}

// run calls orca with --json. orca answers failures as JSON on stdout with
// exit status 1, so the envelope decides, not the exit status.
func (o *Orca) run(ctx context.Context, into any, args ...string) error {
	cmd := exec.CommandContext(ctx, o.bin(), append(args, "--json")...)
	if args[0] == "orchestration" {
		cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
			k, _, _ := strings.Cut(kv, "=")
			return slices.Contains(callerVars, k)
		})
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	verb := args[:min(2, len(args))]
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		if runErr != nil && cmd.ProcessState == nil {
			return fmt.Errorf("orca %s: %w: %w", strings.Join(verb, " "), errNotStarted, runErr)
		}
		if runErr != nil {
			return fmt.Errorf("orca %s: %w: %s", strings.Join(verb, " "), runErr, strings.TrimSpace(cmp.Or(stderr.String(), stdout.String())))
		}
		return fmt.Errorf("orca %s: unreadable answer: %w", strings.Join(verb, " "), err)
	}
	if !env.OK || env.Error != nil {
		e := &cliError{args: verb, Code: "unknown"}
		if env.Error != nil {
			e.Code, e.Message = env.Error.Code, env.Error.Message
		}
		return e
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(env.Result, into)
}
