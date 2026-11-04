// Package acp puts kelpie between an editor and an ACP (Agent Client
// Protocol) agent, so it can prompt the agent the editor runs. Zed and other
// ACP editors have no API to write into an agent thread, but they start every
// external agent as a command, so the command can be kelpie wrapping the real
// agent. One proxy serves any ACP agent; nothing in it knows Claude or Codex.
//
// The proxy forwards every line unchanged, except the answers to its own
// prompts, which the editor never asked for and would reject.
package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// ownPrefix marks the request IDs the proxy sends. Editors number their
// requests, so a string ID with this prefix cannot be theirs.
const ownPrefix = "kelpie-"

var ErrBusy = errors.New("agent is busy")

// Session is what the registry file lists for each session.
type Session struct {
	ID   string `json:"id"`
	Cwd  string `json:"cwd"`
	Busy bool   `json:"busy"`
}

// Registry is <dir>/<pid>.json, how the daemon and `kelpie acp send` find a
// proxy and its sessions.
type Registry struct {
	PID      int        `json:"pid"`
	Agent    []string   `json:"agent"`
	Socket   string     `json:"socket"`
	Sessions []*Session `json:"sessions"`
	Updated  time.Time  `json:"updated"`
}

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

type Proxy struct {
	Agent []string
	Dir   string
	Log   *slog.Logger

	toClient lineWriter
	toAgent  lineWriter

	mu       sync.Mutex
	sessions map[string]*Session // by session ID; order keeps them oldest first
	order    []string
	newCwd   map[string]string // session/new request ID -> cwd, until the agent answers
	prompts  map[string]string // open session/prompt request ID -> session ID
	nextID   int
}

type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lineWriter) write(line []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(line); err != nil {
		return err
	}
	if len(line) == 0 || line[len(line)-1] != '\n' {
		_, err := l.w.Write([]byte{'\n'})
		return err
	}
	return nil
}

// Run starts the agent and proxies between it and in/out (the editor) until
// either side closes. It returns the agent's exit error.
func (p *Proxy) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	p.sessions = map[string]*Session{}
	p.newCwd = map[string]string{}
	p.prompts = map[string]string{}
	p.toClient.w = out

	cmd := exec.CommandContext(ctx, p.Agent[0], p.Agent[1:]...)
	cmd.Stderr = os.Stderr
	agentIn, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	agentOut, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	p.toAgent.w = agentIn

	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return err
	}
	sock := filepath.Join(p.Dir, strconv.Itoa(os.Getpid())+".sock")
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer os.Remove(sock)
	defer os.Remove(p.registryPath())
	defer ln.Close()
	go p.serve(ln)
	p.saveRegistry()

	go func() {
		p.pump(in, &p.toAgent, p.fromClient)
		agentIn.Close() // the editor is gone: let the agent exit on EOF
	}()
	p.pump(agentOut, &p.toClient, p.fromAgent)
	return cmd.Wait()
}

// pump copies lines from r to w, letting inspect see each one first and drop
// it. ReadBytes rather than a Scanner: a session/update with a file diff can
// be larger than any fixed buffer.
func (p *Proxy) pump(r io.Reader, w *lineWriter, inspect func([]byte) bool) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 && inspect(line) {
			if werr := w.write(line); werr != nil {
				p.Log.Warn("write", "err", werr)
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// fromClient notes sessions and prompts from the editor's requests.
func (p *Proxy) fromClient(line []byte) bool {
	var m message
	if json.Unmarshal(line, &m) != nil || m.Method == "" {
		return true
	}
	var params struct {
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
	}
	_ = json.Unmarshal(m.Params, &params)
	p.mu.Lock()
	defer p.mu.Unlock()
	switch m.Method {
	case "session/new":
		p.newCwd[string(m.ID)] = params.Cwd
	case "session/load", "session/resume":
		p.addSession(params.SessionID, params.Cwd)
	case "session/prompt":
		p.prompts[string(m.ID)] = params.SessionID
		p.setBusy(params.SessionID, true)
	}
	p.Log.Debug("client", "method", m.Method, "session", params.SessionID)
	return true
}

// fromAgent notes the answers to session/new and session/prompt, and drops
// the answers to the proxy's own prompts.
func (p *Proxy) fromAgent(line []byte) bool {
	var m message
	if json.Unmarshal(line, &m) != nil || m.Method != "" || len(m.ID) == 0 {
		return true
	}
	id := string(m.ID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if cwd, ok := p.newCwd[id]; ok {
		delete(p.newCwd, id)
		var res struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(m.Result, &res) == nil && res.SessionID != "" {
			p.addSession(res.SessionID, cwd)
		}
	}
	if sid, ok := p.prompts[id]; ok {
		delete(p.prompts, id)
		p.setBusy(sid, p.promptOpen(sid))
		p.Log.Info("turn ended", "session", sid, "id", id, "result", string(m.Result), "error", string(m.Error))
	}
	var s string
	return json.Unmarshal(m.ID, &s) != nil || len(s) < len(ownPrefix) || s[:len(ownPrefix)] != ownPrefix
}

func (p *Proxy) promptOpen(sid string) bool {
	for _, s := range p.prompts {
		if s == sid {
			return true
		}
	}
	return false
}

func (p *Proxy) addSession(id, cwd string) {
	if id == "" {
		return
	}
	if _, ok := p.sessions[id]; !ok {
		p.order = append(p.order, id)
	}
	p.sessions[id] = &Session{ID: id, Cwd: cwd}
	p.Log.Info("session", "id", id, "cwd", cwd)
	p.saveRegistryLocked()
}

func (p *Proxy) setBusy(id string, busy bool) {
	if s, ok := p.sessions[id]; ok && s.Busy != busy {
		s.Busy = busy
		p.saveRegistryLocked()
	}
}

// Prompt sends text to the agent as a prompt of session sid (the newest
// session when empty), and shows it in the editor as a user message, since
// the editor did not send it. It refuses a busy session: agents differ in
// whether a second prompt queues, steers or cancels the running turn.
func (p *Proxy) Prompt(sid, text string) (string, error) {
	p.mu.Lock()
	if sid == "" && len(p.order) > 0 {
		sid = p.order[len(p.order)-1]
	}
	s, ok := p.sessions[sid]
	if !ok {
		p.mu.Unlock()
		return "", fmt.Errorf("no session %q", sid)
	}
	if s.Busy {
		p.mu.Unlock()
		return sid, ErrBusy
	}
	p.nextID++
	id := ownPrefix + strconv.Itoa(p.nextID)
	rawID, _ := json.Marshal(id)
	p.prompts[string(rawID)] = sid
	s.Busy = true
	p.saveRegistryLocked()
	p.mu.Unlock()

	content := []map[string]string{{"type": "text", "text": text}}
	echo, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sid,
			"update":    map[string]any{"sessionUpdate": "user_message_chunk", "content": content[0]},
		},
	})
	req, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "session/prompt",
		"params":  map[string]any{"sessionId": sid, "prompt": content},
	})
	if err := p.toClient.write(echo); err != nil {
		return sid, err
	}
	p.Log.Info("prompt", "session", sid, "id", id)
	return sid, p.toAgent.write(req)
}

// serve answers `kelpie acp send`: one JSON request per connection.
func (p *Proxy) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			var req struct {
				Session string `json:"session"`
				Text    string `json:"text"`
			}
			resp := map[string]string{}
			if err := json.NewDecoder(c).Decode(&req); err != nil {
				resp["error"] = err.Error()
			} else if sid, err := p.Prompt(req.Session, req.Text); err != nil {
				resp["session"], resp["error"] = sid, err.Error()
			} else {
				resp["session"] = sid
			}
			_ = json.NewEncoder(c).Encode(resp)
		}()
	}
}

func (p *Proxy) registryPath() string {
	return filepath.Join(p.Dir, strconv.Itoa(os.Getpid())+".json")
}

func (p *Proxy) saveRegistry() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saveRegistryLocked()
}

func (p *Proxy) saveRegistryLocked() {
	r := Registry{
		PID:     os.Getpid(),
		Agent:   p.Agent,
		Socket:  filepath.Join(p.Dir, strconv.Itoa(os.Getpid())+".sock"),
		Updated: time.Now(),
	}
	for _, id := range p.order {
		s := *p.sessions[id]
		r.Sessions = append(r.Sessions, &s)
	}
	data, _ := json.MarshalIndent(r, "", "  ")
	tmp := p.registryPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		_ = os.Rename(tmp, p.registryPath())
	}
}

// List reads the registries in dir, skipping proxies that are gone.
func List(dir string) ([]Registry, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Registry
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var r Registry
		if json.Unmarshal(data, &r) != nil {
			continue
		}
		if c, err := net.Dial("unix", r.Socket); err != nil {
			continue
		} else {
			c.Close()
		}
		out = append(out, r)
	}
	return out, nil
}

// Send asks the proxy listening on socket to prompt session (its newest when
// empty) with text, and returns the session it went to.
func Send(socket, session, text string) (string, error) {
	c, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(map[string]string{"session": session, "text": text}); err != nil {
		return "", err
	}
	var resp struct {
		Session string `json:"session"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return "", err
	}
	if resp.Error != "" {
		return resp.Session, errors.New(resp.Error)
	}
	return resp.Session, nil
}
