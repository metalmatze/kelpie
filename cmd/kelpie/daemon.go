package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	"github.com/metalmatze/kelpie/internal/schedule"
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
	sched   *schedule.Scheduler
	hosts   []host.Host
	runners map[string]*runner

	mu       sync.Mutex
	lastSeen map[string]string // host -> pane IDs at the last resync, to log only changes

	badgeMu sync.Mutex
	badges  map[core.Subject]string // latest badge per subject, from its source's badge expression
	shown   map[string]string       // workspace -> badge the host shows, to report only changes
}

type runner struct {
	s        config.Source
	a        *adapter.CLI
	d        *diff.Differ
	pattern  *core.Pattern // for_each.subjects
	children []*runner     // for_each.source pointing at this runner
	badge    *diff.Expr
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

	d := &daemon{ctx: ctx, runners: map[string]*runner{}, lastSeen: map[string]string{}, badges: map[core.Subject]string{}, shown: map[string]string{}}
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
	// Before the first workspace refresh, which ends the reminders of panes
	// that closed while kelpie was down.
	d.sched, err = schedule.New(filepath.Join(control.StateDir(), "reminders.json"), cfg.Reminders.CatchUp.Duration,
		func(r core.Reminder, late time.Duration) { go d.fireReminder(r, late) })
	if err != nil {
		return err
	}

	// Workspaces first: a watch whose pane closed while kelpie was down ends
	// here instead of polling for nobody.
	d.refreshWorkspaces()
	for _, bd := range d.binder.AllBindings() {
		d.startWatches(bd.Subject) // once per subject: the adapter skips running loops
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		if err := control.Serve(ctx, control.SocketPath(), d); err != nil {
			slog.Error("control socket failed; kelpie watch will not work", "err", err)
		}
	})
	wg.Go(func() { d.sched.Run(ctx) })
	// Bindings also end without the daemon hearing of it: the router unbinds
	// after a merged PR's last delivery, a fired one-shot reminder is gone.
	wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
				d.refreshBadges()
			}
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
	d.clearBadges()
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
		if r.badge, err = diff.NewExpr(s.Badge); err != nil {
			return fmt.Errorf("source %s: badge: %w", s.Name, err)
		}
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
		if r.badge != nil {
			d.setBadges(r, obs)
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
// each running for as long as some workspace watches it. A subject has one
// loop no matter how many workspaces watch it.
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
			return len(d.binder.Bindings(subject)) > 0
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
	if _, err := d.pollSubject(ctx, subject, false); err != nil {
		return control.WatchResponse{}, err
	}
	ws, err := d.workspace(req.Workspace)
	if err != nil {
		return control.WatchResponse{}, err
	}
	resp := control.WatchResponse{Subject: subject, Workspace: ws.ID}
	for _, bd := range d.binder.Bindings(subject) {
		if bd.Workspace != ws.ID {
			resp.Others = append(resp.Others, bd.Workspace)
		}
	}
	// Watching again from the same workspace keeps the original time.
	if _, ok := d.binder.Binding(subject, ws.ID); !ok {
		d.binder.Bind(core.Binding{Subject: subject, Workspace: ws.ID, Via: "explicit", Since: time.Now()})
	}
	d.startWatches(subject)
	d.refreshBadges()
	slog.Info("watch", "subject", subject, "workspace", ws.ID, "others", resp.Others)
	return resp, nil
}

// pollSubject runs every for_each.subjects source that handles subject once
// and returns the fetch command of the first observation. With observe, the
// observations go through the diff like a tick of the watch loop, so pending
// changes are routed now.
func (d *daemon) pollSubject(ctx context.Context, subject core.Subject, observe bool) ([]string, error) {
	matched := false
	var fetch []string
	for _, r := range d.runners {
		if r.pattern == nil {
			continue
		}
		fields, ok := r.pattern.Match(subject)
		if !ok {
			continue
		}
		matched = true
		_, obs, err := r.a.Once(ctx, &core.Observation{Subject: subject, Fields: fields})
		if err != nil {
			return nil, err
		}
		if observe {
			d.observer(r)(obs)
		}
		if fetch == nil && len(obs) > 0 {
			fetch = obs[0].Fetch
		}
	}
	if !matched {
		return nil, fmt.Errorf("no source watches subjects like %s", subject)
	}
	return fetch, nil
}

func (d *daemon) workspace(id string) (host.Workspace, error) {
	if ws, ok := d.binder.Workspace(id); ok {
		return ws, nil
	}
	d.refreshWorkspaces() // a pane opened since the last host poll
	if ws, ok := d.binder.Workspace(id); ok {
		return ws, nil
	}
	return host.Workspace{}, fmt.Errorf("workspace %s is not known to any host", id)
}

func (d *daemon) Remind(ctx context.Context, req control.RemindRequest) (core.Reminder, error) {
	ws, err := d.workspace(req.Workspace)
	if err != nil {
		return core.Reminder{}, err
	}
	r := core.Reminder{Workspace: ws.ID, Text: strings.Join(strings.Fields(req.Text), " "), Due: req.Due, Cron: req.Cron}
	if req.Subject != "" {
		if r.Subject, err = core.ParseSubject(string(req.Subject)); err != nil {
			return core.Reminder{}, err
		}
		if _, err := d.pollSubject(ctx, r.Subject, false); err != nil {
			return core.Reminder{}, err
		}
	}
	if r, err = d.sched.Add(r); err != nil {
		return core.Reminder{}, err
	}
	d.refreshBadges()
	slog.Info("remind", "id", r.ID, "workspace", r.Workspace, "due", r.Due, "cron", r.Cron, "subject", r.Subject)
	return r, nil
}

func (d *daemon) Unremind(_ context.Context, id string) error {
	if err := d.sched.Remove(id); err != nil {
		return err
	}
	d.refreshBadges()
	slog.Info("unremind", "id", id)
	return nil
}

// fireReminder routes a due reminder to its workspace. With a subject, that
// subject is polled first, so its pending changes go out in the same prompt.
func (d *daemon) fireReminder(r core.Reminder, late time.Duration) {
	ev := core.Event{
		ID:        fmt.Sprintf("%s@%d", r.ID, r.Due.Unix()),
		Kind:      "reminder.due",
		Subject:   r.Subject,
		At:        time.Now(),
		Urgent:    true,
		Workspace: r.Workspace,
	}
	if r.Subject == "" {
		ev.Subject = core.Subject("reminder:" + r.ID)
	} else {
		ctx, cancel := context.WithTimeout(d.ctx, time.Minute)
		fetch, err := d.pollSubject(ctx, r.Subject, true)
		cancel()
		if err != nil {
			slog.Warn("reminder: polling its subject failed", "id", r.ID, "subject", r.Subject, "err", err)
		}
		ev.Fetch = fetch
	}
	var notes []string
	if r.Cron != "" {
		notes = append(notes, "every "+r.Cron)
	} else {
		notes = append(notes, "set "+r.Created.Local().Format("Jan 2 15:04"))
	}
	if late >= time.Minute {
		notes = append(notes, fmt.Sprintf("due %s, %s late", r.Due.Local().Format("Jan 2 15:04"), shortDuration(late)))
	}
	ev.Summary = fmt.Sprintf("%s (%s)", r.Text, strings.Join(notes, "; "))
	d.router.Route(ev)
}

// shortDuration rounds to minutes and drops zero units: 1h, 1h40m, 40m.
func shortDuration(d time.Duration) string {
	s := d.Round(time.Minute).String()
	s = strings.TrimSuffix(s, "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// Unwatch ends the watch of one workspace. Others watching the subject keep
// theirs, and its loop runs until the last one ends.
func (d *daemon) Unwatch(_ context.Context, req control.WatchRequest) error {
	subject, err := core.ParseSubject(string(req.Subject))
	if err != nil {
		return err
	}
	if req.Workspace == "" {
		return errors.New("no workspace given")
	}
	if _, ok := d.binder.Binding(subject, req.Workspace); !ok {
		return fmt.Errorf("pane %s is not watching %s", req.Workspace, subject)
	}
	d.binder.Unbind(subject, req.Workspace)
	d.refreshBadges()
	slog.Info("unwatch", "subject", subject, "workspace", req.Workspace)
	return nil
}

func (d *daemon) Status(context.Context) control.Status {
	return control.Status{Watches: d.binder.AllBindings(), Reminders: d.sched.List(""), Queued: d.router.Queued()}
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
	d.sched.Prune(d.alive)
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
	// A resync follows a reconnect, maybe to a restarted herdr that forgot
	// every token, so report them all again.
	d.badgeMu.Lock()
	for id := range d.shown {
		if ws, ok := d.binder.Workspace(id); !ok || ws.Host == hostName {
			delete(d.shown, id)
		}
	}
	d.badgeMu.Unlock()
	d.refreshBadges()
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
	d.sched.Prune(d.alive)
	slog.Info("pane closed", "id", id)
}

func (d *daemon) alive(workspace string) bool {
	_, ok := d.binder.Workspace(workspace)
	return ok
}

func (d *daemon) Move(_, from string, ws host.Workspace) {
	d.binder.Move(from, ws)
	d.sched.Move(from, ws.ID)
	slog.Info("pane moved", "from", from, "to", ws.ID)
	d.refreshBadges()
}

// setBadges stores the badge of each observed subject and shows the changes.
func (d *daemon) setBadges(r *runner, obs []core.Observation) {
	d.badgeMu.Lock()
	for _, o := range obs {
		v, err := r.badge.Eval(o.Fields, nil)
		text, _ := v.(string)
		if err != nil {
			slog.Warn("badge failed", "source", r.s.Name, "subject", o.Subject, "err", err)
		}
		d.badges[o.Subject] = text
	}
	d.badgeMu.Unlock()
	d.refreshBadges()
}

// refreshBadges shows each workspace the badges of what it watches, and how
// many reminders it has, reporting to the host only what changed.
func (d *daemon) refreshBadges() {
	d.badgeMu.Lock()
	defer d.badgeMu.Unlock()
	parts := map[string][]string{}
	watched := map[core.Subject]bool{}
	for _, bd := range d.binder.AllBindings() {
		watched[bd.Subject] = true
		if b := d.badges[bd.Subject]; b != "" {
			parts[bd.Workspace] = append(parts[bd.Workspace], b)
		}
	}
	for s := range d.badges {
		if !watched[s] {
			delete(d.badges, s)
		}
	}
	reminders := map[string]int{}
	for _, r := range d.sched.List("") {
		reminders[r.Workspace]++
	}

	live := map[string]bool{}
	for _, ws := range d.binder.Workspaces() {
		live[ws.ID] = true
		text := badgeText(parts[ws.ID], reminders[ws.ID])
		if old, ok := d.shown[ws.ID]; ok && old == text || !ok && text == "" {
			continue
		}
		h := d.host(ws.Host)
		if h == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
		err := h.Badge(ctx, ws, text)
		cancel()
		if err != nil {
			slog.Warn("badge not shown", "workspace", ws.ID, "err", err)
			continue
		}
		slog.Debug("badge", "workspace", ws.ID, "text", text)
		d.shown[ws.ID] = text
	}
	maps.DeleteFunc(d.shown, func(id, _ string) bool { return !live[id] })
}

func badgeText(parts []string, reminders int) string {
	slices.Sort(parts)
	if reminders > 0 {
		parts = append(parts, fmt.Sprintf("⏰%d", reminders))
	}
	return strings.Join(parts, " · ")
}

// clearBadges removes every badge on shutdown, so no pane shows a watch that
// nothing runs anymore. d.ctx is done by then.
func (d *daemon) clearBadges() {
	d.badgeMu.Lock()
	defer d.badgeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for id, text := range d.shown {
		ws, ok := d.binder.Workspace(id)
		h := d.host(ws.Host)
		if !ok || h == nil || text == "" {
			continue
		}
		if err := h.Badge(ctx, ws, ""); err != nil {
			slog.Warn("badge not cleared", "workspace", id, "err", err)
		}
	}
}

func (d *daemon) host(name string) host.Host {
	for _, h := range d.hosts {
		if h.Name() == name {
			return h
		}
	}
	return nil
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

func (p printHost) Badge(_ context.Context, ws host.Workspace, text string) error {
	fmt.Printf("--- badge on %s %s: %q\n", ws.Host, ws.ID, text)
	return nil
}

func (p printHost) Deliver(_ context.Context, ws host.Workspace, text string) error {
	fmt.Printf("--- to %s %s (%s)\n%s\n---\n", ws.Host, ws.ID, ws.Path, text)
	return nil
}
