package rex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"sync"

	"github.com/coder/websocket"
)

// client speaks the control protocol the rex CLI uses, which Rex does not
// document; it was read off the CLI's own traffic at API revision 18. It is a
// WebSocket at /api/control with the subprotocol rex.control.v1, carrying
// JSON text messages: the client says hello and the server answers welcome,
// then each request {id, method, payload} gets a response with the same id
// and either payload or error. After session.attach_all the server also sends
// event {seq, payload}, where payload is what rex events --json prints.
//
// Dialing the socket can never start a server, unlike the CLI without
// --autostart=false: a daemon retrying every poll must not bring back a
// server the person stopped.
type client struct {
	ws     *websocket.Conn
	events chan json.RawMessage
	done   chan struct{}

	mu      sync.Mutex
	err     error // why done closed
	next    int
	pending map[string]chan reply
}

// apiRevision is the revision kelpie was written against. The server says
// whether it still speaks it; method names and shapes may move after it.
const apiRevision = 18

// errEventsLost is kelpie reading events slower than Rex sends them, or a
// connection opened for a call while nothing reads its events yet. Either
// way the connection is dropped and a new one resyncs.
var errEventsLost = errors.New("rex events not read in time")

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return "rex: " + e.Code + ": " + e.Message }

func errCode(err error) string {
	var e *apiError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

type reply struct {
	payload json.RawMessage
	err     error
}

func dial(ctx context.Context, socket string) (*client, error) {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	ws, _, err := websocket.Dial(ctx, "ws://rex/api/control", &websocket.DialOptions{
		HTTPClient:   hc,
		Subprotocols: []string{"rex.control.v1"},
	})
	if err != nil {
		return nil, err
	}
	// The server announces 1 MiB per message; scrollback reads can be
	// larger, and kelpie never asks for those, but a cap below the server's
	// would fail mid-stream instead of at a request.
	ws.SetReadLimit(16 << 20)

	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	hello, _ := json.Marshal(map[string]any{
		"type":         "hello",
		"client":       map[string]string{"kind": "other", "platform": platform, "name": "kelpie", "version": "dev"},
		"api_revision": apiRevision,
	})
	if err := ws.Write(ctx, websocket.MessageText, hello); err != nil {
		ws.CloseNow()
		return nil, err
	}
	_, data, err := ws.Read(ctx)
	if err != nil {
		ws.CloseNow()
		return nil, err
	}
	var welcome struct {
		Type  string    `json:"type"`
		Error *apiError `json:"error"`
	}
	if err := json.Unmarshal(data, &welcome); err != nil || welcome.Type != "welcome" {
		ws.CloseNow()
		if welcome.Error != nil {
			return nil, welcome.Error
		}
		return nil, fmt.Errorf("rex: expected welcome, got %.200s", data)
	}

	c := &client{
		ws: ws,
		// Room for a burst, like an agent's status reports while the
		// daemon lists every block after connecting.
		events:  make(chan json.RawMessage, 4096),
		done:    make(chan struct{}),
		pending: map[string]chan reply{},
	}
	go c.read()
	// Attaching to every session is what lets block calls reach any of
	// them on this one connection, and starts the events.
	if err := c.call(ctx, "session.attach_all", map[string]string{"event_scope": "all"}, nil); err != nil {
		c.fail(err)
		return nil, err
	}
	return c, nil
}

func (c *client) read() {
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.fail(err)
			return
		}
		var m struct {
			Type    string          `json:"type"`
			ID      string          `json:"id"`
			Payload json.RawMessage `json:"payload"`
			Error   *apiError       `json:"error"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "response":
			c.mu.Lock()
			ch := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ch == nil {
				continue
			}
			r := reply{payload: m.Payload}
			if m.Error != nil {
				r.err = m.Error
			}
			ch <- r
		case "event":
			select {
			case c.events <- m.Payload:
			default:
				c.fail(errEventsLost)
				return
			}
		}
	}
}

// fail ends the connection once and fails every call still waiting.
func (c *client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	close(c.done)
	c.ws.CloseNow()
	for id, ch := range c.pending {
		ch <- reply{err: err}
		delete(c.pending, id)
	}
}

func (c *client) closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *client) call(ctx context.Context, method string, payload, into any) error {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.next++
	id := "kelpie-" + strconv.Itoa(c.next)
	ch := make(chan reply, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := map[string]any{"type": "request", "id": id, "method": method}
	if payload != nil {
		req["payload"] = payload
	}
	msg, _ := json.Marshal(req)
	if err := c.ws.Write(ctx, websocket.MessageText, msg); err != nil {
		c.fail(err)
		return err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return fmt.Errorf("rex %s: %w", method, r.err)
		}
		if into == nil {
			return nil
		}
		return json.Unmarshal(r.payload, into)
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}
