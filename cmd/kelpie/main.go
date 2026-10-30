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
	"github.com/metalmatze/kelpie/internal/state"
)

type Globals struct {
	Config string `help:"Config file; the built-in default is used when it does not exist." type:"path" default:"${config}"`
}

type CLI struct {
	Globals

	Watch     WatchCmd     `cmd:"" help:"Watch a pull request and prompt this pane's agent about its updates."`
	Unwatch   UnwatchCmd   `cmd:"" help:"Stop watching a pull request."`
	Remind    RemindCmd    `cmd:"" help:"Prompt this pane's agent with a text later, once or on a cron schedule."`
	Reminders RemindersCmd `cmd:"" help:"List this pane's reminders."`
	Unremind  UnremindCmd  `cmd:"" help:"Cancel a reminder."`
	Status    StatusCmd    `cmd:"" help:"List watched pull requests, reminders and queued updates."`
	Daemon    DaemonCmd    `cmd:"" help:"Watch sources and deliver updates to agents."`
	Init      InitCmd      `cmd:"" help:"Write the default config, unless one exists."`
	Source    struct {
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

type InitCmd struct{}

func (InitCmd) Run(g *Globals) error {
	if _, err := os.Stat(g.Config); err == nil {
		return fmt.Errorf("%s already exists", g.Config)
	}
	if err := os.MkdirAll(filepath.Dir(g.Config), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(g.Config, config.Default, 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", g.Config)
	return nil
}

type SourceTestCmd struct {
	Name    string `arg:"" help:"Source name."`
	Raw     bool   `help:"Print the command's raw output instead of observations (for fixtures)."`
	Subject string `help:"For a for_each source: the subject (PR URL or owner/repo#number) to run for. Default for for_each.source: the first parent subject matching while."`
}

func (c *SourceTestCmd) Run(ctx context.Context, g *Globals) error {
	cfg, err := config.Load(g.Config)
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
		subject, err := core.ParseSubject(c.Subject)
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
		if subject, err = core.ParseSubject(c.Subject); err != nil {
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
	PR        string `arg:"" help:"Pull request URL or owner/repo#number."`
	Workspace string `help:"Pane to prompt. Default: this pane ($HERDR_PANE_ID)." env:"HERDR_PANE_ID"`
}

func (c *WatchCmd) Run(ctx context.Context) error {
	if c.Workspace == "" {
		return errors.New("not inside a herdr pane; pass --workspace <pane id>")
	}
	subject, err := core.ParseSubject(c.PR)
	if err != nil {
		return err
	}
	resp, err := control.NewClient(control.SocketPath()).Watch(ctx, control.WatchRequest{Subject: subject, Host: "herdr", Workspace: c.Workspace})
	if err != nil {
		return err
	}
	fmt.Printf("watching %s; updates go to the agent in pane %s\n", resp.Subject, resp.Workspace)
	if resp.Moved != "" {
		fmt.Printf("(moved from pane %s)\n", resp.Moved)
	}
	return nil
}

type UnwatchCmd struct {
	PR string `arg:"" help:"Pull request URL or owner/repo#number."`
}

func (c *UnwatchCmd) Run(ctx context.Context) error {
	subject, err := core.ParseSubject(c.PR)
	if err != nil {
		return err
	}
	if err := control.NewClient(control.SocketPath()).Unwatch(ctx, subject); err != nil {
		return err
	}
	fmt.Println("stopped watching", subject)
	return nil
}

type StatusCmd struct{}

func (StatusCmd) Run(ctx context.Context) error {
	st, err := control.NewClient(control.SocketPath()).Status(ctx)
	if err != nil {
		return err
	}
	if len(st.Watches) == 0 {
		fmt.Println("watching nothing; run kelpie watch <pr-url> inside an agent's pane")
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
	Subject   string        `help:"Pull request the reminder is about. Its pending updates and fetch command come with the reminder."`
	Workspace string        `help:"Pane to prompt. Default: this pane." env:"HERDR_PANE_ID"`
}

func (c *RemindCmd) Run(ctx context.Context) error {
	if c.Workspace == "" {
		return errors.New("not inside a herdr pane; pass --workspace <pane id>")
	}
	req := control.RemindRequest{Workspace: c.Workspace, Text: strings.Join(c.Text, " "), Subject: core.Subject(c.Subject), Cron: c.Cron}
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
	Workspace string `help:"Pane whose reminders to list. Default: this pane." env:"HERDR_PANE_ID"`
}

func (c *RemindersCmd) Run(ctx context.Context) error {
	st, err := control.NewClient(control.SocketPath()).Status(ctx)
	if err != nil {
		return err
	}
	n := 0
	for _, r := range st.Reminders {
		if c.All || c.Workspace == "" || r.Workspace == c.Workspace {
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
