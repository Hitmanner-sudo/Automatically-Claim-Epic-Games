// Package cdp is a small Chrome DevTools Protocol client.
//
// It talks to the DevTools endpoint that a Chromium-based application exposes
// on localhost (here: the embedded browser of the Epic Games Launcher). Only
// what the claimer needs is implemented: target discovery over HTTP and a
// JSON-RPC connection to a single target over WebSocket.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Target is one debuggable page, iframe or worker.
type Target struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// VersionInfo is the reply of /json/version.
type VersionInfo struct {
	Browser              string `json:"Browser"`
	ProtocolVersion      string `json:"Protocol-Version"`
	UserAgent            string `json:"User-Agent"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// The endpoint is always on localhost; never route it through a system proxy.
var httpClient = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
}

func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, res.StatusCode)
	}
	return json.Unmarshal(body, out)
}

// Base returns the HTTP base URL of a DevTools endpoint on localhost.
func Base(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }

// Version queries /json/version. It doubles as a liveness probe.
func Version(ctx context.Context, base string) (VersionInfo, error) {
	var v VersionInfo
	err := getJSON(ctx, strings.TrimRight(base, "/")+"/json/version", &v)
	return v, err
}

// Targets lists the debuggable targets.
func Targets(ctx context.Context, base string) ([]Target, error) {
	var t []Target
	err := getJSON(ctx, strings.TrimRight(base, "/")+"/json/list", &t)
	return t, err
}

// Error is a protocol-level error returned by the browser.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (e *Error) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("cdp error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message)
}

// Event is an unsolicited message from the browser.
type Event struct {
	Method string
	Params json.RawMessage
}

type reply struct {
	result json.RawMessage
	err    error
}

// Conn is a connection to one target.
type Conn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
	nextID  atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan reply
	closed  bool
	err     error

	evMu    sync.Mutex
	evQueue []Event
	evWake  chan struct{}
	events  chan Event
	done    chan struct{}
}

// ErrClosed is returned by calls on a connection that has gone away.
var ErrClosed = errors.New("cdp: connection closed")

// Dial connects to a target's webSocketDebuggerUrl.
//
// No Origin header is sent: Chromium (111+) rejects DevTools connections that
// carry an Origin it was not told to allow, and accepts those without one.
func Dial(ctx context.Context, wsURL string) (*Conn, error) {
	d := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   64 << 10,
		WriteBufferSize:  64 << 10,
		Proxy:            nil, // always direct: the endpoint is on localhost
	}
	ws, res, err := d.DialContext(ctx, wsURL, nil)
	if err != nil {
		if res != nil {
			return nil, fmt.Errorf("dial %s: %w (HTTP %d)", wsURL, err, res.StatusCode)
		}
		return nil, fmt.Errorf("dial %s: %w", wsURL, err)
	}
	ws.SetReadLimit(256 << 20) // screenshots and DOM dumps can be large
	c := &Conn{
		ws:      ws,
		pending: map[int64]chan reply{},
		evWake:  make(chan struct{}, 1),
		events:  make(chan Event),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	go c.pumpEvents()
	return c, nil
}

// pumpEvents hands queued events to whoever reads Events(). The queue is
// unbounded on purpose: some events (a paused network request) must never be
// lost, because the browser waits for an answer to them.
func (c *Conn) pumpEvents() {
	for {
		c.evMu.Lock()
		var next Event
		has := len(c.evQueue) > 0
		if has {
			next = c.evQueue[0]
			c.evQueue = c.evQueue[1:]
		}
		c.evMu.Unlock()
		if !has {
			select {
			case <-c.evWake:
				continue
			case <-c.done:
				return
			}
		}
		select {
		case c.events <- next:
		case <-c.done:
			return
		}
	}
}

func (c *Conn) queueEvent(e Event) {
	c.evMu.Lock()
	c.evQueue = append(c.evQueue, e)
	c.evMu.Unlock()
	select {
	case c.evWake <- struct{}{}:
	default:
	}
}

func (c *Conn) readLoop() {
	var failure error
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			failure = err
			break
		}
		var msg struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *Error          `json:"error"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.ID != 0 {
			c.mu.Lock()
			ch := c.pending[msg.ID]
			delete(c.pending, msg.ID)
			c.mu.Unlock()
			if ch != nil {
				if msg.Error != nil {
					ch <- reply{err: msg.Error}
				} else {
					ch <- reply{result: msg.Result}
				}
			}
			continue
		}
		if msg.Method != "" {
			c.queueEvent(Event{Method: msg.Method, Params: msg.Params})
		}
	}
	c.mu.Lock()
	c.closed = true
	c.err = failure
	pending := c.pending
	c.pending = map[int64]chan reply{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- reply{err: ErrClosed}
	}
	close(c.done)
}

// Events delivers browser events in order. None are dropped; events only
// arrive for protocol domains that have been enabled.
func (c *Conn) Events() <-chan Event { return c.events }

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Call sends a command and waits for its reply. result may be nil.
func (c *Conn) Call(ctx context.Context, method string, params any, result any) error {
	// A command that is sent is carried out, whether or not anyone waits
	// for the reply: nothing is sent once the caller has been called off.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	id := c.nextID.Add(1)
	ch := make(chan reply, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()

	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, err := json.Marshal(msg)
	if err != nil {
		c.forget(id)
		return err
	}
	c.writeMu.Lock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
	err = c.ws.WriteMessage(websocket.TextMessage, data)
	c.writeMu.Unlock()
	if err != nil {
		c.forget(id)
		return fmt.Errorf("%s: %w", method, err)
	}

	select {
	case r := <-ch:
		if r.err != nil {
			return fmt.Errorf("%s: %w", method, r.err)
		}
		if result != nil && len(r.result) > 0 {
			if err := json.Unmarshal(r.result, result); err != nil {
				return fmt.Errorf("%s: decode result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.forget(id)
		return fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

func (c *Conn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Close ends the connection.
func (c *Conn) Close() error {
	c.writeMu.Lock()
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	c.writeMu.Unlock()
	return c.ws.Close()
}
