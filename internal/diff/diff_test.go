package diff

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/metalmatze/kelpie/internal/config"
	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/state"
)

func obs(subject string, fields map[string]any) core.Observation {
	return core.Observation{Subject: core.Subject(subject), Fields: fields}
}

func kinds(evs []core.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, string(e.Subject)+" "+e.Kind)
	}
	slices.Sort(out)
	return out
}

func TestDifferBaselineAndTransitions(t *testing.T) {
	d, err := New("s", []config.Transition{
		{Field: "checks", To: "FAILURE", Kind: "checks.failed"},
		{Field: "comments", Increased: true, Kind: "comment.added"},
		{Field: "ts", Increased: true, Kind: "slack.replied"},
		{Field: "status", Changed: true, Kind: "status.changed"},
		{Field: "mergeable", To: "CONFLICTING", Ignore: []any{"UNKNOWN"}, Kind: "merge.conflict"},
	}, state.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	step := func(o ...core.Observation) []string { return kinds(d.Observe(o)) }

	if got := step(obs("a", map[string]any{"checks": "FAILURE", "comments": 1.0, "ts": "1761811234.5", "status": "Todo", "mergeable": "CONFLICTING"})); got != nil {
		t.Fatalf("baseline emitted %v", got)
	}
	if got := step(
		obs("a", map[string]any{"checks": "FAILURE", "comments": 1.0, "ts": "1761811234.5", "status": "Todo", "mergeable": "UNKNOWN"}),
		obs("b", map[string]any{"checks": "FAILURE"}),
	); got != nil {
		t.Fatalf("unchanged or new subject emitted %v", got)
	}
	got := step(obs("a", map[string]any{"checks": "PENDING", "comments": 3.0, "ts": "1761811299.1", "status": "In Review", "mergeable": "CONFLICTING"}))
	want := []string{"a comment.added", "a slack.replied", "a status.changed"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v (UNKNOWN must not reset mergeable)", got, want)
	}
	if got := step(obs("a", map[string]any{"checks": "FAILURE", "comments": 2.0})); !slices.Equal(got, []string{"a checks.failed"}) {
		t.Fatalf("got %v", got)
	}
}

// An issue assigned away and back, or closed long enough to age out of the
// linear list and then reopened, must not come back with every change since.
func TestDifferForgetMissing(t *testing.T) {
	rules := []config.Transition{{Field: "status", Changed: true, Kind: "status.changed"}}
	for _, tc := range []struct {
		name                           string
		forget, newFire                bool
		wantReturn                     []string
		wantSnapshotWhileGone, wantOld bool
	}{
		{name: "list source", forget: true, wantReturn: nil},
		{name: "list source with new subjects firing", forget: true, newFire: true, wantReturn: []string{"a status.changed"}},
		{name: "for_each source", wantReturn: []string{"a status.changed"}, wantSnapshotWhileGone: true, wantOld: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := state.NewMemory()
			st.SetSnapshot("other", "a", map[string]any{"status": "Todo"})
			d, err := New("s", rules, st)
			if err != nil {
				t.Fatal(err)
			}
			d.ForgetMissing, d.NewSubjectsFire = tc.forget, tc.newFire
			d.Observe([]core.Observation{obs("a", map[string]any{"status": "Todo"}), obs("b", map[string]any{"status": "Todo"})})
			d.Observe([]core.Observation{obs("b", map[string]any{"status": "Todo"})})

			if _, ok := st.Snapshot("s", "a"); ok != tc.wantSnapshotWhileGone {
				t.Errorf("snapshot of a while gone: %v, want %v", ok, tc.wantSnapshotWhileGone)
			}
			if _, ok := st.Snapshot("s", "b"); !ok {
				t.Error("snapshot of b, still listed, was dropped")
			}
			if _, ok := st.Snapshot("other", "a"); !ok {
				t.Error("another source's snapshot of a was dropped")
			}

			evs := d.Observe([]core.Observation{obs("a", map[string]any{"status": "Done"}), obs("b", map[string]any{"status": "Todo"})})
			if got := kinds(evs); !slices.Equal(got, tc.wantReturn) {
				t.Fatalf("a came back: got %v, want %v", got, tc.wantReturn)
			}
			if len(evs) == 1 {
				// A new subject is diffed against nothing, never the snapshot it left.
				want := `status: null -> "Done"`
				if tc.wantOld {
					want = `status: "Todo" -> "Done"`
				}
				if evs[0].Summary != want {
					t.Errorf("summary %q, want %q", evs[0].Summary, want)
				}
			}
		})
	}
}

// A closed issue ages out of the linear list a day after its last update
// and comes back with the next one; a reopen must not be silent.
func TestDifferForgetMissingKeepsClosed(t *testing.T) {
	d, err := New("s", []config.Transition{
		{Field: "status", Changed: true, Kind: "status.changed"},
		{Field: "state", To: "closed", Terminal: true, Kind: "state.closed"},
	}, state.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	d.ForgetMissing = true
	step := func(o ...core.Observation) []string { return kinds(d.Observe(o)) }
	open := func() core.Observation { return obs("a", map[string]any{"status": "Todo", "state": "open"}) }

	step(open())
	if got := step(obs("a", map[string]any{"status": "Done", "state": "closed"})); !slices.Equal(got, []string{"a state.closed", "a status.changed"}) {
		t.Fatalf("closed: got %v", got)
	}
	step() // aged out of the list
	if got := step(open()); !slices.Equal(got, []string{"a status.changed"}) {
		t.Fatalf("reopened: got %v, want a status.changed", got)
	}
}

func TestIncreased(t *testing.T) {
	for _, tc := range []struct {
		prev, cur any
		want      bool
	}{
		{1.0, 2.0, true},
		{2.0, 2.0, false},
		{nil, 1.0, true},
		{nil, 0.0, false},
		{"1761811234.5", "1761811299.1", true},
		{"2026-10-30T09:00:00Z", "2026-10-30T10:00:00Z", true},
		{"2026-10-30T10:00:00Z", "2026-10-30T09:00:00Z", false},
		{"2026-10-30T10:00:00Z", "2026-10-30T10:00:00Z", false},
		{"2026-10-30T10:00:00Z", "2026-10-30T11:00:00+02:00", false}, // 09:00Z
		{nil, "2026-10-30T09:00:00Z", true},
		{"not a time", "2026-10-30T09:00:00Z", false},
	} {
		if got := increased(tc.prev, tc.cur); got != tc.want {
			t.Errorf("increased(%v, %v) = %v, want %v", tc.prev, tc.cur, got, tc.want)
		}
	}
}

// defaultSource builds the mapper and differ of a source from the default config.
func defaultSource(t *testing.T, name string) (*Mapper, *Differ) {
	t.Helper()
	cfg, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	src, ok := cfg.Source(name)
	if !ok {
		t.Fatalf("no %s source", name)
	}
	m, err := NewMapper(src.Map, src.Slurp)
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(src.Name, src.Transitions, state.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	return m, d
}

// The fixtures are synthetic pages of GET /repos/acme/api/notifications:
// a baseline (1), one thread read, two new and one updated (2), all but the
// oldest read (3), all read (4), and one new thread after that (5).
func TestDefaultGitHubInboxSource(t *testing.T) {
	m, d := defaultSource(t, "github-inbox")
	parent := &core.Observation{Subject: "github-inbox:acme/api", Fields: map[string]any{"owner": "acme", "name": "api"}}
	for _, step := range []struct {
		file           string
		summary, fetch string // empty: no event
	}{
		{file: "1.json"},
		{"2.json", "3 new or updated (1 comment, 1 mention, 1 review requested): #430, #431, #17", "gh api repos/acme/api/notifications?since=2026-10-30T09:00:00Z"},
		{file: "3.json"}, // the newest unread thread is older now: not news
		{file: "4.json"}, // all read: the newest updated_at seen stays
		{"5.json", "1 new or updated (1 author): #432", "gh api repos/acme/api/notifications?since=2026-10-29T12:00:00Z"},
	} {
		events := observeFixture(t, m, d, parent, step.file)
		if step.summary == "" {
			if len(events) > 0 {
				t.Fatalf("%s: unexpected events %v", step.file, events)
			}
			continue
		}
		if len(events) != 1 {
			t.Fatalf("%s: got %d events, want 1", step.file, len(events))
		}
		e := events[0]
		if e.Kind != "notifications.new" || e.Subject != parent.Subject || e.Summary != step.summary || strings.Join(e.Fetch, " ") != step.fetch {
			t.Errorf("%s: got %s %s %q fetch %q, want summary %q fetch %q", step.file, e.Subject, e.Kind, e.Summary, e.Fetch, step.summary, step.fetch)
		}
	}
}

// Page 1 of GET /notifications mixes orgs; only acme's threads count, in any
// letter case, and other orgs' activity is not news.
func TestDefaultGitHubInboxOrgSource(t *testing.T) {
	m, d := defaultSource(t, "github-inbox-org")
	parent := &core.Observation{Subject: "github-inbox:acme", Fields: map[string]any{"owner": "acme"}}
	observeFixture(t, m, d, parent, "org-1.json")
	events := observeFixture(t, m, d, parent, "org-2.json")
	if len(events) != 1 || events[0].Summary != "1 new or updated (1 review requested): Acme/cli#9" {
		t.Fatalf("org-2: got %v", events)
	}
	want := []string{"gh", "api", "notifications?since=2026-10-30T09:00:00Z", "--jq", `[.[] | select(.repository.owner.login | ascii_downcase == "acme")]`}
	if !slices.Equal(events[0].Fetch, want) {
		t.Errorf("fetch %q, want %q", events[0].Fetch, want)
	}
	if events := observeFixture(t, m, d, parent, "org-3.json"); len(events) > 0 {
		t.Fatalf("org-3: another org's thread fired %v", events)
	}
}

func observeFixture(t *testing.T, m *Mapper, d *Differ, parent *core.Observation, file string) []core.Event {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/github-inbox/" + file)
	if err != nil {
		t.Fatal(err)
	}
	o, err := m.Map(raw, parent)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return d.Observe(o)
}

// The fixtures are pyrra#1987 as gh api graphql returned it (1.json), and the
// same response after a review, a bot comment, a title edit and a label (2.json).
func TestDefaultGitHubPRSource(t *testing.T) {
	m, d := defaultSource(t, "github-pr")
	parent := &core.Observation{
		Subject: "github:prometheus-community/pyrra#1987",
		Fields:  map[string]any{"owner": "prometheus-community", "name": "pyrra", "number": "1987"},
	}
	var events []core.Event
	for _, f := range []string{"1.json", "2.json"} {
		raw, err := os.ReadFile("../../testdata/github-pr/" + f)
		if err != nil {
			t.Fatal(err)
		}
		o, err := m.Map(raw, parent)
		if err != nil {
			t.Fatal(err)
		}
		events = d.Observe(o)
	}

	got := map[string]core.Event{}
	for _, e := range events {
		got[e.Kind] = e
	}
	want := map[string]struct{ summary, actor string }{
		"review.submitted": {"changes requested by alice", "alice"},
		"comment.added":    {"1 new, latest by cursor[bot]", "cursor[bot]"},
		"title.changed":    {"title edited", ""},
		"labels.changed":   {"labels now: dependencies", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got events %v, want %d", slices.Sorted(maps.Keys(got)), len(want))
	}
	for kind, w := range want {
		e := got[kind]
		if e.Summary != w.summary || e.Actor != w.actor {
			t.Errorf("%s: summary %q actor %q, want %q %q", kind, e.Summary, e.Actor, w.summary, w.actor)
		}
		if e.Subject != parent.Subject {
			t.Errorf("%s: subject %s, want the parent's", kind, e.Subject)
		}
	}
}
