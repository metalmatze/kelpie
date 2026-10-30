package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metalmatze/kelpie/internal/config"
	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/diff"
)

// CLI runs a command every interval and maps its JSON stdout to observations.
type CLI struct {
	source config.Source
	mapper *diff.Mapper

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
	m, err := diff.NewMapper(s.Map, s.Slurp)
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", s.Name, err)
	}
	return &CLI{source: s, mapper: m, watching: map[core.Subject]bool{}}, nil
}

func (c *CLI) Run(ctx context.Context, observe func([]core.Observation), _ func(core.Event)) error {
	log := slog.With("source", c.source.Name)
	for {
		if _, obs, err := c.Once(ctx, nil); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Warn("poll failed", "err", err)
		} else {
			observe(obs)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.source.Every.Duration):
		}
	}
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
		c.watch(ctx, parent, observe, keep)
	}()
}

func (c *CLI) watch(ctx context.Context, parent core.Observation, observe func([]core.Observation), keep func([]core.Observation, error) bool) {
	log := slog.With("source", c.source.Name, "subject", parent.Subject)
	log.Debug("watch started")
	defer log.Debug("watch stopped")
	for {
		_, obs, err := c.Once(ctx, &parent)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Warn("watch poll failed", "err", err)
		} else {
			observe(obs)
		}
		if !keep(obs, err) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.source.Every.Duration):
		}
	}
}

// Once runs the command a single time and returns its raw output too, for
// kelpie source test and for recording fixtures.
func (c *CLI) Once(ctx context.Context, parent *core.Observation) ([]byte, []core.Observation, error) {
	args := c.source.Run
	if parent != nil {
		var err error
		if args, err = expand(args, parent.Fields); err != nil {
			return nil, nil, err
		}
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	if c.source.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.source.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), nil, fmt.Errorf("%s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	obs, err := c.mapper.Map(stdout.Bytes(), parent)
	if err != nil {
		return stdout.Bytes(), nil, err
	}
	return stdout.Bytes(), obs, graphqlErrors(stdout.Bytes())
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
