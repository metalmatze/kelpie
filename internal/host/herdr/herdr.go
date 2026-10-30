// Package herdr is the herdr host, built on the herdr CLI.
package herdr

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	statusKey string // pane IDs the status subscription was last built for
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

func (h *Herdr) forget(id string) {
	h.mu.Lock()
	delete(h.panes, id)
	h.mu.Unlock()
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
func (h *Herdr) Deliver(ctx context.Context, ws host.Workspace, text string) error {
	return h.run(ctx, nil, "agent", "prompt", ws.ID, text)
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
			if e.Error.Code == "agent_blocked" {
				return host.ErrBlocked
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
