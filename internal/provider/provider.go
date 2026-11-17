// Package provider runs the integration binaries registered under
// providers:. kelpie talks to them only through argv, stdin and stdout, so a
// provider can live in its own repo and language without sharing Go types.
package provider

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/metalmatze/kelpie/internal/core"
)

// Timeouts per subcommand. describe runs at every start of the daemon and of
// commands that route targets, so a hanging provider must not hold them up;
// subject may call an API (gh api) and gets longer. Vars so tests need not
// wait for them.
var (
	DescribeTimeout = 10 * time.Second
	SubjectTimeout  = 30 * time.Second
	VerifyTimeout   = 30 * time.Second
)

// Describe runs `<command> describe` and returns what it printed.
func Describe(ctx context.Context, command []string) ([]byte, error) {
	return run(ctx, DescribeTimeout, command, nil, "describe")
}

// Subject runs `<command> subject <args>` in the caller's directory and
// environment, which is where it can find the repo `gh repo view` needs.
func Subject(ctx context.Context, command []string, args []string) ([]core.Subject, error) {
	out, err := run(ctx, SubjectTimeout, command, nil, append([]string{"subject"}, args...)...)
	if err != nil {
		return nil, err
	}
	var subjects []core.Subject
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !core.IsSubject(line) {
			return nil, fmt.Errorf("%s subject printed %q, which is not a subject like linear:ENG-123", filepath.Base(command[0]), line)
		}
		subjects = append(subjects, core.Subject(line))
	}
	if len(subjects) == 0 {
		return nil, fmt.Errorf("%s subject printed no subject", filepath.Base(command[0]))
	}
	return subjects, nil
}

// Verify runs `<command> <verify...>` with token on stdin and returns who it
// belongs to, the first line printed. The token is redacted from errors, in
// case the provider echoes what the vendor rejected.
func Verify(ctx context.Context, command, verify []string, token string) (string, error) {
	out, err := run(ctx, VerifyTimeout, command, strings.NewReader(token+"\n"), verify...)
	if err != nil {
		return "", redact(err, token)
	}
	who, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if who == "" {
		return "", fmt.Errorf("%s printed nobody", strings.Join(append([]string{filepath.Base(command[0])}, verify...), " "))
	}
	return strings.ReplaceAll(who, token, "[redacted]"), nil
}

// run starts command with args appended. Without stdin it reads EOF, so a
// provider cannot prompt and hang: it says what it needs on stderr and exits.
func run(ctx context.Context, timeout time.Duration, command []string, stdin io.Reader, args ...string) ([]byte, error) {
	if len(command) == 0 {
		return nil, errors.New("command is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append(command[1:len(command):len(command)], args...)
	cmd := exec.CommandContext(ctx, command[0], argv...)
	cmd.Stdin = stdin
	// A provider that forks a child keeping stdout open would otherwise hold
	// Wait past the timeout.
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	// The binary's base name: a registered command is often an absolute path
	// that would bury the reason.
	name := strings.Join(append([]string{filepath.Base(command[0])}, args[:min(1, len(args))]...), " ")
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return nil, fmt.Errorf("%s: no answer within %s", name, timeout)
	case err != nil:
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return nil, fmt.Errorf("%s: %s", name, s)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return stdout.Bytes(), nil
}

func redact(err error, token string) error {
	if token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "[redacted]"))
}
