package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	"github.com/metalmatze/kelpie/internal/host/external"
	"github.com/metalmatze/kelpie/internal/host/hc"
	"github.com/metalmatze/kelpie/internal/host/herdr"
	"github.com/metalmatze/kelpie/internal/host/orca"
	"github.com/metalmatze/kelpie/internal/host/rex"
	"github.com/metalmatze/kelpie/internal/provider"
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
	shown   map[string]string       // workspace -> badge the host shows, as text, to report only changes
}

type runner struct {
	s        config.Source
	a        *adapter.CLI
	d        *diff.Differ
	pattern  *core.Pattern // for_each.subjects
	children []*runner     // for_each.source pointing at this runner
	badge    *diff.Expr
	refuse   *diff.Expr
}

func (c *DaemonCmd) Run(ctx context.Context, g *Globals) error {
	if c.Debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	// An exclusive flock is the single-instance guard. The herdr hook and a
	// launchd/systemd unit start nearly together at login, and a probe of the
	// socket followed by remove-and-listen lets both win, so every update
	// would be delivered twice. The kernel drops the lock when the process
	// dies, so a crash leaves nothing stale. Exit 0 when held: a unit that
	// restarts on failure must not loop against the daemon the hook started.
	lock, err := control.Lock()
	if errors.Is(err, control.ErrLocked) {
		fmt.Printf("kelpie daemon already running (socket %s); exiting\n", control.SocketPath())
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close() // the lock lives as long as this fd stays open

	// The socket answers before the slow startup work (gh, herdr, sources):
	// until it does, the herdr plugin's "no daemon answers" check would start
	// a second daemon. Unwinding on an early error stops it again.
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	starting := &startingHandler{ready: make(chan struct{})}
	// With the lock held, a failure here is real: exit non-zero rather than
	// run unreachable, so a unit restarts the daemon and the hook's status
	// check stays truthful.
	var serveErr error
	wg.Go(func() {
		if serveErr = control.Serve(ctx, control.SocketPath(), starting); serveErr != nil {
			slog.Error("control socket failed", "err", serveErr)
			cancel()
		}
	})

	cfg, err := config.Load(ctx, g.Config)
	if err != nil {
		return err
	}
	for _, s := range cfg.Skipped {
		slog.Warn(s, "file", g.Config)
	}
	for _, p := range cfg.Provided {
		var sources []string
		for _, s := range p.Sources {
			if cfg.From[s.Name] == p.Name {
				sources = append(sources, s.Name)
			}
		}
		slog.Info("provider loaded", "provider", p.Name, "command", p.Command, "sources", sources, "branch_patterns", len(p.Branches), "targets", len(p.Targets))
	}
	for _, w := range cfg.Warnings {
		slog.Warn("config: "+w, "file", g.Config)
	}
	st := state.NewMemory()
	if err := st.PersistWatches(filepath.Join(control.StateDir(), "watches.json")); err != nil {
		return err
	}

	d := &daemon{
		ctx:      ctx,
		binder:   bind.New(st, cfg.Binders.Branch),
		runners:  map[string]*runner{},
		lastSeen: map[string]string{},
		badges:   map[core.Subject]string{},
		shown:    map[string]string{},
	}
	if err := d.initHosts(cfg, c.DryRun); err != nil {
		return err
	}
	for _, p := range cfg.Provided {
		if !cfg.Binders.ByBranch(p.Name) {
			continue
		}
		for _, br := range p.Branches {
			bp, err := core.NewBranchPattern(br.Subject, br.Branch)
			if err != nil {
				return fmt.Errorf("provider %s: %w", p.Name, err)
			}
			d.binder.Branches = append(d.binder.Branches, bp)
		}
	}
	d.router = route.New(ctx, cfg.Delivery, route.NewRules(cfg.Rules, me(ctx, cfg.Provided)), d.binder, d.hosts)

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
		// Once per subject: the adapter skips running loops.
		if !d.startWatches(bd.Subject) {
			slog.Warn("no source polls this watch; is its provider registered under providers:?", "subject", bd.Subject, "workspace", bd.Workspace)
		}
	}

	starting.set(d)
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
	if serveErr != nil {
		return serveErr
	}
	return ctx.Err()
}

func (d *daemon) initHosts(cfg *config.Config, dryRun bool) error {
	if cfg.Hosts.Herdr.Enabled {
		d.hosts = append(d.hosts, &herdr.Herdr{Bin: cfg.Hosts.Herdr.Bin, Socket: cfg.Hosts.Herdr.Socket, Poll: cfg.Hosts.Herdr.Poll.Duration})
	}
	if o := cfg.Hosts.Orca; o.Enabled {
		d.hosts = append(d.hosts, &orca.Orca{Bin: o.Bin, Poll: o.Poll.Duration, Comment: o.Comment, CommentsFile: filepath.Join(control.StateDir(), "orca-comments.json")})
	}
	if c := cfg.Hosts.HC; c.Enabled {
		d.hosts = append(d.hosts, &hc.HC{Socket: c.Socket, Poll: c.Poll.Duration})
	}
	if r := cfg.Hosts.Rex; r.Enabled {
		d.hosts = append(d.hosts, &rex.Rex{Socket: r.Socket, Poll: r.Poll.Duration})
	}
	registerConfigHosts(cfg)
	for _, eh := range cfg.ExternalHosts {
		d.hosts = append(d.hosts, external.New(eh.Name, eh.Command, eh.Poll, eh.Capabilities))
		slog.Info("host loaded", "host", eh.Name, "command", eh.Command, "reports_agent", eh.ReportsAgent, "capabilities", eh.Capabilities)
	}
	if len(d.hosts) == 0 {
		return errors.New("no host enabled")
	}
	if dryRun {
		for i, h := range d.hosts {
			d.hosts[i] = printHost{h}
		}
	}
	for _, h := range d.hosts {
		d.binder.Await(h.Name())
	}
	// Await enabled external hosts even when describe skipped them at startup,
	// so Binder.Resolve keeps their bindings and queues batches instead of
	// deleting saved watches while the host binary is briefly broken or slow.
	for name, eh := range cfg.Hosts.External {
		if eh.IsEnabled() {
			d.binder.Await(name)
		}
	}
	return nil
}

func (d *daemon) buildRunners(sources []config.Source, st state.State) error {
	for _, s := range sources {
		// Forgetting a missing subject assumes each Observe is one whole poll
		// of the list: a for_each Observe holds one parent's subjects, and a
		// streaming adapter would observe one change at a time.
		switch {
		case s.ForgetMissing && s.ForEach != nil:
			return fmt.Errorf("source %s: forget_missing is for a list source; each poll of a for_each source holds one parent's subjects", s.Name)
		case s.ForgetMissing && s.Adapter != "cli":
			return fmt.Errorf("source %s: forget_missing needs adapter cli, whose every poll is the whole list", s.Name)
		case s.Page < 0 || s.Page > 0 && s.ForEach != nil:
			return fmt.Errorf("source %s: page is the page size of a list source, a for_each source has none", s.Name)
		}
		a, err := adapter.New(s)
		if err != nil {
			return err
		}
		df, err := diff.New(s.Name, s.Transitions, st)
		if err != nil {
			return fmt.Errorf("source %s: %w", s.Name, err)
		}
		// The adapter observes only polls that succeeded whole, and only
		// for_each.subjects runners are polled once on top, so a list
		// source's Observe always sees its complete list.
		df.ForgetMissing = s.ForgetMissing
		df.Page = s.Page
		r := &runner{s: s, a: a, d: df}
		if r.badge, err = diff.NewExpr(s.Badge); err != nil {
			return fmt.Errorf("source %s: badge: %w", s.Name, err)
		}
		if r.refuse, err = diff.NewExpr(s.Refuse); err != nil {
			return fmt.Errorf("source %s: refuse: %w", s.Name, err)
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
			// An issue both assigned to you and watched would otherwise reach
			// the agent twice, once from the list and once from its watch.
			if r.s.ForEach == nil && d.polledOnItsOwn(ev.Subject) {
				slog.Debug("left to the subject's own watch", "source", r.s.Name, "subject", ev.Subject, "kind", ev.Kind)
				continue
			}
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

// polledOnItsOwn reports whether a for_each.subjects source runs a loop for
// subject. That loop is the subject's source of events: it polls just this
// subject, usually more often than a list does, and its terminal event ends
// the watch. A binding alone is not enough: one by branch starts no loop, and
// the list is all that reports on it.
func (d *daemon) polledOnItsOwn(subject core.Subject) bool {
	for _, r := range d.runners {
		if r.pattern != nil && r.a.Watching(subject) {
			return true
		}
	}
	return false
}

// startWatches starts every for_each.subjects source that handles subject,
// each running for as long as some workspace watches it. A subject has one
// loop no matter how many workspaces watch it.
// startWatches reports whether a source polls subject. A saved watch whose
// provider is gone stays saved, so it resumes once the provider is back.
func (d *daemon) startWatches(subject core.Subject) bool {
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
		r.a.Watch(d.ctx, core.Observation{Subject: subject, Fields: fields}, d.watchObserver(r, subject), func([]core.Observation, error) bool {
			return len(d.binder.Bindings(subject)) > 0
		})
	}
	return matched
}

// watchObserver drops what a loop observes once nobody watches its subject.
// The loop polls once more before it sees that, up to every later, and the
// snapshot that poll stored would be what a later watch diffs against.
func (d *daemon) watchObserver(r *runner, subject core.Subject) func([]core.Observation) {
	observe := d.observer(r)
	return func(obs []core.Observation) {
		if len(d.binder.Bindings(subject)) == 0 {
			return
		}
		observe(obs)
	}
}

func (d *daemon) Watch(ctx context.Context, req control.WatchRequest) (control.WatchResponse, error) {
	subject, err := core.ParseSubject(string(req.Subject))
	if err != nil {
		return control.WatchResponse{}, err
	}
	// One poll up front, so a typo or a PR you cannot read fails here, at the
	// command, instead of as a warning in the daemon log every tick.
	_, polls, err := d.pollSubject(ctx, subject, false, true)
	if err != nil {
		return control.WatchResponse{}, err
	}
	ws, err := d.workspace(req.Workspace)
	if err != nil {
		return control.WatchResponse{}, err
	}
	resp := control.WatchResponse{Subject: subject, Workspace: ws.ID}
	// A pane takes over its fallback's binding of the subject: a branch
	// binding, or a watch made before panes were workspaces, would deliver
	// the same update a second time, to the worktree's best agent.
	from, _ := d.fallback(ws.ID)
	for _, bd := range d.binder.Bindings(subject) {
		if bd.Workspace != ws.ID && bd.Workspace != from {
			resp.Others = append(resp.Others, bd.Workspace)
		}
	}
	unwatched := len(d.binder.Bindings(subject)) == 0
	// Watching again from the same workspace keeps the original time; taking
	// over the worktree's binding keeps its time.
	since := time.Now()
	if old, ok := d.binder.Binding(subject, from); ok {
		since = old.Since
	}
	if _, ok := d.binder.Binding(subject, ws.ID); !ok {
		d.binder.Bind(core.Binding{Subject: subject, Workspace: ws.ID, Via: "explicit", Since: since})
	}
	// After the Bind: forgetting the last binding of a subject forgets its snapshots.
	if from != "" && from != ws.ID {
		if _, ok := d.binder.Binding(subject, from); ok {
			d.binder.Unbind(subject, from)
			slog.Info("watch moved to pane", "subject", subject, "from", from, "to", ws.ID)
		}
	}
	// The last watch ending forgot the subject's snapshots, and the loop of
	// that watch, which startWatches joins instead of replacing, may sleep up
	// to every before its next poll. The up-front poll is the baseline, so
	// nothing that changes in between becomes a silent one.
	if unwatched {
		for _, p := range polls {
			d.observer(p.r)(p.obs)
		}
	}
	d.startWatches(subject)
	d.refreshBadges()
	slog.Info("watch", "subject", subject, "workspace", ws.ID, "others", resp.Others)
	return resp, nil
}

// polled is what one source observed in a pollSubject call.
type polled struct {
	r   *runner
	obs []core.Observation
}

// pollSubject runs every for_each.subjects source that handles subject once
// and returns the fetch command of the first observation, and what each
// source observed. With observe, the
// observations go through the diff like a tick of the watch loop, so pending
// changes are routed now. With refuse, a source's refuse expression can turn
// the poll into an error, for a watch that would never see a change.
func (d *daemon) pollSubject(ctx context.Context, subject core.Subject, observe, refuse bool) ([]string, []polled, error) {
	matched := false
	var fetch []string
	var polls []polled
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
			return nil, nil, err
		}
		if refuse && r.refuse != nil {
			for _, o := range obs {
				if why, err := r.refuse.Eval(o.Fields, nil); err == nil && why != nil && why != "" {
					return nil, nil, fmt.Errorf("not watching %s: %v", subject, why)
				}
			}
		}
		polls = append(polls, polled{r, obs})
		if observe {
			d.observer(r)(obs)
		}
		if fetch == nil && len(obs) > 0 {
			fetch = obs[0].Fetch
		}
	}
	if !matched {
		return nil, nil, fmt.Errorf("no source watches subjects like %s", subject)
	}
	return fetch, polls, nil
}

func (d *daemon) workspace(id string) (host.Workspace, error) {
	if ws, ok := d.binder.Workspace(id); ok {
		return ws, nil
	}
	d.refreshWorkspaces() // a pane opened since the last host poll
	if ws, ok := d.binder.Workspace(id); ok {
		return ws, nil
	}
	// A pane the host does not list, like an Orca terminal without a tab, in
	// a worktree it does: the watch belongs to the worktree then.
	if to, ok := d.fallback(id); ok {
		if ws, ok := d.binder.Workspace(to); ok {
			slog.Info("workspace not listed; using its fallback", "workspace", id, "fallback", to)
			return ws, nil
		}
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
		if _, _, err := d.pollSubject(ctx, r.Subject, false, false); err != nil {
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
		fetch, _, err := d.pollSubject(ctx, r.Subject, true, false)
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
// Under a minute it says so rather than printing nothing.
func shortDuration(d time.Duration) string {
	if d < 30*time.Second {
		return "under a minute"
	}
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
	id := req.Workspace
	if _, ok := d.binder.Binding(subject, id); !ok {
		// A pane that is not watching may sit in a worktree that is: a watch
		// made before panes were workspaces, or one that fell back.
		from, _ := d.fallback(id)
		if _, ok := d.binder.Binding(subject, from); !ok || from == "" {
			return fmt.Errorf("pane %s is not watching %s", req.Workspace, subject)
		}
		id = from
	}
	d.binder.Unbind(subject, id)
	d.refreshBadges()
	slog.Info("unwatch", "subject", subject, "workspace", id)
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
	d.fallBack(hostName, list)
	d.binder.SetWorkspaces(hostName, list)
	d.sched.Prune(d.alive(hostName))
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
	// every token, so report them all again. Forgetting what was shown
	// would skip a badge that turned empty meanwhile (a merged PR, a fired
	// reminder), and leave it up for good: Orca keeps comments, and resyncs
	// on every poll. unknownBadge differs from every text, the empty one
	// included.
	d.badgeMu.Lock()
	for id, text := range d.shown {
		switch {
		case host.HostOf(id) != hostName:
		case text == "":
			delete(d.shown, id)
		default:
			d.shown[id] = unknownBadge
		}
	}
	d.badgeMu.Unlock()
	d.refreshBadges()
}

// fallBack moves the watches and reminders of the workspaces of hostName that
// list no longer has to their fallback, when list has that, before
// SetWorkspaces ends them. It goes by what is bound, not by what the host
// listed last: a pane can be gone since before the daemon started.
func (d *daemon) fallBack(hostName string, list []host.Workspace) {
	listed := map[string]host.Workspace{}
	for _, ws := range list {
		listed[ws.ID] = ws
	}
	gone := map[string]bool{}
	note := func(id string) {
		if _, ok := listed[id]; !ok && host.HostOf(id) == hostName {
			gone[id] = true
		}
	}
	for _, bd := range d.binder.AllBindings() {
		note(bd.Workspace)
	}
	for _, r := range d.sched.List("") {
		note(r.Workspace)
	}
	for _, id := range slices.Sorted(maps.Keys(gone)) {
		to, ok := d.fallback(id)
		if !ok {
			continue
		}
		ws, ok := listed[to]
		if !ok {
			continue
		}
		d.dropCovered(id, to, listed)
		d.binder.Move(id, ws)
		d.sched.Move(id, to)
		slog.Info("watch falls back", "from", id, "to", to)
	}
}

// dropCovered ends the bindings of gone workspace id whose subject a listed
// sibling with the same fallback already watches: moving them to the
// fallback would deliver every update twice.
func (d *daemon) dropCovered(id, to string, listed map[string]host.Workspace) {
	for _, bd := range d.binder.AllBindings() {
		if bd.Workspace != id {
			continue
		}
		for _, other := range d.binder.Bindings(bd.Subject) {
			if _, ok := listed[other.Workspace]; !ok || other.Workspace == id {
				continue
			}
			if f, _ := d.fallback(other.Workspace); f == to {
				d.binder.Unbind(bd.Subject, id)
				slog.Info("watch ends with its pane: a sibling watches it", "subject", bd.Subject, "from", id, "sibling", other.Workspace)
				break
			}
		}
	}
}

// fallback is the workspace that takes over from workspace id once it is
// gone, if its host has one.
func (d *daemon) fallback(id string) (string, bool) {
	f, ok := d.host(host.HostOf(id)).(host.Fallback)
	if !ok {
		return "", false
	}
	return f.Fallback(id)
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

func (d *daemon) Remove(hostName, id string) {
	d.binder.Remove(id)
	d.sched.Prune(d.alive(hostName))
	slog.Info("pane closed", "id", id)
}

// alive judges only the workspaces of hostName, the host that just
// reported its list: another host's may be unknown because it is down.
func (d *daemon) alive(hostName string) func(workspace string) bool {
	return func(workspace string) bool {
		if host.HostOf(workspace) != hostName {
			return true
		}
		_, ok := d.binder.Workspace(workspace)
		return ok
	}
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
		slices.Sort(parts[ws.ID])
		b := host.Badge{Subjects: parts[ws.ID], Reminders: reminders[ws.ID]}
		text := b.String()
		if old, ok := d.shown[ws.ID]; ok && old == text || !ok && text == "" {
			continue
		}
		h := d.host(ws.Host)
		if h == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
		err := h.Badge(ctx, ws, b)
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

const unknownBadge = "\x00"

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
		if err := h.Badge(ctx, ws, host.Badge{}); err != nil {
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

// me resolves "@me" in ignore_actors with each provider's me. On failure
// kelpie still runs; it just forwards that provider's own changes too.
func me(ctx context.Context, provided []config.Described) []string {
	var names []string
	for _, p := range provided {
		if len(p.Me) == 0 {
			continue
		}
		who, err := provider.Who(ctx, p.Command, p.Me, "")
		if err != nil {
			slog.Warn("could not resolve @me; your own activity is not ignored", "provider", p.Name, "err", err)
			continue
		}
		slog.Info("@me resolved", "provider", p.Name, "me", who)
		names = append(names, who)
	}
	return names
}

// printHost delivers to stdout, for --dry-run against real workspaces.
type printHost struct{ host.Host }

func (p printHost) Badge(_ context.Context, ws host.Workspace, b host.Badge) error {
	fmt.Printf("--- badge on %s %s: %q\n", ws.Host, ws.ID, b)
	return nil
}

func (p printHost) Notify(_ context.Context, n host.Notification) error {
	fmt.Printf("--- notification: %s: %s\n", n.Title, n.Body)
	return nil
}

func (p printHost) Deliver(_ context.Context, ws host.Workspace, text string) error {
	fmt.Printf("--- to %s %s (%s)\n%s\n---\n", ws.Host, ws.ID, ws.Path, text)
	return nil
}

func (p printHost) Fallback(id string) (string, bool) {
	f, ok := p.Host.(host.Fallback)
	if !ok {
		return "", false
	}
	return f.Fallback(id)
}
