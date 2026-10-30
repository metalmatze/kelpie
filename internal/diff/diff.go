package diff

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/metalmatze/kelpie/internal/config"
	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/state"
)

// Differ turns successive snapshots of one source into events.
type Differ struct {
	// NewSubjectsFire compares a subject's first observation against an empty
	// one instead of storing it silently. for_each sources set it: their
	// parent only starts them when something is happening (checks pending),
	// so the first look is news, not a baseline.
	NewSubjectsFire bool

	source    string
	state     state.State
	rules     []rule
	baselined bool
	now       func() time.Time
	log       *slog.Logger
}

type rule struct {
	config.Transition
	summary, actor, fetch *Expr
}

func New(source string, transitions []config.Transition, st state.State) (*Differ, error) {
	d := &Differ{source: source, state: st, now: time.Now, log: slog.With("source", source)}
	for i, t := range transitions {
		if t.Field == "" || t.Kind == "" {
			return nil, fmt.Errorf("transition %d: field and kind are required", i)
		}
		if t.To == nil && !t.Changed && !t.Increased {
			return nil, fmt.Errorf("transition %s: one of to, changed or increased is required", t.Kind)
		}
		r := rule{Transition: t}
		var err error
		if r.summary, err = NewExpr(t.Summary); err != nil {
			return nil, fmt.Errorf("transition %s summary: %w", t.Kind, err)
		}
		if r.actor, err = NewExpr(t.Actor); err != nil {
			return nil, fmt.Errorf("transition %s actor: %w", t.Kind, err)
		}
		if r.fetch, err = NewExpr(t.Fetch); err != nil {
			return nil, fmt.Errorf("transition %s fetch: %w", t.Kind, err)
		}
		d.rules = append(d.rules, r)
	}
	return d, nil
}

// Observe stores the new snapshot and returns the transitions since the last.
// The first call is a baseline and returns nothing, so a restart never floods
// agents with old news. A subject seen for the first time later on is stored
// silently too: it has no previous state to transition from.
func (d *Differ) Observe(obs []core.Observation) []core.Event {
	var events []core.Event
	for _, o := range obs {
		old, seen := d.state.Snapshot(d.source, o.Subject)
		d.keepKnown(o.Fields, old)
		d.state.SetSnapshot(d.source, o.Subject, o.Fields)
		if !d.NewSubjectsFire && (!d.baselined || !seen) {
			continue
		}
		for _, r := range d.rules {
			if ev, ok := d.fire(r, o, old); ok {
				events = append(events, ev)
			}
		}
	}
	d.baselined = true
	return events
}

func (d *Differ) keepKnown(fields, old map[string]any) {
	for _, r := range d.rules {
		cur, ok := fields[r.Field]
		if !ok || !slices.ContainsFunc(r.Ignore, func(v any) bool { return equal(v, cur) }) {
			continue
		}
		if prev, ok := old[r.Field]; ok {
			fields[r.Field] = prev
		}
	}
}

func (d *Differ) fire(r rule, o core.Observation, old map[string]any) (core.Event, bool) {
	prev, hadPrev := old[r.Field]
	cur, hasCur := o.Fields[r.Field]
	if !hasCur || !hadPrev && !d.NewSubjectsFire {
		return core.Event{}, false
	}
	switch {
	case r.To != nil:
		if equal(prev, r.To) || !equal(cur, r.To) {
			return core.Event{}, false
		}
		if r.From != nil && !equal(prev, r.From) {
			return core.Event{}, false
		}
	case r.Increased:
		if prev == nil {
			prev = 0.0 // NewSubjectsFire: a count rising from nothing
		}
		p, ok1 := number(prev)
		c, ok2 := number(cur)
		if !ok1 || !ok2 || c <= p {
			return core.Event{}, false
		}
	case r.Changed:
		if equal(prev, cur) {
			return core.Event{}, false
		}
	}

	now := d.now()
	ev := core.Event{
		ID:       fmt.Sprintf("%s/%s/%d", o.Subject, r.Kind, now.UnixNano()),
		Kind:     r.Kind,
		Subject:  o.Subject,
		URL:      o.URL,
		At:       now,
		Links:    o.Links,
		Fetch:    o.Fetch,
		Bind:     o.Bind,
		Urgent:   r.Urgent,
		Terminal: r.Terminal,
		Summary:  fmt.Sprintf("%s: %s -> %s", r.Field, compact(prev), compact(cur)),
	}
	// A broken expression degrades the message, it does not drop the event.
	if r.summary != nil {
		if s, err := d.evalString(r.summary, o.Fields, old); err == nil {
			ev.Summary = s
		} else {
			d.log.Warn("summary expression failed", "kind", r.Kind, "err", err)
		}
	}
	if r.actor != nil {
		if s, err := d.evalString(r.actor, o.Fields, old); err == nil {
			ev.Actor = s
		} else {
			d.log.Warn("actor expression failed", "kind", r.Kind, "err", err)
		}
	}
	if r.fetch != nil {
		if f, err := d.evalStrings(r.fetch, o.Fields, old); err == nil {
			ev.Fetch = f
		} else {
			d.log.Warn("fetch expression failed", "kind", r.Kind, "err", err)
		}
	}
	return ev, true
}

func (d *Differ) evalString(e *Expr, fields, old map[string]any) (string, error) {
	v, err := e.Eval(fields, old)
	if err != nil {
		return "", err
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	return compact(v), nil
}

func (d *Differ) evalStrings(e *Expr, fields, old map[string]any) ([]string, error) {
	v, err := e.Eval(fields, old)
	if err != nil {
		return nil, err
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("want an array of strings, got %s", compact(v))
	}
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("want an array of strings, got %s", compact(v))
		}
		out = append(out, s)
	}
	return out, nil
}

// equal compares by JSON encoding, so a TOML int64 matches a float64 field.
func equal(a, b any) bool { return compact(a) == compact(b) }

// number also parses strings, because Slack timestamps ("1761811234.567890")
// must compare numerically for increased to work.
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func compact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
