package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metalmatze/kelpie/internal/config"
	"github.com/metalmatze/kelpie/internal/core"
)

const lastModified = "Fri, 30 Oct 2026 10:00:00 GMT"

// fakeGH puts a gh on PATH that answers like gh api --include: 200 with
// Last-Modified, 304 (and exit 1, as gh does) once that comes back as
// If-Modified-Since, and 404 for a missing repo. The status line ends in \n
// and the headers in \r\n, as gh prints them. It returns the call log.
func fakeGH(t *testing.T, pollInterval string) func() []string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_LOG"
for a in "$@"; do
  case "$a" in
  *missing*)
    printf 'HTTP/2.0 404 Not Found\nContent-Type: application/json\r\n\r\n{"message":"Not Found"}'
    echo 'gh: Not Found (HTTP 404)' >&2; exit 1;;
  "If-Modified-Since: ` + lastModified + `")
    printf 'HTTP/2.0 304 Not Modified\nLast-Modified: ` + lastModified + `\r\nX-Poll-Interval: %s\r\n\r\n' "$FAKE_POLL"
    echo 'gh: HTTP 304' >&2; exit 1;;
  esac
done
printf 'HTTP/2.0 200 OK\nLast-Modified: ` + lastModified + `\r\nX-Poll-Interval: %s\r\n\r\n[{"id":"1"},{"id":"2"}]' "$FAKE_POLL"
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("FAKE_LOG", log)
	t.Setenv("FAKE_POLL", pollInterval)
	return func() []string {
		b, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

func inboxSource(every time.Duration) config.Source {
	return config.Source{
		Name: "inbox", Adapter: "cli", Conditional: true, Every: config.Duration{Duration: every},
		Run: []string{"gh", "api", "repos/{owner}/{name}/notifications"},
		Map: `{fields: {threads: length}}`,
	}
}

func parent(name string) *core.Observation {
	return &core.Observation{Subject: core.Subject("github-inbox:acme/" + name), Fields: map[string]any{"owner": "acme", "name": name}}
}

func TestConditionalPoll(t *testing.T) {
	calls := fakeGH(t, "120")
	c, err := New(inboxSource(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	p, err := c.poll(t.Context(), parent("api"), "")
	if err != nil {
		t.Fatal(err)
	}
	if p.notModified || len(p.obs) != 1 || p.obs[0].Fields["threads"] != 2.0 || p.obs[0].Subject != "github-inbox:acme/api" {
		t.Fatalf("first poll: %+v", p)
	}
	if p.lastModified != lastModified || p.pollInterval != 2*time.Minute || string(p.raw) != `[{"id":"1"},{"id":"2"}]` {
		t.Fatalf("first poll: last modified %q, poll interval %s, raw %q", p.lastModified, p.pollInterval, p.raw)
	}

	p, err = c.poll(t.Context(), parent("api"), p.lastModified)
	if err != nil {
		t.Fatalf("a 304 is not an error: %v", err)
	}
	if !p.notModified || p.obs != nil {
		t.Fatalf("second poll: %+v", p)
	}

	want := []string{
		"api --include repos/acme/api/notifications",
		"api --include -H If-Modified-Since: " + lastModified + " repos/acme/api/notifications",
	}
	if got := calls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	if _, err := c.poll(t.Context(), parent("missing"), ""); err == nil || !strings.Contains(err.Error(), "Not Found (HTTP 404)") {
		t.Fatalf("404: got %v", err)
	}
}

// The loop observes the 200, sends its Last-Modified back, skips observing
// the 304s and hands keep the previous observations meanwhile.
func TestConditionalLoop(t *testing.T) {
	fakeGH(t, "0")
	c, err := New(inboxSource(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	observed, kept := 0, 0
	c.loop(t.Context(), parent("api"), func([]core.Observation) { observed++ }, func(obs []core.Observation, err error) bool {
		if err != nil || len(obs) != 1 {
			t.Errorf("keep %d: obs %v, err %v", kept, obs, err)
		}
		kept++
		return kept < 3
	})
	if observed != 1 || kept != 3 {
		t.Fatalf("observed %d, kept %d; want 1 and 3", observed, kept)
	}
}

func TestConditionalNeedsGHAPI(t *testing.T) {
	for _, run := range [][]string{
		{"curl", "-sf", "https://api.github.com/notifications"},
		{"gh", "api", "notifications", "--paginate"},
		{"gh", "api"},
	} {
		s := inboxSource(time.Minute)
		s.Run = run
		if _, err := New(s); err == nil {
			t.Errorf("conditional with run %q: no error", run)
		}
	}
}

func TestParseInclude(t *testing.T) {
	resp, err := parseInclude([]byte("HTTP/2.0 200 OK\nX-Poll-Interval: 60\r\nLast-Modified: " + lastModified + "\r\n\r\n[]"))
	if err != nil || resp.status != 200 || resp.header.Get("x-poll-interval") != "60" || string(resp.body) != "[]" {
		t.Fatalf("200: %+v %v", resp, err)
	}
	resp, err = parseInclude([]byte("HTTP/2.0 304 Not Modified\nLast-Modified: " + lastModified + "\r\n\r\n"))
	if err != nil || resp.status != 304 || resp.header.Get("Last-Modified") != lastModified || len(resp.body) != 0 {
		t.Fatalf("304: %+v %v", resp, err)
	}
	if _, err := parseInclude([]byte(`[{"id":"1"}]`)); err == nil {
		t.Fatal("output without a status line parsed")
	}
}
