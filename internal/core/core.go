package core

import (
	"context"
	"time"
)

// Subject is a stable URI for a watched thing.
// github:org/repo#421, linear:ENG-123, slack:C0123ABCD/1761811234.567890,
// k8s:kind-dev/dev-eng-123/deploy/api
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
}

type Binding struct {
	Subject   Subject
	Workspace string // stable workspace ID from the host
	Via       string // branch | link | k8s | explicit | confirmed
	Since     time.Time
}

// Adapter is one built-in way of reading a source: cli, cli-watch or mcp.
type Adapter interface {
	Run(ctx context.Context, observe func([]Observation), emit func(Event)) error
}
