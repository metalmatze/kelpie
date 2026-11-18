package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"

	"github.com/metalmatze/kelpie/internal/adapter"
	"github.com/metalmatze/kelpie/internal/config"
	"github.com/metalmatze/kelpie/internal/control"
	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/diff"
	"github.com/metalmatze/kelpie/internal/host"
	"github.com/metalmatze/kelpie/internal/host/hc"
	"github.com/metalmatze/kelpie/internal/host/herdr"
	"github.com/metalmatze/kelpie/internal/host/orca"
	"github.com/metalmatze/kelpie/internal/state"
)

type Globals struct {
	Config string `help:"Config file, layered over the built-in default; it need not exist." type:"path" default:"${config}"`
}

type CLI struct {
	Globals

	Watch     WatchCmd     `cmd:"" help:"Watch a subject and prompt this pane's agent about its updates."`
	Unwatch   UnwatchCmd   `cmd:"" help:"Stop watching a subject."`
	Remind    RemindCmd    `cmd:"" help:"Prompt this pane's agent with a text later, once or on a cron schedule."`
	Reminders RemindersCmd `cmd:"" help:"List this pane's reminders."`
	Unremind  UnremindCmd  `cmd:"" help:"Cancel a reminder."`
	Status    StatusCmd    `cmd:"" help:"List watches, reminders and queued updates."`
	Daemon    DaemonCmd    `cmd:"" help:"Watch sources and deliver updates to agents."`
	Init      InitCmd      `cmd:"" help:"Write a commented starter config, unless one exists."`
	ConfigCmd struct {
		Show ConfigShowCmd `cmd:"" help:"Print the config in effect: the built-in default with your file layered over it."`
	} `cmd:"" name:"config" help:"Inspect the config."`
	Service ServiceCmd `cmd:"" help:"Run the daemon at login as a launchd or systemd service."`
	Auth    AuthCmd    `cmd:"" help:"Store, check or remove kelpie's own credential for a vendor without a CLI, in the system keychain."`
	Source  struct {
		Test SourceTestCmd `cmd:"" help:"Run one source once and print its observations."`
	} `cmd:"" help:"Inspect configured sources."`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cli CLI
	kctx := kong.Parse(&cli,
		kong.Name("kelpie"),
		kong.Description("Route SDLC events to the coding agent that owns the work."),
		kong.Vars{"config": config.DefaultPath()},
		kong.BindTo(ctx, (*context.Context)(nil)),
		kong.Bind(&cli.Globals),
	)
	if err := kctx.Run(); err != nil && !errors.Is(err, context.Canceled) {
		kctx.FatalIfErrorf(err)
	}
}

// callers detect the workspace a command runs in, one per host, keyed by
// the TERM_PROGRAM the host sets. hc sets none of its own (it passes on the
// terminal it impersonates, like ghostty) and comes first: when herdr or Orca
// runs inside an hc terminal, their TERM_PROGRAM picks them over it.
var callers = []struct {
	term   string
	detect func() (string, bool)
}{
	{"", hc.Caller},
	{"orca", orca.Caller},
	{"herdr", herdr.Caller},
}

// callerWorkspace is the workspace given with --workspace, else the pane or
// terminal the command runs in. Hosts pass their variables on to whatever
// they start: the Orca app launched from a herdr pane gives every Orca
// terminal that pane's HERDR_PANE_ID. When several hosts' variables are
// set, TERM_PROGRAM names the innermost terminal.
func callerWorkspace(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	var found []string
	for _, c := range callers {
		if id, ok := c.detect(); ok {
			if c.term != "" && strings.EqualFold(os.Getenv("TERM_PROGRAM"), c.term) {
				return id, nil
			}
			found = append(found, id)
		}
	}
	if len(found) > 0 {
		return found[0], nil
	}
	return "", errors.New("not inside a herdr pane, an Orca terminal or chat, or an hc terminal; pass --workspace <id as kelpie status lists it>")
}

type InitCmd struct{}

func (InitCmd) Run(g *Globals) error {
	if _, err := os.Stat(g.Config); err == nil {
		return fmt.Errorf("%s already exists", g.Config)
	}
	if err := os.MkdirAll(filepath.Dir(g.Config), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(g.Config, config.Starter, 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", g.Config)
	return nil
}

type ConfigShowCmd struct{}

func (ConfigShowCmd) Run(ctx context.Context, g *Globals) error {
	out, warnings, err := config.Effective(ctx, g.Config)
	if err != nil {
		return err
	}
	os.Stdout.Write(out)
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %s: %s\n", g.Config, w)
	}
	return nil
}

type SourceTestCmd struct {
	Name    string `arg:"" help:"Source name."`
	Raw     bool   `help:"Print the command's raw output instead of observations (for fixtures)."`
	Subject string `help:"For a for_each source: the subject (as kelpie status lists it, a URL or ID a provider claims, or a PR or issue URL, owner/repo or org) to run for. Default for for_each.source: the first parent subject matching while."`
}

func (c *SourceTestCmd) Run(ctx context.Context, g *Globals) error {
	cfg, err := loadProviders(ctx, g)
	if err != nil {
		return err
	}
	s, ok := cfg.Source(c.Name)
	if !ok {
		return fmt.Errorf("no source named %q", c.Name)
	}
	a, err := adapter.New(s)
	if err != nil {
		return err
	}
	// Built before running so a broken transition fails here, not in the daemon.
	if _, err := diff.New(s.Name, s.Transitions, state.NewMemory()); err != nil {
		return err
	}
	if _, err := diff.NewExpr(s.Badge); err != nil {
		return fmt.Errorf("badge: %w", err)
	}
	refuse, err := diff.NewExpr(s.Refuse)
	if err != nil {
		return fmt.Errorf("refuse: %w", err)
	}
	var parent *core.Observation
	if s.ForEach != nil {
		if parent, err = c.parent(ctx, cfg, s); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "running for %s\n", parent.Subject)
	}
	out, obs, err := a.Once(ctx, parent)
	if c.Raw {
		os.Stdout.Write(out)
	}
	if err != nil {
		return err
	}
	if !c.Raw {
		enc := json.NewEncoder(os.Stdout)
		for _, o := range obs {
			if err := enc.Encode(o); err != nil {
				return err
			}
			if refuse == nil {
				continue
			}
			if why, err := refuse.Eval(o.Fields, nil); err != nil {
				fmt.Fprintf(os.Stderr, "refuse: %v\n", err)
			} else if why != nil && why != "" {
				fmt.Fprintf(os.Stderr, "kelpie watch would refuse %s: %v\n", o.Subject, why)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "%d observations\n", len(obs))
	return nil
}

func (c *SourceTestCmd) parent(ctx context.Context, cfg *config.Config, s config.Source) (*core.Observation, error) {
	if s.ForEach.Subjects != "" {
		if c.Subject == "" {
			return nil, fmt.Errorf("source %s runs per watched subject; pass --subject", s.Name)
		}
		subject, err := subjectOf(ctx, cfg, c.Subject)
		if err != nil {
			return nil, err
		}
		p, err := core.NewPattern(s.ForEach.Subjects)
		if err != nil {
			return nil, err
		}
		fields, ok := p.Match(subject)
		if !ok {
			return nil, fmt.Errorf("%s does not match %s", subject, s.ForEach.Subjects)
		}
		return &core.Observation{Subject: subject, Fields: fields}, nil
	}
	ps, ok := cfg.Source(s.ForEach.Source)
	if !ok {
		return nil, fmt.Errorf("for_each source %q does not exist", s.ForEach.Source)
	}
	pa, err := adapter.New(ps)
	if err != nil {
		return nil, err
	}
	// The parent may itself run per subject (github-checks under github-pr).
	var pp *core.Observation
	if ps.ForEach != nil {
		if pp, err = c.parent(ctx, cfg, ps); err != nil {
			return nil, err
		}
	}
	_, obs, err := pa.Once(ctx, pp)
	if err != nil {
		return nil, fmt.Errorf("parent %s: %w", ps.Name, err)
	}
	var subject core.Subject
	if c.Subject != "" {
		if subject, err = subjectOf(ctx, cfg, c.Subject); err != nil {
			return nil, err
		}
	}
	for _, o := range obs {
		if subject != "" && o.Subject == subject || subject == "" && s.ForEach.While.Matches(o.Fields) {
			return &o, nil
		}
	}
	if c.Subject != "" {
		return nil, fmt.Errorf("parent %s has no subject %s", ps.Name, c.Subject)
	}
	return nil, fmt.Errorf("no subject of %s has %s = %v right now; pass --subject", ps.Name, s.ForEach.While.Field, s.ForEach.While.Equals)
}

type WatchCmd struct {
	Target    []string `arg:"" name:"provider target" help:"A provider and its target (github acme/api#7, linear ENG-123, github alone for the current directory's repo), a target alone that one provider's patterns claim (a pull request or Linear issue URL), or a subject as kelpie status lists it."`
	Workspace string   `help:"Pane or terminal to prompt. Default: the one this runs in."`
}

func (c *WatchCmd) Run(ctx context.Context, g *Globals) error {
	workspace, err := callerWorkspace(c.Workspace)
	if err != nil {
		return err
	}
	cfg, err := loadProviders(ctx, g)
	if err != nil {
		return err
	}
	subjects, err := resolve(ctx, cfg, c.Target)
	if err != nil {
		return err
	}
	for _, s := range subjects {
		if err := watch(ctx, s, workspace); err != nil {
			return err
		}
	}
	return nil
}

func watch(ctx context.Context, subject core.Subject, workspace string) error {
	resp, err := control.NewClient(control.SocketPath()).Watch(ctx, control.WatchRequest{Subject: subject, Host: host.HostOf(workspace), Workspace: workspace})
	if err != nil {
		return err
	}
	fmt.Printf("watching %s; updates go to the agent in pane %s\n", resp.Subject, resp.Workspace)
	if len(resp.Others) > 0 {
		fmt.Printf("(pane %s watches it too and keeps its own updates)\n", strings.Join(resp.Others, ", "))
	}
	return nil
}

type UnwatchCmd struct {
	Subject UnwatchSubjectCmd `cmd:"" name:"subject" default:"withargs" help:"Stop watching a subject as kelpie status lists it, or a provider's target, like github acme/api#7 or linear ENG-123."`
}

type UnwatchSubjectCmd struct {
	Target    []string `arg:"" name:"subject or provider target" help:"Subject as kelpie status lists it (github-inbox:prometheus), a provider and its target (github prometheus, linear ENG-123), or a target alone that one provider claims."`
	Workspace string   `help:"Pane or terminal whose watch to end; others keep theirs. Default: the one this runs in."`
}

func (c *UnwatchSubjectCmd) Run(ctx context.Context, g *Globals) error {
	var subjects []core.Subject
	if len(c.Target) == 1 && core.IsSubject(c.Target[0]) {
		// As kelpie status lists it: no provider needs to run, so this works
		// even when the provider that made it is gone.
		subjects = []core.Subject{core.Subject(c.Target[0])}
	} else {
		cfg, err := loadProviders(ctx, g)
		if err != nil {
			return err
		}
		if subjects, err = resolve(ctx, cfg, c.Target); err != nil {
			return err
		}
	}
	for _, s := range subjects {
		if err := unwatch(ctx, s, c.Workspace); err != nil {
			return err
		}
	}
	return nil
}

func unwatch(ctx context.Context, subject core.Subject, workspace string) error {
	workspace, err := callerWorkspace(workspace)
	if err != nil {
		return err
	}
	if err := control.NewClient(control.SocketPath()).Unwatch(ctx, subject, workspace); err != nil {
		return err
	}
	fmt.Printf("pane %s stopped watching %s\n", workspace, subject)
	return nil
}

type StatusCmd struct{}

func (StatusCmd) Run(ctx context.Context) error {
	st, err := control.NewClient(control.SocketPath()).Status(ctx)
	if err != nil {
		return err
	}
	if len(st.Watches) == 0 {
		fmt.Println("watching nothing; run kelpie watch <provider> <target>, like kelpie watch github <pr or issue url, repo or org>, inside an agent's pane")
	}
	for _, w := range st.Watches {
		fmt.Printf("%-50s pane %-8s since %s\n", w.Subject, w.Workspace, w.Since.Format("Jan 2 15:04"))
	}
	for _, r := range st.Reminders {
		printReminder(r, true)
	}
	for ws, n := range st.Queued {
		fmt.Printf("%d updates queued for pane %s\n", n, ws)
	}
	return nil
}

type RemindCmd struct {
	Text      []string      `arg:"" help:"What to tell the agent when it is due."`
	In        time.Duration `xor:"when" help:"Due after this long, like 1h or 90m."`
	At        string        `xor:"when" help:"Due at this local time: 10:00 (the next one), 2026-10-31T10:00, or RFC 3339."`
	Cron      string        `xor:"when" help:"Due on this cron schedule, like '0 9 * * 1-5' or @daily; local time unless prefixed with CRON_TZ=."`
	Subject   string        `help:"What the reminder is about: a PR or issue URL, a URL or ID a provider claims (a Linear issue), or the subject as kelpie status lists it. Its pending updates and fetch command come with the reminder."`
	Workspace string        `help:"Pane or terminal to prompt. Default: the one this runs in."`
}

func (c *RemindCmd) Run(ctx context.Context, g *Globals) error {
	workspace, err := callerWorkspace(c.Workspace)
	if err != nil {
		return err
	}
	req := control.RemindRequest{Workspace: workspace, Text: strings.Join(c.Text, " "), Cron: c.Cron}
	if c.Subject != "" {
		cfg, err := loadProviders(ctx, g)
		if err != nil {
			return err
		}
		if req.Subject, err = subjectOf(ctx, cfg, c.Subject); err != nil {
			return err
		}
	}
	switch {
	case c.In > 0:
		req.Due = time.Now().Add(c.In)
	case c.In < 0:
		return errors.New("--in must be positive")
	case c.At != "":
		due, err := parseAt(c.At, time.Now())
		if err != nil {
			return err
		}
		req.Due = due
	case c.Cron == "":
		return errors.New("one of --in, --at or --cron is required")
	}
	r, err := control.NewClient(control.SocketPath()).Remind(ctx, req)
	if err != nil {
		return err
	}
	printReminder(r, false)
	return nil
}

// parseAt reads a local time. A bare clock time means its next occurrence.
func parseAt(s string, now time.Time) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			if !t.After(now) {
				return time.Time{}, fmt.Errorf("%s is in the past", s)
			}
			return t, nil
		}
	}
	clock, err := time.Parse("15:04", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot parse --at %q: want 10:00, 2026-10-31T10:00 or RFC 3339", s)
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, time.Local)
	if !t.After(now) {
		t = time.Date(now.Year(), now.Month(), now.Day()+1, clock.Hour(), clock.Minute(), 0, 0, time.Local)
	}
	return t, nil
}

type RemindersCmd struct {
	All       bool   `help:"List the reminders of every pane."`
	Workspace string `help:"Pane or terminal whose reminders to list. Default: the one this runs in."`
}

func (c *RemindersCmd) Run(ctx context.Context) error {
	st, err := control.NewClient(control.SocketPath()).Status(ctx)
	if err != nil {
		return err
	}
	// Outside any pane or terminal, list every reminder.
	workspace, _ := callerWorkspace(c.Workspace)
	n := 0
	for _, r := range st.Reminders {
		if c.All || workspace == "" || r.Workspace == workspace {
			printReminder(r, true)
			n++
		}
	}
	if n == 0 {
		fmt.Println("no reminders")
	}
	return nil
}

type UnremindCmd struct {
	ID string `arg:"" help:"Reminder ID, as kelpie reminders lists it."`
}

func (c *UnremindCmd) Run(ctx context.Context) error {
	if err := control.NewClient(control.SocketPath()).Unremind(ctx, c.ID); err != nil {
		return err
	}
	fmt.Println("cancelled reminder", c.ID)
	return nil
}

func printReminder(r core.Reminder, list bool) {
	when := r.Due.Local().Format("Mon Jan 2 15:04")
	if r.Cron != "" {
		when = fmt.Sprintf("%s, next %s", r.Cron, when)
	}
	if !list {
		fmt.Printf("reminder %s for pane %s due %s (in %s)\n", r.ID, r.Workspace, when, shortDuration(time.Until(r.Due)))
		return
	}
	about := ""
	if r.Subject != "" {
		about = " [" + string(r.Subject) + "]"
	}
	fmt.Printf("%-4s pane %-8s %s: %s%s\n", r.ID, r.Workspace, when, r.Text, about)
}
