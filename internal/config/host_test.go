package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metalmatze/kelpie/internal/host/external"
)

func fakeHost(t *testing.T, out string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host-describe.yaml")
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	return `{command: [sh, -c, "cat '` + path + `'"]}`
}

const customHost = `kelpie: 1
name: custom
reports_agent: true
term_program: customterm
caller_env: [CUSTOM_WS_ID]
capabilities: [badge, notify]
`

func TestExternalHostLayer(t *testing.T) {
	file := "hosts:\n  custom: " + fakeHost(t, customHost) + "\n"
	c, err := Parse(t.Context(), []byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Skipped) > 0 || len(c.Warnings) > 0 {
		t.Fatalf("skipped %v, warnings %v", c.Skipped, c.Warnings)
	}
	if !c.Hosts.Herdr.Enabled || c.Hosts.Orca.Enabled || c.Hosts.HC.Enabled {
		t.Fatalf("built-in hosts changed: %+v", c.Hosts)
	}
	if len(c.ExternalHosts) != 1 {
		t.Fatalf("external hosts = %+v", c.ExternalHosts)
	}
	eh := c.ExternalHosts[0]
	if eh.Name != "custom" || !eh.ReportsAgent || eh.TermProgram != "customterm" || !slices.Equal(eh.CallerEnv, []string{"CUSTOM_WS_ID"}) || !eh.HasCapability("badge") || !eh.HasCapability("notify") || eh.Poll != 15*time.Second {
		t.Fatalf("described host = %+v", eh)
	}

	// Custom poll overrides the 15s default.
	c, err = Parse(t.Context(), []byte("hosts:\n  custom:\n    command: [sh, -c, \"printf 'kelpie: 1\\nname: custom\\n'\"]\n    poll: 5s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ExternalHosts) != 1 || c.ExternalHosts[0].Poll != 5*time.Second {
		t.Fatalf("custom poll: %+v", c.ExternalHosts)
	}

	// A disabled external host does not run describe and produces no skipped warning.
	marker := filepath.Join(t.TempDir(), "ran")
	c, err = Parse(t.Context(), []byte("hosts:\n  custom:\n    enabled: false\n    command: [sh, -c, \"touch '"+marker+"'\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ExternalHosts) != 0 || len(c.Skipped) != 0 {
		t.Fatalf("disabled host: %+v skipped %v", c.ExternalHosts, c.Skipped)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("disabled external host ran describe")
	}
}

func TestExternalHostValidation(t *testing.T) {
	for _, tc := range []struct {
		name, file, err string
	}{
		{"typo in built-in host", "hosts: {ocra: {enabled: true}}", "hosts.ocra: command is required (built-in hosts are herdr, orca, hc)"},
		{"unknown field", "hosts: {custom: {command: [x], pol: 5s}}", "field pol not found in type config.ExternalHost"},
		{"colon in name", `hosts: {"bad:name": {command: [x]}}`, "host name must match"},
		{"uppercase name", `hosts: {Custom: {command: [x]}}`, "host name must match"},
		{"herdr workspace id w1", `hosts: {w1: {command: [x]}}`, "reserved for herdr pane IDs"},
		{"herdr workspace id w12", `hosts: {w12: {command: [x]}}`, "reserved for herdr pane IDs"},
		{"negative poll", `hosts: {custom: {command: [x], poll: -1s}}`, "poll must not be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(t.Context(), []byte(tc.file)); err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("got %v, want %q", err, tc.err)
			}
		})
	}
	for _, name := range []string{"w", "web", "w1-host"} {
		if err := validateExternalHosts(map[string]ExternalHost{name: {Command: []string{"x"}}}); err != nil {
			t.Errorf("validateExternalHosts(%q) unexpected error: %v", name, err)
		}
	}
}

func TestExternalHostSkipped(t *testing.T) {
	orig := external.DescribeTimeout
	t.Cleanup(func() { external.DescribeTimeout = orig })
	external.DescribeTimeout = 200 * time.Millisecond

	for _, tc := range []struct {
		name, entry, want string
	}{
		{"missing binary", `{command: [/nonexistent/kelpie-host-x]}`, "no such file"},
		{"failing", `{command: [sh, -c, "echo broken >&2; exit 2"]}`, "broken"},
		{"hanging", `{command: [sh, -c, "sleep 5"]}`, "no answer within 200ms"},
		{"newer schema", fakeHost(t, "kelpie: 2\nname: custom\n"), "kelpie: 2, this kelpie reads up to 1"},
		{"other name", fakeHost(t, "kelpie: 1\nname: other\n"), `names it "other"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse(t.Context(), []byte("hosts:\n  custom: "+tc.entry+"\n"))
			if err != nil {
				t.Fatal(err)
			}
			if len(c.ExternalHosts) != 0 {
				t.Fatalf("skipped host loaded: %+v", c.ExternalHosts)
			}
			if len(c.Skipped) != 1 || !strings.Contains(c.Skipped[0], "host custom skipped") || !strings.Contains(c.Skipped[0], tc.want) {
				t.Errorf("skipped = %v, want %q", c.Skipped, tc.want)
			}
		})
	}
}

func TestEffectiveAnnotatesExternalHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	file := "hosts:\n  custom: " + fakeHost(t, customHost) + "\n  gone: {command: [/nonexistent/kelpie-host-gone]}\n"
	if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	out, warnings, err := Effective(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# external host; reports agent; term_program customterm; caller_env CUSTOM_WS_ID; capabilities badge, notify\n  custom:",
		"# skipped, see the warnings\n  gone:",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("config show lacks %q\ngot:\n%s", want, string(out))
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "host gone skipped") {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestLoadExternalHostsSkipsProviders(t *testing.T) {
	dir := t.TempDir()
	provMarker := filepath.Join(dir, "provider-ran")
	path := filepath.Join(dir, "config.yaml")
	file := "providers:\n  acme: {command: [sh, -c, \"touch '" + provMarker + "'\"]}\n" +
		"hosts:\n  custom: " + fakeHost(t, customHost) + "\n  off: {enabled: false, command: [true]}\n"
	if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	names, hosts, skipped, err := LoadExternalHosts(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"custom", "off"}) || len(hosts) != 1 || hosts[0].Name != "custom" || len(skipped) != 0 {
		t.Fatalf("got names=%v hosts=%+v skipped=%v", names, hosts, skipped)
	}
	if _, err := os.Stat(provMarker); err == nil {
		t.Fatal("LoadExternalHosts ran provider describe")
	}
}
