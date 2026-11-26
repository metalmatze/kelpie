package orca

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
		{"s2", "/elsewhere", "r::/src/other"},
		{"s9", "/src/wt/internal", "r::/src/wt"},
		{"s9", "/src/wtx", "r::/src"},
		{"s9", "/elsewhere", ""},
	} {
		w, _, ok := callerWorktree(rows, tc.session, tc.dir)
		if w.WorktreeID != tc.want || ok != (tc.want != "") {
			t.Errorf("callerWorktree(%s, %s) = %q, %v; want %q", tc.session, tc.dir, w.WorktreeID, ok, tc.want)
		}
	}
}

// A chat names its pane when its session is in ps, and none when only the
// directory matched.
func TestCallerWorktreePane(t *testing.T) {
	rows := []worktree{{WorktreeID: "r::/src", Path: "/src", Agents: []agent{{PaneKey: chatPane + "s1:leaf"}}}}
	if _, key, ok := callerWorktree(rows, "s1", "/elsewhere"); !ok || key != chatPane+"s1:leaf" {
		t.Errorf("by session: pane key = %q, %v", key, ok)
	}
	if _, key, ok := callerWorktree(rows, "s9", "/src"); !ok || key != "" {
		t.Errorf("by directory: pane key = %q, %v", key, ok)
	}
}

func TestParseID(t *testing.T) {
	for _, wt := range []string{"r::/src/wt", "r::/src/a@b", "r::/src/@"} {
		for _, key := range []string{"tab1:leaf1", chatPane + "claude_1:leaf"} {
			id := paneID(wt, key)
			if got, k := parseID(id); got != wt || k != key {
				t.Errorf("parseID(%q) = %q, %q; want %q, %q", id, got, k, wt, key)
			}
		}
		if got, k := parseID(prefix + wt); got != wt || k != "" {
			t.Errorf("parseID(%q) = %q, %q; want %q, no pane", prefix+wt, got, k, wt)
		}
	}
}

// A terminal is its pane only when an agent runs there. A person typing
// kelpie watch in a shell tab next to the agent gets the worktree, as
// before panes, so the update reaches that agent instead of waiting for one
// in the shell.
func TestCallerPane(t *testing.T) {
	const worktrees = `{"worktreeId":"r::/src","path":"/src","agents":[{"paneKey":"stale:leaf","state":"idle"}]}`
	const terminals = `{"handle":"h1","worktreeId":"r::/src","tabId":"agent","leafId":"leaf","writable":true,"agentIdentity":"claude"},` +
		`{"handle":"h2","worktreeId":"r::/src","tabId":"shell","leafId":"leaf","writable":true,"agentIdentity":null},` +
		`{"handle":"h3","worktreeId":"r::/src","tabId":"stale","leafId":"leaf","writable":true,"agentIdentity":null}`
	o, _ := fake(t, worktrees, terminals, "")
	for _, tc := range []struct {
		name, worktree, key, want string
		down                      bool
	}{
		{name: "an agent's terminal is its pane", worktree: "r::/src", key: "agent:leaf", want: "orca:pane:agent:leaf@r::/src"},
		{name: "a shell is its worktree", worktree: "r::/src", key: "shell:leaf", want: "orca:r::/src"},
		{name: "a shell ps still lists an agent in is its worktree", worktree: "r::/src", key: "stale:leaf", want: "orca:r::/src"},
		{name: "a worktree Orca does not list", worktree: "r::/gone", key: "agent:leaf", want: "orca:r::/gone"},
		{name: "Orca not answering", worktree: "r::/src", key: "agent:leaf", want: "orca:r::/src", down: true},
		{name: "no pane key", worktree: "r::/src", key: "", want: "orca:r::/src"},
		{name: "a pane key with @", worktree: "r::/src", key: "a@b", want: "orca:r::/src"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := o.Bin
			if tc.down {
				bin = filepath.Join(t.TempDir(), "missing")
			}
			t.Setenv("ORCA_CLI_COMMAND", bin)
			t.Setenv("ORCA_WORKTREE_ID", tc.worktree)
			t.Setenv("ORCA_PANE_KEY", tc.key)
			if got, ok := Caller(); !ok || got != tc.want {
				t.Errorf("Caller with pane key %q = %q, %v; want %q", tc.key, got, ok, tc.want)
			}
		})
	}
}

// A worktree runs an agent when ps lists one, a chat included, or when
// Orca names one in a live terminal that no hook reports.
func TestWorkspacesAgent(t *testing.T) {
	o, _ := fake(t,
		`{"worktreeId":"r::/chat","path":"/chat","agents":[{"paneKey":"structured-agent-session-s1:leaf","state":"idle"}]},`+
			`{"worktreeId":"r::/hookless","path":"/hookless"},`+
			`{"worktreeId":"r::/shell","path":"/shell"}`,
		`{"handle":"t1","worktreeId":"r::/hookless","writable":true,"agentIdentity":"cursor"},`+
			`{"handle":"t2","worktreeId":"r::/shell","writable":true,"agentIdentity":null},`+
			`{"handle":"t3","worktreeId":"r::/shell","orphaned":true,"agentIdentity":"claude"}`,
		"")
	list, err := o.Workspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Only the worktrees here; the panes are TestWorkspacesPanes's.
	list = slices.DeleteFunc(list, func(ws host.Workspace) bool { return strings.HasPrefix(ws.ID, panePrefix) })
	want := map[string]bool{"orca:r::/chat": true, "orca:r::/hookless": true, "orca:r::/shell": false}
	if len(list) != len(want) {
		t.Fatalf("listed %+v; want %d workspaces", list, len(want))
	}
	for _, ws := range list {
		if ws.Agent != want[ws.ID] {
			t.Errorf("%s: Agent = %v; want %v", ws.ID, ws.Agent, want[ws.ID])
		}
	}
}

// Orca lists a pane workspace for each live pane, next to its worktree: an
// agent and a shell alike, as herdr does. Orphaned terminals have no pane.
func TestWorkspacesPanes(t *testing.T) {
	o, _ := fake(t,
		`{"worktreeId":"r::/src","path":"/src","branch":"refs/heads/feat","agents":[`+
			`{"paneKey":"structured-agent-session-s1:leaf","state":"idle"},{"paneKey":"t1:leaf","state":"working"}]},`+
			`{"worktreeId":"r::/shell","path":"/shell"}`,
		`{"handle":"h1","worktreeId":"r::/src","tabId":"t1","leafId":"leaf","writable":true,"agentIdentity":"claude"},`+
			`{"handle":"h2","worktreeId":"r::/src","tabId":"t2","leafId":"leaf","writable":true,"agentIdentity":"cursor"},`+
			`{"handle":"h3","worktreeId":"r::/src","tabId":"t3","leafId":"leaf","writable":true,"agentIdentity":null},`+
			`{"handle":"h4","worktreeId":"r::/src","tabId":"t4","leafId":"leaf","orphaned":true,"agentIdentity":"claude"},`+
			`{"handle":"h5","worktreeId":"r::/shell","tabId":"t5","leafId":"leaf","writable":true,"agentIdentity":null}`,
		"")
	o.repos = map[string]cachedRepos{"/src": {repos: []string{"acme/api"}, at: time.Now()}}
	list, err := o.Workspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Every live terminal is a pane, shells and read-only ones too; the
	// orphaned h4 is not.
	want := map[string]bool{
		"orca:r::/src": true,
		"orca:pane:structured-agent-session-s1:leaf@r::/src": false,
		"orca:pane:t1:leaf@r::/src":                          false,
		"orca:pane:t2:leaf@r::/src":                          false,
		"orca:pane:t3:leaf@r::/src":                          false,
		"orca:r::/shell":                                     false,
		"orca:pane:t5:leaf@r::/shell":                        false,
	}
	if len(list) != len(want) {
		t.Fatalf("listed %+v; want %d workspaces", list, len(want))
	}
	for _, ws := range list {
		agent, ok := want[ws.ID]
		if !ok || ws.Agent != agent {
			t.Errorf("%s: Agent = %v; want %v (listed: %v)", ws.ID, ws.Agent, agent, ok)
		}
		if strings.HasSuffix(ws.ID, "@r::/src") && (ws.Branch != "feat" || !slices.Equal(ws.Repos, []string{"acme/api"})) {
			t.Errorf("%s: branch %q, repos %v; want its worktree's", ws.ID, ws.Branch, ws.Repos)
		}
		if strings.HasSuffix(ws.ID, "@r::/src") && ws.Path != "/src" {
			t.Errorf("%s: path %q; want its worktree's", ws.ID, ws.Path)
		}
	}
}

// An idle agent's pane is idle while a sibling works or waits at a
// question; the worktree still counts the sibling.
func TestPaneState(t *testing.T) {
	o, _ := fake(t,
		`{"worktreeId":"r::/src","path":"/src","agents":[{"paneKey":"t1:leaf","state":"idle"},{"paneKey":"t2:leaf","state":"working"},{"paneKey":"t3:leaf","state":"blocked"}]}`, "", "")
	ctx := context.Background()
	for id, want := range map[string]host.AgentState{
		"orca:pane:t1:leaf@r::/src": host.Idle,
		"orca:pane:t2:leaf@r::/src": host.Working,
		"orca:pane:t3:leaf@r::/src": host.Blocked,
		"orca:pane:t9:leaf@r::/src": host.Unknown, // no hook reports it
		"orca:r::/src":              host.Blocked,
	} {
		if got, err := o.State(ctx, host.Workspace{ID: id}); err != nil || got != want {
			t.Errorf("State(%s) = %s, %v; want %s", id, got, err, want)
		}
	}
}

type readyEvents struct{ ready []string }

func (*readyEvents) Resync(string, []host.Workspace)     {}
func (*readyEvents) Upsert(host.Workspace)               {}
func (*readyEvents) Remove(string, string)               {}
func (*readyEvents) Move(string, string, host.Workspace) {}
func (e *readyEvents) AgentReady(_, id string)           { e.ready = append(e.ready, id) }

// A pane turning idle releases the deliveries queued for it, the worktree
// only when all of its agents are.
func TestPaneStateReady(t *testing.T) {
	rows := func(second string) []worktree {
		return []worktree{{WorktreeID: "r::/src", Path: "/src", Agents: []agent{{PaneKey: "t1:leaf", State: "idle"}, {PaneKey: "t2:leaf", State: second}}}}
	}
	o := &Orca{}
	var ev readyEvents
	o.ready(rows("working"), &ev)
	if want := []string{"orca:pane:t1:leaf@r::/src"}; !slices.Equal(ev.ready, want) {
		t.Fatalf("ready = %v; want %v", ev.ready, want)
	}
	ev.ready = nil
	o.ready(rows("idle"), &ev)
	slices.Sort(ev.ready)
	if want := []string{"orca:pane:t2:leaf@r::/src", "orca:r::/src"}; !slices.Equal(ev.ready, want) {
		t.Errorf("ready = %v; want %v", ev.ready, want)
	}
}

func TestPickPane(t *testing.T) {
	claude := "claude"
	term := func(handle, tab string) terminal {
		return terminal{Handle: handle, Writable: true, TabID: tab, LeafID: "leaf", AgentIdentity: &claude, LastOutputAt: 1}
	}
	ps := []agent{
		{PaneKey: chatPane + "s1:leaf", State: "idle", UpdatedAt: 10},
		{PaneKey: chatPane + "s2:leaf", State: "idle", UpdatedAt: 99},
		{PaneKey: "t5:leaf", State: "idle"},
	}
	for _, tc := range []struct {
		name  string
		key   string
		terms []terminal
		want  target
		err   bool
	}{
		{name: "a chat pane is its own session", key: chatPane + "s1:leaf", want: target{session: "s1", known: true, active: 10}},
		{name: "a terminal pane is its terminal", key: "t1:leaf", terms: []terminal{term("a", "t0"), term("b", "t1")}, want: target{handle: "b", active: 1}},
		{name: "a terminal known only from ps is a shell now", key: "t5:leaf", terms: []terminal{{Handle: "c", Writable: true, TabID: "t5", LeafID: "leaf"}}, err: true},
		{name: "a terminal in ps and named an agent is known", key: "t5:leaf", terms: []terminal{{Handle: "c", Writable: true, TabID: "t5", LeafID: "leaf", AgentIdentity: &claude}}, want: target{handle: "c", known: true}},
		{name: "a shell is refused", key: "t1:leaf", terms: []terminal{{Handle: "sh", Writable: true, TabID: "t1", LeafID: "leaf"}}, err: true},
		{name: "an orphaned terminal is refused", key: "t1:leaf", terms: []terminal{{Handle: "o", Orphaned: true, Writable: true, TabID: "t1", LeafID: "leaf", AgentIdentity: &claude}}, err: true},
		{name: "a read-only terminal is refused", key: "t1:leaf", terms: []terminal{{Handle: "r", TabID: "t1", LeafID: "leaf", AgentIdentity: &claude}}, err: true},
		{name: "a missing pane is refused", key: "t9:leaf", terms: []terminal{term("a", "t0")}, err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickPane(worktree{Agents: ps}, tc.key, tc.terms)
			if (err != nil) != tc.err || got != tc.want {
				t.Errorf("pickPane = %+v, %v; want %+v, error %v", got, err, tc.want, tc.err)
			}
		})
	}
}

// fake writes an orca stand-in whose runtime is ready, that answers
// worktree ps with worktrees and terminal list with terminals (JSON
// objects, comma-separated), answers orchestration send with send, and
// records the arguments of each send in a file, and those of terminal send
// and worktree set in log+".terminal" and log+".set". terminal send reports
// a started turn.
func fake(t *testing.T, worktrees, terminals, send string) (*Orca, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "sends")
	script := `#!/bin/sh
case "$1 $2" in
"status --json") echo '{"ok":true,"result":{"runtime":{"state":"ready"}}}' ;;
"worktree ps")
	if [ -s '` + log + `.comment' ]; then
		echo '{"ok":true,"result":{"worktrees":[' | tr -d '\n'
		echo '` + worktrees + `' | sed "s|\"comment\":\"[^\"]*\"|\"comment\":\"$(cat '` + log + `.comment')\"|" | tr -d '\n'
		echo ']}}'
	else echo '{"ok":true,"result":{"worktrees":[` + worktrees + `]}}'; fi ;;
"terminal list") echo '{"ok":true,"result":{"terminals":[` + terminals + `]}}' ;;
"orchestration send")
	if [ -n "$ORCA_AGENT_SESSION_ID$ORCA_AGENT_SESSION_SPAWN_TOKEN$ORCA_TERMINAL_HANDLE" ]; then echo '{"ok":false,"error":{"code":"consumer_fenced"}}'; exit 1; fi
	printf '%s\n' "$*" >> '` + log + `'; echo '` + send + `'; exit 1 ;;
"terminal send") printf '%s\n' "$*" >> '` + log + `.terminal'; echo '{"ok":true,"result":{"send":{"prompt":{"stages":["input_accepted","turn_started"]}}}}' ;;
"terminal wait") printf '%s\n' "$*" >> '` + log + `.terminal'; echo '{"ok":true,"result":{}}' ;;
"worktree set") printf '%s\n' "$*" >> '` + log + `.set'
	while [ $# -gt 0 ]; do if [ "$1" = --comment ]; then printf '%s' "$2" > '` + log + `.comment'; fi; shift; done
	echo '{"ok":true,"result":{}}' ;;
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
		ok       = `{"ok":true,"result":{"message":{"id":"msg_1"}}}`
		unknown  = `{"ok":false,"error":{"code":"session_caller_unknown","message":"No message was sent."}}`
		fenced   = `{"ok":false,"error":{"code":"consumer_fenced","message":"No request was sent."}}`
		noSender = `{"ok":false,"error":{"code":"no_active_sender_terminal","message":"Could not determine the calling terminal."}}`
		timeout  = `{"ok":false,"error":{"code":"runtime_timeout","message":"timed out"}}`
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
		{name: "an unknown sender is retryable", state: "idle", send: noSender, sent: true, want: errAny},
		{name: "other failures are never resent", state: "idle", send: timeout, sent: true, want: host.ErrUnconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, log := fake(t, `{"worktreeId":"r::/src","path":"/src","agents":[{"paneKey":"structured-agent-session-s1:leaf","state":"`+tc.state+`","updatedAt":5}]}`, "", tc.send)
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

// A pane gets the prompt in its own chat or terminal, whichever sibling was
// active later or is busy, and never in a shell.
func TestDeliverPane(t *testing.T) {
	for _, v := range callerVars {
		t.Setenv(v, "")
	}
	const ok = `{"ok":true,"result":{"message":{"id":"msg_1"}}}`
	const worktrees = `{"worktreeId":"r::/src","path":"/src","agents":[` +
		`{"paneKey":"structured-agent-session-s1:leaf","state":"idle","updatedAt":5},` +
		`{"paneKey":"structured-agent-session-s2:leaf","state":"working","updatedAt":50},` +
		`{"paneKey":"t1:leaf","state":"idle"}]}`
	const terminals = `{"handle":"h1","worktreeId":"r::/src","tabId":"t1","leafId":"leaf","writable":true,"agentIdentity":"claude","lastOutputAt":1},` +
		`{"handle":"h2","worktreeId":"r::/src","tabId":"t2","leafId":"leaf","writable":true,"agentIdentity":"claude","lastOutputAt":99},` +
		`{"handle":"h3","worktreeId":"r::/src","tabId":"t3","leafId":"leaf","writable":true,"agentIdentity":null,"lastOutputAt":200}`
	for _, tc := range []struct {
		name, key string
		log       string // which log should hold the send
		want      string // what that log should contain
		err       error  // nil, errAny, or a sentinel
	}{
		{name: "a chat pane, though its sibling is newer and working", key: chatPane + "s1:leaf", log: "", want: "--to orca_session_id:s1 "},
		{name: "a busy chat pane waits", key: chatPane + "s2:leaf", err: host.ErrBusy},
		{name: "a terminal pane, though another agent output later", key: "t1:leaf", log: ".terminal", want: "terminal send --terminal h1 "},
		{name: "a terminal Orca names an agent in but no hook reports", key: "t2:leaf", log: ".terminal", want: "terminal send --terminal h2 "},
		{name: "a shell pane is never typed into", key: "t3:leaf", err: errAny},
		{name: "a missing pane", key: "t9:leaf", err: errAny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, log := fake(t, worktrees, terminals, ok)
			err := o.Deliver(context.Background(), host.Workspace{Host: "orca", ID: paneID("r::/src", tc.key)}, "[kelpie] hello")
			switch {
			case tc.err == nil && err != nil,
				tc.err == errAny && (err == nil || errors.Is(err, host.ErrUnconfirmed) || errors.Is(err, host.ErrBusy) || errors.Is(err, host.ErrBlocked)),
				tc.err != nil && tc.err != errAny && !errors.Is(err, tc.err):
				t.Fatalf("Deliver = %v; want %v", err, tc.err)
			}
			sent := ""
			for _, f := range []string{"", ".terminal"} {
				data, _ := os.ReadFile(log + f)
				if len(data) > 0 && tc.err != nil {
					t.Errorf("sent %s despite %v", data, err)
				}
				if f == tc.log {
					sent = string(data)
				}
			}
			if tc.want != "" && !strings.Contains(sent, tc.want) {
				t.Errorf("sent %q; want it to contain %q", sent, tc.want)
			}
		})
	}
}

// The Orca comment of a worktree shows what it and its panes watch.
func TestBadgePanes(t *testing.T) {
	o, log := fake(t, `{"worktreeId":"r::/src","path":"/src","comment":"mine"}`, "", "")
	o.Comment = true
	ctx := context.Background()
	wt := host.Workspace{Host: "orca", ID: "orca:r::/src"}
	pane := host.Workspace{Host: "orca", ID: paneID("r::/src", "t1:leaf")}
	other := host.Workspace{Host: "orca", ID: paneID("r::/src", "t2:leaf")}
	set := func() string { data, _ := os.ReadFile(log + ".set"); return string(data) }

	if err := o.Badge(ctx, pane, host.Badge{Subjects: []string{"o/r#2", "o/r#1"}}); err != nil {
		t.Fatal(err)
	}
	if err := o.Badge(ctx, wt, host.Badge{Subjects: []string{"o/r#3", "o/r#1"}, Reminders: 1}); err != nil {
		t.Fatal(err)
	}
	if err := o.Badge(ctx, other, host.Badge{Reminders: 2}); err != nil {
		t.Fatal(err)
	}
	// Sorted, once each, reminders summed, written to the worktree.
	if want := "--worktree id:r::/src --comment o/r#1 · o/r#2 · o/r#3 · ⏰3"; !strings.Contains(set(), want) {
		t.Errorf("worktree set calls:\n%s\nwant one with %q", set(), want)
	}
	// A pane that is gone leaves the comment.
	o.pruneBadges(ctx, []host.Workspace{wt, other})
	if want := "--comment o/r#1 · o/r#3 · ⏰3"; !strings.Contains(set(), want) {
		t.Errorf("after the pane left, worktree set calls:\n%s\nwant one with %q", set(), want)
	}
	// Nothing left to show puts the comment back.
	for _, ws := range []host.Workspace{wt, other} {
		if err := o.Badge(ctx, ws, host.Badge{}); err != nil {
			t.Fatal(err)
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.comments) != 0 {
		t.Errorf("replaced comments = %v; want the original back", o.comments)
	}
	lines := strings.Split(strings.TrimSpace(set()), "\n")
	if last := lines[len(lines)-1]; !strings.HasSuffix(last, "--comment mine --json") {
		t.Errorf("last worktree set = %q; want the original comment written back", last)
	}
}

func TestNotifyPane(t *testing.T) {
	o, log := fake(t, `{"worktreeId":"r::/src","path":"/src"}`, "", "")
	ws := host.Workspace{Host: "orca", ID: paneID("r::/src", "t1:leaf")}
	err := o.Notify(context.Background(), host.Notification{Workspace: ws, NeedsYou: true})
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Notify = %v; want ErrUnsupported", err)
	}
	data, _ := os.ReadFile(log + ".set")
	if !strings.Contains(string(data), "--worktree id:r::/src --unread") {
		t.Errorf("worktree set calls: %s", data)
	}
}

func TestParent(t *testing.T) {
	o := &Orca{}
	if to, ok := o.Parent(paneID("r::/src/a@b", "t1:leaf")); !ok || to != "orca:r::/src/a@b" {
		t.Errorf("Parent(pane) = %q, %v; want its worktree", to, ok)
	}
	if to, ok := o.Parent("orca:r::/src"); ok {
		t.Errorf("Parent(worktree) = %q, true; want none", to)
	}
}

// fakeAt points o at a stand-in orca whose worktree ps lists worktrees.
func fakeAt(t *testing.T, o *Orca, worktrees, terminals string) {
	t.Helper()
	other, _ := fake(t, worktrees, terminals, "")
	o.Bin = other.Bin
}

// An agent leaving its pane, with the terminal staying as a shell, keeps the
// pane listed, as in herdr; delivery waits and never types into the shell.
func TestPaneAgentLeaves(t *testing.T) {
	const withAgent = `{"worktreeId":"r::/src","path":"/src","agents":[{"paneKey":"t1:leaf","state":"idle"}]}`
	const without = `{"worktreeId":"r::/src","path":"/src"}`
	agentTerm := `{"handle":"h1","worktreeId":"r::/src","tabId":"t1","leafId":"leaf","writable":true,"agentIdentity":"claude"}`
	shellTerm := `{"handle":"h1","worktreeId":"r::/src","tabId":"t1","leafId":"leaf","writable":true,"agentIdentity":null}`
	ctx := context.Background()
	pane := host.Workspace{Host: "orca", ID: paneID("r::/src", "t1:leaf")}
	listed := func(o *Orca) bool {
		list, err := o.Workspaces(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(list, func(ws host.Workspace) bool { return ws.ID == pane.ID })
	}

	o, _ := fake(t, withAgent, agentTerm, "")
	if !listed(o) {
		t.Fatal("pane with an agent is not listed")
	}
	o, log := fake(t, without, shellTerm, "")
	if !listed(o) {
		t.Fatal("pane is dropped when its agent left")
	}
	if s, err := o.State(ctx, pane); err != nil || s != host.Unknown {
		t.Errorf("State of a pane without an agent = %s, %v; want unknown", s, err)
	}
	err := o.Deliver(ctx, pane, "x")
	if err == nil || errors.Is(err, host.ErrUnconfirmed) || errors.Is(err, host.ErrBusy) || errors.Is(err, host.ErrBlocked) {
		t.Errorf("Deliver to a pane without an agent = %v; want a plain retryable error", err)
	}
	for _, f := range []string{"", ".terminal"} {
		if data, _ := os.ReadFile(log + f); len(data) > 0 {
			t.Errorf("typed into the shell: %s", data)
		}
	}
}
