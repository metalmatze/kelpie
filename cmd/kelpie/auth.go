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
)

type AuthCmd struct {
	Login  AuthLoginCmd  `cmd:"" help:"Store the credential of an integration in the system keychain."`
	Status AuthStatusCmd `cmd:"" help:"Tell whether an integration has a credential, and whether it still works."`
	Logout AuthLogoutCmd `cmd:"" help:"Remove the credential of an integration from the system keychain."`
}

type AuthLoginCmd struct {
	Integration string `arg:"" help:"Integration: ${integrations}."`
}

func (c *AuthLoginCmd) Run(ctx context.Context) error {
	in, err := integration(c.Integration)
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
	who, err := in.Verify(ctx, token)
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
	Integration string `arg:"" optional:"" help:"Integration: ${integrations}. Default: all of them."`
}

// Run fails when any integration asked about is missing or rejected, so a
// script can test the exit code.
func (c *AuthStatusCmd) Run(ctx context.Context) error {
	ins := auth.Integrations
	if c.Integration != "" {
		in, err := integration(c.Integration)
		if err != nil {
			return err
		}
		ins = []auth.Integration{in}
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
		who, err := in.Verify(ctx, token)
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
	Integration string `arg:"" help:"Integration: ${integrations}."`
}

func (c *AuthLogoutCmd) Run(ctx context.Context) error {
	in, err := integration(c.Integration)
	if err != nil {
		return err
	}
	if err := auth.Delete(ctx, in.Name); err != nil {
		if errors.Is(err, auth.ErrNotStored) {
			fmt.Printf("%s: nothing stored\n", in.Name)
			return nil
		}
		return err
	}
	fmt.Printf("removed the %s credential\n", in.Name)
	return nil
}

func integration(name string) (auth.Integration, error) {
	in, ok := auth.Lookup(name)
	if !ok {
		return auth.Integration{}, fmt.Errorf("no integration %q; kelpie holds credentials for %s", name, strings.Join(auth.Names(), ", "))
	}
	return in, nil
}
