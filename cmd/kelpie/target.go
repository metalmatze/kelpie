package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/metalmatze/kelpie/internal/config"
	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/provider"
)

// loadProviders loads the config for a command that routes targets, and
// tells why a provider it registers is missing, since the target then falls
// through to GitHub or fails with less to go on.
func loadProviders(ctx context.Context, g *Globals) (*config.Config, error) {
	cfg, err := config.Load(ctx, g.Config)
	if err != nil {
		return nil, err
	}
	for _, s := range cfg.Skipped {
		fmt.Fprintf(os.Stderr, "warning: %s\n", s)
	}
	return cfg, nil
}

// resolve turns the arguments of kelpie watch or unwatch into subjects: a
// provider's name and its arguments, which its subject subcommand parses in
// this shell, or a single target. github is how a target no provider claims
// is read, as long as GitHub is built in.
func resolve(ctx context.Context, cfg *config.Config, args []string, github func(string) (core.Subject, error)) ([]core.Subject, error) {
	if p, ok := cfg.Provider(args[0]); ok {
		return provider.Subject(ctx, p.Command, args[1:])
	}
	if len(args) > 1 {
		return nil, fmt.Errorf("no provider %q; %s", args[0], providerNames(cfg))
	}
	return resolveTarget(ctx, cfg, args[0], github)
}

// resolveTarget routes a target given without a provider name, like a
// clicked link, to the one provider whose target patterns claim it. Two
// claiming it (ENG-123 could be Linear or Jira) is refused rather than
// guessed: the first to answer would win without anyone seeing the other.
func resolveTarget(ctx context.Context, cfg *config.Config, target string, github func(string) (core.Subject, error)) ([]core.Subject, error) {
	target = strings.TrimSpace(target)
	var claims []config.Described
	for _, d := range cfg.Provided {
		if d.Claims(target) {
			claims = append(claims, d)
		}
	}
	switch len(claims) {
	case 0:
	case 1:
		return provider.Subject(ctx, claims[0].Command, []string{target})
	default:
		names := make([]string, len(claims))
		for i, d := range claims {
			names[i] = d.Name
		}
		return nil, fmt.Errorf("%s could be %s; name the provider, as in kelpie watch %s %s", target, strings.Join(names, " or "), names[0], target)
	}
	if core.IsSubject(target) {
		return []core.Subject{core.Subject(target)}, nil
	}
	s, err := github(target)
	if err != nil && strings.Contains(target, "://") && !strings.Contains(target, "github.com/") {
		// A clicked link of an integration whose provider is not registered.
		return nil, fmt.Errorf("no provider claims %s; %s", target, providerNames(cfg))
	}
	if err != nil {
		return nil, err
	}
	return []core.Subject{s}, nil
}

// subjectOf resolves a --subject flag, which names one subject. Without a
// lookup, owner/repo#N stays a PR, as core.ParseSubject reads it.
func subjectOf(ctx context.Context, cfg *config.Config, s string) (core.Subject, error) {
	subjects, err := resolveTarget(ctx, cfg, s, func(s string) (core.Subject, error) { return core.ParseSubject(s) })
	if err != nil {
		return "", err
	}
	if len(subjects) != 1 {
		return "", fmt.Errorf("%s names %d subjects; --subject takes one", s, len(subjects))
	}
	return subjects[0], nil
}

func providerNames(cfg *config.Config) string {
	if len(cfg.Provided) == 0 {
		return "no provider is registered under providers: in the config"
	}
	names := []string{"github (built in)"}
	for _, d := range cfg.Provided {
		names = append(names, d.Name)
	}
	return "providers: " + strings.Join(names, ", ")
}
