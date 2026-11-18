package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/metalmatze/kelpie/internal/auth"
	"github.com/metalmatze/kelpie/internal/config"
	"github.com/metalmatze/kelpie/internal/provider"
)

type AuthCmd struct {
	Login  AuthLoginCmd  `cmd:"" help:"Store the credential of an integration in the system keychain."`
	Status AuthStatusCmd `cmd:"" help:"Tell whether an integration has a credential, and whether it still works."`
	Logout AuthLogoutCmd `cmd:"" help:"Remove the credential of an integration from the system keychain."`
}

type AuthLoginCmd struct {
	Integration string `arg:"" help:"Credential, as the provider that needs it names it, like linear; kelpie auth status lists them."`
}

func (c *AuthLoginCmd) Run(ctx context.Context, g *Globals) error {
	in, p, err := credential(ctx, g, c.Integration)
	if err != nil {
		return err
	}
	token, err := readSecret(ctx, in.Prompt)
	if err == nil {
		err = auth.Set(ctx, in.Name, token)
	}
	// main swallows context.Canceled to exit quietly on Ctrl-C, which here
	// would read as success with nothing stored.
	if ctx.Err() != nil {
		return fmt.Errorf("cancelled; no %s credential stored", in.Name)
	}
	if err != nil {
		return err
	}
	fmt.Printf("stored the %s credential\n", in.Name)
	// Stored first: a network hiccup should not lose the paste. Failing still
	// keeps login && something from going on with a key that does not work.
	who, err := provider.Who(ctx, p.Command, in.Verify, token)
	if err != nil {
		return fmt.Errorf("stored, but checking it with %s failed: %v; check again with kelpie auth status %s", in.Name, err, in.Name)
	}
	fmt.Printf("it works, as %s\n", who)
	return nil
}

// readSecret reads a line without echo from a terminal, or from stdin
// otherwise, so the secret never has to be an argument or a variable that a
// shell history keeps. Ctrl-C only cancels ctx, since main traps SIGINT, and
// term.ReadPassword does not watch ctx: the read runs aside so a cancel can
// restore echo and return instead of hanging at the prompt.
func readSecret(ctx context.Context, prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	tty := term.IsTerminal(fd)
	var state *term.State
	if tty {
		var err error
		if state, err = term.GetState(fd); err != nil {
			return "", err
		}
		fmt.Fprintf(os.Stderr, "%s: ", prompt)
	}
	type result struct {
		s   string
		err error
	}
	done := make(chan result, 1)
	go func() {
		if tty {
			b, err := term.ReadPassword(fd)
			done <- result{string(b), err}
			return
		}
		s, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if errors.Is(err, io.EOF) {
			err = nil
		}
		done <- result{s, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		if tty {
			term.Restore(fd, state)
		}
		r.err = ctx.Err()
	}
	if tty {
		fmt.Fprintln(os.Stderr)
	}
	if r.err != nil {
		return "", r.err
	}
	s := strings.TrimSpace(r.s)
	if s == "" {
		return "", errors.New("no credential given")
	}
	return s, nil
}

type AuthStatusCmd struct {
	Integration string `arg:"" optional:"" help:"Credential, as the provider that needs it names it. Default: the credentials of every registered provider."`
}

// Run fails when any credential asked about is missing or rejected, so a
// script can test the exit code.
func (c *AuthStatusCmd) Run(ctx context.Context, g *Globals) error {
	cfg, err := loadProviders(ctx, g)
	if err != nil {
		return err
	}
	ins := cfg.Credentials()
	if c.Integration != "" {
		in, _, err := lookupCredential(cfg, c.Integration)
		if err != nil {
			return err
		}
		ins = []config.Credential{in}
	}
	if len(ins) == 0 {
		fmt.Println("no registered provider needs a credential")
		return nil
	}
	failed := 0
	for _, in := range ins {
		token, err := auth.Get(ctx, in.Name)
		switch {
		case errors.Is(err, auth.ErrNotStored):
			fmt.Printf("%s: not stored; run kelpie auth login %s\n", in.Name, in.Name)
			failed++
			continue
		case err != nil:
			fmt.Printf("%s: %v\n", in.Name, err)
			failed++
			continue
		}
		_, p, _ := cfg.Credential(in.Name)
		who, err := provider.Who(ctx, p.Command, in.Verify, token)
		if err != nil {
			fmt.Printf("%s: stored, but rejected: %v\n", in.Name, err)
			failed++
			continue
		}
		fmt.Printf("%s: stored, works as %s\n", in.Name, who)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d integrations have no working credential", failed, len(ins))
	}
	return nil
}

type AuthLogoutCmd struct {
	Integration string `arg:"" help:"Credential, as the provider that needs it names it."`
}

// Run needs no provider: a credential whose provider was unregistered must
// still be removable.
func (c *AuthLogoutCmd) Run(ctx context.Context) error {
	if err := auth.Delete(ctx, c.Integration); err != nil {
		if errors.Is(err, auth.ErrNotStored) {
			fmt.Printf("%s: nothing stored\n", c.Integration)
			return nil
		}
		return err
	}
	fmt.Printf("removed the %s credential\n", c.Integration)
	return nil
}

// credential loads the providers and finds the one holding name.
func credential(ctx context.Context, g *Globals, name string) (config.Credential, config.Described, error) {
	cfg, err := loadProviders(ctx, g)
	if err != nil {
		return config.Credential{}, config.Described{}, err
	}
	return lookupCredential(cfg, name)
}

func lookupCredential(cfg *config.Config, name string) (config.Credential, config.Described, error) {
	in, p, ok := cfg.Credential(name)
	if !ok {
		var names []string
		for _, c := range cfg.Credentials() {
			names = append(names, c.Name)
		}
		if len(names) == 0 {
			return in, p, fmt.Errorf("no credential %q: no registered provider needs one; register its provider under providers: in the config", name)
		}
		return in, p, fmt.Errorf("no credential %q; registered providers need %s", name, strings.Join(names, ", "))
	}
	return in, p, nil
}
