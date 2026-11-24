// Package rex is the host for Rex. Every terminal block is a workspace;
// agents are told apart by the program status (OSC 7501) they report, which
// Claude Code does by itself. kelpie talks to the Rex server over its socket
// with the protocol in client.go, on one connection for calls and events.
package rex

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/metalmatze/kelpie/internal/host"
)

const (
	name     = "rex"
	terminal = "com.superlogical.terminal"
)

type Rex struct {
	Socket string        // empty: see socketPath
	Poll   time.Duration // interval to list blocks while the connection is down

	connMu sync.Mutex
	c      *client

	mu     sync.Mutex
	blocks map[string]cachedBlock // by block ID
	local  string                 // host_id of the machine kelpie runs on
}

type cachedBlock struct {
	session string
	ws      host.Workspace
	at      time.Time // of the git read
	remote  bool      // runs on another machine than kelpie
}

func (h *Rex) Name() string { return name }

type block struct {
	ID      string `json:"block_id"`
	Creator string `json:"creator_name"`
	HostID  string `json:"host_id"`
}

type proc struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
	Cwd  string `json:"cwd"`
}

type record struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at"`
	Owner     *proc  `json:"owner"`
}

// conn is the open connection, or a new one if it closed. Calls and Watch
// share it, so Watch's resync and a delivery never open a second one.
func (h *Rex) conn(ctx context.Context) (*client, error) {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	if h.c != nil && !h.c.closed() {
		return h.c, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := dial(ctx, h.socketPath())
	if err != nil {
		return nil, err
	}
	h.c = c
	return c, nil
}

func (h *Rex) call(ctx context.Context, method string, payload, into any) error {
	c, err := h.conn(ctx)
	if err != nil {
		return err
	}
	return c.call(ctx, method, payload, into)
}

// blockCall calls one of a block's own methods, such as process.
func (h *Rex) blockCall(ctx context.Context, session, id, method string, into any) error {
	payload := map[string]string{"session_id": session, "block_id": id, "method": method}
	err := h.call(ctx, "block.method", payload, into)
	if errCode(err) == "conflict" {
		// Not attached: attach_all should cover every session, this is
		// the fallback should a session made later not be.
		if h.call(ctx, "session.attach", map[string]string{"session_id": session}, nil) == nil {
			err = h.call(ctx, "block.method", payload, into)
		}
	}
	return err
}

// socketPath follows the CLI: $REX_SERVER when it is a unix socket, else the
// server's default socket. A TCP or Tailscale server is not covered.
func (h *Rex) socketPath() string {
	if h.Socket != "" {
		return h.Socket
	}
	if u, err := url.Parse(os.Getenv("REX_SERVER")); err == nil && u.Scheme == "unix" {
		return u.Path
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "rex", "server.sock")
	}
	dir := cmp.Or(os.Getenv("XDG_RUNTIME_DIR"), fmt.Sprintf("/run/user/%d", os.Getuid()))
	return filepath.Join(dir, "rex", "server.sock")
}

// Workspaces are all terminal blocks, not only those with an agent: a watch
// must outlive the agent restarting in its block.
func (h *Rex) Workspaces(ctx context.Context) ([]host.Workspace, error) {
	var resp struct {
		Sessions []struct {
			ID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := h.call(ctx, "session.list", nil, &resp); err != nil {
		return nil, err
	}
	var out []host.Workspace
	for _, s := range resp.Sessions {
		list, err := h.sessionWorkspaces(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
	}
	return out, nil
}

func (h *Rex) sessionWorkspaces(ctx context.Context, session string) ([]host.Workspace, error) {
	var resp struct {
		Blocks []block `json:"blocks"`
	}
	if err := h.call(ctx, "session.list_blocks", map[string]string{"session_id": session}, &resp); err != nil {
		return nil, err
	}
	var out []host.Workspace
	for _, b := range resp.Blocks {
		if b.Creator != terminal {
			continue
		}
		ws, err := h.workspace(ctx, session, b.ID, b.HostID)
		if err != nil {
			// Closed between listing and reading; its block_closed follows.
			slog.Debug("rex block skipped", "block", b.ID, "err", err)
			continue
		}
		out = append(out, ws)
	}
	return out, nil
}

// workspace reads a block's folder and agent. Git runs when the folder
// changed or the cached result is 30 s old, the bound that catches a git
// checkout. A block on another machine keeps no git info: its folder is not
// on this one.
func (h *Rex) workspace(ctx context.Context, session, id, hostID string) (host.Workspace, error) {
	fg, recs, err := h.status(ctx, session, id)
	if err != nil {
		return host.Workspace{}, err
	}
	path := ""
	if fg != nil {
		path = fg.Cwd
	}
	if path == "" {
		path = h.pwd(ctx, session, id)
	}
	_, agent := agentRecord(fg, recs)

	c, ok := h.cached(id)
	// Block events carry no host_id; a known block keeps what its listing said.
	remote := c.remote
	if !ok || hostID != "" {
		remote = !h.isLocal(ctx, hostID)
	}
	if ok && c.ws.Path == path && time.Since(c.at) < 30*time.Second {
		c.ws.Agent = agent
		c.session = session
		h.remember(id, c)
		return c.ws, nil
	}
	ws := host.Workspace{Host: name, ID: name + ":" + id, Path: path, Agent: agent}
	if path != "" && !remote {
		ws.Branch, ws.Repos = host.GitInfo(ctx, path)
	}
	h.remember(id, cachedBlock{session: session, ws: ws, at: time.Now(), remote: remote})
	return ws, nil
}

func (h *Rex) remember(id string, c cachedBlock) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.blocks == nil {
		h.blocks = map[string]cachedBlock{}
	}
	h.blocks[id] = c
}

// isLocal is true for a block on kelpie's own machine, whose folders kelpie
// can run git in. An unknown hostID counts as local.
func (h *Rex) isLocal(ctx context.Context, hostID string) bool {
	if hostID == "" {
		return true
	}
	h.mu.Lock()
	local := h.local
	h.mu.Unlock()
	if local == "" {
		var st struct {
			HostID string `json:"host_id"`
		}
		if h.call(ctx, "server.status", nil, &st) != nil {
			return true
		}
		local = st.HostID
		h.mu.Lock()
		h.local = local
		h.mu.Unlock()
	}
	return hostID == local
}

// status is the block's foreground process and its program status records.
func (h *Rex) status(ctx context.Context, session, id string) (*proc, []record, error) {
	var p struct {
		Foreground *proc `json:"foreground"`
	}
	if err := h.blockCall(ctx, session, id, "process", &p); err != nil {
		return nil, nil, err
	}
	var s struct {
		Records []record `json:"records"`
	}
	if err := h.blockCall(ctx, session, id, "program_status", &s); err != nil {
		return nil, nil, err
	}
	return p.Foreground, s.Records, nil
}

// pwd is the folder the shell last reported, for a block whose foreground
// process is unknown. Rex reports it as a file:// URL.
func (h *Rex) pwd(ctx context.Context, session, id string) string {
	var resp struct {
		Pwd string `json:"pwd"`
	}
	if h.blockCall(ctx, session, id, "pwd", &resp) != nil {
		return ""
	}
	if u, err := url.Parse(resp.Pwd); err == nil && u.Scheme == "file" {
		return u.Path
	}
	return resp.Pwd
}

// agentRecord is the status record of the program in the foreground, the
// root record (no id) if it reported several. Records outlive their program
// (done and error survive its exit), so a record whose owner is not the
// foreground process is a leftover, not an agent: the owner is the
// foreground process at the time of the report, and pid plus name guard
// against a reused pid.
func agentRecord(fg *proc, recs []record) (record, bool) {
	if fg == nil {
		return record{}, false
	}
	var found record
	ok := false
	for _, r := range recs {
		if r.Owner == nil || r.Owner.PID != fg.PID || r.Owner.Name != fg.Name {
			continue
		}
		if !ok || r.ID == "" {
			found, ok = r, true
		}
	}
	return found, ok
}

func agentState(r record) host.AgentState {
	switch r.State {
	case "working":
		return host.Working
	case "blocked":
		return host.Blocked
	}
	// idle, done, error, and states added later, which the protocol says to
	// read as idle: the agent is at rest and takes input.
	return host.Idle
}

func (h *Rex) State(ctx context.Context, ws host.Workspace) (host.AgentState, error) {
	session, id, err := h.locate(ctx, ws.ID)
	if err != nil {
		return host.Unknown, err
	}
	fg, recs, err := h.status(ctx, session, id)
	if err != nil {
		return host.Unknown, err
	}
	r, ok := agentRecord(fg, recs)
	if !ok {
		return host.Unknown, nil
	}
	return agentState(r), nil
}

var errNoAgent = errors.New("no agent reports its status in this block")

// Deliver pastes the text and presses Enter. Rex types into whatever runs in
// the block, so kelpie checks first: a blocked agent would take the text as
// its answer, and a block without a reporting agent is most likely a shell
// that would run it as a command. Between that check and the paste the
// agent can still change state; Rex offers nothing atomic. A working agent
// gets the text queued by its own TUI, as in herdr.
func (h *Rex) Deliver(ctx context.Context, ws host.Workspace, text string) error {
	session, id, err := h.locate(ctx, ws.ID)
	if err != nil {
		return err
	}
	fg, recs, err := h.status(ctx, session, id)
	if err != nil {
		return err
	}
	before, ok := agentRecord(fg, recs)
	if !ok {
		return errNoAgent
	}
	if agentState(before) == host.Blocked {
		return host.ErrBlocked
	}
	// A paste, not typed text: a newline inside the update must not submit
	// it early. block.event goes straight to the block, past the person's
	// Lua key bindings.
	if err := h.input(ctx, session, id, "paste", map[string]string{"text": text}); err != nil {
		return err
	}
	for _, action := range []string{"press", "release"} {
		if err := h.input(ctx, session, id, "key", map[string]string{"key": "enter", "action": action}); err != nil {
			// The text is in the agent's input already; sending it again
			// would add a second copy.
			return fmt.Errorf("%w: %w", host.ErrUnconfirmed, err)
		}
	}
	return h.confirm(ctx, session, id, before)
}

func (h *Rex) input(ctx context.Context, session, id, kind string, payload any) error {
	return h.call(ctx, "block.event", map[string]any{
		"session_id": session,
		"block_id":   id,
		"event":      map[string]any{"type": kind, "payload": payload},
	}, nil)
}

// confirm waits until the agent reports working or blocked (asking about the
// update) after the paste. A working agent reports again for each step, so a
// newer record is a reaction, not the turn it was already in.
func (h *Rex) confirm(ctx context.Context, session, id string, before record) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return host.ErrUnconfirmed
		case <-tick.C:
		}
		fg, recs, err := h.status(ctx, session, id)
		if err != nil {
			continue
		}
		r, ok := agentRecord(fg, recs)
		if ok && r.UpdatedAt != before.UpdatedAt && agentState(r) != host.Idle {
			return nil
		}
	}
}

// Badge does nothing: Rex has no metadata a client shows next to a block,
// and the block label is the person's own.
func (h *Rex) Badge(context.Context, host.Workspace, host.Badge) error { return nil }

// Notify is unsupported: Rex shows notifications only for what a program
// writes to its own terminal (OSC 9, 777, 7501), never one sent through its
// API. Another enabled host shows it instead.
func (h *Rex) Notify(context.Context, host.Notification) error {
	return errors.ErrUnsupported
}

// Caller is the block the current process runs in.
func Caller() (string, bool) {
	id := os.Getenv("REX_BLOCK")
	if id == "" {
		return "", false
	}
	return name + ":" + id, true
}

// locate is the session and block ID of a workspace. Block calls need the
// session; a block kelpie has not seen yet, like one named with --workspace
// right after it opened, is looked up again.
func (h *Rex) locate(ctx context.Context, wsID string) (session, id string, err error) {
	id = strings.TrimPrefix(wsID, name+":")
	if c, ok := h.cached(id); ok {
		return c.session, id, nil
	}
	if _, err := h.Workspaces(ctx); err != nil {
		return "", "", err
	}
	c, ok := h.cached(id)
	if !ok {
		return "", "", fmt.Errorf("rex block %s not found", id)
	}
	return c.session, id, nil
}

// Watch follows the connection's events. While Rex is unreachable it keeps
// redialing every Poll, and each successful dial resyncs.
func (h *Rex) Watch(ctx context.Context, ev host.Events) error {
	down := false
	for {
		err := h.follow(ctx, ev, func() {
			if down {
				slog.Info("rex connection is back")
			}
			down = false
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errEventsLost) {
			slog.Info("kelpie read rex events too slowly, reconnecting")
			continue
		}
		if !down {
			slog.Warn("rex connection lost; retrying", "err", err, "every", h.Poll)
			down = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(h.Poll):
		}
	}
}

type event struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func (h *Rex) follow(ctx context.Context, ev host.Events, connected func()) error {
	c, err := h.conn(ctx)
	if err != nil {
		return err
	}
	// Listing after attach_all means a change is either in the list or
	// still to come as an event, never lost.
	list, err := h.Workspaces(ctx)
	if err != nil {
		return err
	}
	connected()
	h.report(ev, list)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return c.err
		case raw := <-c.events:
			var e event
			if err := json.Unmarshal(raw, &e); err != nil {
				slog.Warn("unreadable rex event", "err", err)
				continue
			}
			h.handle(ctx, ev, e)
		}
	}
}

func (h *Rex) handle(ctx context.Context, ev host.Events, e event) {
	switch e.Type {
	case "session_event":
	case "session_created":
		var p struct {
			Session struct {
				ID string `json:"session_id"`
			} `json:"session"`
		}
		if json.Unmarshal(e.Payload, &p) == nil {
			h.upsertSession(ctx, ev, p.Session.ID)
		}
		return
	case "session_destroyed":
		var p struct {
			ID string `json:"session_id"`
		}
		if json.Unmarshal(e.Payload, &p) == nil {
			for _, id := range h.forgetSession(p.ID) {
				ev.Remove(name, name+":"+id)
			}
		}
		return
	default:
		return
	}

	var se struct {
		Session string `json:"session_id"`
		Event   event  `json:"event"`
	}
	if json.Unmarshal(e.Payload, &se) != nil {
		return
	}
	var b struct {
		Block   string          `json:"block_id"`
		Creator string          `json:"creator_name"`
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(se.Event.Payload, &b) != nil || b.Block == "" {
		return
	}
	switch se.Event.Type {
	case "block_created":
		// The event names no creator; only terminals are workspaces.
		var d struct {
			Block block `json:"block"`
		}
		err := h.call(ctx, "session.describe_block", map[string]string{"session_id": se.Session, "block_id": b.Block}, &d)
		if err == nil && d.Block.Creator == terminal {
			h.upsert(ctx, ev, se.Session, b.Block, d.Block.HostID)
		}
	case "block_closed":
		if h.forget(b.Block) {
			ev.Remove(name, name+":"+b.Block)
		}
	case "block_event":
		if b.Creator != terminal {
			return
		}
		h.blockEvent(ctx, ev, se.Session, b.Block, b.Name, b.Payload)
	}
}

func (h *Rex) blockEvent(ctx context.Context, ev host.Events, session, id, kind string, payload json.RawMessage) {
	c, known := h.cached(id)
	switch kind {
	case "process_changed", "pwd_changed":
		// The foreground process changed or moved to another folder: an
		// agent starting or leaving, or a cd.
		h.upsert(ctx, ev, session, id, "")
	case "title_changed":
		// Agents retitle their terminal many times a minute; this is only
		// the chance to notice a git checkout, which the 30 s cache bounds.
		if known && time.Since(c.at) >= 30*time.Second {
			h.upsert(ctx, ev, session, id, "")
		}
	case "program_status_changed":
		var p struct {
			Record record `json:"record"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return
		}
		// An agent reports its status only after it started, so the
		// process_changed of its start found no record yet.
		if !known || !c.ws.Agent {
			h.upsert(ctx, ev, session, id, "")
		}
		if agentState(p.Record) == host.Idle {
			ev.AgentReady(name, name+":"+id)
		}
	case "program_status_removed":
		if known && c.ws.Agent {
			h.upsert(ctx, ev, session, id, "")
		}
	}
}

func (h *Rex) upsert(ctx context.Context, ev host.Events, session, id, hostID string) {
	ws, err := h.workspace(ctx, session, id, hostID)
	if err != nil {
		slog.Debug("rex block not read", "block", id, "err", err)
		return
	}
	ev.Upsert(ws)
}

func (h *Rex) upsertSession(ctx context.Context, ev host.Events, session string) {
	list, err := h.sessionWorkspaces(ctx, session)
	if err != nil {
		slog.Warn("list rex blocks failed", "session", session, "err", err)
		return
	}
	for _, ws := range list {
		ev.Upsert(ws)
	}
}

func (h *Rex) cached(id string) (cachedBlock, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.blocks[id]
	return c, ok
}

// forget reports whether the block was known, so a block closed along with
// its session is removed once.
func (h *Rex) forget(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.blocks[id]
	delete(h.blocks, id)
	return ok
}

func (h *Rex) forgetSession(session string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var gone []string
	for id, c := range h.blocks {
		if c.session == session {
			gone = append(gone, id)
			delete(h.blocks, id)
		}
	}
	return gone
}

// report drops cached blocks that are gone and hands over the full list.
func (h *Rex) report(ev host.Events, list []host.Workspace) {
	live := map[string]bool{}
	for _, ws := range list {
		live[strings.TrimPrefix(ws.ID, name+":")] = true
	}
	h.mu.Lock()
	maps.DeleteFunc(h.blocks, func(id string, _ cachedBlock) bool { return !live[id] })
	h.mu.Unlock()
	ev.Resync(name, list)
}
