package config

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/metalmatze/kelpie/internal/host/external"
)

// ExternalHost is a host binary registered under hosts:. Like a provider,
// kelpie runs only what the file names and never searches PATH for host
// binaries.
type ExternalHost struct {
	Enabled *bool    `yaml:"enabled,omitempty"`
	Command []string `yaml:"command,flow"`
	Poll    Duration `yaml:"poll,omitempty"`
}

// IsEnabled reports whether the external host is turned on. Listing a host
// under hosts: enables it unless enabled: false is set.
func (h ExternalHost) IsEnabled() bool {
	return h.Enabled == nil || *h.Enabled
}

// DescribedHost holds the describe output and registered config of an
// external host that loaded cleanly.
type DescribedHost struct {
	external.Description
	Command []string
	Poll    time.Duration
}

var (
	hostNamePattern   = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	herdrSpacePattern = regexp.MustCompile(`^w[0-9]+$`)
)

func validateExternalHosts(hosts map[string]ExternalHost) error {
	for _, name := range slices.Sorted(maps.Keys(hosts)) {
		h := hosts[name]
		if !hostNamePattern.MatchString(name) {
			return fmt.Errorf("hosts.%s: host name must match %s", name, hostNamePattern)
		}
		// herdr pane IDs stay bare (w1:p2), so a host named w<n> would hijack
		// HostOf for those panes.
		if herdrSpacePattern.MatchString(name) {
			return fmt.Errorf("hosts.%s: host name must not match %s (reserved for herdr pane IDs)", name, herdrSpacePattern)
		}
		if len(h.Command) == 0 {
			return fmt.Errorf("hosts.%s: command is required (built-in hosts are herdr, orca, hc, rex)", name)
		}
		if h.Poll.Duration < 0 {
			return fmt.Errorf("hosts.%s: poll must not be negative", name)
		}
		if h.Poll.Duration == 0 {
			h.Poll = Duration{15 * time.Second}
			hosts[name] = h
		}
	}
	return nil
}

// describeAllHosts runs describe on every enabled external host. One that
// fails, times out, prints unreadable output or speaks a newer schema is
// skipped with a reason, so one broken binary does not stop the daemon.
func describeAllHosts(ctx context.Context, hosts map[string]ExternalHost) (described []DescribedHost, skipped []string) {
	for _, name := range slices.Sorted(maps.Keys(hosts)) {
		h := hosts[name]
		if !h.IsEnabled() {
			continue
		}
		d, err := external.Describe(ctx, h.Command)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("host %s skipped: %v", name, err))
			continue
		}
		if d.Name != name {
			skipped = append(skipped, fmt.Sprintf("host %s skipped: describe names it %q; register it under that name", name, d.Name))
			continue
		}
		described = append(described, DescribedHost{
			Description: d,
			Command:     slices.Clone(h.Command),
			Poll:        h.Poll.Duration,
		})
	}
	return described, skipped
}

// summary is the comment kelpie config show puts above an external host.
func (d DescribedHost) summary() string {
	var parts []string
	if d.ReportsAgent {
		parts = append(parts, "reports agent")
	}
	if d.TermProgram != "" {
		parts = append(parts, "term_program "+d.TermProgram)
	}
	if len(d.CallerEnv) > 0 {
		parts = append(parts, "caller_env "+strings.Join(d.CallerEnv, ", "))
	}
	if len(d.Capabilities) > 0 {
		parts = append(parts, "capabilities "+strings.Join(d.Capabilities, ", "))
	}
	if len(parts) == 0 {
		return "external host"
	}
	return "external host; " + strings.Join(parts, "; ")
}

// LoadExternalHosts reads only the external hosts from path and runs describe
// on the enabled ones, without invoking any provider binaries. Commands that
// only need caller detection use this to avoid running provider describe.
func LoadExternalHosts(ctx context.Context, path string) (names []string, described []DescribedHost, skipped []string, err error) {
	data, err := read(path)
	if err != nil || len(data) == 0 {
		return nil, nil, nil, err
	}
	var cfg Config
	if err := decode(Default, &cfg); err != nil {
		return nil, nil, nil, fmt.Errorf("built-in default: %w", err)
	}
	cfg.Sources, cfg.Defs = nil, ""
	if err := decode(data, &cfg); err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(cfg.Hosts.External) == 0 {
		return nil, nil, nil, nil
	}
	if err := validateExternalHosts(cfg.Hosts.External); err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	names = slices.Sorted(maps.Keys(cfg.Hosts.External))
	described, skipped = describeAllHosts(ctx, cfg.Hosts.External)
	return names, described, skipped, nil
}
