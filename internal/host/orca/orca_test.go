package orca

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metalmatze/kelpie/internal/host"
)

func TestSession(t *testing.T) {
	for key, want := range map[string]string{
		"structured-agent-session-6ce7e7bf-030c-459f-abfb-51a93c636cd8:48cadfc6": "6ce7e7bf-030c-459f-abfb-51a93c636cd8",
		"structured-agent-session-claude_55c7487a_4771:77532f9a":                 "claude_55c7487a_4771",
		"cdceabe1-e8cb-48e1-afdf-4b26f5b9f0d5:c487df0c":                          "",
		"structured-agent-session-":                                              "",
	} {
		got, ok := agent{PaneKey: key}.session()
		if got != want || ok != (want != "") {
			t.Errorf("session(%q) = %q, %v; want %q", key, got, ok, want)
		}
	}
}

func TestAgentState(t *testing.T) {
	for _, tc := range []struct {
		json string
		want host.AgentState
	}{
		{`{"state":"idle"}`, host.Idle},
		// A chat idle at the prompt while a background task runs.
		{`{"state":"working","mainAgent":{"state":"done"},"children":[{"kind":"command","state":"working"}]}`, host.Idle},
		{`{"state":"working","mainAgent":{"state":"working"}}`, host.Working},
		{`{"state":"working","mainAgent":{"state":"blocked"}}`, host.Blocked},
		// A child waits at a question: the main agent being idle does not count.
		{`{"state":"blocked","mainAgent":{"state":"done"}}`, host.Blocked},
		// A terminal: its program status wins over the hooks.
		{`{"state":"waiting","programStatus":{"state":"idle"}}`, host.Idle},
		{`{"state":"idle","mainAgent":{"state":"idle"},"programStatus":{"state":"working"}}`, host.Working},
	} {
		var a agent
		if err := json.Unmarshal([]byte(tc.json), &a); err != nil {
			t.Fatal(err)
		}
		if got := a.state(); got != tc.want {
			t.Errorf("state of %s = %s; want %s", tc.json, got, tc.want)
		}
	}
}

func TestPick(t *testing.T) {
	claude := "claude"
	chat := func(id string, at int64) agent {
		return agent{PaneKey: chatPane + id + ":leaf", State: "idle", UpdatedAt: at}
	}
	term := func(handle, tab string, at int64) terminal {
		return terminal{Handle: handle, Writable: true, TabID: tab, LeafID: "leaf", AgentIdentity: &claude, LastOutputAt: at}
	}
	for _, tc := range []struct {
		name   string
		agents []agent
		terms  []terminal
		want   target
		err    bool
	}{
		{
			name:   "chat only",
			agents: []agent{chat("s1", 10)},
			terms:  []terminal{{Handle: "shell", Writable: true, TabID: "t0", LeafID: "leaf"}},
			want:   target{session: "s1", known: true, active: 10},
		},
		{
			name:   "the chat active last wins",
			agents: []agent{chat("old", 10), chat("new", 20)},
			want:   target{session: "new", known: true, active: 20},
		},
		{
			name:   "a chat wins over a terminal Orca has no state for",
			agents: []agent{chat("s1", 10)},
			terms:  []terminal{term("term", "t1", 99)},
			want:   target{session: "s1", known: true, active: 10},
		},
		{
			name:   "a hooked terminal active later wins over a chat",
			agents: []agent{chat("s1", 10), {PaneKey: "t1:leaf", State: "idle"}},
			terms:  []terminal{term("term", "t1", 20)},
			want:   target{handle: "term", known: true, active: 20},
		},
		{
			name:  "orphaned and read-only terminals are skipped",
			terms: []terminal{{Handle: "gone", Orphaned: true, Writable: true, AgentIdentity: &claude}, {Handle: "ro", AgentIdentity: &claude}},
			err:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pick(worktree{Agents: tc.agents}, tc.terms)
			if (err != nil) != tc.err || got != tc.want {
				t.Errorf("pick = %+v, %v; want %+v, error %v", got, err, tc.want, tc.err)
			}
		})
	}
}

func TestCallerWorktree(t *testing.T) {
	rows := []worktree{
		{WorktreeID: "r::/src", Path: "/src"},
		{WorktreeID: "r::/src/wt", Path: "/src/wt", Agents: []agent{{PaneKey: chatPane + "s1:leaf"}}},
		{WorktreeID: "r::/src/other", Path: "/src/other", Agents: []agent{{PaneKey: chatPane + "s2:leaf"}}},
	}
	for _, tc := range []struct{ session, dir, want string }{
		{"s2", "/elsewhere", "r::/src/other"}, // the session's agent decides
		{"s9", "/src/wt/internal", "r::/src/wt"},
		{"s9", "/src/wtx", "r::/src"},
		{"s9", "/elsewhere", ""},
	} {
		w, ok := callerWorktree(rows, tc.session, tc.dir)
		if w.WorktreeID != tc.want || ok != (tc.want != "") {
			t.Errorf("callerWorktree(%s, %s) = %q, %v; want %q", tc.session, tc.dir, w.WorktreeID, ok, tc.want)
		}
	}
}

// fake writes an orca stand-in that answers worktree ps and terminal list
// with fixed JSON, answers orchestration send with send, and records the
// arguments of each send in a file next to it.
func fake(t *testing.T, state, send string) (*Orca, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "sends")
	ps := `{"ok":true,"result":{"worktrees":[{"worktreeId":"r::/src","path":"/src","agents":[{"paneKey":"structured-agent-session-s1:leaf","state":"` + state + `","updatedAt":5}]}]}}`
	script := `#!/bin/sh
case "$1 $2" in
"worktree ps") echo '` + ps + `' ;;
"terminal list") echo '{"ok":true,"result":{"terminals":[]}}' ;;
"orchestration send")
	if [ -n "$ORCA_AGENT_SESSION_ID$ORCA_AGENT_SESSION_SPAWN_TOKEN$ORCA_TERMINAL_HANDLE" ]; then echo '{"ok":false,"error":{"code":"consumer_fenced"}}'; exit 1; fi
	printf '%s\n' "$*" >> '` + log + `'; echo '` + send + `'; exit 1 ;;
*) echo '{"ok":false,"error":{"code":"unexpected"}}'; exit 1 ;;
esac
`
	bin := filepath.Join(dir, "orca")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Orca{Bin: bin}, log
}

func TestDeliverChat(t *testing.T) {
	const (
		ok      = `{"ok":true,"result":{"message":{"id":"msg_1"}}}`
		unknown = `{"ok":false,"error":{"code":"session_caller_unknown","message":"No message was sent."}}`
		fenced  = `{"ok":false,"error":{"code":"consumer_fenced","message":"No request was sent."}}`
		timeout = `{"ok":false,"error":{"code":"runtime_timeout","message":"timed out"}}`
	)
	ws := host.Workspace{Host: "orca", ID: "orca:r::/src"}
	// A daemon started from an Orca chat or terminal: orca would send as
	// that agent unless every caller variable is stripped.
	for _, v := range callerVars {
		t.Setenv(v, "daemon-parent")
	}
	for _, tc := range []struct {
		name, state, send string
		sent              bool
		want              error // nil, a sentinel, or errAny
	}{
		{name: "idle chat gets the prompt", state: "idle", send: ok, sent: true},
		{name: "a done chat is idle", state: "done", send: ok, sent: true},
		{name: "working chat waits", state: "working", send: ok, want: host.ErrBusy},
		{name: "blocked chat waits", state: "blocked", send: ok, want: host.ErrBlocked},
		{name: "unknown session is retryable", state: "idle", send: unknown, sent: true, want: errAny},
		{name: "a fenced sender is retryable", state: "idle", send: fenced, sent: true, want: errAny},
		{name: "other failures are never resent", state: "idle", send: timeout, sent: true, want: host.ErrUnconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, log := fake(t, tc.state, tc.send)
			err := o.Deliver(context.Background(), ws, "[kelpie] hello")
			switch {
			case tc.want == nil && err != nil,
				tc.want == errAny && (err == nil || errors.Is(err, host.ErrUnconfirmed)),
				tc.want != nil && tc.want != errAny && !errors.Is(err, tc.want):
				t.Errorf("Deliver = %v; want %v", err, tc.want)
			}
			data, _ := os.ReadFile(log)
			if sent := len(data) > 0; sent != tc.sent {
				t.Fatalf("sent = %v; want %v", sent, tc.sent)
			}
			if tc.sent && !strings.Contains(string(data), "--from kelpie --to orca_session_id:s1 ") || tc.sent && !strings.Contains(string(data), "--body [kelpie] hello --full") {
				t.Errorf("send args = %s", data)
			}
		})
	}
}

// errAny is a failure that is neither a sentinel nor unconfirmed: the
// router retries it.
var errAny = errors.New("any retryable error")
