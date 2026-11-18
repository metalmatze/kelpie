// Command kelpie-github is kelpie's GitHub integration, registered in
// kelpie's config as providers: { github: { command: [kelpie-github] } }.
//
// It imports nothing from kelpie's internal packages: everything kelpie needs
// crosses as YAML and text on stdout, so it can move to its own repo as is.
// kelpie polls GitHub itself, with gh and the sources describe prints; this
// binary only runs when kelpie starts and at kelpie watch.
package main

import (
	"context"
	_ "embed"
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
	Describe DescribeCmd `cmd:"" help:"Print the sources, defs and target patterns kelpie needs, as YAML."`
	Subject  SubjectCmd  `cmd:"" help:"Print the subject of each GitHub URL, owner/repo#number, owner/repo or org, one per line."`
	Me       MeCmd       `cmd:"" help:"Print the login gh acts as."`
}

func main() {
	var cli CLI
	kctx := kong.Parse(&cli,
		kong.Name("kelpie-github"),
		kong.Description("GitHub integration for kelpie. kelpie runs it; people rarely need to."),
		kong.BindTo(context.Background(), (*context.Context)(nil)),
	)
	// kelpie shows stderr as the reason, so no "kelpie-github: error:" prefix.
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
	Targets []string `arg:"" optional:"" help:"Pull request, issue, workflow run or releases URL, owner/repo#number, owner/repo, an org, or a github.com URL of a repo or org. None: the notifications of the current directory's repo."`
}

// Run asks GitHub what parse cannot tell from the text: whether
// owner/repo#123 is a PR or an issue, and whether an org exists, since the
// notifications of one that does not are just always empty. kelpie runs this
// in the shell's own directory, where gh repo view finds the repo.
func (c *SubjectCmd) Run(ctx context.Context) error {
	targets := c.Targets
	if len(targets) == 0 {
		out, err := exec.CommandContext(ctx, "gh", "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner").Output()
		if err != nil {
			return fmt.Errorf("no GitHub repo in the current directory (gh repo view: %w); pass a pull request, owner/repo or an org", err)
		}
		targets = []string{strings.TrimSpace(string(out))}
	}
	for _, t := range targets {
		s, err := parse(t, func(owner, name, number string) (bool, error) {
			// The issues endpoint serves PRs too, with a pull_request key.
			out, err := gh(ctx, "api", fmt.Sprintf("repos/%s/%s/issues/%s", owner, name, number), "--jq", ".pull_request != null")
			return out == "true", err
		})
		if err != nil {
			return err
		}
		if owner, ok := strings.CutPrefix(s, "github-inbox:"); ok && !strings.Contains(owner, "/") {
			if _, err := gh(ctx, "api", "users/"+owner, "--silent"); err != nil {
				return fmt.Errorf("no GitHub org or user %s (%w)", owner, err)
			}
		}
		fmt.Println(s)
	}
	return nil
}

type MeCmd struct{}

func (MeCmd) Run(ctx context.Context) error {
	login, err := gh(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return err
	}
	fmt.Println(login)
	return nil
}

// gh returns what gh printed, or its own complaint as the error.
func gh(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", args...).Output()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return "", fmt.Errorf("gh %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
	} else if err != nil {
		return "", fmt.Errorf("gh %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

var (
	prURL    = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/pull/(\d+)`)
	issueURL = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/issues/(\d+)`)
	// Any page of a run: the run itself, /job/<id> or /attempts/<n>.
	runURL   = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/actions/runs/(\d+)`)
	numShort = regexp.MustCompile(`^([\w.-]+)/([\w.-]+)#(\d+)$`)

	// A single discussion has no subject of its own yet. Without this ownerURL
	// would take its URL for the repo and start a repo-wide watch the person
	// never asked for.
	discussionURL = regexp.MustCompile(`^https?://github\.com/([\w.-]+)/([\w.-]+)/discussions/\d+`)

	releasesURL = regexp.MustCompile(`^https?://github\.com/([\w.-]+)/([\w.-]+)/releases(?:[/?#].*)?$`)

	ownerURL   = regexp.MustCompile(`^https?://github\.com/([\w.-]+)(?:/([\w.-]+))?(?:[/?#].*)?$`)
	ownerShort = regexp.MustCompile(`^([\w.-]+)(?:/([\w.-]+))?$`)
)

// isPR tells whether number is a pull request rather than an issue of
// owner/name. GitHub numbers both in one sequence, so only GitHub knows.
type isPR func(owner, name, number string) (bool, error)

// parse accepts what a person or agent would paste about GitHub. A pull
// request URL (any sub-page, like /files or #discussion_r1) is the PR:
// github:owner/repo#123. An issue URL is the issue: github-issue:owner/repo#7.
// A workflow run URL (any of its pages) is the run:
// github-run:owner/repo/42043979918. Any releases page of a repo is its
// releases: github-release:owner/repo. owner/repo#123 is whichever pr says, a
// PR when pr is nil. A repo (owner/repo), an org, or a github.com URL of
// either is its notifications: github-inbox:owner/repo or github-inbox:owner.
// Other repo pages (/actions, /tree/...) still mean the repo; a single
// discussion is an error.
//
// Issues get a prefix of their own rather than sharing github: with PRs:
// github-pr runs for every github: subject, and PR watches saved as github:
// must keep working.
func parse(s string, pr isPR) (string, error) {
	s = strings.TrimSpace(s)
	if m := prURL.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("github:%s/%s#%s", m[1], m[2], m[3]), nil
	}
	if m := issueURL.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("github-issue:%s/%s#%s", m[1], m[2], m[3]), nil
	}
	if m := runURL.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("github-run:%s/%s/%s", m[1], m[2], m[3]), nil
	}
	if m := releasesURL.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("github-release:%s/%s", m[1], m[2]), nil
	}
	if m := numShort.FindStringSubmatch(s); m != nil {
		isPR := true
		if pr != nil {
			var err error
			if isPR, err = pr(m[1], m[2], m[3]); err != nil {
				return "", fmt.Errorf("is %s a pull request or an issue? %w", s, err)
			}
		}
		if isPR {
			return fmt.Sprintf("github:%s/%s#%s", m[1], m[2], m[3]), nil
		}
		return fmt.Sprintf("github-issue:%s/%s#%s", m[1], m[2], m[3]), nil
	}
	if m := discussionURL.FindStringSubmatch(s); m != nil {
		return "", fmt.Errorf("kelpie can't watch a single discussion yet; %q covers the repo's notifications, discussions included", m[1]+"/"+m[2])
	}
	m := ownerURL.FindStringSubmatch(s)
	if m == nil {
		m = ownerShort.FindStringSubmatch(s)
	}
	if m == nil {
		return "", fmt.Errorf("cannot parse %q: want a pull request, issue, workflow run or releases URL, owner/repo#number, owner/repo or an org", s)
	}
	if m[2] == "" {
		return "github-inbox:" + m[1], nil
	}
	return "github-inbox:" + m[1] + "/" + m[2], nil
}
