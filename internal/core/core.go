package core

import (
	"context"
	"time"
)

// Subject is a stable URI for a watched thing.
// github:org/repo#421, github-issue:org/repo#17, linear:ENG-123, slack:C0123ABCD/1761811234.567890,
// kubernetes:kind-dev/dev-eng-123/deploy/api
type Subject string

// Observation is the current state of one subject, as a CLI or MCP tool reports it.
type Observation struct {
	Subject Subject        `json:"subject"`
	Fields  map[string]any `json:"fields"`
	URL     string         `json:"url,omitzero"`
	Links   []Subject      `json:"links,omitzero"`
	Fetch   []string       `json:"fetch,omitzero"` // the exact command an agent runs for details
	// Bind carries the keys binders match on (repo, head_repo, branch), kept
	// apart from Fields so binders don't depend on any one source's field names.
	Bind map[string]string `json:"bind,omitzero"`
}

// Event is a transition between two observations, or a native stream event.
type Event struct {
	ID, Kind, Actor, Summary, URL string
	Subject                       Subject
	At                            time.Time
	Links                         []Subject
	Fetch                         []string
	Bind                          map[string]string
	Urgent                        bool
	Terminal                      bool // merged, Done, resolved: unbind after delivery
	// Workspace targets the event at one workspace directly, bypassing the
	// binders. Reminders set it: they belong to the pane that set them,
	// whether or not their subject is watched.
	Workspace string
}

// Reminder prompts a workspace's agent with its own text when due, once or
// on a cron schedule.
type Reminder struct {
	ID        string    `json:"id"`
	Workspace string    `json:"workspace"`
	Text      string    `json:"text"`
	Subject   Subject   `json:"subject,omitzero"`
	Cron      string    `json:"cron,omitzero"` // empty: one-shot
	Due       time.Time `json:"due"`
	Created   time.Time `json:"created"`
}

type Binding struct {
	Subject   Subject
	Workspace string // stable workspace ID from the host
	Via       string // branch | link | kubernetes | explicit | confirmed
	Since     time.Time
}

// Adapter is one built-in way of reading a source: cli, cli-watch or mcp.
type Adapter interface {
	Run(ctx context.Context, observe func([]Observation), emit func(Event)) error
}
