package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/provider"
)

// Provider is an integration binary. It is registered by its command only:
// kelpie never looks for kelpie-* on PATH, so what runs is what the file names.
type Provider struct {
	Command []string `yaml:"command,flow"`
}

// SchemaVersion is the newest describe output this kelpie reads. A provider
// adding a field keeps its version; removing or redefining one raises it, and
// an older kelpie skips that provider instead of misreading it.
const SchemaVersion = 1

// Described is what a provider's describe prints, in the source schema of
// this file, so a provider needs nothing from kelpie but this shape.
type Described struct {
	Kelpie int    `yaml:"kelpie"`
	Name   string `yaml:"name"`
	// Defs are jq helpers its sources share, layered after the built-in ones.
	Defs       string      `yaml:"defs,omitempty"`
	Sources    []Source    `yaml:"sources,omitempty"`
	Credential *Credential `yaml:"credential,omitempty"`
	// Branches bind its subjects to the workspace whose branch names them.
	Branches []Branch `yaml:"branches,omitempty"`
	// Targets are regexes for the URLs and IDs it parses, so a target given
	// without a provider name goes to the one provider that claims it.
	Targets []string `yaml:"targets,omitempty"`
	// Me are the arguments appended to its command for a check that prints
	// whom its sources act as, which "@me" in rules.ignore_actors stands for.
	// The daemon runs it once at start. A provider whose sources mark the
	// person's own changes "@me" themselves, as Linear's isMe allows, needs
	// none.
	Me []string `yaml:"me,flow,omitempty"`

	// Command is the registered command, not part of describe.
	Command []string `yaml:"-"`
	targets []*regexp.Regexp
}

// Credential is a secret kelpie holds for the provider's sources in the
// system keychain, under the name as its account.
type Credential struct {
	Name   string `yaml:"name"`
	Prompt string `yaml:"prompt"`
	// Stdin is what a source with the short form auth: <name> writes to its
	// command, {token} being the secret.
	Stdin string `yaml:"stdin"`
	// Verify are the arguments appended to the provider's command for a
	// check that reads the token on stdin and prints who it belongs to.
	Verify []string `yaml:"verify,flow"`
}

// Branch binds subjects matching Subject (a pattern like linear:{id}) by
// branch name: Branch is a regex in which each {placeholder} of Subject
// stands for the subject's value, quoted.
type Branch struct {
	Subject string `yaml:"subject"`
	Branch  string `yaml:"branch"`
}

// Claims tells whether target is one of the provider's URLs or IDs.
func (d Described) Claims(target string) bool {
	return slices.ContainsFunc(d.targets, func(re *regexp.Regexp) bool { return re.MatchString(target) })
}

// describeAll runs every provider's describe. One that fails, times out,
// prints something kelpie cannot read or speaks a newer schema is left out
// with the reason, so one broken binary does not stop the daemon.
func describeAll(ctx context.Context, providers map[string]Provider) (provided []Described, skipped []string) {
	for _, name := range slices.Sorted(maps.Keys(providers)) {
		d, err := describe(ctx, name, providers[name].Command)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("provider %s skipped: %v", name, err))
			continue
		}
		provided = append(provided, d)
	}
	return provided, skipped
}

func describe(ctx context.Context, name string, command []string) (Described, error) {
	if len(command) == 0 {
		return Described{}, errors.New("command is empty")
	}
	out, err := provider.Describe(ctx, command)
	if err != nil {
		return Described{}, err
	}
	d, err := parseDescribed(out)
	if err != nil {
		return Described{}, fmt.Errorf("describe: %w", err)
	}
	if d.Name != name {
		return Described{}, fmt.Errorf("describe names it %q; register it under that name", d.Name)
	}
	d.Command = command
	return d, nil
}

// parseDescribed reads the version before anything else, since a newer
// schema may not decode at all. Unknown fields are allowed: a provider adds
// fields without raising its version, for kelpies that know them.
func parseDescribed(out []byte) (Described, error) {
	var v struct {
		Kelpie int `yaml:"kelpie"`
	}
	if err := yaml.Unmarshal(out, &v); err != nil {
		return Described{}, err
	}
	switch {
	case v.Kelpie == 0:
		return Described{}, errors.New("no kelpie: version")
	case v.Kelpie > SchemaVersion:
		return Described{}, fmt.Errorf("it speaks kelpie: %d, this kelpie reads up to %d; update kelpie", v.Kelpie, SchemaVersion)
	}
	var d Described
	if err := yaml.NewDecoder(bytes.NewReader(out)).Decode(&d); err != nil {
		return Described{}, err
	}
	if d.Name == "" {
		return Described{}, errors.New("no name")
	}
	for i, s := range d.Sources {
		if s.Name == "" {
			return Described{}, fmt.Errorf("sources[%d]: name is required", i)
		}
	}
	if c := d.Credential; c != nil {
		if c.Name == "" {
			c.Name = d.Name
		}
		switch {
		case !strings.Contains(c.Stdin, "{token}"):
			return Described{}, errors.New("credential: stdin needs {token}")
		case len(c.Verify) == 0:
			return Described{}, errors.New("credential: verify is required")
		}
	}
	for _, b := range d.Branches {
		if _, err := core.NewBranchPattern(b.Subject, b.Branch); err != nil {
			return Described{}, fmt.Errorf("branches: %w", err)
		}
	}
	for _, t := range d.Targets {
		re, err := regexp.Compile(t)
		if err != nil {
			return Described{}, fmt.Errorf("targets: %w", err)
		}
		d.targets = append(d.targets, re)
	}
	return d, nil
}

// summary is the comment kelpie config show puts above a provider.
func (d Described) summary(from map[string]string) string {
	var parts, sources []string
	for _, s := range d.Sources {
		if from[s.Name] == d.Name {
			sources = append(sources, s.Name)
		}
	}
	if len(sources) > 0 {
		parts = append(parts, "sources "+strings.Join(sources, ", "))
	}
	if d.Credential != nil {
		parts = append(parts, "credential "+d.Credential.Name)
	}
	if len(d.Branches) > 0 {
		parts = append(parts, fmt.Sprintf("branch patterns for %s", strings.Join(branchSubjects(d.Branches), ", ")))
	}
	if n := len(d.Targets); n > 0 {
		parts = append(parts, fmt.Sprintf("%d target patterns", n))
	}
	if len(d.Me) > 0 {
		parts = append(parts, "@me")
	}
	if len(parts) == 0 {
		return "describe contributed nothing"
	}
	return "describe contributed " + strings.Join(parts, "; ")
}

func branchSubjects(bs []Branch) []string {
	var subjects []string
	for _, b := range bs {
		subjects = append(subjects, b.Subject)
	}
	return slices.Compact(subjects)
}

// Credential finds a provider's credential by its name.
func (c *Config) Credential(name string) (Credential, Described, bool) {
	for _, d := range c.Provided {
		if d.Credential != nil && d.Credential.Name == name {
			return *d.Credential, d, true
		}
	}
	return Credential{}, Described{}, false
}

// Credentials lists every provider's credential, in provider order.
func (c *Config) Credentials() []Credential {
	var creds []Credential
	for _, d := range c.Provided {
		if d.Credential != nil {
			creds = append(creds, *d.Credential)
		}
	}
	return creds
}

func (c *Config) credentialNames() string {
	var names []string
	for _, cr := range c.Credentials() {
		names = append(names, cr.Name)
	}
	if len(names) == 0 {
		return "no provider that loaded has one"
	}
	return "providers have " + strings.Join(names, ", ")
}

// Provider finds a provider that loaded by its name.
func (c *Config) Provider(name string) (Described, bool) {
	i := slices.IndexFunc(c.Provided, func(d Described) bool { return d.Name == name })
	if i < 0 {
		return Described{}, false
	}
	return c.Provided[i], true
}
