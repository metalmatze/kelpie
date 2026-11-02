// Package herdr is the herdr host, built on the herdr CLI.
package herdr

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/metalmatze/kelpie/internal/host"
)

type Herdr struct {
	Bin    string
	Socket string        // empty: $HERDR_SOCKET_PATH, else herdr's default
	Poll   time.Duration // fallback interval while the event socket is down

	mu        sync.Mutex
	panes     map[string]cachedPane
	statusKey string                // pane IDs the status subscription was last built for
	badges    map[string]host.Badge // per pane
	spaces    map[string]string     // herdr workspace -> its $kelpie token as last reported
}

type cachedPane struct {
	ws host.Workspace
	at time.Time
}

func (h *Herdr) Name() string { return "herdr" }

type paneInfo struct {
	PaneID        string `json:"pane_id"`
	Agent         string `json:"agent"`
	AgentStatus   string `json:"agent_status"`
	Cwd           string `json:"cwd"`
	ForegroundCwd string `json:"foreground_cwd"`
}

// Workspaces are all panes, not only those running an agent: a watch must
// survive the agent restarting in its pane, and ends only when the pane
// closes. Delivery to a pane without an agent fails and is retried.
func (h *Herdr) Workspaces(ctx context.Context) ([]host.Workspace, error) {
	var resp struct {
		Result struct {
			Panes []paneInfo `json:"panes"`
		} `json:"result"`
	}
	if err := h.run(ctx, &resp, "pane", "list"); err != nil {
		return nil, err
	}
	out := make([]host.Workspace, 0, len(resp.Result.Panes))
	for _, p := range resp.Result.Panes {
		out = append(out, h.workspace(ctx, p))
	}
	return out, nil
}

// workspace turns a pane into a workspace. Git runs when the pane's directory
// changed or the cached result is 30 s old, not on every update: herdr reports
// pane_updated for each terminal title change, and an agent's spinner changes
// the title many times a minute. The age bound catches a git checkout.
func (h *Herdr) workspace(ctx context.Context, p paneInfo) host.Workspace {
	path := cmp.Or(p.ForegroundCwd, p.Cwd)
	h.mu.Lock()
	c, ok := h.panes[p.PaneID]
	h.mu.Unlock()
	if ok && c.ws.Path == path && time.Since(c.at) < 30*time.Second {
		return c.ws
	}
	branch, repos := host.GitInfo(ctx, path)
	ws := host.Workspace{Host: h.Name(), ID: p.PaneID, Repos: repos, Branch: branch, Path: path}
	h.mu.Lock()
	if h.panes == nil {
		h.panes = map[string]cachedPane{}
	}
	h.panes[p.PaneID] = cachedPane{ws: ws, at: time.Now()}
	h.mu.Unlock()
	return ws
}

// forget drops a pane that closed or moved, and its share of its
// workspace's badge.
func (h *Herdr) forget(ctx context.Context, id string) {
	h.mu.Lock()
	delete(h.panes, id)
	_, badged := h.badges[id]
	delete(h.badges, id)
	h.mu.Unlock()
	if badged {
		if err := h.reportSpace(ctx, spaceOf(id)); err != nil {
			slog.Warn("workspace badge not updated", "workspace", spaceOf(id), "err", err)
		}
	}
}

func (h *Herdr) State(ctx context.Context, ws host.Workspace) (host.AgentState, error) {
	var resp struct {
		Result struct {
			Agent paneInfo `json:"agent"`
		} `json:"result"`
	}
	if err := h.run(ctx, &resp, "agent", "get", ws.ID); err != nil {
		return host.Unknown, err
	}
	switch s := host.AgentState(resp.Result.Agent.AgentStatus); s {
	case host.Idle, host.Working, host.Blocked:
		return s, nil
	case "done":
		return host.Idle, nil
	}
	return host.Unknown, nil
}

// Deliver relies on herdr refusing a blocked agent with agent_blocked before
// typing anything; a working agent gets the text queued by its own TUI.
// --wait confirms the agent reacted: working or blocked (asking about the
// update) ends the wait at once, instead of at the end of the agent's turn.
func (h *Herdr) Deliver(ctx context.Context, ws host.Workspace, text string) error {
	return h.run(ctx, nil, "agent", "prompt", ws.ID, text,
		"--wait", "--until", "working", "--until", "blocked", "--timeout", "15000")
}

// Badge sets the token $kelpie on the pane, and on its herdr workspace the
// subjects all of the workspace's panes watch, for the Spaces sidebar.
// Tokens show only where the sidebar layout in herdr's config.toml lists
// them. herdr forgets them when its server restarts; the daemon reports them
// again after a resync.
func (h *Herdr) Badge(ctx context.Context, ws host.Workspace, b host.Badge) error {
	if err := h.token(ctx, "pane", ws.ID, b.String()); err != nil {
		return err
	}
	h.mu.Lock()
	if h.badges == nil {
		h.badges = map[string]host.Badge{}
	}
	if b.String() == "" {
		delete(h.badges, ws.ID)
	} else {
		h.badges[ws.ID] = b
	}
	h.mu.Unlock()
	return h.reportSpace(ctx, spaceOf(ws.ID))
}

// reportSpace sets a herdr workspace's token to the subjects its panes
// watch, without reminders: those belong to one agent.
func (h *Herdr) reportSpace(ctx context.Context, space string) error {
	h.mu.Lock()
	var subjects []string
	for id, b := range h.badges {
		if spaceOf(id) == space {
			subjects = append(subjects, b.Subjects...)
		}
	}
	slices.Sort(subjects)
	text := host.Badge{Subjects: slices.Compact(subjects)}.String()
	old, ok := h.spaces[space]
	h.mu.Unlock()
	if ok && old == text || !ok && text == "" {
		return nil
	}
	if err := h.token(ctx, "workspace", space, text); err != nil {
		return err
	}
	h.mu.Lock()
	if h.spaces == nil {
		h.spaces = map[string]string{}
	}
	h.spaces[space] = text
	h.mu.Unlock()
	return nil
}

func (h *Herdr) token(ctx context.Context, kind, id, text string) error {
	args := []string{kind, "report-metadata", id, "--source", "kelpie"}
	if text == "" {
		args = append(args, "--clear-token", "kelpie")
	} else {
		args = append(args, "--token", "kelpie="+text)
	}
	return h.run(ctx, nil, args...)
}

// Notify uses herdr's own notifications, which follow its ui.toast and
// ui.sound settings.
func (h *Herdr) Notify(ctx context.Context, n host.Notification) error {
	sound := "done"
	if n.NeedsYou {
		sound = "request"
	}
	return h.run(ctx, nil, "notification", "show", n.Title, "--body", n.Body, "--sound", sound)
}

// spaceOf is the herdr workspace of a pane: pane IDs are workspace-qualified
// (w3:p1), and a pane moved to another workspace gets a new ID.
func spaceOf(pane string) string {
	space, _, _ := strings.Cut(pane, ":")
	return space
}

type cliError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *Herdr) run(ctx context.Context, into any, args ...string) error {
	cmd := exec.CommandContext(ctx, h.Bin, args...)
	if h.Socket != "" {
		cmd.Env = append(os.Environ(), "HERDR_SOCKET_PATH="+h.Socket)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var e cliError
		if json.Unmarshal(stderr.Bytes(), &e) == nil && e.Error.Code != "" {
			switch e.Error.Code {
			case "agent_blocked":
				return host.ErrBlocked
			case "agent_prompt_stalled", "timeout": // agent prompt --wait
				return fmt.Errorf("%w: %s", host.ErrUnconfirmed, e.Error.Message)
			}
			return fmt.Errorf("herdr %s: %s: %s", strings.Join(args[:2], " "), e.Error.Code, e.Error.Message)
		}
		return fmt.Errorf("herdr %s: %w: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(stderr.String()))
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(stdout.Bytes(), into)
}
