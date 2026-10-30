package diff

import (
	"maps"
	"os"
	"slices"
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

// The fixtures are pyrra#1987 as gh api graphql returned it (1.json), and the
// same response after a review, a bot comment, a title edit and a label (2.json).
func TestDefaultGitHubPRSource(t *testing.T) {
	cfg, err := config.Parse(config.Default)
	if err != nil {
		t.Fatal(err)
	}
	src, ok := cfg.Source("github-pr")
	if !ok {
		t.Fatal("no github-pr source")
	}
	m, err := NewMapper(src.Map, src.Slurp)
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(src.Name, src.Transitions, state.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
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
