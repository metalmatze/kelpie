package herdr

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
	"slices"
	"strings"
	"time"

	"github.com/metalmatze/kelpie/internal/host"
)

// paneEvents never change, so this subscription is never rebuilt and pane
// lifecycle events have no gaps while it is up.
var paneEvents = []subscription{
	{Type: "pane.created"},
	{Type: "pane.updated"},
	{Type: "pane.closed"},
	{Type: "pane.moved"},
	{Type: "pane.agent_detected"},
}

type subscription struct {
	Type   string `json:"type"`
	PaneID string `json:"pane_id,omitzero"`
}

// Watch follows herdr's event stream from its socket; herdr's CLI has no verb
// for it. While the socket is unreachable it polls herdr pane list every Poll
// and keeps trying to reconnect.
func (h *Herdr) Watch(ctx context.Context, ev host.Events) error {
	ids := make(chan []string, 1)
	go h.followStatus(ctx, ids, ev)
	down := false
	for {
		err := h.follow(ctx, ev, ids, func() {
			if down {
				slog.Info("herdr event stream is back")
			}
			down = false
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !down {
			slog.Warn("herdr event stream lost; polling until it is back", "err", err, "every", h.Poll)
			down = true
		}
		h.resync(ctx, ev, ids)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(h.Poll):
		}
	}
}

func (h *Herdr) follow(ctx context.Context, ev host.Events, ids chan []string, connected func()) error {
	conn, r, err := h.subscribe(ctx, paneEvents)
	if err != nil {
		return err
	}
	defer conn.Close()
	connected()
	// Resync only once the subscription is live, so no change falls between
	// the list and the first event.
	h.resync(ctx, ev, ids)

	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return err
		}
		var msg struct {
			Event string `json:"event"`
			Data  struct {
				Pane           paneInfo `json:"pane"`
				PaneID         string   `json:"pane_id"`
				PreviousPaneID string   `json:"previous_pane_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			slog.Warn("unreadable herdr event", "err", err)
			continue
		}
		d := msg.Data
		switch msg.Event {
		case "pane_created":
			ev.Upsert(h.workspace(ctx, d.Pane))
			h.publishIDs(ids)
		case "pane_updated":
			ev.Upsert(h.workspace(ctx, d.Pane))
		case "pane_closed":
			h.forget(d.PaneID)
			ev.Remove(h.Name(), d.PaneID)
			h.publishIDs(ids)
		case "pane_moved":
			h.forget(d.PreviousPaneID)
			ev.Move(h.Name(), d.PreviousPaneID, h.workspace(ctx, d.Pane))
			h.publishIDs(ids)
		case "pane_agent_detected":
			ev.AgentReady(h.Name(), d.PaneID)
		}
	}
}

// resync lists all panes, drops cached panes that are gone and reports the
// full list.
func (h *Herdr) resync(ctx context.Context, ev host.Events, ids chan []string) {
	list, err := h.Workspaces(ctx)
	if err != nil {
		slog.Warn("list herdr panes failed", "err", err)
		return
	}
	live := map[string]bool{}
	for _, ws := range list {
		live[ws.ID] = true
	}
	h.mu.Lock()
	maps.DeleteFunc(h.panes, func(id string, _ cachedPane) bool { return !live[id] })
	h.mu.Unlock()
	ev.Resync(h.Name(), list)
	h.publishIDs(ids)
}

// publishIDs hands the current pane IDs to followStatus when they changed,
// replacing a value it has not picked up yet.
func (h *Herdr) publishIDs(ch chan []string) {
	h.mu.Lock()
	ids := slices.Sorted(maps.Keys(h.panes))
	key := strings.Join(ids, " ")
	if key == h.statusKey {
		h.mu.Unlock()
		return
	}
	h.statusKey = key
	h.mu.Unlock()
	select {
	case <-ch:
	default:
	}
	ch <- ids
}

// followStatus keeps one connection subscribed to pane.agent_status_changed
// for every pane. herdr takes one subscription per connection and requires a
// pane ID for this event, so the connection is rebuilt whenever panes come or
// go. Status events missed while it reconnects only delay a queued delivery
// until the router's retry.
func (h *Herdr) followStatus(ctx context.Context, ch <-chan []string, ev host.Events) {
	stop := func() {}
	defer func() { stop() }()
	for {
		select {
		case <-ctx.Done():
			return
		case ids := <-ch:
			stop()
			sctx, cancel := context.WithCancel(ctx)
			stop = cancel
			go h.statusLoop(sctx, ids, ev)
		}
	}
}

func (h *Herdr) statusLoop(ctx context.Context, ids []string, ev host.Events) {
	if len(ids) == 0 {
		return
	}
	subs := make([]subscription, len(ids))
	for i, id := range ids {
		subs[i] = subscription{Type: "pane.agent_status_changed", PaneID: id}
	}
	for ctx.Err() == nil {
		conn, r, err := h.subscribe(ctx, subs)
		if err == nil {
			err = readStatus(r, func(id string) { ev.AgentReady(h.Name(), id) })
			conn.Close()
		}
		if ctx.Err() != nil {
			return
		}
		slog.Debug("herdr status stream lost, reconnecting", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func readStatus(r *bufio.Reader, ready func(id string)) error {
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return err
		}
		var msg struct {
			Event string `json:"event"`
			Data  struct {
				PaneID      string `json:"pane_id"`
				AgentStatus string `json:"agent_status"`
			} `json:"data"`
		}
		// herdr names this one with dots (pane.agent_status_changed) while
		// its other events use underscores (pane_created); accept both.
		if json.Unmarshal(line, &msg) != nil || strings.ReplaceAll(msg.Event, ".", "_") != "pane_agent_status_changed" {
			continue
		}
		// done is idle with an unseen completion; both take input.
		if s := msg.Data.AgentStatus; s == "idle" || s == "done" {
			ready(msg.Data.PaneID)
		}
	}
}

// subscribe opens a connection and starts one events.subscribe on it. A
// second subscribe on the same connection makes herdr close it.
func (h *Herdr) subscribe(ctx context.Context, subs []subscription) (net.Conn, *bufio.Reader, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", h.socketPath())
	if err != nil {
		return nil, nil, err
	}
	context.AfterFunc(ctx, func() { conn.Close() })
	req, _ := json.Marshal(map[string]any{
		"id":     "kelpie",
		"method": "events.subscribe",
		"params": map[string]any{"subscriptions": subs},
	})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		conn.Close()
		return nil, nil, err
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	var resp struct {
		Result struct {
			Type string `json:"type"`
		} `json:"result"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("events.subscribe: %w", err)
	}
	if resp.Error != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("events.subscribe: %s: %s", resp.Error.Code, resp.Error.Message)
	}
	if resp.Result.Type != "subscription_started" {
		conn.Close()
		return nil, nil, errors.New("events.subscribe: unexpected answer " + strings.TrimSpace(string(line)))
	}
	return conn, r, nil
}

func (h *Herdr) socketPath() string {
	home, _ := os.UserHomeDir()
	return cmp.Or(h.Socket, os.Getenv("HERDR_SOCKET_PATH"), filepath.Join(home, ".config", "herdr", "herdr.sock"))
}
