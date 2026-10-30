package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/metalmatze/kelpie/internal/adapter"
	"github.com/metalmatze/kelpie/internal/bind"
	"github.com/metalmatze/kelpie/internal/config"
	"github.com/metalmatze/kelpie/internal/control"
	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/diff"
	"github.com/metalmatze/kelpie/internal/host"
	"github.com/metalmatze/kelpie/internal/host/herdr"
	"github.com/metalmatze/kelpie/internal/route"
	"github.com/metalmatze/kelpie/internal/state"
)

type DaemonCmd struct {
	DryRun bool `help:"Print prompts instead of delivering them."`
	Debug  bool `help:"Log workspaces and every observation tick."`
}

type daemon struct {
	ctx     context.Context
	binder  *bind.Binder
	router  *route.Router
	hosts   []host.Host
	runners map[string]*runner

	mu       sync.Mutex
	lastSeen map[string]string // host -> pane IDs at the last resync, to log only changes
}

type runner struct {
	s        config.Source
	a        *adapter.CLI
	d        *diff.Differ
	pattern  *core.Pattern // for_each.subjects
	children []*runner     // for_each.source pointing at this runner
}

func (c *DaemonCmd) Run(ctx context.Context, g *Globals) error {
	if c.Debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	cfg, err := config.Load(g.Config)
	if err != nil {
		return err
	}
	st := state.NewMemory()
	if err := st.PersistWatches(filepath.Join(control.StateDir(), "watches.json")); err != nil {
		return err
	}

	d := &daemon{ctx: ctx, runners: map[string]*runner{}, lastSeen: map[string]string{}}
	if cfg.Hosts.Herdr.Enabled {
		d.hosts = append(d.hosts, &herdr.Herdr{Bin: cfg.Hosts.Herdr.Bin, Socket: cfg.Hosts.Herdr.Socket, Poll: cfg.Hosts.Herdr.Poll.Duration})
	}
	if len(d.hosts) == 0 {
		return errors.New("no host enabled")
	}
	if c.DryRun {
		for i, h := range d.hosts {
			d.hosts[i] = printHost{h}
		}
	}
	d.binder = bind.New(st, cfg.Binders.Branch)
	d.router = route.New(ctx, cfg.Delivery, route.NewRules(cfg.Rules, ghLogin(ctx)), d.binder, d.hosts)

	if err := d.buildRunners(cfg.Sources, st); err != nil {
		return err
	}

	// Workspaces first: a watch whose pane closed while kelpie was down ends
	// here instead of polling for nobody.
	d.refreshWorkspaces()
	for _, bd := range d.binder.Bindings() {
		d.startWatches(bd.Subject)
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		if err := control.Serve(ctx, control.SocketPath(), d); err != nil {
			slog.Error("control socket failed; kelpie watch will not work", "err", err)
		}
	})
	for _, h := range d.hosts {
		wg.Go(func() {
			err := h.Watch(ctx, d)
			if err != nil && ctx.Err() == nil {
				slog.Error("host watch stopped", "host", h.Name(), "err", err)
			}
		})
	}
	for _, r := range d.runners {
		if r.s.ForEach != nil {
			continue
		}
		wg.Go(func() {
			slog.Info("source started", "source", r.s.Name, "every", r.s.Every.Duration)
			err := r.a.Run(ctx, d.observer(r), d.router.Route)
			if err != nil && ctx.Err() == nil {
				slog.Error("source stopped", "source", r.s.Name, "err", err)
			}
		})
	}
	wg.Wait()
	return ctx.Err()
}

func (d *daemon) buildRunners(sources []config.Source, st state.State) error {
	for _, s := range sources {
		a, err := adapter.New(s)
		if err != nil {
			return err
		}
		df, err := diff.New(s.Name, s.Transitions, st)
		if err != nil {
			return fmt.Errorf("source %s: %w", s.Name, err)
		}
		r := &runner{s: s, a: a, d: df}
		if fe := s.ForEach; fe != nil {
			switch {
			case fe.Subjects != "" && fe.Source != "":
				return fmt.Errorf("source %s: for_each takes subjects or source, not both", s.Name)
			case fe.Subjects != "":
				p, err := core.NewPattern(fe.Subjects)
				if err != nil {
					return fmt.Errorf("source %s: %w", s.Name, err)
				}
				r.pattern = &p
			case fe.Source != "":
				// Started because something is happening (checks pending), so
				// its first look is news, not a baseline.
				df.NewSubjectsFire = true
			default:
				return fmt.Errorf("source %s: for_each needs subjects or source", s.Name)
			}
		}
		d.runners[s.Name] = r
	}
	for _, r := range d.runners {
		if r.s.ForEach == nil || r.s.ForEach.Source == "" {
			continue
		}
		p, ok := d.runners[r.s.ForEach.Source]
		if !ok {
			return fmt.Errorf("source %s: for_each source %q does not exist", r.s.Name, r.s.ForEach.Source)
		}
		p.children = append(p.children, r)
	}
	return nil
}

// observer diffs a runner's observations into events and starts the
// for_each.source children whose while condition now holds.
func (d *daemon) observer(r *runner) func([]core.Observation) {
	return func(obs []core.Observation) {
		slog.Debug("observed", "source", r.s.Name, "subjects", len(obs))
		for _, ev := range r.d.Observe(obs) {
			d.router.Route(ev)
		}
		for _, child := range r.children {
			while := child.s.ForEach.While
			for _, o := range obs {
				if !while.Matches(o.Fields) {
					continue
				}
				failures := 0
				child.a.Watch(d.ctx, o, d.observer(child), func(obs []core.Observation, err error) bool {
					if err != nil {
						failures++
						return failures < 3 // a closed PR fails every time
					}
					failures = 0
					for _, o := range obs {
						if while.Matches(o.Fields) {
							return true
						}
					}
					return false
				})
			}
		}
	}
}

// startWatches starts every for_each.subjects source that handles subject,
// each running for as long as the subject stays bound.
func (d *daemon) startWatches(subject core.Subject) {
	for _, r := range d.runners {
		if r.pattern == nil {
			continue
		}
		fields, ok := r.pattern.Match(subject)
		if !ok {
			continue
		}
		r.a.Watch(d.ctx, core.Observation{Subject: subject, Fields: fields}, d.observer(r), func([]core.Observation, error) bool {
			_, bound := d.binder.Binding(subject)
			return bound
		})
	}
}

func (d *daemon) Watch(ctx context.Context, req control.WatchRequest) (control.WatchResponse, error) {
	subject, err := core.ParseSubject(string(req.Subject))
	if err != nil {
		return control.WatchResponse{}, err
	}
	// One poll up front, so a typo or a PR you cannot read fails here, at the
	// command, instead of as a warning in the daemon log every tick.
	matched := false
	for _, r := range d.runners {
		if r.pattern == nil {
			continue
		}
		fields, ok := r.pattern.Match(subject)
		if !ok {
			continue
		}
		matched = true
		if _, _, err := r.a.Once(ctx, &core.Observation{Subject: subject, Fields: fields}); err != nil {
			return control.WatchResponse{}, err
		}
	}
	if !matched {
		return control.WatchResponse{}, fmt.Errorf("no source watches subjects like %s", subject)
	}
	ws, ok := d.binder.Workspace(req.Workspace)
	if !ok {
		d.refreshWorkspaces() // a pane opened since the last host poll
		if ws, ok = d.binder.Workspace(req.Workspace); !ok {
			return control.WatchResponse{}, fmt.Errorf("workspace %s is not known to any host", req.Workspace)
		}
	}
	resp := control.WatchResponse{Subject: subject, Workspace: ws.ID}
	if prev, ok := d.binder.Binding(subject); ok && prev.Workspace != ws.ID {
		resp.Moved = prev.Workspace
	}
	d.binder.Bind(core.Binding{Subject: subject, Workspace: ws.ID, Via: "explicit", Since: time.Now()})
	d.startWatches(subject)
	slog.Info("watch", "subject", subject, "workspace", ws.ID, "moved_from", resp.Moved)
	return resp, nil
}

func (d *daemon) Unwatch(_ context.Context, subject core.Subject) error {
	subject, err := core.ParseSubject(string(subject))
	if err != nil {
		return err
	}
	if _, ok := d.binder.Binding(subject); !ok {
		return fmt.Errorf("not watching %s", subject)
	}
	d.binder.Unbind(subject)
	slog.Info("unwatch", "subject", subject)
	return nil
}

func (d *daemon) Status(context.Context) control.Status {
	return control.Status{Watches: d.binder.Bindings(), Queued: d.router.Queued()}
}

// refreshWorkspaces asks every host for its full list, for startup and for a
// watch from a pane the event stream has not reported yet.
func (d *daemon) refreshWorkspaces() {
	for _, h := range d.hosts {
		list, err := h.Workspaces(d.ctx)
		if err != nil {
			slog.Warn("list workspaces failed", "host", h.Name(), "err", err)
			continue
		}
		d.Resync(h.Name(), list)
	}
}

// Resync logs the pane list only when it changed: the polling fallback
// resyncs every few seconds.
func (d *daemon) Resync(hostName string, list []host.Workspace) {
	d.binder.SetWorkspaces(hostName, list)
	ids := make([]string, len(list))
	for i, ws := range list {
		ids[i] = ws.ID
	}
	slices.Sort(ids)
	seen := strings.Join(ids, " ")
	d.mu.Lock()
	changed := d.lastSeen[hostName] != seen
	d.lastSeen[hostName] = seen
	d.mu.Unlock()
	if changed {
		slog.Info("workspaces", "host", hostName, "panes", ids)
	}
}

func (d *daemon) Upsert(ws host.Workspace) {
	prev, existed := d.binder.Upsert(ws)
	switch {
	case !existed:
		slog.Info("pane opened", "id", ws.ID, "path", ws.Path, "branch", ws.Branch)
	case prev.Path != ws.Path || prev.Branch != ws.Branch:
		slog.Info("pane changed", "id", ws.ID, "path", ws.Path, "branch", ws.Branch)
	}
}

func (d *daemon) Remove(_, id string) {
	d.binder.Remove(id)
	slog.Info("pane closed", "id", id)
}

func (d *daemon) Move(_, from string, ws host.Workspace) {
	d.binder.Move(from, ws)
	slog.Info("pane moved", "from", from, "to", ws.ID)
}

func (d *daemon) AgentReady(_, id string) { d.router.Kick(id) }

// ghLogin resolves "@me" in ignore_actors. On failure kelpie still runs; it
// just forwards your own comments too.
func ghLogin(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "gh", "api", "user", "--jq", ".login").Output()
	if err != nil {
		slog.Warn("could not resolve @me with gh api user; your own activity is not ignored", "err", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// printHost delivers to stdout, for --dry-run against real workspaces.
type printHost struct{ host.Host }

func (p printHost) Deliver(_ context.Context, ws host.Workspace, text string) error {
	fmt.Printf("--- to %s %s (%s)\n%s\n---\n", ws.Host, ws.ID, ws.Path, text)
	return nil
}
