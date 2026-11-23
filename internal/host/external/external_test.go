package external

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metalmatze/kelpie/internal/host"
)

func sh(script string) []string { return []string{"sh", "-c", script, "host"} }

func TestDescribe(t *testing.T) {
	d, err := Describe(t.Context(), sh(`[ "$1" = describe ] && cat <<'EOF'
kelpie: 1
name: custom
reports_agent: true
term_program: customterm
caller_env: [CUSTOM_WS_ID]
capabilities: [badge, notify]
some_future_field: ignored
EOF`))
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "custom" || !d.ReportsAgent || d.TermProgram != "customterm" || !slices.Equal(d.CallerEnv, []string{"CUSTOM_WS_ID"}) || !d.HasCapability("badge") || !d.HasCapability("notify") {
		t.Fatalf("got %+v", d)
	}

	// JSON describe output works too.
	d, err = Describe(t.Context(), sh(`echo '{"kelpie":1,"name":"jsonhost"}'`))
	if err != nil || d.Name != "jsonhost" || d.ReportsAgent || d.HasCapability("badge") {
		t.Fatalf("json describe: %+v, %v", d, err)
	}

	orig := DescribeTimeout
	t.Cleanup(func() { DescribeTimeout = orig })
	DescribeTimeout = 100 * time.Millisecond

	for _, tc := range []struct {
		name string
		cmd  []string
		want string
	}{
		{"empty command", nil, "command is empty"},
		{"missing binary", []string{"/nonexistent/kelpie-host-x"}, "no such file"},
		{"failing", sh(`echo "boom" >&2; exit 2`), "boom"},
		{"hanging", sh(`sleep 5`), "no answer within 100ms"},
		{"no version", sh(`echo "name: custom"`), "no kelpie: version"},
		{"newer schema", sh(`printf "kelpie: 2\nname: custom\n"`), "kelpie: 2, this kelpie reads up to 1"},
		{"no name", sh(`echo "kelpie: 1"`), "no name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Describe(t.Context(), tc.cmd); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCaller(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	cmd := sh(`echo ran > "` + marker + `"; [ "$1" = caller ] && printf '%s\n' "$TEST_EXTERNAL_HOST_WS"`)

	// caller_env skips invoking the binary when unset.
	t.Setenv("TEST_EXTERNAL_HOST_WS", "")
	if id, ok := Caller(t.Context(), cmd, "custom", []string{"TEST_EXTERNAL_HOST_WS"}); ok || id != "" {
		t.Fatalf("got %q %v, want empty", id, ok)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("caller ran despite empty caller_env")
	}

	// When the env var is set, caller runs and prefixes the raw ID.
	t.Setenv("TEST_EXTERNAL_HOST_WS", "sess-42")
	if id, ok := Caller(t.Context(), cmd, "custom", []string{"TEST_EXTERNAL_HOST_WS"}); !ok || id != "custom:sess-42" {
		t.Fatalf("got %q %v, want custom:sess-42", id, ok)
	}

	// Empty output, multiline output and non-zero exit mean "not inside".
	for _, script := range []string{
		`true`,
		`printf "a\nb\n"`,
		`exit 1`,
	} {
		if id, ok := Caller(t.Context(), sh(script), "custom", nil); ok || id != "" {
			t.Errorf("%s: got %q %v", script, id, ok)
		}
	}
}

type fakeEvents struct {
	mu      sync.Mutex
	resyncs [][]host.Workspace
	ready   []string
}

func (f *fakeEvents) Resync(_ string, list []host.Workspace) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resyncs = append(f.resyncs, list)
}

func (f *fakeEvents) Upsert(host.Workspace)               {}
func (f *fakeEvents) Remove(string, string)               {}
func (f *fakeEvents) Move(string, string, host.Workspace) {}

func (f *fakeEvents) AgentReady(_, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ready = append(f.ready, id)
}

func TestWorkspacesAndWatch(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "workspaces.json")
	write := func(s string) {
		if err := os.WriteFile(stateFile, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`[{"id":"s1","branch":"fix","repos":["acme/api"],"agent":true,"state":"working"},{"id":"s2","state":"done"}]`)

	h := New("custom", sh(`[ "$1" = workspaces ] && cat "`+stateFile+`"`), 20*time.Millisecond, nil)
	list, err := h.Workspaces(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Host != "custom" || list[0].ID != "custom:s1" || list[0].Branch != "fix" || !list[0].Agent || list[1].ID != "custom:s2" {
		t.Fatalf("workspaces = %+v", list)
	}

	// Watch resyncs and emits AgentReady on transitions to idle (or done).
	ev := &fakeEvents{}
	h.pollOnce(t.Context(), ev)
	if !slices.Equal(ev.ready, []string{"custom:s2"}) {
		t.Fatalf("first poll ready = %v, want [custom:s2]", ev.ready)
	}

	// Still working -> no new AgentReady.
	h.pollOnce(t.Context(), ev)
	if len(ev.ready) != 1 {
		t.Fatalf("second poll ready = %v, want still 1", ev.ready)
	}

	// s1 transitions working -> idle -> AgentReady fires.
	write(`[{"id":"s1","state":"idle"},{"id":"s2","state":"done"}]`)
	h.pollOnce(t.Context(), ev)
	if !slices.Equal(ev.ready, []string{"custom:s2", "custom:s1"}) {
		t.Fatalf("third poll ready = %v, want [custom:s2 custom:s1]", ev.ready)
	}

	// Transient failure does not resync (keeps existing bindings).
	write(`not json`)
	resyncCount := len(ev.resyncs)
	h.pollOnce(t.Context(), ev)
	if len(ev.resyncs) != resyncCount {
		t.Fatal("failed poll called Resync")
	}

	// Missing id is rejected.
	write(`[{"branch":"main"}]`)
	if _, err := h.Workspaces(t.Context()); err == nil || !strings.Contains(err.Error(), "id is required") {
		t.Errorf("missing id: %v", err)
	}
}

func TestStateAndDeliver(t *testing.T) {
	ws := host.Workspace{Host: "custom", ID: "custom:s1"}

	for raw, want := range map[string]host.AgentState{
		"idle":    host.Idle,
		"done":    host.Idle,
		"working": host.Working,
		"blocked": host.Blocked,
		"weird":   host.Unknown,
	} {
		h := New("custom", sh(`[ "$1" = state ] && [ "$2" = s1 ] && echo `+raw), 0, nil)
		got, err := h.State(t.Context(), ws)
		if err != nil || got != want {
			t.Errorf("state %q: got %q %v, want %q", raw, got, err, want)
		}
	}

	// Deliver passes the full prompt on stdin and strips the host prefix from id.
	gotFile := filepath.Join(t.TempDir(), "prompt.txt")
	h := New("custom", sh(`[ "$1" = deliver ] && [ "$2" = s1 ] && cat > "`+gotFile+`"`), 0, nil)
	prompt := "[kelpie] 2 updates\n- line 1\n- line 2"
	if err := h.Deliver(t.Context(), ws, prompt); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(gotFile)
	if err != nil || string(b) != prompt {
		t.Fatalf("delivered %q %v, want %q", string(b), err, prompt)
	}

	// Structured error codes map to sentinel errors from stderr or stdout.
	for _, tc := range []struct {
		name   string
		script string
		want   error
		substr string
	}{
		{"blocked stderr", `echo '{"code":"blocked","message":"waiting on approval"}' >&2; exit 1`, host.ErrBlocked, "waiting on approval"},
		{"busy stdout", `echo '{"code":"busy"}'; exit 1`, host.ErrBusy, "agent is busy"},
		{"unconfirmed stderr", `echo '{"code":"unconfirmed","message":"no turn"}' >&2; exit 1`, host.ErrUnconfirmed, "no turn"},
		{"plain failure", `echo "socket down" >&2; exit 1`, nil, "socket down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New("custom", sh(tc.script), 0, nil)
			err := h.Deliver(t.Context(), ws, "hi")
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.substr) {
				t.Errorf("got %v, want %q", err, tc.substr)
			}
		})
	}
}

func TestBadgeAndNotify(t *testing.T) {
	ws := host.Workspace{Host: "custom", ID: "custom:s1"}

	// Without capabilities, Badge is a no-op and Notify returns ErrUnsupported
	// without invoking the binary.
	marker := filepath.Join(t.TempDir(), "called")
	noCaps := New("custom", sh(`touch "`+marker+`"`), 0, nil)
	if err := noCaps.Badge(t.Context(), ws, host.Badge{Subjects: []string{"#1 ✓"}}); err != nil {
		t.Fatalf("Badge without cap: %v", err)
	}
	if err := noCaps.Notify(t.Context(), host.Notification{Title: "t", Body: "b", Workspace: ws}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Notify without cap: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("binary invoked without capabilities")
	}

	// With capabilities, Badge and Notify send structured JSON on stdin.
	badgeFile := filepath.Join(t.TempDir(), "badge.json")
	notifyFile := filepath.Join(t.TempDir(), "notify.json")
	withCaps := New("custom", sh(`
case "$1" in
  badge)  [ "$2" = s1 ] && cat > "`+badgeFile+`" ;;
  notify) cat > "`+notifyFile+`" ;;
  *) exit 1 ;;
esac`), 0, []string{"badge", "notify"})

	if err := withCaps.Badge(t.Context(), ws, host.Badge{Subjects: []string{"#1 ✓"}, Reminders: 2}); err != nil {
		t.Fatal(err)
	}
	var gotBadge struct {
		Text      string   `json:"text"`
		Subjects  []string `json:"subjects"`
		Reminders int      `json:"reminders"`
	}
	b, _ := os.ReadFile(badgeFile)
	if err := json.Unmarshal(b, &gotBadge); err != nil || gotBadge.Text != "#1 ✓ · ⏰2" || !slices.Equal(gotBadge.Subjects, []string{"#1 ✓"}) || gotBadge.Reminders != 2 {
		t.Fatalf("badge payload = %s (%v)", string(b), err)
	}

	// Notify strips the host prefix for its own workspaces and keeps it for others.
	if err := withCaps.Notify(t.Context(), host.Notification{Title: "hi", Body: "body", Workspace: ws, NeedsYou: true}); err != nil {
		t.Fatal(err)
	}
	var gotNotify struct {
		Title         string `json:"title"`
		Body          string `json:"body"`
		Workspace     string `json:"workspace"`
		WorkspaceHost string `json:"workspace_host"`
		NeedsYou      bool   `json:"needs_you"`
	}
	nb, _ := os.ReadFile(notifyFile)
	if err := json.Unmarshal(nb, &gotNotify); err != nil || gotNotify.Workspace != "s1" || gotNotify.WorkspaceHost != "custom" || !gotNotify.NeedsYou {
		t.Fatalf("notify payload = %s (%v)", string(nb), err)
	}

	// Cross-host notification keeps the foreign workspace ID intact.
	if err := withCaps.Notify(t.Context(), host.Notification{Title: "hi", Workspace: host.Workspace{Host: "orca", ID: "orca:w1"}}); err != nil {
		t.Fatal(err)
	}
	nb, _ = os.ReadFile(notifyFile)
	if err := json.Unmarshal(nb, &gotNotify); err != nil || gotNotify.Workspace != "orca:w1" || gotNotify.WorkspaceHost != "orca" {
		t.Fatalf("foreign notify payload = %s (%v)", string(nb), err)
	}

	// Returning {"code":"unsupported"} maps to ErrUnsupported for Notify and nil for Badge.
	unsup := New("custom", sh(`echo '{"code":"unsupported"}' >&2; exit 1`), 0, []string{"badge", "notify"})
	if err := unsup.Badge(t.Context(), ws, host.Badge{}); err != nil {
		t.Fatalf("unsupported badge: %v", err)
	}
	if err := unsup.Notify(t.Context(), host.Notification{Title: "t"}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("unsupported notify: %v", err)
	}
}

func TestWatchContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	h := New("custom", sh(`echo '[]'`), time.Hour, nil)
	done := make(chan error, 1)
	go func() { done <- h.Watch(ctx, &fakeEvents{}) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not stop on cancel")
	}
}

func TestWorkspacesGitInfoFallback(t *testing.T) {
	// Clear git hook/rebase env vars so git -C operates on the temp repo.
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "fix-auth"},
		{"remote", "add", "origin", "https://github.com/acme/api.git"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	payload, _ := json.Marshal([]map[string]any{{"id": "t1", "path": repo}})
	h := New("custom", sh(`printf '%s' '`+string(payload)+`'`), 0, nil)
	list, err := h.Workspaces(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Branch != "fix-auth" || !slices.Equal(list[0].Repos, []string{"acme/api"}) {
		t.Fatalf("workspaces = %+v", list)
	}
}
