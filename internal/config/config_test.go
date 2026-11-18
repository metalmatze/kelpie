package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// githubProvider registers the describe output kelpie-github embeds, without
// building the binary.
func githubProvider(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../../cmd/kelpie-github/describe.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return "providers:\n  github: {command: [sh, -c, \"cat '" + path + "'\"]}\n"
}

// githubCopy is a file holding a full copy of kelpie-github's defs and
// sources, as an old kelpie's default had them.
func githubCopy(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../cmd/kelpie-github/describe.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(b), "\ndefs: |")
	if !ok {
		t.Fatal("describe.yaml has no defs")
	}
	return "defs: |" + after
}

func names(c *Config) []string {
	var n []string
	for _, s := range c.Sources {
		n = append(n, s.Name)
	}
	return n
}

// The layering rules, over kelpie-github's sources and defs as most files
// see them.
func TestLayer(t *testing.T) {
	github := githubProvider(t)
	base, err := layer(t.Context(), []byte(github))
	if err != nil {
		t.Fatal(err)
	}
	builtin := names(base)
	if !slices.Contains(builtin, "github-issue") {
		t.Fatalf("provider sources %v lack github-issue", builtin)
	}
	plain, err := layer(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Sources) > 0 || plain.Defs != "" {
		t.Fatalf("the default has sources %v or defs", names(plain))
	}

	for _, tc := range []struct {
		name  string
		file  string
		err   string // substring; empty: no error
		check func(t *testing.T, c *Config)
	}{
		{
			name: "one host key keeps the other hosts and every source",
			file: "hosts: {orca: {enabled: true}}",
			check: func(t *testing.T, c *Config) {
				h := c.Hosts
				if !h.Orca.Enabled || h.Orca.Poll.Seconds() != 15 || !h.Herdr.Enabled || h.Herdr.Bin != "herdr" || h.HC.Enabled {
					t.Errorf("hosts = %+v", h)
				}
				if !slices.Equal(names(c), builtin) || len(c.Warnings) > 0 {
					t.Errorf("sources %v, warnings %v", names(c), c.Warnings)
				}
			},
		},
		{
			name: "nested key keeps its siblings",
			file: "delivery: {notify: {stuck: 1m}}",
			check: func(t *testing.T, c *Config) {
				n := c.Delivery.Notify
				if n.Stuck.Minutes() != 1 || !slices.Equal(n.Kinds, base.Delivery.Notify.Kinds) || c.Delivery.Debounce != base.Delivery.Debounce {
					t.Errorf("delivery = %+v", c.Delivery)
				}
			},
		},
		{
			name: "a list replaces the default's",
			file: "rules: {ignore_actors: [someone]}",
			check: func(t *testing.T, c *Config) {
				if !slices.Equal(c.Rules.IgnoreActors, []string{"someone"}) || !slices.Equal(c.Rules.AllowBots, base.Rules.AllowBots) {
					t.Errorf("rules = %+v", c.Rules)
				}
			},
		},
		{
			name: "an empty list clears the default's",
			file: "delivery: {notify: {kinds: []}}",
			check: func(t *testing.T, c *Config) {
				if len(c.Delivery.Notify.Kinds) != 0 {
					t.Errorf("kinds = %v", c.Delivery.Notify.Kinds)
				}
			},
		},
		{
			name: "a source of a provider's name replaces it whole, in place",
			file: "sources: [{name: github-pr, adapter: cli, run: [echo, '{}'], map: '{fields: {}}'}]",
			check: func(t *testing.T, c *Config) {
				if !slices.Equal(names(c), builtin) {
					t.Errorf("sources %v, want %v", names(c), builtin)
				}
				s, _ := c.Source("github-pr")
				if s.Stdin != "" || s.ForEach != nil || len(s.Transitions) != 0 || s.Badge != "" || s.Every.Seconds() != 60 {
					t.Errorf("github-pr kept default fields: %+v", s)
				}
				if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "github-pr (provider github)") {
					t.Errorf("warnings = %v", c.Warnings)
				}
			},
		},
		{
			name: "enabled false switches a provider source off",
			file: "sources: [{name: github-inbox-org, enabled: false}]",
			check: func(t *testing.T, c *Config) {
				want := slices.DeleteFunc(slices.Clone(builtin), func(n string) bool { return n == "github-inbox-org" })
				if !slices.Equal(names(c), want) || len(c.Warnings) > 0 {
					t.Errorf("sources %v, warnings %v", names(c), c.Warnings)
				}
			},
		},
		{
			name: "enabled true alone keeps the provider source",
			file: "sources: [{name: github-pr, enabled: true}]",
			check: func(t *testing.T, c *Config) {
				s, _ := c.Source("github-pr")
				if s.Map == "" || len(c.Warnings) > 0 {
					t.Errorf("github-pr replaced: %+v, warnings %v", s, c.Warnings)
				}
			},
		},
		{
			name: "switching an unknown source warns",
			file: "sources: [{name: nope, enabled: false}]",
			check: func(t *testing.T, c *Config) {
				if !slices.Equal(names(c), builtin) || len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "nope") {
					t.Errorf("sources %v, warnings %v", names(c), c.Warnings)
				}
			},
		},
		{
			name: "a new name is added after the providers'",
			file: "sources: [{name: mine, adapter: cli, run: [true]}]",
			check: func(t *testing.T, c *Config) {
				if !slices.Equal(names(c), append(slices.Clone(builtin), "mine")) || len(c.Warnings) > 0 {
					t.Errorf("sources %v, warnings %v", names(c), c.Warnings)
				}
			},
		},
		{
			name: "defs add to the providers'",
			file: "defs: 'def mine: 1;'",
			check: func(t *testing.T, c *Config) {
				if !strings.HasPrefix(c.Defs, base.Defs) || !strings.HasSuffix(c.Defs, "def mine: 1;") || len(c.Warnings) > 0 {
					t.Errorf("defs = ...%q, warnings %v", c.Defs[max(0, len(c.Defs)-40):], c.Warnings)
				}
			},
		},
		{
			name: "redefining a provider def warns",
			file: "defs: 'def login: .name;'",
			check: func(t *testing.T, c *Config) {
				if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "login") {
					t.Errorf("warnings = %v", c.Warnings)
				}
			},
		},
		{
			name: "a full copy of a provider's sources and defs warns about them",
			file: githubCopy(t),
			check: func(t *testing.T, c *Config) {
				if !slices.Equal(names(c), builtin) || len(c.Warnings) != 2 {
					t.Errorf("sources %v, warnings %v", names(c), c.Warnings)
				}
			},
		},
		{name: "unknown key", file: "hosts: {orca: {enabeld: true}}", err: "field enabeld not found in type config.Orca"},
		{name: "source without name", file: "sources: [{adapter: cli}]", err: "sources[0]: name is required"},
		{name: "source twice", file: "sources: [{name: a}, {name: a}]", err: "a is listed twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := layer(t.Context(), []byte(github+tc.file))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, c)
		})
	}
}

// The starter registers kelpie-github and changes nothing else, so a new
// install watches GitHub as before GitHub became a provider.
func TestStarter(t *testing.T) {
	var file Config
	if err := decode(Starter, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Providers) != 1 || !slices.Equal(file.Providers["github"].Command, []string{"kelpie-github"}) {
		t.Errorf("starter providers %+v", file.Providers)
	}
	file.Providers = nil
	var empty Config
	if !reflect.DeepEqual(file, empty) {
		t.Errorf("starter sets more than providers: %+v", file)
	}
}

// Parse compiles the layered defs into every expression, so a file def is
// usable by its own sources.
func TestParseDefs(t *testing.T) {
	c, err := Parse(t.Context(), []byte(githubProvider(t)+"defs: 'def mine: 1;'\nsources: [{name: x, adapter: cli, run: [true], map: mine}]"))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := c.Source("x")
	if !strings.Contains(s.Map, "def login") || !strings.HasSuffix(s.Map, "def mine: 1;\nmine") {
		t.Errorf("map = ...%q", s.Map[max(0, len(s.Map)-40):])
	}
}

func TestAuthForms(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want Auth
		err  string
	}{
		{yaml: `auth: linear`, want: Auth{Name: "linear"}},
		{yaml: `auth: { name: linear, stdin: "x {token}" }`, want: Auth{Name: "linear", Stdin: "x {token}"}},
		{yaml: `auth: { name: linear, stdn: "x" }`, err: `auth has no field "stdn"`},
		{yaml: `auth: [linear]`, err: "auth is an integration name"},
	} {
		c, err := layer(t.Context(), []byte("sources:\n  - name: s\n    run: [cat]\n    "+tc.yaml+"\n"))
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: got %v, want %q", tc.yaml, err, tc.err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.yaml, err)
		}
		s, _ := c.Source("s")
		if s.Auth == nil || *s.Auth != tc.want {
			t.Errorf("%s: got %+v", tc.yaml, s.Auth)
		}
	}
}
