package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

//go:embed default.yaml
var Default []byte

// Starter is what kelpie init writes: a file that states nothing yet, so all
// of Default applies until the person uncomments or adds something.
//
//go:embed starter.yaml
var Starter []byte

// Config is Default with the person's file layered over it, so a new or
// fixed default source reaches everyone who has a file, without copying.
// The file wins key by key in mappings; a list the file sets replaces the
// default's, the way people expect `ignore_actors: [...]` to read. Sources
// and defs merge by their own rules, see layer.
type Config struct {
	Hosts     Hosts     `yaml:"hosts"`
	Delivery  Delivery  `yaml:"delivery"`
	Reminders Reminders `yaml:"reminders"`
	Rules     Rules     `yaml:"rules"`
	Binders   Binders   `yaml:"binders"`
	// Defs are jq function definitions shared by every map, transition
	// expression and badge, so two sources reading the same GraphQL shape
	// (github-pr and github-checks) judge it the same way.
	Defs    string   `yaml:"defs"`
	Sources []Source `yaml:"sources"`

	// Warnings are parts of the file that shadow the default, which a full
	// copy of an older default does without the person meaning to.
	Warnings []string `yaml:"-"`
}

// Hosts lists the agent hosts kelpie delivers to. Enable several to reach
// agents in all of them from one daemon; each workspace belongs to one host.
type Hosts struct {
	Herdr Herdr `yaml:"herdr"`
	Orca  Orca  `yaml:"orca"`
	HC    HC    `yaml:"hc"`
}

// HC is herdr-core, the terminal runtime behind the Herdr desktop app. The
// herdr host above is the herdr TUI, a separate program.
type HC struct {
	Enabled bool     `yaml:"enabled"`
	Socket  string   `yaml:"socket"`
	Poll    Duration `yaml:"poll"`
}

type Orca struct {
	Enabled bool `yaml:"enabled"`
	// Bin defaults to $ORCA_CLI_COMMAND, else orca, or orca-ide on Linux
	// where bare orca is the GNOME screen reader.
	Bin  string   `yaml:"bin"`
	Poll Duration `yaml:"poll"`
	// Comment shows what a worktree watches as its Orca comment, replacing
	// whatever comment it had until nothing is watched anymore.
	Comment bool `yaml:"comment"`
}

type Herdr struct {
	Enabled bool     `yaml:"enabled"`
	Bin     string   `yaml:"bin"`
	Socket  string   `yaml:"socket"`
	Poll    Duration `yaml:"poll"`
}

type Delivery struct {
	Debounce       Duration `yaml:"debounce"`
	UrgentDebounce Duration `yaml:"urgent_debounce"`
	Retry          Duration `yaml:"retry"`
	Prefix         string   `yaml:"prefix"`
	Notify         Notify   `yaml:"notify"`
}

// Notify raises host notifications for the person, not the agent.
type Notify struct {
	// Stuck notifies once when a workspace's updates could not be delivered
	// for this long, typically an agent waiting at a question. 0: never.
	Stuck Duration `yaml:"stuck"`
	// Kinds also notify when delivered, for news the person wants to see
	// even while the agent handles it.
	Kinds []string `yaml:"kinds"`
}

type Reminders struct {
	// CatchUp is how late a reminder may still fire after kelpie was down or
	// the machine slept. Older ones are skipped.
	CatchUp Duration `yaml:"catch_up"`
}

type Rules struct {
	IgnoreActors []string `yaml:"ignore_actors"`
	AllowBots    []string `yaml:"allow_bots"`
	Forward      []string `yaml:"forward"` // empty forwards every kind
}

type Binders struct {
	Branch bool `yaml:"branch"`
	// Linear binds linear:ENG-123 to the one workspace on a branch naming
	// the issue, like eng-123-fix-auth.
	Linear bool `yaml:"linear"`
}

// omitempty here only keeps kelpie config show to what a source sets.
type Source struct {
	Name string `yaml:"name"`
	// Enabled false drops a source. An entry with only name and enabled
	// switches a default source off or on without replacing it.
	Enabled *bool    `yaml:"enabled,omitempty"`
	Adapter string   `yaml:"adapter,omitempty"`
	Every   Duration `yaml:"every,omitempty"`
	ForEach *ForEach `yaml:"for_each,omitempty"`
	Run     []string `yaml:"run,omitempty"`
	Stdin   string   `yaml:"stdin,omitempty"`
	Auth    *Auth    `yaml:"auth,omitempty"`
	Slurp   bool     `yaml:"slurp,omitempty"` // join multiple JSON documents (gh --paginate) into one array
	// Conditional polls a REST endpoint through gh api with If-Modified-Since
	// set to the last Last-Modified. A 304 means no change and costs no rate
	// limit, and GitHub's X-Poll-Interval raises every. Needs run: [gh, api,
	// ...] without --paginate, because gh would send the header for every page.
	Conditional bool `yaml:"conditional,omitempty"`
	// Page is the most subjects a list source's command returns, such as
	// first: 50 in a GraphQL query. A poll that fills it may have cut off
	// subjects that are still listed, so it forgets none of the missing.
	Page        int          `yaml:"page,omitempty"`
	Map         string       `yaml:"map,omitempty"`
	Transitions []Transition `yaml:"transitions,omitempty"`
	// Badge is a jq expression over the fields giving a short label, shown on
	// the panes watching the subject. Empty: no label.
	Badge string `yaml:"badge,omitempty"`
	// Refuse is a jq expression over the fields of the poll kelpie watch
	// makes up front. A non-empty string refuses the watch with that reason,
	// like a run that has already completed: it would never change again.
	Refuse string `yaml:"refuse,omitempty"`
}

// Auth hands the command a credential from kelpie auth on stdin, in place
// of stdin: in Stdin, {token} becomes the secret, as in
// 'header = "Authorization: {token}"' for curl -K -. Never an argument,
// which ps and kelpie's logs would show. The short form `auth: linear`
// leaves Stdin empty for the integration's own default.
type Auth struct {
	Name  string `yaml:"name"`
	Stdin string `yaml:"stdin,omitempty"`
}

func (a *Auth) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*a = Auth{Name: n.Value}
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: auth is an integration name or { name, stdin }", n.Line)
	}
	// Node.Decode does not inherit KnownFields, and a typo such as "stdn"
	// would silently fall back to the default stdin.
	for i := 0; i < len(n.Content); i += 2 {
		if k := n.Content[i].Value; k != "name" && k != "stdin" {
			return fmt.Errorf("line %d: auth has no field %q", n.Content[i].Line, k)
		}
	}
	type plain Auth
	return n.Decode((*plain)(a))
}

// MarshalYAML keeps the short form short in kelpie config show.
func (a Auth) MarshalYAML() (any, error) {
	if a.Stdin == "" {
		return a.Name, nil
	}
	type plain Auth
	return plain(a), nil
}

// ForEach runs a source once per subject instead of once for everything, the
// way gh pr checks --watch polls one PR. Two kinds:
//
//   - Subjects: one loop per watched subject matching a pattern such as
//     "github:{owner}/{name}#{number}", running while the watch exists. The
//     pattern's {placeholders} become the parent fields.
//   - Source + While: one loop per subject of another source, started when its
//     observation matches While and kept running while the source's own
//     observation still matches.
//
// Run arguments may use {field} placeholders from the parent's fields.
type ForEach struct {
	Subjects string `yaml:"subjects,omitempty"`
	Source   string `yaml:"source,omitempty"`
	While    Match  `yaml:"while,omitempty"`
}

type Match struct {
	Field  string `yaml:"field,omitempty"`
	Equals any    `yaml:"equals,omitempty"`
}

// Matches compares by JSON encoding, so a YAML int matches a float64 field.
func (m Match) Matches(fields map[string]any) bool {
	got, err1 := json.Marshal(fields[m.Field])
	want, err2 := json.Marshal(m.Equals)
	return err1 == nil && err2 == nil && bytes.Equal(got, want)
}

type Transition struct {
	Field     string `yaml:"field,omitempty"`
	From      any    `yaml:"from,omitempty"`
	To        any    `yaml:"to,omitempty"`
	Changed   bool   `yaml:"changed,omitempty"`
	Increased bool   `yaml:"increased,omitempty"`
	// Ignore lists placeholder values that mean "not known right now", like
	// mergeable UNKNOWN while GitHub recomputes it. The last known value is
	// kept, so CONFLICTING -> UNKNOWN -> CONFLICTING does not fire twice.
	Ignore   []any  `yaml:"ignore,omitempty"`
	Kind     string `yaml:"kind,omitempty"`
	Urgent   bool   `yaml:"urgent,omitempty"`
	Terminal bool   `yaml:"terminal,omitempty"`
	// jq expressions over the new fields, with the previous fields as $old.
	Summary string `yaml:"summary,omitempty"`
	Actor   string `yaml:"actor,omitempty"`
	Fetch   string `yaml:"fetch,omitempty"`
}

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	d.Duration = v
	return err
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// DefaultPath is ~/.config/kelpie/config.yaml on every platform. Not
// os.UserConfigDir, which is ~/Library/Application Support on macOS.
func DefaultPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "kelpie", "config.yaml")
}

// Load layers the file at path over Default. A missing file means Default
// alone, so kelpie runs before kelpie init.
func Load(path string) (*Config, error) {
	data, err := read(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse layers file over Default and prepends defs to every expression.
func Parse(file []byte) (*Config, error) {
	cfg, err := layer(file)
	if err != nil {
		return nil, err
	}
	for i := range cfg.Sources {
		src := &cfg.Sources[i]
		for _, e := range []*string{&src.Map, &src.Badge, &src.Refuse} {
			*e = withDefs(cfg.Defs, *e)
		}
		for j := range src.Transitions {
			t := &src.Transitions[j]
			for _, e := range []*string{&t.Summary, &t.Actor, &t.Fetch} {
				*e = withDefs(cfg.Defs, *e)
			}
		}
	}
	return cfg, nil
}

// Effective is the layered config at path as YAML, before defs are
// prepended, which would repeat them in every expression.
func Effective(path string) ([]byte, []string, error) {
	data, err := read(path)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := layer(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, nil, err
	}
	return b.Bytes(), cfg.Warnings, nil
}

func read(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// layer decodes Default and then the file into one Config. Sources merge by
// name, and a file source replaces the default one whole rather than field
// by field: run, stdin, map, transitions and badge are one program over one
// data shape, so a file's map mixed with a newer default query (or the other
// way round) would break without any error. defs append to the default's, so
// a file adds helpers and keeps the shipped ones; a jq def of the same name
// and arity shadows the earlier one for every expression, though not inside
// default defs that already call it.
//
// A file source of a default's name is kept even when it is just an old copy:
// kelpie cannot tell that from a deliberate change, so it warns instead.
func layer(file []byte) (*Config, error) {
	var cfg Config
	if err := decode(Default, &cfg); err != nil {
		return nil, fmt.Errorf("built-in default: %w", err)
	}
	builtin, builtinDefs := cfg.Sources, cfg.Defs
	cfg.Sources, cfg.Defs = nil, ""
	if err := decode(file, &cfg); err != nil {
		return nil, err
	}

	sources := slices.Clone(builtin)
	var replaced []string
	for i, s := range cfg.Sources {
		if s.Name == "" {
			return nil, fmt.Errorf("sources[%d]: name is required", i)
		}
		if slices.ContainsFunc(cfg.Sources[:i], func(o Source) bool { return o.Name == s.Name }) {
			return nil, fmt.Errorf("sources[%d]: %s is listed twice", i, s.Name)
		}
		j := slices.IndexFunc(sources, func(b Source) bool { return b.Name == s.Name })
		toggle := s.Enabled != nil && reflect.DeepEqual(s, Source{Name: s.Name, Enabled: s.Enabled})
		switch {
		case j >= 0 && toggle:
			sources[j].Enabled = s.Enabled
		case j >= 0:
			sources[j] = s
			if s.Enabled == nil || *s.Enabled {
				replaced = append(replaced, s.Name)
			}
		case toggle:
			// A warning, not an error: a default source that a later kelpie
			// renames or drops must not stop the daemon from starting.
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("sources: %s is switched on or off, but there is no built-in source of that name", s.Name))
		default:
			sources = append(sources, s)
		}
	}
	cfg.Sources = slices.DeleteFunc(sources, func(s Source) bool { return s.Enabled != nil && !*s.Enabled })
	for i := range cfg.Sources {
		if cfg.Sources[i].Every.Duration == 0 {
			cfg.Sources[i].Every = Duration{60 * time.Second}
		}
	}
	if len(replaced) > 0 {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("replaces built-in sources: %s. A replaced source no longer gets kelpie's fixes; delete it from the file unless you changed it on purpose", strings.Join(replaced, ", ")))
	}

	if cfg.Defs == "" {
		cfg.Defs = builtinDefs
	} else {
		have := defNames(builtinDefs)
		if again := slices.DeleteFunc(defNames(cfg.Defs), func(n string) bool { return !slices.Contains(have, n) }); len(again) > 0 {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("redefines built-in defs: %s. A redefined def no longer gets kelpie's fixes; delete it from the file unless you changed it on purpose", strings.Join(slices.Compact(again), ", ")))
		}
		cfg.Defs = builtinDefs + "\n" + cfg.Defs
	}
	return &cfg, nil
}

// decode fails on unknown keys: a typo like "transition:" would otherwise
// silently disable every event of a source. A file of only comments is
// empty, not an error.
func decode(data []byte, cfg *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

var defName = regexp.MustCompile(`(?m)^def\s+([A-Za-z_][A-Za-z0-9_]*)`)

func defNames(defs string) []string {
	var names []string
	for _, m := range defName.FindAllStringSubmatch(defs, -1) {
		names = append(names, m[1])
	}
	slices.Sort(names)
	return names
}

// withDefs leaves an empty expression empty: empty means "not set".
func withDefs(defs, expr string) string {
	if expr == "" {
		return ""
	}
	return defs + "\n" + expr
}

func (c *Config) Source(name string) (Source, bool) {
	for _, s := range c.Sources {
		if s.Name == name {
			return s, true
		}
	}
	return Source{}, false
}
