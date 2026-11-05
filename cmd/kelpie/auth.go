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
	token, err := readSecret(in.Prompt)
	if err != nil {
		return err
	}
	if err := auth.Set(ctx, in.Name, token); err != nil {
		return err
	}
	fmt.Printf("stored the %s credential\n", in.Name)
	// Stored first: a network hiccup should not lose the paste.
	if who, err := in.Verify(ctx, token); err != nil {
		fmt.Printf("but %s rejected it: %v\n", in.Name, err)
	} else {
		fmt.Printf("it works, as %s\n", who)
	}
	return nil
}

// readSecret reads a line without echo from a terminal, or all of stdin
// otherwise, so the secret never has to be an argument or a variable that a
// shell history keeps.
func readSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprintf(os.Stderr, "%s: ", prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	b, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if b = strings.TrimSpace(b); b == "" {
		return "", errors.New("no credential on stdin")
	}
	return b, nil
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
