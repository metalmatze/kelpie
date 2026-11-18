package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metalmatze/kelpie/internal/provider"
)

// fakeProvider registers a provider whose describe prints out.
func fakeProvider(t *testing.T, out string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "describe.yaml")
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	// kelpie appends describe, which sh -c takes as $0.
	return `{command: [sh, -c, "cat '` + path + `'"]}`
}

const acme = `kelpie: 1
name: acme
some_later_field: kelpie 1 ignores it
defs: 'def acme_id: .id;'
credential:
  prompt: an acme token
  stdin: 'header = "X-Token: {token}"'
  verify: [verify]
branches:
  - { subject: "acme:{id}", branch: '{id}' }
targets: ['^ACME-\d+$']
me: [me]
sources:
  - name: acme
    adapter: cli
    auth: acme
    run: [cat]
    map: '{subject: "acme:\(acme_id)"}'
  - name: acme-item
    adapter: cli
    for_each: { subjects: "acme:{id}" }
    run: [cat]
    map: .
`

func TestProviderLayer(t *testing.T) {
	file := "providers:\n  acme: " + fakeProvider(t, acme) + "\n"
	c, err := Parse(t.Context(), []byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Skipped) > 0 || len(c.Warnings) > 0 {
		t.Fatalf("skipped %v, warnings %v", c.Skipped, c.Warnings)
	}
	if n := names(c); !slices.Equal(n, []string{"acme", "acme-item"}) {
		t.Fatalf("sources %v", n)
	}
	if c.From["acme"] != "acme" || c.From["acme-item"] != "acme" {
		t.Errorf("from %v", c.From)
	}
	d, ok := c.Provider("acme")
	if !ok || !d.Claims("ACME-7") || d.Claims("acme/api") || d.Credential.Name != "acme" {
		t.Fatalf("provider %+v", d)
	}
	// The short form takes the provider's stdin, and its defs reach its maps.
	s, _ := c.Source("acme")
	if s.Auth.Stdin != `header = "X-Token: {token}"` || !strings.Contains(s.Map, "def acme_id") {
		t.Errorf("auth %+v, map %q", s.Auth, s.Map)
	}

	// A file source of a provider's name replaces it with a warning, and an
	// entry with only enabled switches one off.
	c, err = Parse(t.Context(), []byte(file+`sources:
  - { name: acme, adapter: cli, run: [true], map: . }
  - { name: acme-item, enabled: false }
`))
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := c.Source("acme"); s.Run[0] != "true" || c.From["acme"] != "" {
		t.Errorf("file did not replace acme: %+v", s)
	}
	if _, ok := c.Source("acme-item"); ok {
		t.Error("acme-item not switched off")
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "acme (provider acme)") {
		t.Errorf("warnings %v", c.Warnings)
	}

	// Without the provider, a source naming its credential has nothing to
	// hand its command.
	_, err = Parse(t.Context(), []byte("sources: [{name: x, adapter: cli, run: [cat], map: ., auth: acme}]"))
	if err == nil || !strings.Contains(err.Error(), `auth "acme" is no credential of a provider`) {
		t.Errorf("unknown credential: %v", err)
	}
}

// A provider that fails, hangs, speaks a newer schema or names itself
// otherwise is left out with the reason; the rest of the config loads.
func TestProviderSkipped(t *testing.T) {
	defer func(d time.Duration) { provider.DescribeTimeout = d }(provider.DescribeTimeout)
	provider.DescribeTimeout = 200 * time.Millisecond
	for _, tc := range []struct {
		name, provider, want string
	}{
		{"missing binary", `{command: [/nonexistent/kelpie-acme]}`, "no such file"},
		{"failing", `{command: [sh, -c, "echo broken >&2; exit 3"]}`, "broken"},
		{"hanging", `{command: [sh, -c, "sleep 5"]}`, "no answer within 200ms"},
		{"newer schema", fakeProvider(t, strings.Replace(acme, "kelpie: 1", "kelpie: 2", 1)), "kelpie: 2, this kelpie reads up to 1"},
		{"no version", fakeProvider(t, strings.Replace(acme, "kelpie: 1\n", "", 1)), "no kelpie: version"},
		{"other name", fakeProvider(t, strings.Replace(acme, "name: acme", "name: other", 1)), `names it "other"`},
		{"bad target", fakeProvider(t, strings.Replace(acme, `'^ACME-\d+$'`, `'('`, 1)), "targets"},
		{"bad branch", fakeProvider(t, strings.Replace(acme, `branch: '{id}'`, `branch: '({id}'`, 1)), "branches"},
		{"no verify", fakeProvider(t, strings.Replace(acme, "  verify: [verify]\n", "", 1)), "verify is required"},
		{"empty command", `{command: []}`, "command is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			c, err := Parse(t.Context(), []byte("providers:\n  acme: "+tc.provider+"\n"))
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Skipped) != 1 || !strings.Contains(c.Skipped[0], "provider acme skipped") || !strings.Contains(c.Skipped[0], tc.want) {
				t.Errorf("skipped %v, want %q", c.Skipped, tc.want)
			}
			if _, ok := c.Source("acme"); ok || len(c.Provided) > 0 {
				t.Error("a skipped provider contributed")
			}
			if c.Hosts.Herdr.Bin != "herdr" {
				t.Error("the default did not load")
			}
			if time.Since(start) > 3*time.Second {
				t.Error("describe was not cut off")
			}
		})
	}
}

// kelpie config show says which provider each source came from and what
// each provider contributed.
func TestEffectiveNamesProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	file := "providers:\n  acme: " + fakeProvider(t, acme) + "\n  gone: {command: [/nonexistent/kelpie-gone]}\n"
	if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	out, warnings, err := Effective(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# describe contributed sources acme, acme-item; credential acme; branch patterns for acme:{id}; 1 target patterns; @me\n  acme:",
		"# skipped, see the warnings\n  gone:",
		"  # from provider acme\n  - name: acme\n",
		"  # from provider acme\n  - name: acme-item\n",
		"def acme_id",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("config show lacks %q", want)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "provider gone skipped") {
		t.Errorf("warnings %v", warnings)
	}
}

// binders.<provider>: false turns that provider's branch binding off, under
// the same key binders.linear had before Linear became a provider.
func TestBindersByProvider(t *testing.T) {
	c, err := Parse(t.Context(), []byte("providers:\n  acme: "+fakeProvider(t, acme)+"\nbinders: {branch: true, acme: false, nobody: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Binders.Branch || c.Binders.ByBranch("acme") || !c.Binders.ByBranch("other") {
		t.Errorf("binders %+v", c.Binders)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "binders: nobody") {
		t.Errorf("warnings %v", c.Warnings)
	}
}
