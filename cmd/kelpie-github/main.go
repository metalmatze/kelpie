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
	Targets []string `arg:"" optional:"" help:"Pull request, issue, discussion, workflow run or releases URL, owner/repo#number, owner/repo, an org, or a github.com URL of a repo or org. None: the notifications of the current directory's repo."`
}

// Run asks GitHub what parse cannot tell from the text: whether
// owner/repo#123 is a PR, an issue or a discussion, and whether an org
// exists, since the notifications of one that does not are just always
// empty. kelpie runs this in the shell's own directory, where gh repo view
// finds the repo.
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
		s, err := parse(t, func(owner, name, number string) (kind, error) {
			// The issues endpoint serves PRs too, with a pull_request key, and
			// 404s for a discussion. Asking it first keeps PRs and issues, by
			// far the most common, at one call.
			out, err := gh(ctx, "api", fmt.Sprintf("repos/%s/%s/issues/%s", owner, name, number), "--jq", ".pull_request != null")
			switch {
			case err == nil && out == "true":
				return pullRequest, nil
			case err == nil:
				return issue, nil
			case !strings.Contains(err.Error(), "(HTTP 404)"):
				return 0, err
			}
			if _, err := gh(ctx, "api", "graphql", "-F", "owner="+owner, "-F", "name="+name, "-F", "number="+number,
				"-f", "query=query($owner: String!, $name: String!, $number: Int!) { repository(owner: $owner, name: $name) { discussion(number: $number) { number } } }"); err != nil {
				return 0, fmt.Errorf("none of them: %w", err)
			}
			return discussion, nil
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
	// Any comment of a discussion too (#discussioncomment-1). The list,
	// /discussions alone, falls through to the repo.
	discussionURL = regexp.MustCompile(`^https?://github\.com/([\w.-]+)/([\w.-]+)/discussions/(\d+)`)
	// Any page of a run: the run itself, /job/<id> or /attempts/<n>.
	runURL   = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/actions/runs/(\d+)`)
	numShort = regexp.MustCompile(`^([\w.-]+)/([\w.-]+)#(\d+)$`)

	releasesURL = regexp.MustCompile(`^https?://github\.com/([\w.-]+)/([\w.-]+)/releases(?:[/?#].*)?$`)

	ownerURL   = regexp.MustCompile(`^https?://github\.com/([\w.-]+)(?:/([\w.-]+))?(?:[/?#].*)?$`)
	ownerShort = regexp.MustCompile(`^([\w.-]+)(?:/([\w.-]+))?$`)
)

// kind is what a number of a repo is. GitHub numbers pull requests, issues
// and discussions in one sequence, so only GitHub knows.
type kind int

const (
	pullRequest kind = iota
	issue
	discussion
)

// prefix is the subject prefix of each kind.
var prefix = [...]string{pullRequest: "github", issue: "github-issue", discussion: "github-discussion"}

// kindOf asks GitHub what number of owner/name is.
type kindOf func(owner, name, number string) (kind, error)

// parse accepts what a person or agent would paste about GitHub. A pull
// request URL (any sub-page, like /files or #discussion_r1) is the PR:
// github:owner/repo#123. An issue URL is the issue: github-issue:owner/repo#7.
// A discussion URL is the discussion: github-discussion:owner/repo#9. A
// workflow run URL (any of its pages) is the run:
// github-run:owner/repo/42043979918. Any releases page of a repo is its
// releases: github-release:owner/repo. owner/repo#123 is whichever kindOf
// says, a PR when kindOf is nil. A repo (owner/repo), an org, or a github.com
// URL of either is its notifications: github-inbox:owner/repo or
// github-inbox:owner. Other repo pages (/actions, /tree/...) still mean the
// repo.
//
// Issues and discussions get a prefix of their own rather than sharing
// github: with PRs: github-pr runs for every github: subject, and PR watches
// saved as github: must keep working.
func parse(s string, kindOf kindOf) (string, error) {
	s = strings.TrimSpace(s)
	if m := prURL.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("github:%s/%s#%s", m[1], m[2], m[3]), nil
	}
	if m := issueURL.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("github-issue:%s/%s#%s", m[1], m[2], m[3]), nil
	}
	if m := discussionURL.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("github-discussion:%s/%s#%s", m[1], m[2], m[3]), nil
	}
	if m := runURL.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("github-run:%s/%s/%s", m[1], m[2], m[3]), nil
	}
	if m := releasesURL.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("github-release:%s/%s", m[1], m[2]), nil
	}
	if m := numShort.FindStringSubmatch(s); m != nil {
		k := pullRequest
		if kindOf != nil {
			var err error
			if k, err = kindOf(m[1], m[2], m[3]); err != nil {
				return "", fmt.Errorf("is %s a pull request, an issue or a discussion? %w", s, err)
			}
		}
		return fmt.Sprintf("%s:%s/%s#%s", prefix[k], m[1], m[2], m[3]), nil
	}
	m := ownerURL.FindStringSubmatch(s)
	if m == nil {
		m = ownerShort.FindStringSubmatch(s)
	}
	if m == nil {
		return "", fmt.Errorf("cannot parse %q: want a pull request, issue, discussion, workflow run or releases URL, owner/repo#number, owner/repo or an org", s)
	}
	if m[2] == "" {
		return "github-inbox:" + m[1], nil
	}
	return "github-inbox:" + m[1] + "/" + m[2], nil
}
