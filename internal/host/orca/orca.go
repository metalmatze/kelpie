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
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/metalmatze/kelpie/internal/host"
)

// A workspace is an Orca worktree, not a terminal: terminal handles are
// issued per Orca runtime and go stale when Orca restarts, while the
// worktree ID ("<repoId>::<path>") stays. The agent's terminal is looked up
// at delivery time.
const prefix = "orca:"

type Orca struct {
	Bin  string        // empty: $ORCA_CLI_COMMAND, else orca (orca-ide on Linux)
	Poll time.Duration // orca has no event stream, so Watch polls worktree ps
	// Comment shows what a worktree watches as its Orca comment. Off by
	// default: the comment is one field that people and Orca's own skill
	// write too, and kelpie would overwrite it.
	Comment bool

	mu       sync.Mutex
	repos    map[string]cachedRepos     // by worktree path
	states   map[string]host.AgentState // by workspace, as of the last poll
	comments map[string]string          // by workspace: the comment before kelpie's badge replaced it
	// shown is the badge last written per workspace. The daemon reports
	// every badge again after each resync, for herdr, which forgets them;
	// Orca keeps comments, and polls resync every 15 s.
	shown map[string]string
	down  bool
}

type cachedRepos struct {
	repos []string
	at    time.Time
}

func (o *Orca) Name() string { return "orca" }

// Caller is the worktree of the Orca terminal the current process runs in.
func Caller() (string, bool) {
	id := os.Getenv("ORCA_WORKTREE_ID")
	return prefix + id, id != ""
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
}

func (a agent) state() host.AgentState {
	s := a.State
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
// watch survives the agent restarting. A runtime that is not ready fails
// instead of listing nothing, which would end every Orca watch.
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
	out := make([]host.Workspace, len(rows))
	for i, w := range rows {
		out[i] = o.workspace(ctx, w)
	}
	return out, rows, nil
}

// workspace takes the branch from Orca, which knows it anyway, and the
// remotes from git, cached for minutes: a poll lists every worktree, and
// running git twice for each every 15 s adds up for remotes that hardly
// ever change.
func (o *Orca) workspace(ctx context.Context, w worktree) host.Workspace {
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
	ev.Resync(o.Name(), list)
	o.ready(rows, ev)
}

func (o *Orca) ready(rows []worktree, ev host.Events) {
	now := map[string]host.AgentState{}
	for _, w := range rows {
		now[prefix+w.WorktreeID] = w.state()
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

func (o *Orca) row(ctx context.Context, ws host.Workspace) (worktree, error) {
	rows, err := o.list(ctx)
	if err != nil {
		return worktree{}, err
	}
	id := strings.TrimPrefix(ws.ID, prefix)
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
	return w.state(), nil
}

type terminal struct {
	Handle        string  `json:"handle"`
	Orphaned      bool    `json:"orphaned"`
	Writable      bool    `json:"writable"`
	TabID         string  `json:"tabId"`
	LeafID        string  `json:"leafId"`
	AgentIdentity *string `json:"agentIdentity"`
	LastOutputAt  int64   `json:"lastOutputAt"`
}

// terminal picks the agent's terminal in a worktree: a worktree also has
// plain shells, and orphaned terminals (left from before Orca restarted)
// are not attached to any tab. An agent Orca reports state for wins over
// one it only recognises by name, then the one that printed last.
func (o *Orca) terminal(ctx context.Context, w worktree) (terminal, error) {
	var resp struct {
		Terminals []terminal `json:"terminals"`
	}
	if err := o.run(ctx, &resp, "terminal", "list", "--worktree", "id:"+w.WorktreeID); err != nil {
		return terminal{}, err
	}
	known := map[string]bool{}
	for _, a := range w.Agents {
		known[a.PaneKey] = true
	}
	cands := slices.DeleteFunc(resp.Terminals, func(t terminal) bool {
		return t.Orphaned || !t.Writable || t.AgentIdentity == nil
	})
	if len(cands) == 0 {
		return terminal{}, errors.New("no agent terminal in this worktree")
	}
	return slices.MaxFunc(cands, func(a, b terminal) int {
		ka, kb := known[a.TabID+":"+a.LeafID], known[b.TabID+":"+b.LeafID]
		if ka != kb {
			if ka {
				return 1
			}
			return -1
		}
		return cmp.Compare(a.LastOutputAt, b.LastOutputAt)
	}), nil
}

// Deliver types only into an idle agent. Orca refuses a blocked agent by
// itself (agent_prompt_blocked), but a working one takes the text into its
// input, where it interleaves with the running turn and its pickup cannot
// be confirmed; such a delivery waits for the agent to turn idle. An agent
// Orca has no state for (not hooked up, like a plain shell) gets the TUI
// idle probe instead.
func (o *Orca) Deliver(ctx context.Context, ws host.Workspace, text string) error {
	w, err := o.row(ctx, ws)
	if err != nil {
		return err
	}
	switch w.state() {
	case host.Blocked:
		return host.ErrBlocked
	case host.Working:
		return host.ErrBusy
	}
	t, err := o.terminal(ctx, w)
	if err != nil {
		return err
	}
	if w.state() == host.Unknown {
		err := o.run(ctx, nil, "terminal", "wait", "--terminal", t.Handle, "--for", "tui-idle", "--timeout-ms", "1000")
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
	err = o.run(ctx, &resp, "terminal", "send", "--terminal", t.Handle, "--text", text, "--enter", "--wait-submit", "10")
	switch code(err) {
	case "":
	case "agent_prompt_blocked":
		return host.ErrBlocked
	default:
		return err
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

// Badge replaces the worktree's comment, the line on its card in Orca's
// sidebar, and puts back the comment it replaced once there is nothing to
// show. Orca ignores --comment "", so an empty original comes back as a
// single space, which shows as blank.
func (o *Orca) Badge(ctx context.Context, ws host.Workspace, b host.Badge) error {
	if !o.Comment {
		return nil
	}
	text := b.String()
	o.mu.Lock()
	orig, saved := o.comments[ws.ID]
	same := saved && o.shown[ws.ID] == text
	o.mu.Unlock()
	if same {
		return nil
	}
	if !saved {
		if text == "" {
			return nil
		}
		w, err := o.row(ctx, ws)
		if err != nil {
			return err
		}
		orig = w.Comment
	}
	comment := text
	if text == "" {
		comment = cmp.Or(orig, " ")
	}
	if err := o.run(ctx, nil, "worktree", "set", "--worktree", "id:"+strings.TrimPrefix(ws.ID, prefix), "--comment", comment); err != nil {
		return err
	}
	o.mu.Lock()
	if o.comments == nil {
		o.comments, o.shown = map[string]string{}, map[string]string{}
	}
	if text == "" {
		delete(o.comments, ws.ID)
		delete(o.shown, ws.ID)
	} else {
		o.comments[ws.ID], o.shown[ws.ID] = orig, text
	}
	o.mu.Unlock()
	return nil
}

// Notify marks the worktree unread when the person is needed there, the
// sidebar dot Orca uses to ask for attention. Orca has no notification
// with a title and body, so another host shows the text.
func (o *Orca) Notify(ctx context.Context, n host.Notification) error {
	if !n.NeedsYou || n.Workspace.Host != o.Name() {
		return errors.ErrUnsupported
	}
	err := o.run(ctx, nil, "worktree", "set", "--worktree", "id:"+strings.TrimPrefix(n.Workspace.ID, prefix), "--unread")
	return errors.Join(err, errors.ErrUnsupported)
}

type cliError struct {
	Code    string
	Message string
	args    []string
}

func (e *cliError) Error() string {
	return fmt.Sprintf("orca %s: %s: %s", strings.Join(e.args, " "), e.Code, e.Message)
}

// code is the orca error code of err, "" for nil or another error.
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

// run calls orca with --json. orca answers failures as JSON on stdout with
// exit status 1, so the envelope decides, not the exit status.
func (o *Orca) run(ctx context.Context, into any, args ...string) error {
	cmd := exec.CommandContext(ctx, o.bin(), append(args, "--json")...)
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
