// Package service installs the kelpie daemon as a launchd LaunchAgent (macOS)
// or a systemd user unit (Linux), so it runs without herdr's plugin starting
// it. Both coexist with the plugin: the daemon's control socket is the lock,
// and whoever starts second exits 0.
package service

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"
)

//go:embed launchd.plist.tmpl
var launchdTmpl string

//go:embed systemd.service.tmpl
var systemdTmpl string

type EnvVar struct{ Name, Value string }

// Spec is everything baked into the unit. launchd and systemd start jobs with
// a bare environment and do not expand ~ or $HOME, so every path is absolute
// and PATH is the installer's: gh, jq, herdr and orca must resolve as they do
// in the person's terminal.
type Spec struct {
	Label  string
	Binary string
	Config string
	Log    string // launchd only; systemd logs to the journal.
	Env    []EnvVar
}

// Service is one target OS's way of running a unit.
type Service struct {
	GOOS string
	Out  io.Writer
	Spec Spec
	// Unit is the plist (darwin) or unit file (linux).
	Unit string
}

var labelRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// New resolves defaults for goos. label may be empty.
func New(goos, label, config string, out io.Writer) (*Service, error) {
	// The label becomes a file name and a launchctl/systemctl argument.
	if label != "" && !labelRE.MatchString(label) {
		return nil, fmt.Errorf("invalid --label %q: use only letters, digits, '.', '_' and '-'", label)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	s := &Service{GOOS: goos, Out: out}
	switch goos {
	case "darwin":
		if label == "" {
			label = "dev.kelpie.daemon"
		}
		s.Unit = filepath.Join(home, "Library", "LaunchAgents", label+".plist")
		s.Spec.Log = filepath.Join(home, "Library", "Logs", label+".log")
	case "linux":
		if label == "" {
			label = "kelpie"
		}
		label = strings.TrimSuffix(label, ".service")
		dir := os.Getenv("XDG_CONFIG_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".config")
		}
		s.Unit = filepath.Join(dir, "systemd", "user", label+".service")
	default:
		return nil, fmt.Errorf("kelpie service supports macOS (launchd) and Linux (systemd), not %s; run kelpie daemon yourself", goos)
	}
	bin, err := binary()
	if err != nil {
		return nil, err
	}
	s.Spec.Label, s.Spec.Binary, s.Spec.Config = label, bin, config
	s.Spec.Env = environment()
	return s, nil
}

// binary is the path kelpie was invoked as, made absolute but not
// symlink-resolved, so a rebuild or upgrade that replaces ~/bin/kelpie (or
// repoints a Nix profile link) is picked up on the next restart. A resolved
// path would pin the unit to one immutable store path or build.
func binary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return "", err
	}
	if strings.Contains(exe, "go-build") {
		return "", fmt.Errorf("%s is a go run temp binary that disappears; go build it and run that", exe)
	}
	return exe, nil
}

// environment forwards what decides where the daemon's socket and config live,
// so kelpie status in the person's shell finds the service's daemon.
func environment() []EnvVar {
	var env []EnvVar
	for _, name := range []string{"PATH", "XDG_STATE_HOME", "XDG_CONFIG_HOME"} {
		if v := os.Getenv(name); v != "" {
			env = append(env, EnvVar{name, v})
		}
	}
	return env
}

func (s *Service) Render() ([]byte, error) {
	src := systemdTmpl
	if s.GOOS == "darwin" {
		src = launchdTmpl
	}
	t, err := template.New("unit").Funcs(template.FuncMap{"xml": xmlEscape, "unit": systemdQuote, "env": systemdEnvQuote}).Parse(src)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, s.Spec); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// systemdQuote makes s one word of an ExecStart= line. Always quoted: a path
// with a space would otherwise split into several arguments. systemd expands
// % specifiers and $VARS even inside quotes, so those are escaped.
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s) + `"`
}

// systemdEnvQuote quotes a whole NAME=value assignment: Environment= splits
// unquoted values on whitespace. Unlike ExecStart=, it does not expand $.
func systemdEnvQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(s) + `"`
}

func (s *Service) Install(ctx context.Context) error {
	unit, err := s.Render()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Unit), 0o755); err != nil {
		return err
	}
	if s.GOOS == "darwin" {
		// launchd opens the log file but not its directory.
		if err := os.MkdirAll(filepath.Dir(s.Spec.Log), 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(s.Unit, unit, 0o644); err != nil {
		return err
	}
	if s.GOOS == "darwin" {
		err = s.bootstrap(ctx)
	} else {
		err = s.enable(ctx)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "installed %s and started it\n", s.Unit)
	return nil
}

func (s *Service) domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func (s *Service) target() string { return s.domain() + "/" + s.Spec.Label }

// bootstrap loads the plist, replacing a loaded one so reinstall picks up a
// changed binary path or PATH.
func (s *Service) bootstrap(ctx context.Context) error {
	if s.loaded(ctx) {
		if err := run(ctx, "launchctl", "bootout", s.target()); err != nil {
			return err
		}
	}
	// A label disabled earlier (launchctl disable, or a login-item toggle)
	// makes bootstrap fail with "Input/output error", which the retry below
	// would only repeat.
	if err := run(ctx, "launchctl", "enable", s.target()); err != nil {
		return err
	}
	// bootout returns before the job is fully gone; an immediate bootstrap
	// can fail with "Input/output error" (5).
	var err error
	for range 10 {
		if err = run(ctx, "launchctl", "bootstrap", s.domain(), s.Unit); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return err
}

func (s *Service) loaded(ctx context.Context) bool {
	return exec.CommandContext(ctx, "launchctl", "print", s.target()).Run() == nil
}

func (s *Service) enable(ctx context.Context) error {
	name := s.Spec.Label + ".service"
	if err := run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := run(ctx, "systemctl", "--user", "enable", name); err != nil {
		return err
	}
	// restart, not enable --now: that leaves a running daemon on the old unit.
	return run(ctx, "systemctl", "--user", "restart", name)
}

func (s *Service) Uninstall(ctx context.Context) error {
	removed := false
	// Checked apart from the file: a job can stay loaded after its plist or
	// unit was deleted by hand, and would keep running unseen.
	if s.GOOS == "darwin" {
		if s.loaded(ctx) {
			if err := run(ctx, "launchctl", "bootout", s.target()); err != nil {
				return err
			}
			removed = true
		}
	} else {
		name := s.Spec.Label + ".service"
		if s.active(ctx) {
			if err := run(ctx, "systemctl", "--user", "stop", name); err != nil {
				return err
			}
			removed = true
		}
		if _, err := os.Stat(s.Unit); err == nil {
			if err := run(ctx, "systemctl", "--user", "disable", name); err != nil {
				return err
			}
		}
	}
	switch err := os.Remove(s.Unit); {
	case err == nil:
		removed = true
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if s.GOOS == "linux" {
		if err := run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
	}
	if !removed {
		fmt.Fprintf(s.Out, "nothing to remove: %s does not exist and the service is not loaded\n", s.Unit)
		return nil
	}
	fmt.Fprintf(s.Out, "removed %s\n", s.Unit)
	return nil
}

func (s *Service) active(ctx context.Context) bool {
	return exec.CommandContext(ctx, "systemctl", "--user", "is-active", "--quiet", s.Spec.Label+".service").Run() == nil
}

func (s *Service) Status(ctx context.Context) error {
	_, err := os.Stat(s.Unit)
	exists := err == nil
	if s.GOOS == "linux" {
		return s.statusLinux(ctx, exists)
	}
	loaded := s.loaded(ctx)
	switch {
	case !exists && !loaded:
		fmt.Fprintf(s.Out, "not installed (no %s); run kelpie service install\n", s.Unit)
		return nil
	case !exists:
		fmt.Fprintf(s.Out, "unit:   missing (%s), but the job is still loaded; kelpie service uninstall unloads it\n", s.Unit)
	default:
		fmt.Fprintf(s.Out, "unit:   %s\n", s.Unit)
	}
	fmt.Fprintf(s.Out, "logs:   %s\n", s.Spec.Log)
	if !loaded {
		fmt.Fprintln(s.Out, "state:  not loaded; run kelpie service install")
		return nil
	}
	out, err := exec.CommandContext(ctx, "launchctl", "print", s.target()).Output()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(out), "\n") {
		// One tab of indent is the job's own fields; deeper lines belong to
		// nested blocks (endpoints, event triggers) that also have a state.
		line, ok := strings.CutPrefix(line, "\t")
		if !ok || strings.HasPrefix(line, "\t") {
			continue
		}
		for _, key := range []string{"state = ", "pid = ", "runs = ", "last exit code = "} {
			if strings.HasPrefix(line, key) {
				fmt.Fprintf(s.Out, "%-7s %s\n", strings.TrimSuffix(key, " = ")+":", strings.TrimPrefix(line, key))
			}
		}
	}
	return nil
}

func (s *Service) statusLinux(ctx context.Context, exists bool) error {
	name := s.Spec.Label + ".service"
	switch {
	case !exists && !s.active(ctx):
		fmt.Fprintf(s.Out, "not installed (no %s); run kelpie service install\n", s.Unit)
		return nil
	case !exists:
		fmt.Fprintf(s.Out, "unit:   missing (%s), but %s is still running; kelpie service uninstall stops it\n", s.Unit, name)
	default:
		fmt.Fprintf(s.Out, "unit:   %s\n", s.Unit)
	}
	fmt.Fprintln(s.Out, "logs:   journalctl --user -u "+name)
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "status", "--no-pager", name)
	cmd.Stdout = s.Out
	// A stopped unit exits non-zero; its output is the answer.
	_ = cmd.Run()
	return nil
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Current is runtime.GOOS, a variable so --os can render the other platform's
// unit for review.
var Current = runtime.GOOS
