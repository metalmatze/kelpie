package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.yaml.in/yaml/v3"
)

//go:embed default.yaml
var Default []byte

type Config struct {
	Hosts     Hosts     `yaml:"hosts"`
	Delivery  Delivery  `yaml:"delivery"`
	Reminders Reminders `yaml:"reminders"`
	Rules     Rules     `yaml:"rules"`
	Binders   Binders   `yaml:"binders"`
	Sources   []Source  `yaml:"sources"`
}

type Hosts struct {
	Herdr Herdr `yaml:"herdr"`
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
}

type Source struct {
	Name        string       `yaml:"name"`
	Adapter     string       `yaml:"adapter"`
	Every       Duration     `yaml:"every"`
	ForEach     *ForEach     `yaml:"for_each"`
	Run         []string     `yaml:"run"`
	Stdin       string       `yaml:"stdin"`
	Slurp       bool         `yaml:"slurp"` // join multiple JSON documents (gh --paginate) into one array
	Map         string       `yaml:"map"`
	Transitions []Transition `yaml:"transitions"`
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
	Subjects string `yaml:"subjects"`
	Source   string `yaml:"source"`
	While    Match  `yaml:"while"`
}

type Match struct {
	Field  string `yaml:"field"`
	Equals any    `yaml:"equals"`
}

// Matches compares by JSON encoding, so a YAML int matches a float64 field.
func (m Match) Matches(fields map[string]any) bool {
	got, err1 := json.Marshal(fields[m.Field])
	want, err2 := json.Marshal(m.Equals)
	return err1 == nil && err2 == nil && bytes.Equal(got, want)
}

type Transition struct {
	Field     string `yaml:"field"`
	From      any    `yaml:"from"`
	To        any    `yaml:"to"`
	Changed   bool   `yaml:"changed"`
	Increased bool   `yaml:"increased"`
	// Ignore lists placeholder values that mean "not known right now", like
	// mergeable UNKNOWN while GitHub recomputes it. The last known value is
	// kept, so CONFLICTING -> UNKNOWN -> CONFLICTING does not fire twice.
	Ignore   []any  `yaml:"ignore"`
	Kind     string `yaml:"kind"`
	Urgent   bool   `yaml:"urgent"`
	Terminal bool   `yaml:"terminal"`
	// jq expressions over the new fields, with the previous fields as $old.
	Summary string `yaml:"summary"`
	Actor   string `yaml:"actor"`
	Fetch   string `yaml:"fetch"`
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

// Load reads path, or the embedded default when path does not exist.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data = Default
	} else if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func Parse(data []byte) (*Config, error) {
	cfg := &Config{
		Hosts: Hosts{Herdr: Herdr{Bin: "herdr", Poll: Duration{15 * time.Second}}},
		Delivery: Delivery{
			Debounce:       Duration{30 * time.Second},
			UrgentDebounce: Duration{5 * time.Second},
			Retry:          Duration{30 * time.Second},
			Prefix:         "[kelpie]",
		},
		Reminders: Reminders{CatchUp: Duration{12 * time.Hour}},
	}
	// Unknown keys are errors: a typo like "transition:" would otherwise
	// silently disable every event of a source.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, err
	}
	for i, s := range cfg.Sources {
		if s.Name == "" {
			return nil, fmt.Errorf("sources[%d]: name is required", i)
		}
		if s.Every.Duration == 0 {
			cfg.Sources[i].Every = Duration{60 * time.Second}
		}
	}
	return cfg, nil
}

func (c *Config) Source(name string) (Source, bool) {
	for _, s := range c.Sources {
		if s.Name == name {
			return s, true
		}
	}
	return Source{}, false
}
