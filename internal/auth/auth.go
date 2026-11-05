// Package auth keeps kelpie's own credential per integration in the system
// keychain: macOS Keychain through security, Secret Service on Linux through
// secret-tool. Both run under launchd and systemd, which pass no shell
// environment, and keep the secret out of config.yaml.
//
// The secret never goes into a process's argv, where ps and kelpie's logs
// would show it: it goes in on stdin and comes back on stdout.
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"strings"
)

// service is the keychain service of every kelpie item; the account is the
// integration's name.
const service = "kelpie"

// ErrNotStored is a missing credential, as opposed to a keychain that failed.
var ErrNotStored = errors.New("not stored")

// Integration is a vendor without a CLI of its own, whose credential kelpie
// holds.
type Integration struct {
	Name string
	// Prompt tells what to paste on login.
	Prompt string
	// Stdin is what a source with the short form `auth: <name>` writes to
	// its command, {token} being the secret: a curl config line for the
	// vendor's usual header, since sources call vendor APIs with curl -K -.
	Stdin string
	// Verify calls the vendor with token and returns who it belongs to.
	Verify func(ctx context.Context, token string) (string, error)
}

var Integrations = []Integration{
	{
		Name:   "linear",
		Prompt: "Linear personal API key, created with Read permission only (Settings > Security & access > Personal API keys)",
		// An API key goes in as is; Bearer is only for OAuth tokens.
		Stdin:  `header = "Authorization: {token}"`,
		Verify: verifyLinear,
	},
}

func Lookup(name string) (Integration, bool) {
	i := slices.IndexFunc(Integrations, func(i Integration) bool { return i.Name == name })
	if i < 0 {
		return Integration{}, false
	}
	return Integrations[i], true
}

func Names() []string {
	names := make([]string, len(Integrations))
	for i, in := range Integrations {
		names[i] = in.Name
	}
	return names
}

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
	return token, nil
}

// Set stores token for integration, replacing any earlier one.
func Set(ctx context.Context, integration, token string) error {
	if err := checkToken(token); err != nil {
		return err
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
	// security -i exits 0 even when the command it read failed, so read the
	// item back to know it was stored.
	got, err := Get(ctx, integration)
	if err != nil {
		return fmt.Errorf("stored, but reading it back failed: %w", err)
	}
	if got != token {
		return errors.New("stored, but reading it back gave a different value")
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
			return errors.New("the credential has a space, quote, backslash or non-ASCII character; paste only the key itself")
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

// curl posts body to url with header on stdin as a curl config line, the way
// a source passes its credential, and returns the response.
func curl(ctx context.Context, header, url, body string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "curl", "-sSfK", "-", "-H", "Content-Type: application/json", "--data", body, url)
	cmd.Stdin = strings.NewReader(fmt.Sprintf("header = %q\n", header))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("curl %s: %w: %s", url, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// verifyLinear asks for the key's own user. An API key goes in the
// Authorization header as is; Bearer is only for OAuth tokens.
func verifyLinear(ctx context.Context, token string) (string, error) {
	out, err := curl(ctx, "Authorization: "+token, "https://api.linear.app/graphql", `{"query":"{ viewer { name } }"}`)
	if err != nil {
		return "", err
	}
	var resp struct {
		Data struct {
			Viewer struct {
				Name string `json:"name"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("api.linear.app: %w", err)
	}
	if len(resp.Errors) > 0 {
		return "", fmt.Errorf("api.linear.app: %s", resp.Errors[0].Message)
	}
	return resp.Data.Viewer.Name, nil
}
