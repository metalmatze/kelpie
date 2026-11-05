package adapter

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/textproto"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metalmatze/kelpie/internal/auth"
	"github.com/metalmatze/kelpie/internal/config"
	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/diff"
)

// CLI runs a command every interval and maps its JSON stdout to observations.
type CLI struct {
	source config.Source
	mapper *diff.Mapper
	// credential is auth.Get, swapped out by tests.
	credential func(ctx context.Context, integration string) (string, error)

	mu       sync.Mutex
	watching map[core.Subject]bool
}

func New(s config.Source) (*CLI, error) {
	if s.Adapter != "cli" {
		return nil, fmt.Errorf("source %s: adapter %q is not implemented yet", s.Name, s.Adapter)
	}
	if len(s.Run) == 0 {
		return nil, fmt.Errorf("source %s: run is required", s.Name)
	}
	if s.Conditional && (len(s.Run) < 3 || s.Run[0] != "gh" || s.Run[1] != "api" || slices.Contains(s.Run, "--paginate")) {
		return nil, fmt.Errorf("source %s: conditional needs run: [gh, api, <endpoint>, ...] without --paginate", s.Name)
	}
	if a := s.Auth; a != nil {
		switch _, known := auth.Lookup(a.Name); {
		case a.Name == "":
			return nil, fmt.Errorf("source %s: auth needs a name, one of %s", s.Name, strings.Join(auth.Names(), ", "))
		case !known:
			return nil, fmt.Errorf("source %s: auth %q is not an integration kelpie holds credentials for; one of %s", s.Name, a.Name, strings.Join(auth.Names(), ", "))
		case !strings.Contains(a.Stdin, "{token}"):
			return nil, fmt.Errorf("source %s: auth stdin needs {token}, or the credential never reaches the command", s.Name)
		case s.Stdin != "":
			return nil, fmt.Errorf("source %s: auth stdin replaces stdin; set only one", s.Name)
		}
	}
	m, err := diff.NewMapper(s.Map, s.Slurp)
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", s.Name, err)
	}
	return &CLI{source: s, mapper: m, credential: auth.Get, watching: map[core.Subject]bool{}}, nil
}

func (c *CLI) Run(ctx context.Context, observe func([]core.Observation), _ func(core.Event)) error {
	c.loop(ctx, nil, observe, func([]core.Observation, error) bool { return true })
	return ctx.Err()
}

// Watch starts a for_each loop for parent unless one is already running. The
// loop ends when keep, called after every poll, returns false.
func (c *CLI) Watch(ctx context.Context, parent core.Observation, observe func([]core.Observation), keep func([]core.Observation, error) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.watching[parent.Subject] {
		return
	}
	c.watching[parent.Subject] = true
	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.watching, parent.Subject)
			c.mu.Unlock()
		}()
		log := slog.With("source", c.source.Name, "subject", parent.Subject)
		log.Debug("watch started")
		defer log.Debug("watch stopped")
		c.loop(ctx, &parent, observe, keep)
	}()
}

// loop polls until ctx ends or keep returns false. A conditional source sends
// the Last-Modified of its last observed response along; on a 304 nothing is
// observed and keep sees the previous observations, since nothing changed.
func (c *CLI) loop(ctx context.Context, parent *core.Observation, observe func([]core.Observation), keep func([]core.Observation, error) bool) {
	log := slog.With("source", c.source.Name)
	if parent != nil {
		log = log.With("subject", parent.Subject)
	}
	var (
		last  []core.Observation
		since string
	)
	for {
		p, err := c.poll(ctx, parent, since)
		if ctx.Err() != nil {
			return
		}
		obs := p.obs
		switch {
		case err != nil:
			log.Warn("poll failed", "err", err)
		case p.notModified:
			log.Debug("not modified", "since", since)
			obs = last
		default:
			last = obs
			observe(obs)
			// Only once observed: a response that failed to map must come
			// back on the next poll instead of as a 304.
			since = cmp.Or(p.lastModified, since)
		}
		if !keep(obs, err) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(max(c.source.Every.Duration, p.pollInterval)):
		}
	}
}

// Once runs the command a single time and returns its raw output too, for
// kelpie source test and for recording fixtures.
func (c *CLI) Once(ctx context.Context, parent *core.Observation) ([]byte, []core.Observation, error) {
	p, err := c.poll(ctx, parent, "")
	return p.raw, p.obs, err
}

type poll struct {
	raw []byte // the response body, without headers for a conditional source
	obs []core.Observation

	// Conditional sources only.
	notModified  bool
	lastModified string
	pollInterval time.Duration
}

// poll runs the command once. For a conditional source, since is the
// Last-Modified to send as If-Modified-Since; empty sends none.
func (c *CLI) poll(ctx context.Context, parent *core.Observation, since string) (poll, error) {
	args := c.source.Run
	if parent != nil {
		var err error
		if args, err = expand(args, parent.Fields); err != nil {
			return poll{}, err
		}
	}
	if c.source.Conditional {
		args = conditional(args, since)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	if c.source.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.source.Stdin)
	}
	if a := c.source.Auth; a != nil {
		// Read on every poll, so a login or a rotated key applies without
		// restarting the daemon.
		token, err := c.credential(ctx, a.Name)
		if errors.Is(err, auth.ErrNotStored) {
			return poll{}, fmt.Errorf("no %s credential; run kelpie auth login %s", a.Name, a.Name)
		} else if err != nil {
			return poll{}, fmt.Errorf("%s credential: %w", a.Name, err)
		}
		cmd.Stdin = strings.NewReader(strings.ReplaceAll(a.Stdin, "{token}", token))
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	p := poll{raw: stdout.Bytes()}
	if c.source.Conditional {
		// gh exits 1 on a 304, so the status line decides, not the exit code.
		resp, err := parseInclude(stdout.Bytes())
		if err == nil {
			p.raw = resp.body
			if s, err := strconv.Atoi(resp.header.Get("X-Poll-Interval")); err == nil {
				p.pollInterval = time.Duration(s) * time.Second
			}
			if resp.status == http.StatusNotModified {
				p.notModified = true
				return p, nil
			}
			if runErr == nil {
				p.lastModified = resp.header.Get("Last-Modified")
			}
		} else if runErr == nil {
			return p, err
		}
	}
	if runErr != nil {
		return p, fmt.Errorf("%s: %w: %s", args[0], runErr, strings.TrimSpace(stderr.String()))
	}
	obs, err := c.mapper.Map(p.raw, parent)
	if err != nil {
		return p, err
	}
	p.obs = obs
	return p, graphqlErrors(p.raw)
}

// conditional asks gh api for the status line and headers with --include,
// and adds If-Modified-Since once a Last-Modified is known.
func conditional(args []string, since string) []string {
	out := []string{args[0], args[1], "--include"}
	if since != "" {
		out = append(out, "-H", "If-Modified-Since: "+since)
	}
	return append(out, args[2:]...)
}

type response struct {
	status int
	header textproto.MIMEHeader
	body   []byte
}

// parseInclude splits gh api --include output: a status line such as
// "HTTP/2.0 304 Not Modified", the headers, a blank line, then the body.
func parseInclude(out []byte) (response, error) {
	br := bufio.NewReader(bytes.NewReader(out))
	tp := textproto.NewReader(br)
	line, err := tp.ReadLine()
	if err != nil || !strings.HasPrefix(line, "HTTP/") {
		return response{}, fmt.Errorf("gh api --include printed no status line: %q", truncate(line, 80))
	}
	_, code, _ := strings.Cut(line, " ")
	code, _, _ = strings.Cut(code, " ")
	status, err := strconv.Atoi(code)
	if err != nil {
		return response{}, fmt.Errorf("gh api --include status line %q: %w", line, err)
	}
	header, err := tp.ReadMIMEHeader()
	if err != nil && !errors.Is(err, io.EOF) {
		return response{}, fmt.Errorf("gh api --include headers: %w", err)
	}
	body, err := io.ReadAll(br)
	if err != nil {
		return response{}, err
	}
	return response{status: status, header: header, body: body}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var placeholder = regexp.MustCompile(`\{(\w+)\}`)

// expand fills {field} from the parent's fields. Each argument stays one argv
// element and no shell runs, so a value cannot inject flags or commands.
func expand(args []string, fields map[string]any) ([]string, error) {
	out := make([]string, len(args))
	var missing []string
	for i, a := range args {
		out[i] = placeholder.ReplaceAllStringFunc(a, func(m string) string {
			key := m[1 : len(m)-1]
			v, ok := fields[key]
			if !ok {
				missing = append(missing, key)
				return m
			}
			switch x := v.(type) {
			case string:
				return x
			case float64:
				return strconv.FormatFloat(x, 'f', -1, 64)
			}
			b, _ := json.Marshal(v)
			return string(b)
		})
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("parent has no field %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// graphqlErrors catches partial GraphQL responses: gh exits 0 when data and
// errors come back together, and a silently missing PR would look closed.
func graphqlErrors(raw []byte) error {
	var resp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &resp) != nil || len(resp.Errors) == 0 {
		return nil
	}
	msgs := make([]string, len(resp.Errors))
	for i, e := range resp.Errors {
		msgs[i] = e.Message
	}
	return errors.New("graphql: " + strings.Join(msgs, "; "))
}
