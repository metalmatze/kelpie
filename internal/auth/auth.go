// Package auth keeps kelpie's own credential per integration in the system
// keychain: macOS Keychain through security, Secret Service on Linux through
// secret-tool. Both run under launchd and systemd, which pass no shell
// environment, and keep the secret out of config.yaml. What a credential is
// for and how to check it comes from the provider that names it; the
// keychain stays in kelpie, so no provider has to store a secret itself.
//
// The secret never goes into a process's argv, where ps and kelpie's logs
// would show it: it goes in on stdin and comes back on stdout.
package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// service is the keychain service of every kelpie item; the account is the
// name of the credential, as the provider that needs it names it.
const service = "kelpie"

// ErrNotStored is a missing credential, as opposed to a keychain that failed.
var ErrNotStored = errors.New("not stored")

// Get returns the credential of integration, or ErrNotStored.
func Get(ctx context.Context, integration string) (string, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-a", integration, "-w")
	case "linux":
		cmd = exec.CommandContext(ctx, "secret-tool", "lookup", "service", service, "account", integration)
	default:
		return "", unsupported()
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if notFound(err, stderr.String()) {
			return "", ErrNotStored
		}
		return "", keychainErr(cmd, err, stderr.String())
	}
	token := strings.TrimRight(stdout.String(), "\r\n")
	if token == "" {
		return "", ErrNotStored
	}
	// The item can be edited outside kelpie, bypassing Set's check.
	if err := checkToken(token); err != nil {
		return "", fmt.Errorf("the stored %s credential is malformed: %v; run kelpie auth login %s", integration, err, integration)
	}
	return token, nil
}

// Set stores token for integration, replacing any earlier one.
func Set(ctx context.Context, integration, token string) error {
	if err := checkToken(token); err != nil {
		return fmt.Errorf("the credential is malformed: %w; paste only the key itself", err)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		// security add-generic-password only takes the password as an
		// argument, or prompts for it on /dev/tty, which a pipe does not
		// reach. security -i reads the same command from stdin instead.
		// checkToken leaves nothing that its tokenizer would split or unquote.
		cmd = exec.CommandContext(ctx, "security", "-i")
		cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -l %s -w %s\n", service, integration, service+"-"+integration, token))
	case "linux":
		cmd = exec.CommandContext(ctx, "secret-tool", "store", "--label", "kelpie "+integration, "service", service, "account", integration)
		cmd.Stdin = strings.NewReader(token)
	default:
		return unsupported()
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return keychainErr(cmd, err, stderr.String())
	}
	return nil
}

// Delete removes the credential of integration, or returns ErrNotStored.
func Delete(ctx context.Context, integration string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "security", "delete-generic-password", "-s", service, "-a", integration)
	case "linux":
		// secret-tool clear succeeds when nothing matches, and logout should
		// say that nothing was stored.
		if _, err := Get(ctx, integration); err != nil {
			return err
		}
		cmd = exec.CommandContext(ctx, "secret-tool", "clear", "service", service, "account", integration)
	default:
		return unsupported()
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = nil, &stderr
	if err := cmd.Run(); err != nil {
		if notFound(err, stderr.String()) {
			return ErrNotStored
		}
		return keychainErr(cmd, err, stderr.String())
	}
	return nil
}

// checkToken accepts printable ASCII without spaces, quotes or backslashes.
// API keys and tokens of the integrations kelpie knows look like that, and
// so the token needs no quoting on a security -i line nor in the curl config
// line a source renders it into, where a quote would end the value early.
func checkToken(token string) error {
	if token == "" {
		return errors.New("empty credential")
	}
	for _, r := range token {
		if r <= ' ' || r > '~' || r == '"' || r == '\'' || r == '\\' {
			return errors.New("it has a space, quote, backslash or non-ASCII character")
		}
	}
	return nil
}

// notFound tells a missing item from a failing keychain: security exits 44
// (errSecItemNotFound), secret-tool exits 1 and prints nothing.
func notFound(err error, stderr string) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	switch runtime.GOOS {
	case "darwin":
		return ee.ExitCode() == 44
	case "linux":
		return ee.ExitCode() == 1 && strings.TrimSpace(stderr) == ""
	}
	return false
}

func keychainErr(cmd *exec.Cmd, err error, stderr string) error {
	if s := strings.TrimSpace(stderr); s != "" {
		return fmt.Errorf("%s: %w: %s", cmd.Args[0], err, s)
	}
	return fmt.Errorf("%s: %w", cmd.Args[0], err)
}

func unsupported() error {
	return fmt.Errorf("no keychain support on %s; kelpie stores credentials with security on macOS and secret-tool on Linux", runtime.GOOS)
}
