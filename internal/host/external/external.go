// Package external runs out-of-tree host binaries registered under hosts:.
// kelpie talks to them only through argv, stdin and stdout, so a host
// integration can live in its own repo and language without sharing Go types.
package external

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/metalmatze/kelpie/internal/host"
)

// SchemaVersion is the newest host describe output this kelpie reads.
const SchemaVersion = 1

// Timeouts per subcommand. Vars so tests need not wait for them.
var (
	DescribeTimeout = 10 * time.Second
	CallerTimeout   = 10 * time.Second
	CallTimeout     = 15 * time.Second
)

// Description is what an external host binary's describe prints.
type Description struct {
	Kelpie int    `yaml:"kelpie"`
	Name   string `yaml:"name"`
	// ReportsAgent tells whether workspaces[].agent distinguishes an agent
	// from a plain shell, for branch-binding tie-breaks.
	ReportsAgent bool `yaml:"reports_agent,omitempty"`
	// TermProgram matches $TERM_PROGRAM to win caller detection when nested
	// inside another host.
	TermProgram string `yaml:"term_program,omitempty"`
	// CallerEnv lists environment variables of which at least one must be
	// non-empty before kelpie runs the caller subcommand. Empty means always
	// run caller.
	CallerEnv []string `yaml:"caller_env,flow,omitempty"`
	// Capabilities lists optional subcommands the binary implements:
	// "badge", "notify".
	Capabilities []string `yaml:"capabilities,flow,omitempty"`
}

func (d Description) HasCapability(cap string) bool {
	return slices.Contains(d.Capabilities, cap)
}

// Describe runs `<command> describe` and parses its YAML or JSON output.
func Describe(ctx context.Context, command []string) (Description, error) {
	stdout, stderr, err := execCmd(ctx, DescribeTimeout, command, nil, "describe")
	if err != nil {
		return Description{}, formatErr(command, "describe", DescribeTimeout, ctx, stderr, stdout, err)
	}
	d, err := ParseDescription(stdout)
	if err != nil {
		return Description{}, fmt.Errorf("describe: %w", err)
	}
	return d, nil
}

// ParseDescription reads the schema version first so a newer host schema fails
// cleanly instead of decoding partially. Unknown fields are allowed under the
// current version for forward compatibility.
func ParseDescription(out []byte) (Description, error) {
	var v struct {
		Kelpie int `yaml:"kelpie"`
	}
	if err := yaml.Unmarshal(out, &v); err != nil {
		return Description{}, err
	}
	switch {
	case v.Kelpie == 0:
		return Description{}, errors.New("no kelpie: version")
	case v.Kelpie > SchemaVersion:
		return Description{}, fmt.Errorf("it speaks kelpie: %d, this kelpie reads up to %d; update kelpie", v.Kelpie, SchemaVersion)
	}
	var d Description
	if err := yaml.NewDecoder(bytes.NewReader(out)).Decode(&d); err != nil {
		return Description{}, err
	}
	if d.Name == "" {
		return Description{}, errors.New("no name")
	}
	return d, nil
}

// Caller asks an external host binary whether the current shell runs inside
// one of its workspaces. When callerEnv is non-empty, the binary is skipped
// unless at least one listed variable is set.
func Caller(ctx context.Context, command []string, name string, callerEnv []string) (string, bool) {
	if len(callerEnv) > 0 && !slices.ContainsFunc(callerEnv, func(k string) bool { return os.Getenv(k) != "" }) {
		return "", false
	}
	stdout, stderr, err := execCmd(ctx, CallerTimeout, command, nil, "caller")
	if err != nil {
		slog.Debug("external host caller did not match", "host", name, "err", err, "stderr", strings.TrimSpace(string(stderr)))
		return "", false
	}
	id := strings.TrimSpace(string(stdout))
	if id == "" || strings.ContainsAny(id, "\r\n") {
		return "", false
	}
	return name + ":" + id, true
}

// Host implements host.Host by invoking an external binary.
type Host struct {
	name         string
	command      []string
	poll         time.Duration
	capabilities []string

	mu     sync.Mutex
	git    map[string]cachedGit
	states map[string]host.AgentState
	down   bool
}

var _ host.Host = (*Host)(nil)

type cachedGit struct {
	branch string
	repos  []string
	at     time.Time
}

// New constructs an external Host for a registered binary.
func New(name string, command []string, poll time.Duration, capabilities []string) *Host {
	if poll <= 0 {
		poll = 15 * time.Second
	}
	return &Host{
		name:         name,
		command:      slices.Clone(command),
		poll:         poll,
		capabilities: slices.Clone(capabilities),
	}
}

func (h *Host) Name() string { return h.name }

type workspaceJSON struct {
	ID     string   `json:"id"`
	Path   string   `json:"path"`
	Branch string   `json:"branch"`
	Repos  []string `json:"repos"`
	Agent  bool     `json:"agent"`
	State  string   `json:"state"`
}

func (h *Host) Workspaces(ctx context.Context) ([]host.Workspace, error) {
	list, _, err := h.list(ctx)
	return list, err
}

func (h *Host) list(ctx context.Context) ([]host.Workspace, map[string]host.AgentState, error) {
	stdout, stderr, err := execCmd(ctx, CallTimeout, h.command, nil, "workspaces")
	if err != nil {
		return nil, nil, formatErr(h.command, "workspaces", CallTimeout, ctx, stderr, stdout, err)
	}
	var rows []workspaceJSON
	if err := json.Unmarshal(stdout, &rows); err != nil {
		return nil, nil, fmt.Errorf("%s workspaces: %w", h.name, err)
	}
	out := make([]host.Workspace, 0, len(rows))
	states := make(map[string]host.AgentState, len(rows))
	livePaths := make(map[string]bool, len(rows))
	for i, w := range rows {
		if w.ID == "" {
			return nil, nil, fmt.Errorf("%s workspaces[%d]: id is required", h.name, i)
		}
		branch, repos := w.Branch, w.Repos
		if w.Path != "" {
			livePaths[w.Path] = true
			if branch == "" || len(repos) == 0 {
				gb, gr := h.gitInfo(ctx, w.Path)
				if branch == "" {
					branch = gb
				}
				if len(repos) == 0 {
					repos = gr
				}
			}
		}
		id := h.name + ":" + w.ID
		out = append(out, host.Workspace{
			Host:   h.name,
			ID:     id,
			Repos:  repos,
			Branch: branch,
			Path:   w.Path,
			Agent:  w.Agent,
		})
		if w.State != "" {
			states[id] = parseState(w.State)
		}
	}
	h.mu.Lock()
	for p := range h.git {
		if !livePaths[p] {
			delete(h.git, p)
		}
	}
	h.mu.Unlock()
	return out, states, nil
}

// gitInfo caches branch and GitHub remotes per path for 30 s so frequent
// workspace polls do not re-run git on every tick.
func (h *Host) gitInfo(ctx context.Context, path string) (string, []string) {
	h.mu.Lock()
	c, ok := h.git[path]
	h.mu.Unlock()
	if ok && time.Since(c.at) < 30*time.Second {
		return c.branch, c.repos
	}
	branch, repos := host.GitInfo(ctx, path)
	h.mu.Lock()
	if h.git == nil {
		h.git = map[string]cachedGit{}
	}
	h.git[path] = cachedGit{branch: branch, repos: repos, at: time.Now()}
	h.mu.Unlock()
	return branch, repos
}

// Watch polls workspaces every poll interval, resyncs the daemon and emits
// AgentReady whenever a workspace's reported state turns idle.
func (h *Host) Watch(ctx context.Context, ev host.Events) error {
	for {
		h.pollOnce(ctx, ev)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(h.poll):
		}
	}
}

func (h *Host) pollOnce(ctx context.Context, ev host.Events) {
	list, now, err := h.list(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		h.mu.Lock()
		wasDown := h.down
		h.down = true
		h.mu.Unlock()
		if !wasDown {
			slog.Warn("external host unavailable; its workspaces keep their watches until it is back", "host", h.name, "err", err, "every", h.poll)
		}
		return
	}
	h.mu.Lock()
	wasDown := h.down
	h.down = false
	h.mu.Unlock()
	if wasDown {
		slog.Info("external host is back", "host", h.name)
	}
	ev.Resync(h.name, list)
	h.mu.Lock()
	prev := h.states
	h.states = now
	h.mu.Unlock()
	for id, s := range now {
		if s == host.Idle && prev[id] != host.Idle {
			ev.AgentReady(h.name, id)
		}
	}
}

func (h *Host) State(ctx context.Context, ws host.Workspace) (host.AgentState, error) {
	stdout, stderr, err := execCmd(ctx, CallTimeout, h.command, nil, "state", h.rawID(ws.ID))
	if err != nil {
		return host.Unknown, formatErr(h.command, "state", CallTimeout, ctx, stderr, stdout, err)
	}
	return parseState(strings.TrimSpace(string(stdout))), nil
}

// Deliver passes the prompt on stdin to `<command> deliver <raw-id>`.
// Structured JSON error responses map to host.ErrBlocked, host.ErrBusy or
// host.ErrUnconfirmed.
func (h *Host) Deliver(ctx context.Context, ws host.Workspace, text string) error {
	stdout, stderr, err := execCmd(ctx, 0, h.command, strings.NewReader(text), "deliver", h.rawID(ws.ID))
	if err == nil {
		return nil
	}
	if ce, ok := parseError(stderr, stdout); ok {
		return mapDeliveryError(h.name, ce)
	}
	return formatErr(h.command, "deliver", 0, ctx, stderr, stdout, err)
}

func (h *Host) Badge(ctx context.Context, ws host.Workspace, b host.Badge) error {
	if !slices.Contains(h.capabilities, "badge") {
		return nil
	}
	subjects := b.Subjects
	if subjects == nil {
		subjects = []string{}
	}
	body, err := json.Marshal(struct {
		Text      string   `json:"text"`
		Subjects  []string `json:"subjects"`
		Reminders int      `json:"reminders"`
	}{
		Text:      b.String(),
		Subjects:  subjects,
		Reminders: b.Reminders,
	})
	if err != nil {
		return err
	}
	stdout, stderr, err := execCmd(ctx, CallTimeout, h.command, bytes.NewReader(body), "badge", h.rawID(ws.ID))
	if err == nil {
		return nil
	}
	if ce, ok := parseError(stderr, stdout); ok && ce.Code == "unsupported" {
		return nil
	}
	return formatErr(h.command, "badge", CallTimeout, ctx, stderr, stdout, err)
}

func (h *Host) Notify(ctx context.Context, n host.Notification) error {
	if !slices.Contains(h.capabilities, "notify") {
		return errors.ErrUnsupported
	}
	wsID := n.Workspace.ID
	if n.Workspace.Host == h.name {
		wsID = h.rawID(wsID)
	}
	body, err := json.Marshal(struct {
		Title         string `json:"title"`
		Body          string `json:"body"`
		Workspace     string `json:"workspace,omitempty"`
		WorkspaceHost string `json:"workspace_host,omitempty"`
		NeedsYou      bool   `json:"needs_you"`
	}{
		Title:         n.Title,
		Body:          n.Body,
		Workspace:     wsID,
		WorkspaceHost: n.Workspace.Host,
		NeedsYou:      n.NeedsYou,
	})
	if err != nil {
		return err
	}
	stdout, stderr, err := execCmd(ctx, CallTimeout, h.command, bytes.NewReader(body), "notify")
	if err == nil {
		return nil
	}
	if ce, ok := parseError(stderr, stdout); ok && ce.Code == "unsupported" {
		return errors.ErrUnsupported
	}
	return formatErr(h.command, "notify", CallTimeout, ctx, stderr, stdout, err)
}

func (h *Host) rawID(id string) string {
	return strings.TrimPrefix(id, h.name+":")
}

func parseState(s string) host.AgentState {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "idle", "done":
		return host.Idle
	case "working":
		return host.Working
	case "blocked":
		return host.Blocked
	default:
		return host.Unknown
	}
}

type hostError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func parseError(stderr, stdout []byte) (hostError, bool) {
	for _, b := range [][]byte{stderr, stdout} {
		b = bytes.TrimSpace(b)
		if len(b) == 0 || b[0] != '{' {
			continue
		}
		var e hostError
		if json.Unmarshal(b, &e) == nil && e.Code != "" {
			return e, true
		}
	}
	return hostError{}, false
}

func mapDeliveryError(hostName string, ce hostError) error {
	var sentinel error
	switch ce.Code {
	case "blocked":
		sentinel = host.ErrBlocked
	case "busy":
		sentinel = host.ErrBusy
	case "unconfirmed":
		sentinel = host.ErrUnconfirmed
	default:
		if ce.Message != "" {
			return fmt.Errorf("%s deliver: %s: %s", hostName, ce.Code, ce.Message)
		}
		return fmt.Errorf("%s deliver: %s", hostName, ce.Code)
	}
	if ce.Message != "" {
		return fmt.Errorf("%w: %s", sentinel, ce.Message)
	}
	return sentinel
}

// execCmd runs command with args appended. When timeout > 0, it caps the
// execution duration without extending an earlier deadline on ctx.
func execCmd(ctx context.Context, timeout time.Duration, command []string, stdin io.Reader, args ...string) ([]byte, []byte, error) {
	if len(command) == 0 {
		return nil, nil, errors.New("command is empty")
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	argv := append(command[1:len(command):len(command)], args...)
	cmd := exec.CommandContext(ctx, command[0], argv...)
	cmd.Stdin = stdin
	// A host binary that forks a helper keeping stdout open would otherwise
	// hold Wait past the timeout.
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		return stdout.Bytes(), stderr.Bytes(), ctx.Err()
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func formatErr(command []string, sub string, timeout time.Duration, parentCtx context.Context, stderr, stdout []byte, err error) error {
	name := sub
	if len(command) > 0 {
		name = filepath.Base(command[0]) + " " + sub
	}
	if errors.Is(err, context.DeadlineExceeded) && parentCtx.Err() == nil && timeout > 0 {
		return fmt.Errorf("%s: no answer within %s", name, timeout)
	}
	if s := strings.TrimSpace(cmp.Or(string(stderr), string(stdout))); s != "" {
		return fmt.Errorf("%s: %s", name, s)
	}
	return fmt.Errorf("%s: %w", name, err)
}
