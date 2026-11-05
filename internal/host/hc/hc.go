// Package hc is the host for herdr-core, the terminal runtime behind the
// Herdr desktop app. It shares nothing with the herdr TUI but the name:
// terminals (t7) instead of panes, and a JSON-RPC API on a unix socket
// instead of CLI verbs.
//
// kelpie dials the socket itself rather than going through `hc connect` or
// any other hc command: those start a core when none runs, so a daemon
// retrying every Poll would bring back a core the person stopped on
// purpose, and the next core start turns kept terminals into fresh shells.
package hc

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/metalmatze/kelpie/internal/host"
)

const name = "hc"

type HC struct {
	Socket string        // empty: see socketPath
	Poll   time.Duration // interval to list terminals while the event stream is down

	mu    sync.Mutex
	terms map[string]cachedTerm // by hc address
}

type cachedTerm struct {
	ws host.Workspace
	at time.Time
}

func (h *HC) Name() string { return name }

type terminal struct {
	Address    string      `json:"address"`
	State      string      `json:"state"` // live | finished
	Cwd        string      `json:"cwd"`   // where it started
	Foreground *foreground `json:"foreground"`
	Status     *status     `json:"status"`
}

type foreground struct {
	Cwd string `json:"cwd"`
}

type status struct {
	State string `json:"state"`
}

// Workspaces are all live terminals, not only those reporting an agent: a
// watch must outlive the agent restarting in its terminal. Unlabeled workers
// that Herdr does not show are terminals too, and an agent may run in one.
func (h *HC) Workspaces(ctx context.Context) ([]host.Workspace, error) {
	var resp struct {
		Terminals []terminal `json:"terminals"`
	}
	if err := h.call(ctx, "terminal.list", map[string]any{}, &resp); err != nil {
		return nil, err
	}
	return h.workspaces(ctx, resp.Terminals), nil
}

func (h *HC) workspaces(ctx context.Context, list []terminal) []host.Workspace {
	out := make([]host.Workspace, 0, len(list))
	for _, t := range list {
		if t.State == "live" {
			out = append(out, h.workspace(ctx, t))
		}
	}
	return out
}

// workspace runs git only when the terminal's folder changed or the cached
// result is 30 s old: status and title events arrive many times a minute
// while an agent works, and the age bound is what catches a git checkout.
// A terminal on another machine (vps/t7) keeps no git info, its folder is
// not on this one.
func (h *HC) workspace(ctx context.Context, t terminal) host.Workspace {
	path := t.Cwd
	if t.Foreground != nil {
		path = cmp.Or(t.Foreground.Cwd, path)
	}
	id := name + ":" + t.Address
	h.mu.Lock()
	c, ok := h.terms[t.Address]
	h.mu.Unlock()
	if ok && c.ws.Path == path && time.Since(c.at) < 30*time.Second {
		return c.ws
	}
	ws := host.Workspace{Host: name, ID: id, Path: path}
	if !strings.Contains(t.Address, "/") {
		ws.Branch, ws.Repos = host.GitInfo(ctx, path)
	}
	h.mu.Lock()
	if h.terms == nil {
		h.terms = map[string]cachedTerm{}
	}
	h.terms[t.Address] = cachedTerm{ws: ws, at: time.Now()}
	h.mu.Unlock()
	return ws
}

func (h *HC) State(ctx context.Context, ws host.Workspace) (host.AgentState, error) {
	var t terminal
	if err := h.call(ctx, "terminal.get", map[string]any{"terminal": address(ws.ID)}, &t); err != nil {
		return host.Unknown, err
	}
	if t.Status == nil {
		return host.Unknown, nil
	}
	switch s := host.AgentState(t.Status.State); s {
	case host.Idle, host.Working, host.Blocked:
		return s, nil
	case "done":
		return host.Idle, nil
	}
	return host.Unknown, nil
}

// Deliver relies on the core refusing a blocked program before typing; a
// working agent gets the text queued by its own TUI, as in herdr. The core
// pastes only once nobody has typed for ~3 s, so a person mid-sentence is
// not interleaved. started is false when the agent did not start working
// within a few seconds, null for a program that reports no status at all.
func (h *HC) Deliver(ctx context.Context, ws host.Workspace, text string) error {
	var resp struct {
		Started *bool `json:"started"`
	}
	if err := h.call(ctx, "terminal.send", map[string]any{"terminal": address(ws.ID), "text": text}, &resp); err != nil {
		return err
	}
	if resp.Started != nil && !*resp.Started {
		return host.ErrUnconfirmed
	}
	return nil
}

// Badge sets the label kelpie on the terminal. Herdr draws no labels by
// itself; a mod can read them from terminal.list or the event stream.
// Labels live in the core, so they survive the app restarting.
func (h *HC) Badge(ctx context.Context, ws host.Workspace, b host.Badge) error {
	return h.call(ctx, "terminal.label", map[string]any{
		"terminal": address(ws.ID),
		"labels":   map[string]string{"kelpie": b.String()},
	}, nil)
}

// Notify is unsupported: Herdr posts toasts only from mods, and the core has
// no notification at all. Another enabled host shows it instead.
func (h *HC) Notify(context.Context, host.Notification) error {
	return errors.ErrUnsupported
}

// Caller is the terminal the current process runs in.
func Caller() (string, bool) {
	id := os.Getenv("HC_TERMINAL_ID")
	if id == "" {
		return "", false
	}
	return name + ":" + id, true
}

func address(id string) string {
	return strings.TrimPrefix(id, name+":")
}

type rpcError struct {
	Message string `json:"message"`
	Data    struct {
		Kind string `json:"kind"`
		Hint string `json:"hint"`
	} `json:"data"`
}

// conn is one JSON-RPC session on the core's socket.
type conn struct {
	net.Conn
	out *bufio.Reader
}

func (h *HC) connect(ctx context.Context) (*conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", h.socketPath())
	if err != nil {
		return nil, err
	}
	context.AfterFunc(ctx, func() { c.Close() })
	return &conn{Conn: c, out: bufio.NewReader(c)}, nil
}

// socketPath follows hc's own lookup, minus the socket key of hc's
// config.toml, which kelpie's own socket setting stands in for.
// HC_TERMINAL_SOCKET is the core of the terminal kelpie runs in.
func (h *HC) socketPath() string {
	if p := cmp.Or(h.Socket, os.Getenv("HC_SOCKET"), os.Getenv("HC_TERMINAL_SOCKET")); p != "" {
		return p
	}
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "state")
	}
	session := cmp.Or(os.Getenv("HC_SESSION"), "default")
	return filepath.Join(dir, "herdr-core", session, session+".sock")
}

func (c *conn) request(method string, params any) error {
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	_, err := c.Write(append(req, '\n'))
	return err
}

// response reads the answer to the one request on this connection.
func (c *conn) response(method string, into any) error {
	line, err := c.out.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("hc %s: %w", method, err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("hc %s: %w", method, err)
	}
	if e := resp.Error; e != nil {
		if e.Data.Kind == "blocked" {
			return fmt.Errorf("%w: %s", host.ErrBlocked, e.Message)
		}
		return fmt.Errorf("hc %s: %s: %s", method, e.Data.Kind, e.Message)
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(resp.Result, into)
}

func (h *HC) call(ctx context.Context, method string, params, into any) error {
	c, err := h.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.request(method, params); err != nil {
		return err
	}
	return c.response(method, into)
}

// Watch follows the core's event stream. While it is down it lists the
// terminals every Poll and keeps trying to reconnect.
func (h *HC) Watch(ctx context.Context, ev host.Events) error {
	down := false
	for {
		err := h.follow(ctx, ev, func() {
			if down {
				slog.Info("hc event stream is back")
			}
			down = false
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !down {
			slog.Warn("hc event stream lost; polling until it is back", "err", err, "every", h.Poll)
			down = true
		}
		h.resync(ctx, ev)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(h.Poll):
		}
	}
}

func (h *HC) follow(ctx context.Context, ev host.Events, connected func()) error {
	c, err := h.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.request("events.subscribe", map[string]any{}); err != nil {
		return err
	}
	// The answer is a snapshot that the events continue from, so no change
	// falls between listing and subscribing.
	var snap struct {
		Terminals []terminal `json:"terminals"`
	}
	if err := c.response("events.subscribe", &snap); err != nil {
		return err
	}
	connected()
	h.report(ev, h.workspaces(ctx, snap.Terminals))

	for {
		line, err := c.out.ReadBytes('\n')
		if err != nil {
			return err
		}
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Type     string          `json:"type"`
				Terminal string          `json:"terminal"`
				Data     json.RawMessage `json:"data"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			slog.Warn("unreadable hc event", "err", err)
			continue
		}
		switch msg.Method {
		case "event":
		case "missed":
			// kelpie read too slowly and the core dropped events; a fresh
			// list replaces whatever they would have said.
			slog.Info("hc dropped events for kelpie, listing terminals again")
			h.resync(ctx, ev)
			continue
		default: // heartbeat
			continue
		}
		p := msg.Params
		switch p.Type {
		case "created":
			var t terminal
			if json.Unmarshal(p.Data, &t) == nil && t.State == "live" {
				ev.Upsert(h.workspace(ctx, t))
			}
		case "foreground", "title":
			// foreground is the cwd change; title is frequent and the chance
			// to notice a git checkout. Both re-read only a known terminal.
			var fg foreground
			_ = json.Unmarshal(p.Data, &fg)
			if ws, ok := h.known(p.Terminal); ok {
				t := terminal{Address: p.Terminal, Cwd: ws.Path}
				if p.Type == "foreground" && fg.Cwd != "" {
					t.Foreground = &fg
				}
				ev.Upsert(h.workspace(ctx, t))
			}
		case "restarted":
			// A kept terminal lost its program and got a shell, same address.
			var t terminal
			if err := h.call(ctx, "terminal.get", map[string]any{"terminal": p.Terminal}, &t); err == nil {
				ev.Upsert(h.workspace(ctx, t))
			}
		case "finished", "removed":
			if h.forget(p.Terminal) {
				ev.Remove(name, name+":"+p.Terminal)
			}
		case "status":
			var d struct {
				Status *status `json:"status"`
			}
			// done is idle with an unseen completion; both take input.
			if json.Unmarshal(p.Data, &d) == nil && d.Status != nil && (d.Status.State == "idle" || d.Status.State == "done") {
				ev.AgentReady(name, name+":"+p.Terminal)
			}
		}
	}
}

func (h *HC) known(addr string) (host.Workspace, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.terms[addr]
	return c.ws, ok
}

// forget reports whether the terminal was known, so finished followed by
// removed closes the workspace once.
func (h *HC) forget(addr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.terms[addr]
	delete(h.terms, addr)
	return ok
}

func (h *HC) resync(ctx context.Context, ev host.Events) {
	list, err := h.Workspaces(ctx)
	if err != nil {
		slog.Warn("list hc terminals failed", "err", err)
		return
	}
	h.report(ev, list)
}

// report drops cached terminals that are gone and hands over the full list.
func (h *HC) report(ev host.Events, list []host.Workspace) {
	live := map[string]bool{}
	for _, ws := range list {
		live[address(ws.ID)] = true
	}
	h.mu.Lock()
	maps.DeleteFunc(h.terms, func(addr string, _ cachedTerm) bool { return !live[addr] })
	h.mu.Unlock()
	ev.Resync(name, list)
}
