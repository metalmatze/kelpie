// Command kelpie-linear is kelpie's Linear integration, registered in
// kelpie's config as providers: { linear: { command: [kelpie-linear] } }.
//
// It imports nothing from kelpie's internal packages: everything kelpie needs
// crosses as YAML and text on stdout, so it can move to its own repo as is.
// kelpie polls Linear itself, with the sources describe prints; this binary
// only runs when kelpie starts, at kelpie watch and at kelpie auth.
package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/alecthomas/kong"
)

//go:embed describe.yaml
var describe []byte

type CLI struct {
	Describe DescribeCmd `cmd:"" help:"Print the sources, credential, branch and target patterns kelpie needs, as YAML."`
	Subject  SubjectCmd  `cmd:"" help:"Print the subject of each Linear issue ID or URL, one per line."`
	Verify   VerifyCmd   `cmd:"" help:"Read a Linear API key on stdin and print whose it is."`
}

func main() {
	var cli CLI
	kctx := kong.Parse(&cli,
		kong.Name("kelpie-linear"),
		kong.Description("Linear integration for kelpie. kelpie runs it; people rarely need to."),
		kong.BindTo(context.Background(), (*context.Context)(nil)),
	)
	// kelpie shows stderr as the reason, so no "kelpie-linear: error:" prefix.
	if err := kctx.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type DescribeCmd struct{}

func (DescribeCmd) Run() error {
	_, err := os.Stdout.Write(describe)
	return err
}

type SubjectCmd struct {
	Targets []string `arg:"" help:"Issue ID like ENG-123, its subject linear:ENG-123, its URL, or inbox."`
}

func (c *SubjectCmd) Run() error {
	for _, t := range c.Targets {
		s, err := parse(t)
		if err != nil {
			return err
		}
		fmt.Println(s)
	}
	return nil
}

var (
	// linear.app/<workspace>/issue/ENG-123/<slug>; the slug follows the title
	// and may be missing or stale, the identifier never is. The targets in
	// describe.yaml match the same inputs.
	issueURL = regexp.MustCompile(`^https?://linear\.app/[^/]+/issue/([A-Za-z][A-Za-z0-9]*-\d+)(?:[/?#].*)?$`)
	issueID  = regexp.MustCompile(`^(?i:linear:)?([A-Za-z][A-Za-z0-9]*-\d+)$`)
	// The inbox is the key's own: linear.app/<workspace>/inbox, its subject
	// linear-inbox:me, or the word inbox, which only kelpie watch linear
	// passes on, since a bare word is not a target of its own.
	inboxURL = regexp.MustCompile(`^https?://linear\.app/[^/]+/inbox(?:[/?#].*)?$`)
	inboxID  = regexp.MustCompile(`^(?i:inbox|linear-inbox:me)$`)
)

// parse accepts an issue identifier (ENG-123), its subject (linear:ENG-123)
// or its URL. Linear matches identifiers in any case, but the subject is a
// map key for kelpie's watches and snapshots, so it is upper case.
func parse(s string) (string, error) {
	s = strings.TrimSpace(s)
	if inboxURL.MatchString(s) || inboxID.MatchString(s) {
		return "linear-inbox:me", nil
	}
	m := issueURL.FindStringSubmatch(s)
	if m == nil {
		m = issueID.FindStringSubmatch(s)
	}
	if m == nil {
		return "", fmt.Errorf("cannot parse %q: want a Linear issue ID like ENG-123, its URL, or inbox", s)
	}
	return "linear:" + strings.ToUpper(m[1]), nil
}

type VerifyCmd struct{}

// Run asks for the key's own user. A rejected key comes back as an HTTP 400
// whose GraphQL errors name the reason.
func (VerifyCmd) Run(ctx context.Context) error {
	token, err := bufio.NewReader(os.Stdin).ReadString('\n')
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("no API key on stdin (%v)", err)
	}
	who, err := viewer(ctx, token)
	if err != nil {
		return redact(err, token)
	}
	fmt.Println(who)
	return nil
}

func viewer(ctx context.Context, token string) (string, error) {
	// The key goes to curl as a config line on stdin, never as an argument
	// that ps would show. --fail-with-body keeps the response of a rejected
	// request, which says why; plain -f drops it.
	cmd := exec.CommandContext(ctx, "curl", "-sSK", "-", "--fail-with-body", "--max-time", "20",
		"-H", "Content-Type: application/json", "--data", `{"query":"{ viewer { name } }"}`, "https://api.linear.app/graphql")
	cmd.Stdin = strings.NewReader(`header = "Authorization: ` + token + "\"\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
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
	jsonErr := json.Unmarshal(stdout.Bytes(), &resp)
	switch {
	case len(resp.Errors) > 0:
		return "", fmt.Errorf("api.linear.app: %s", resp.Errors[0].Message)
	case runErr != nil:
		return "", fmt.Errorf("curl api.linear.app: %w: %s", runErr, strings.TrimSpace(stderr.String()))
	case jsonErr != nil:
		return "", fmt.Errorf("api.linear.app: %w", jsonErr)
	}
	return resp.Data.Viewer.Name, nil
}

// redact keeps the key out of an error built from what Linear sent back, in
// case it echoes the header it rejected.
func redact(err error, token string) error {
	if !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "[redacted]"))
}
