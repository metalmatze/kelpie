package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// dedupeHandler drops a warning identical to one logged recently. Hosts retry
// on their poll cadence while herdr is down (a unit may start before herdr),
// and each retry would otherwise log the same failure every few seconds into
// a file nothing rotates. Only warnings and errors are deduplicated; the
// recovery lines are Info and always pass.
type dedupeHandler struct {
	slog.Handler
	every time.Duration
	// scope is the derived handler's attrs and groups. Part of the key, so
	// two hosts logging the same text through different With attrs stay apart.
	scope string
	mu    *sync.Mutex
	seen  map[string]time.Time
}

func newDedupeHandler(h slog.Handler, every time.Duration) *dedupeHandler {
	return &dedupeHandler{Handler: h, every: every, mu: &sync.Mutex{}, seen: map[string]time.Time{}}
}

func (d *dedupeHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		var key strings.Builder
		key.WriteString(d.scope)
		key.WriteString("\x01" + r.Message)
		r.Attrs(func(a slog.Attr) bool {
			key.WriteString("\x00" + a.String())
			return true
		})
		d.mu.Lock()
		// Entries older than every no longer suppress anything; dropping them
		// keeps the map bounded when error text carries varying detail.
		for k, t := range d.seen {
			if r.Time.Sub(t) >= d.every {
				delete(d.seen, k)
			}
		}
		last, ok := d.seen[key.String()]
		repeat := ok && r.Time.Sub(last) < d.every
		if !repeat {
			d.seen[key.String()] = r.Time
		}
		d.mu.Unlock()
		if repeat {
			return nil
		}
	}
	return d.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must keep the wrapper, or loggers derived from it
// would bypass the deduplication.
func (d *dedupeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	scope := d.scope
	for _, a := range as {
		scope += "\x02" + a.String()
	}
	return &dedupeHandler{Handler: d.Handler.WithAttrs(as), every: d.every, scope: scope, mu: d.mu, seen: d.seen}
}

func (d *dedupeHandler) WithGroup(name string) slog.Handler {
	return &dedupeHandler{Handler: d.Handler.WithGroup(name), every: d.every, scope: d.scope + "\x03" + name, mu: d.mu, seen: d.seen}
}
