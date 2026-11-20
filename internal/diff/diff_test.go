package diff

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// The linear query asks for 50 issues. Past 50 the ones updated longest ago
// fall off the page, and the update that brings one back must not be silent.
func TestDifferForgetMissingFullPage(t *testing.T) {
	st := state.NewMemory()
	d, err := New("s", []config.Transition{{Field: "status", Changed: true, Kind: "status.changed"}}, st)
	if err != nil {
		t.Fatal(err)
	}
	d.ForgetMissing, d.Page = true, 2
	step := func(o ...core.Observation) []string { return kinds(d.Observe(o)) }
	todo := func(s string) core.Observation { return obs(s, map[string]any{"status": "Todo"}) }

	step(todo("a"), todo("b"))
	step(todo("c"), todo("b")) // a fell off a full page
	if got := step(obs("a", map[string]any{"status": "Done"}), todo("c")); !slices.Equal(got, []string{"a status.changed"}) {
		t.Fatalf("a back on the page: got %v, want a status.changed", got)
	}
	step(todo("a"))
	if _, ok := st.Snapshot("s", "c"); ok {
		t.Error("a page that is not full kept the snapshot of c, which left")
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

// githubConfig registers the describe output kelpie-github embeds, without
// building the binary.
func githubConfig(t *testing.T) *config.Config {
	t.Helper()
	path, err := filepath.Abs("../../cmd/kelpie-github/describe.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(t.Context(), []byte("providers:\n  github: {command: [sh, -c, \"cat '"+path+"'\"]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Skipped) > 0 {
		t.Fatal(cfg.Skipped)
	}
	return cfg
}

// githubSource builds the mapper and differ of a kelpie-github source.
func githubSource(t *testing.T, name string) (*Mapper, *Differ) {
	t.Helper()
	cfg := githubConfig(t)
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
func TestGitHubInboxSource(t *testing.T) {
	m, d := githubSource(t, "github-inbox")
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
func TestGitHubInboxOrgSource(t *testing.T) {
	m, d := githubSource(t, "github-inbox-org")
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
	return observeFixtureIn(t, "github-inbox", m, d, parent, file)
}

func observeFixtureIn(t *testing.T, dir string, m *Mapper, d *Differ, parent *core.Observation, file string) []core.Event {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/" + dir + "/" + file)
	if err != nil {
		t.Fatal(err)
	}
	o, err := m.Map(raw, parent)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return d.Observe(o)
}

// The fixtures are synthetic pages of GET /repos/acme/api/releases: a
// baseline of full releases and a pre-release (1), a newer pre-release and a
// draft (2), two new full releases listed out of publish order (3), and the
// same page again (4). Only full releases published since the last poll are
// news, all in one update, and nothing of their names or notes is in it.
func TestGitHubReleaseSource(t *testing.T) {
	cfg := githubConfig(t)
	if src, _ := cfg.Source("github-release"); src.Every.Duration != time.Hour {
		t.Errorf("every %v, want 1h", src.Every.Duration)
	}
	m, d := githubSource(t, "github-release")
	parent := &core.Observation{Subject: "github-release:acme/api", Fields: map[string]any{"owner": "acme", "name": "api"}}
	for _, step := range []struct {
		file           string
		summary, fetch string // empty: no event
	}{
		{file: "1.json"},
		{file: "2.json"}, // a pre-release and a draft are not news
		{"3.json", "2 new releases: v1.3.1, v1.3.0", `gh api repos/acme/api/releases?per_page=30 --jq [.[] | select(.tag_name | IN("v1.3.1", "v1.3.0"))]`},
		{file: "4.json"},
	} {
		events := observeFixtureIn(t, "github-release", m, d, parent, step.file)
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
		if e.Kind != "release.new" || e.Subject != parent.Subject || e.Summary != step.summary || strings.Join(e.Fetch, " ") != step.fetch {
			t.Errorf("%s: got %s %s %q fetch %q, want summary %q fetch %q", step.file, e.Subject, e.Kind, e.Summary, e.Fetch, step.summary, step.fetch)
		}
		if e.Actor != "" {
			t.Errorf("%s: actor %q; a release has none, or the bot rule would drop releases published from CI", step.file, e.Actor)
		}
		if e.URL != "https://github.com/acme/api/releases" {
			t.Errorf("%s: url %q", step.file, e.URL)
		}
	}
}

// Watching a repo's releases again after the last watch ended reports only
// what is published from then on, not what came out in between.
func TestGitHubReleaseSourceRewatch(t *testing.T) {
	cfg := githubConfig(t)
	src, _ := cfg.Source("github-release")
	m, err := NewMapper(src.Map, src.Slurp)
	if err != nil {
		t.Fatal(err)
	}
	st := state.NewMemory()
	d, err := New(src.Name, src.Transitions, st)
	if err != nil {
		t.Fatal(err)
	}
	parent := &core.Observation{Subject: "github-release:acme/api", Fields: map[string]any{"owner": "acme", "name": "api"}}
	st.SetBinding(core.Binding{Subject: parent.Subject, Workspace: "w1:p1"})
	observeFixtureIn(t, "github-release", m, d, parent, "1.json")
	st.DeleteBinding(parent.Subject, "w1:p1")

	st.SetBinding(core.Binding{Subject: parent.Subject, Workspace: "w1:p1"})
	if events := observeFixtureIn(t, "github-release", m, d, parent, "3.json"); len(events) > 0 {
		t.Fatalf("re-watch reported releases from before it: %v", events)
	}
}

// A repo without releases is a baseline too, and its first release is news,
// fetched with gh release view.
func TestGitHubReleaseSourceFirstRelease(t *testing.T) {
	m, d := githubSource(t, "github-release")
	parent := &core.Observation{Subject: "github-release:acme/api", Fields: map[string]any{"owner": "acme", "name": "api"}}
	if events := observeFixtureIn(t, "github-release", m, d, parent, "empty.json"); len(events) > 0 {
		t.Fatalf("empty: unexpected events %v", events)
	}
	events := observeFixtureIn(t, "github-release", m, d, parent, "first.json")
	if len(events) != 1 || events[0].Summary != "new release v0.1.0" || strings.Join(events[0].Fetch, " ") != "gh release view v0.1.0 -R acme/api" {
		t.Fatalf("first: got %v", events)
	}
}

// The fixtures are synthetic responses of the github-discussion query, shaped
// like cli/cli#14934's: a baseline of two comments, one with a reply (1), a
// new comment (2), replies to two comments by a person and a bot (3), carol's
// comment marked as the answer (4), and the discussion closed (5).
func TestGitHubDiscussionSource(t *testing.T) {
	m, d := githubSource(t, "github-discussion")
	parent := &core.Observation{
		Subject: "github-discussion:acme/api#31",
		Fields:  map[string]any{"owner": "acme", "name": "api", "number": "31"},
	}
	for _, step := range []struct {
		file                        string
		kind, summary, actor, fetch string // empty kind: no event
		terminal                    bool
	}{
		{file: "1.json"},
		{
			file: "2.json", kind: "comment.added", summary: "1 new by carol", actor: "carol",
			fetch: `gh api graphql -f query={repository(owner:"acme",name:"api"){discussion(number:31){comments(last:1){nodes{url author{login} body replies(last:10){totalCount nodes{author{login} body}}}}}}}`,
		},
		// Two authors: no actor, so a reply of your own cannot hide the other.
		{
			file: "3.json", kind: "reply.added", summary: "2 new by alice, copilot[bot], to comments by bob, carol",
			fetch: `gh api graphql -f query={nodes(ids:["DC_bob","DC_carol"]){...on DiscussionComment{url author{login} body replies(last:10){nodes{author{login} body}}}}}`,
		},
		{
			file: "4.json", kind: "answer.marked", summary: "the comment by carol marked as the answer by alice", actor: "alice",
			fetch: `gh api graphql -f query={repository(owner:"acme",name:"api"){discussion(number:31){answer{url author{login} body}}}}`,
		},
		{
			file: "5.json", kind: "state.closed", summary: "closed as resolved; kelpie stops watching", terminal: true,
			fetch: `gh api graphql -f query={repository(owner:"acme",name:"api"){discussion(number:31){title body answer{url author{login}} comments(last:20){totalCount nodes{url author{login} body replies(last:10){totalCount nodes{author{login} body}}}}}}}`,
		},
	} {
		events := observeFixtureIn(t, "github-discussion", m, d, parent, step.file)
		if step.kind == "" {
			if len(events) > 0 {
				t.Fatalf("%s: unexpected events %v", step.file, events)
			}
			continue
		}
		if len(events) != 1 {
			t.Fatalf("%s: got %d events %v, want 1", step.file, len(events), events)
		}
		e := events[0]
		if e.Kind != step.kind || e.Summary != step.summary || e.Actor != step.actor || e.Terminal != step.terminal || strings.Join(e.Fetch, " ") != step.fetch {
			t.Errorf("%s: got %s %q actor %q terminal %v fetch %q,\nwant %s %q actor %q terminal %v fetch %q",
				step.file, e.Kind, e.Summary, e.Actor, e.Terminal, strings.Join(e.Fetch, " "), step.kind, step.summary, step.actor, step.terminal, step.fetch)
		}
		if e.Subject != parent.Subject || e.URL != "https://github.com/acme/api/discussions/31" {
			t.Errorf("%s: subject %s url %s", step.file, e.Subject, e.URL)
		}
	}
}

// The fixtures are pyrra#1987 as gh api graphql returned it (1.json), and the
// same response after a review, a bot comment, a title edit and a label (2.json).
func TestGitHubPRSource(t *testing.T) {
	m, d := githubSource(t, "github-pr")
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

// The fixtures are synthetic responses for acme/api#42 in a repo with a merge
// queue: queued (1), removed by the queue for failed checks (2), queued again
// by bob (3), removed again with the same head (4), a push, a re-queue and a
// removal all between two polls (5), queued by the user's own login (6),
// taken out by alice (7), and merged through the queue (8).
func TestGitHubPRSourceMergeQueue(t *testing.T) {
	m, d := githubSource(t, "github-pr")
	parent := &core.Observation{
		Subject: "github:acme/api#42",
		Fields:  map[string]any{"owner": "acme", "name": "api", "number": "42"},
	}
	runs := func(sha string) string { return "gh run list -R acme/api --commit " + sha }
	const view = "gh pr view 42 -R acme/api --comments"
	for _, step := range []struct {
		file                        string
		kind, summary, actor, fetch string // empty kind: no event
		urgent, terminal            bool
	}{
		{file: "queue-1.json"},
		{
			file: "queue-2.json", kind: "queue.removed", urgent: true,
			summary: "removed from the merge queue by github-merge-queue[bot]: failed checks",
			fetch:   runs("m1m1m1m1m1m1m1m1m1m1m1m1m1m1m1m1m1m1m1m1"),
		},
		{file: "queue-3.json", kind: "queue.added", summary: "added to the merge queue by bob", actor: "bob", fetch: view},
		{
			file: "queue-4.json", kind: "queue.removed", urgent: true,
			summary: "removed from the merge queue by github-merge-queue[bot]: failed checks; removed 2 times since the last push",
			fetch:   runs("m2m2m2m2m2m2m2m2m2m2m2m2m2m2m2m2m2m2m2m2"),
		},
		// The push resets the count, and the count dropping does not hide the
		// removal. The queue entry came and went between polls: no queue.added.
		{
			file: "queue-5.json", kind: "queue.removed", urgent: true,
			summary: "removed from the merge queue by github-merge-queue[bot]: failed checks",
			fetch:   runs("m3m3m3m3m3m3m3m3m3m3m3m3m3m3m3m3m3m3m3m3"),
		},
		// The actor is the user's login: ignore_actors @me drops it.
		{file: "queue-6.json", kind: "queue.added", summary: "added to the merge queue by me", actor: "me", fetch: view},
		{
			file: "queue-7.json", kind: "queue.removed", urgent: true, actor: "alice",
			summary: "removed from the merge queue by alice: manually removed; removed 2 times since the last push",
			fetch:   "gh pr view 42 -R acme/api",
		},
		{file: "queue-8.json", kind: "state.merged", summary: "merged; kelpie stops watching", urgent: true, terminal: true, fetch: view},
	} {
		// The push in queue-5.json also fires commits.pushed; only the queue
		// and state events are under test.
		var events []core.Event
		for _, e := range observeFixtureIn(t, "github-pr", m, d, parent, step.file) {
			if strings.HasPrefix(e.Kind, "queue.") || strings.HasPrefix(e.Kind, "state.") {
				events = append(events, e)
			}
		}
		if step.kind == "" {
			if len(events) > 0 {
				t.Fatalf("%s: unexpected events %v", step.file, kinds(events))
			}
			continue
		}
		if len(events) != 1 {
			t.Fatalf("%s: got events %v, want one %s", step.file, kinds(events), step.kind)
		}
		e := events[0]
		if e.Kind != step.kind || e.Summary != step.summary || e.Actor != step.actor || e.Urgent != step.urgent || e.Terminal != step.terminal || strings.Join(e.Fetch, " ") != step.fetch {
			t.Errorf("%s: got %s %q actor %q urgent %v terminal %v fetch %q,\nwant %s %q actor %q urgent %v terminal %v fetch %q",
				step.file, e.Kind, e.Summary, e.Actor, e.Urgent, e.Terminal, strings.Join(e.Fetch, " "),
				step.kind, step.summary, step.actor, step.urgent, step.terminal, step.fetch)
		}
	}
}

func TestGitHubPRBadgeQueued(t *testing.T) {
	m, _ := githubSource(t, "github-pr")
	cfg := githubConfig(t)
	src, _ := cfg.Source("github-pr")
	badge, err := NewExpr(src.Badge)
	if err != nil {
		t.Fatal(err)
	}
	parent := &core.Observation{
		Subject: "github:acme/api#42",
		Fields:  map[string]any{"owner": "acme", "name": "api", "number": "42"},
	}
	for file, want := range map[string]string{
		"queue-1.json": "#42 approved queued",
		"queue-2.json": "#42 approved",
	} {
		raw, err := os.ReadFile("../../testdata/github-pr/" + file)
		if err != nil {
			t.Fatal(err)
		}
		o, err := m.Map(raw, parent)
		if err != nil {
			t.Fatal(err)
		}
		got, err := badge.Eval(o[0].Fields, nil)
		if err != nil || got != want {
			t.Errorf("%s: badge %v, %v; want %q", file, got, err, want)
		}
	}
}
