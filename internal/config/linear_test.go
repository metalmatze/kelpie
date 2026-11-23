package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/metalmatze/kelpie/internal/config"
	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/diff"
	"github.com/metalmatze/kelpie/internal/state"
)

var linearStates = map[string]map[string]any{
	"Todo":        {"id": "s-todo", "name": "Todo", "type": "unstarted"},
	"In Progress": {"id": "s-progress", "name": "In Progress", "type": "started"},
	"Done":        {"id": "s-done", "name": "Done", "type": "completed"},
}

type linearStep struct {
	status string // empty: the issue aged out of the list
	at     string
}

// linearPolls renders the assigned query's response for each step, with
// alice making every status change.
func linearPolls(t *testing.T, steps []linearStep) [][]byte {
	var history []map[string]any
	var polls [][]byte
	for _, s := range steps {
		nodes := []map[string]any{}
		if s.status != "" {
			st := linearStates[s.status]
			history = append([]map[string]any{{
				"createdAt": s.at, "toStateId": st["id"],
				"user": map[string]any{"displayName": "alice", "isMe": false},
			}}, history...)
			nodes = append(nodes, map[string]any{
				"identifier": "ENG-1", "url": "https://linear.app/acme/issue/ENG-1/x", "updatedAt": s.at, "state": st,
				"assignee":         map[string]any{"id": "u-me", "displayName": "me", "isMe": true},
				"comments":         map[string]any{"nodes": []any{}},
				"history":          map[string]any{"nodes": history},
				"inverseRelations": map[string]any{"nodes": []any{}},
			})
		}
		b, err := json.Marshal(map[string]any{"data": map[string]any{"viewer": map[string]any{"assignedIssues": map[string]any{"nodes": nodes}}}})
		if err != nil {
			t.Fatal(err)
		}
		polls = append(polls, b)
	}
	return polls
}

// linearConfig registers the describe output kelpie-linear embeds, without
// building the binary.
func linearConfig(t *testing.T) *config.Config {
	t.Helper()
	path, err := filepath.Abs("../../cmd/kelpie-linear/describe.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(t.Context(), []byte(`providers: {linear: {command: [sh, -c, "cat '`+path+`'"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Skipped) > 0 || len(cfg.Warnings) > 0 {
		t.Fatalf("skipped %v, warnings %v", cfg.Skipped, cfg.Warnings)
	}
	return cfg
}

// The Linear sources come from the provider, with its credential's curl
// header line for the short form auth: linear.
func TestLinearProvider(t *testing.T) {
	cfg := linearConfig(t)
	for _, name := range []string{"linear", "linear-issue", "linear-inbox"} {
		src, ok := cfg.Source(name)
		if !ok || cfg.From[name] != "linear" {
			t.Fatalf("%s: %v, from %q", name, ok, cfg.From[name])
		}
		if want := `header = "Authorization: {token}"`; src.Auth == nil || src.Auth.Stdin != want {
			t.Errorf("%s: auth %+v, want stdin %q", name, src.Auth, want)
		}
	}
	d, _ := cfg.Provider("linear")
	if !d.Claims("https://linear.app/acme/issue/ENG-1/x") || !d.Claims("ENG-1") || !d.Claims("https://linear.app/acme/inbox") || d.Claims("acme/api") || d.Claims("inbox") {
		t.Error("targets")
	}
	plain, err := config.Parse(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.Source("linear"); ok {
		t.Error("linear without its provider")
	}
}

// A completed issue that opens again reports it, whether it goes back to
// the status it had or to another, while still in the list or after it
// aged out of it. The snapshot keeps the last open status, so status.changed
// alone misses a reopen to the same status.
func TestLinearReopen(t *testing.T) {
	cfg := linearConfig(t)
	for _, name := range []string{"linear", "linear-issue"} {
		src, _ := cfg.Source(name)
		if !slices.ContainsFunc(src.Transitions, func(tr config.Transition) bool { return tr.Kind == "state.reopened" }) {
			t.Errorf("%s has no state.reopened transition", name)
		}
	}
	src, _ := cfg.Source("linear")
	for _, tc := range []struct {
		name  string
		steps []linearStep
		want  []string
	}{
		{"same status, within the day", []linearStep{
			{"In Progress", "2026-11-10T10:00:00Z"}, {"Done", "2026-11-10T11:00:00Z"}, {"In Progress", "2026-11-10T12:00:00Z"},
		}, []string{"state.reopened"}},
		{"other status, within the day", []linearStep{
			{"In Progress", "2026-11-10T10:00:00Z"}, {"Done", "2026-11-10T11:00:00Z"}, {"Todo", "2026-11-10T12:00:00Z"},
		}, []string{"state.reopened", "status.changed"}},
		{"same status, after aging out", []linearStep{
			{"In Progress", "2026-11-10T10:00:00Z"}, {"Done", "2026-11-10T11:00:00Z"}, {"", ""}, {"In Progress", "2026-11-13T09:00:00Z"},
		}, []string{"state.reopened"}},
		{"other status, after aging out", []linearStep{
			{"In Progress", "2026-11-10T10:00:00Z"}, {"Done", "2026-11-10T11:00:00Z"}, {"", ""}, {"Todo", "2026-11-13T09:00:00Z"},
		}, []string{"state.reopened", "status.changed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := diff.NewMapper(src.Map, src.Slurp)
			if err != nil {
				t.Fatal(err)
			}
			d, err := diff.New(src.Name, src.Transitions, state.NewMemory())
			if err != nil {
				t.Fatal(err)
			}
			d.ForgetMissing, d.Page = src.ForgetMissing, src.Page
			polls := linearPolls(t, tc.steps)
			var kinds []string
			for i, raw := range polls {
				obs, err := m.Map(raw, nil)
				if err != nil {
					t.Fatal(err)
				}
				evs := d.Observe(obs)
				if i == 1 && (len(evs) != 1 || evs[0].Kind != "state.completed") {
					t.Fatalf("completing: %+v", evs)
				}
				if i != len(polls)-1 {
					continue
				}
				for _, ev := range evs {
					kinds = append(kinds, ev.Kind)
					if ev.Kind == "state.reopened" && (ev.Actor != "alice" || ev.Summary != "reopened: Done → "+tc.steps[len(tc.steps)-1].status+" by alice") {
						t.Errorf("reopened: actor %q, summary %q", ev.Actor, ev.Summary)
					}
				}
			}
			slices.Sort(kinds)
			if !slices.Equal(kinds, tc.want) {
				t.Errorf("reopening fired %v, want %v", kinds, tc.want)
			}
		})
	}
}

// The fixtures are synthetic responses of the inbox query: a baseline with
// one unread and one read notification (1), three new unread ones on two
// issues (2), all but one read (3), and a new project update next to a new
// but snoozed comment (4). Only unread, unsnoozed notifications created
// after the newest one seen are news.
func TestLinearInboxSource(t *testing.T) {
	src, _ := linearConfig(t).Source("linear-inbox")
	m, err := diff.NewMapper(src.Map, src.Slurp)
	if err != nil {
		t.Fatal(err)
	}
	d, err := diff.New(src.Name, src.Transitions, state.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	parent := &core.Observation{Subject: "linear-inbox:me", Fields: map[string]any{"who": "me"}}
	for _, step := range []struct{ file, summary string }{
		{file: "1.json"},
		{"2.json", "3 new (1 issue assigned to you, 1 issue mention, 1 issue new comment): ENG-3, ENG-4"},
		{file: "3.json"},
		{"4.json", "1 new (1 project update created)"},
	} {
		raw, err := os.ReadFile("../../testdata/linear-inbox/" + step.file)
		if err != nil {
			t.Fatal(err)
		}
		obs, err := m.Map(raw, parent)
		if err != nil {
			t.Fatalf("%s: %v", step.file, err)
		}
		evs := d.Observe(obs)
		if step.summary == "" {
			if len(evs) > 0 {
				t.Fatalf("%s: unexpected events %+v", step.file, evs)
			}
			continue
		}
		if len(evs) != 1 || evs[0].Kind != "notifications.new" || evs[0].Subject != "linear-inbox:me" || evs[0].Summary != step.summary {
			t.Fatalf("%s: got %+v, want summary %q", step.file, evs, step.summary)
		}
	}
}
