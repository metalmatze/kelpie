package route

import (
	"slices"
	"strings"

	"github.com/metalmatze/kelpie/internal/config"
	"github.com/metalmatze/kelpie/internal/core"
)

type Rules struct {
	ignore, allow, forward []string
}

// NewRules resolves "@me" to the person's own names, one per provider that
// has a me, like their GitHub login. With none, "@me" matches nobody rather
// than everybody. "@me" also stays as is: a source that knows its viewer,
// like Linear with isMe, reports the person's own changes with the actor
// "@me" itself.
func NewRules(c config.Rules, me []string) Rules {
	r := Rules{allow: c.AllowBots, forward: c.Forward}
	for _, p := range c.IgnoreActors {
		if p == "@me" {
			r.ignore = append(r.ignore, me...)
		}
		r.ignore = append(r.ignore, p)
	}
	return r
}

func (r Rules) Drop(ev core.Event) (reason string, drop bool) {
	if len(r.forward) > 0 && !slices.Contains(r.forward, ev.Kind) {
		return "kind not forwarded", true
	}
	if ev.Actor == "" {
		return "", false
	}
	for _, p := range r.allow {
		if glob(p, ev.Actor) {
			return "", false
		}
	}
	for _, p := range r.ignore {
		if glob(p, ev.Actor) {
			return "ignored actor", true
		}
	}
	return "", false
}

// glob matches with * as the only wildcard. Not path.Match: there "*[bot]"
// is a character class and matches any login ending in b, o or t.
func glob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	rest, ok := strings.CutPrefix(s, parts[0])
	if !ok {
		return false
	}
	for _, p := range parts[1 : len(parts)-1] {
		_, after, found := strings.Cut(rest, p)
		if !found {
			return false
		}
		rest = after
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}
