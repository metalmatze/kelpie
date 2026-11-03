// Package control is the daemon's local API: kelpie watch, unwatch, status
// and the reminder commands talk to the running daemon over a unix socket. A TUI would be another client.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/metalmatze/kelpie/internal/core"
)

// StateDir is $XDG_STATE_HOME/kelpie, else ~/.local/state/kelpie.
func StateDir() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "kelpie")
}

func SocketPath() string { return filepath.Join(StateDir(), "kelpie.sock") }

type WatchRequest struct {
	Subject   core.Subject `json:"subject"`
	Host      string       `json:"host"`
	Workspace string       `json:"workspace"`
}

type WatchResponse struct {
	Subject   core.Subject `json:"subject"`
	Workspace string       `json:"workspace"`
	// Others lists the other workspaces that watch the subject too. Each
	// gets its own updates.
	Others []string `json:"others,omitzero"`
}

type Status struct {
	Watches   []core.Binding  `json:"watches"`
	Reminders []core.Reminder `json:"reminders,omitzero"`
	Queued    map[string]int  `json:"queued,omitzero"` // undelivered events per workspace
}

// RemindRequest carries either Due (one-shot) or Cron.
type RemindRequest struct {
	Workspace string       `json:"workspace"`
	Text      string       `json:"text"`
	Subject   core.Subject `json:"subject,omitzero"`
	Due       time.Time    `json:"due,omitzero"`
	Cron      string       `json:"cron,omitzero"`
}

type Handler interface {
	Watch(ctx context.Context, req WatchRequest) (WatchResponse, error)
	Unwatch(ctx context.Context, req WatchRequest) error
	Remind(ctx context.Context, req RemindRequest) (core.Reminder, error)
	Unremind(ctx context.Context, id string) error
	Status(ctx context.Context) Status
}

// ErrRunning is Serve's answer when another daemon already answers on the
// socket. The daemon's flock normally prevents it; it still happens when a
// daemon from before the flock owns the socket, and the caller should step
// aside rather than fail.
var ErrRunning = errors.New("another kelpie daemon is running")

// Serve listens on path until ctx ends, or returns ErrRunning if another
// daemon answers on it.
func Serve(ctx context.Context, path string, h Handler) error {
	// sun_path is 104 bytes on macOS, 108 on Linux; bind fails with a bare
	// "invalid argument" past that.
	if len(path) >= 104 {
		return fmt.Errorf("socket path %s is too long for a unix socket (%d bytes, max 103); use a shorter XDG_STATE_HOME", path, len(path))
	}
	if _, err := NewClient(path).Status(ctx); err == nil {
		return fmt.Errorf("%w on %s", ErrRunning, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_ = os.Remove(path) // left behind by a daemon that died
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /watch", func(w http.ResponseWriter, r *http.Request) {
		var req WatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reply(w, nil, err)
			return
		}
		resp, err := h.Watch(r.Context(), req)
		reply(w, resp, err)
	})
	mux.HandleFunc("POST /unwatch", func(w http.ResponseWriter, r *http.Request) {
		var req WatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reply(w, nil, err)
			return
		}
		reply(w, struct{}{}, h.Unwatch(r.Context(), req))
	})
	mux.HandleFunc("POST /remind", func(w http.ResponseWriter, r *http.Request) {
		var req RemindRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reply(w, nil, err)
			return
		}
		resp, err := h.Remind(r.Context(), req)
		reply(w, resp, err)
	})
	mux.HandleFunc("POST /unremind", func(w http.ResponseWriter, r *http.Request) {
		var req core.Reminder
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reply(w, nil, err)
			return
		}
		reply(w, struct{}{}, h.Unremind(r.Context(), req.ID))
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		reply(w, h.Status(r.Context()), nil)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
		os.Remove(path)
	}()
	slog.Info("control socket listening", "path", path)
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type apiError struct {
	Error string `json:"error"`
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		v = apiError{err.Error()}
	}
	json.NewEncoder(w).Encode(v)
}

type Client struct{ http *http.Client }

func NewClient(path string) *Client {
	return &Client{http: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}},
	}}
}

// ErrNotRunning means nothing answers on the socket.
var ErrNotRunning = errors.New("kelpie daemon is not running (start it with: kelpie daemon)")

func (c *Client) Watch(ctx context.Context, req WatchRequest) (WatchResponse, error) {
	var resp WatchResponse
	return resp, c.do(ctx, http.MethodPost, "/watch", req, &resp)
}

// Unwatch ends one workspace's watch of subject.
func (c *Client) Unwatch(ctx context.Context, subject core.Subject, workspace string) error {
	return c.do(ctx, http.MethodPost, "/unwatch", WatchRequest{Subject: subject, Workspace: workspace}, nil)
}

func (c *Client) Remind(ctx context.Context, req RemindRequest) (core.Reminder, error) {
	var r core.Reminder
	return r, c.do(ctx, http.MethodPost, "/remind", req, &r)
}

func (c *Client) Unremind(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/unremind", core.Reminder{ID: id}, nil)
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	return s, c.do(ctx, http.MethodGet, "/status", nil, &s)
}

func (c *Client) do(ctx context.Context, method, path string, body, into any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://kelpie"+path, r)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		return ErrNotRunning
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e apiError
		if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("daemon answered %s", resp.Status)
	}
	if into == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(into)
}
