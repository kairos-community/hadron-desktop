package cua

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrDisconnected is returned by Call whenever the client has no live
// session: the child was never started successfully, it exited or the
// transport broke, and (if reconnection is enabled) a reconnect attempt has
// not yet succeeded. Call never blocks waiting for a reconnect and never
// retries a call itself; callers that want the mutation to happen must issue
// a fresh Call once Options.OnReady reports connected again.
var ErrDisconnected = errors.New("cua: client is not connected")

// reconnectBudget bounds the total wall-clock time a single reconnect
// campaign spends retrying before giving up. Call keeps returning
// ErrDisconnected immediately for the whole time a campaign is running or
// exhausted; a later Call failure starts a fresh campaign.
const reconnectBudget = 30 * time.Second

const (
	reconnectInitialBackoff = 200 * time.Millisecond
	reconnectMaxBackoff     = 5 * time.Second
)

// Options configures Start and, if OnReady is set, the reconnect loop a
// Client runs after it detects its session has died.
type Options struct {
	Binary string
	Args   []string
	Env    []string

	// OnReady, if non-nil, is invoked every time the client's connected
	// state changes: once with connected=false the moment a disconnection is
	// detected, and again with connected=true if and when a reconnect
	// attempt succeeds. It is always invoked from its own goroutine, never
	// while any Client lock is held, so it may safely call back into the
	// Client (including Call). The session broker uses this to learn when
	// Cua is back without polling.
	OnReady func(connected bool)
}

// Client is a serialized MCP stdio client for a single Cua child process. All
// CallTool operations are executed one at a time, in the order Call was
// invoked (this is the client's own low-level serialization; the
// agent/internal/cua Adapter layers its own single-seat queue on top for
// multi-call actions, but does not rely on this one).
//
// A Client born from Start with Options.OnReady set additionally survives a
// dead child: the first Call that observes a non-context transport error
// marks the client disconnected and starts a bounded reconnect campaign in
// the background. Calls made while disconnected fail fast with
// ErrDisconnected instead of blocking or retrying.
type Client struct {
	options Options

	callMu sync.Mutex // serializes CallTool invocations end-to-end

	mu           sync.Mutex // guards the fields below
	session      *mcp.ClientSession
	connected    bool
	closed       bool
	reconnecting bool

	closeOnce sync.Once
	closeErr  error
}

// Start launches options.Binary as a child process and connects to it as an
// MCP client over stdio. ctx bounds only the connection handshake; the
// child's lifetime is not tied to ctx (Close stops it).
func Start(ctx context.Context, options Options) (*Client, error) {
	session, err := connectChild(ctx, options)
	if err != nil {
		return nil, err
	}

	return &Client{
		options:   options,
		session:   session,
		connected: true,
	}, nil
}

func connectChild(ctx context.Context, options Options) (*mcp.ClientSession, error) {
	cmd := exec.Command(options.Binary, options.Args...)
	cmd.Env = append(os.Environ(), options.Env...)

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "hadron-cua-client",
		Version: "v0.0.1",
	}, nil)
	return client.Connect(ctx, &mcp.CommandTransport{
		Command:           cmd,
		TerminateDuration: 3 * time.Second,
	}, nil)
}

// Call issues one CallTool request. Every Call is fully serialized against
// every other Call on this Client. If the client currently has no live
// session (never connected, or a disconnection was detected and no
// reconnect has yet succeeded), Call returns ErrDisconnected immediately
// without touching any transport.
func (c *Client) Call(ctx context.Context, name string, arguments any) (*mcp.CallToolResult, error) {
	c.callMu.Lock()
	defer c.callMu.Unlock()

	c.mu.Lock()
	session := c.session
	connected := c.connected
	c.mu.Unlock()

	if !connected || session == nil {
		return nil, ErrDisconnected
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: arguments,
	})
	if err != nil && !isContextErr(err) {
		c.onDisconnect()
	}
	return result, err
}

// Connected reports whether the client currently believes it has a live
// session. It is a snapshot: it can go stale the instant it returns.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// onDisconnect marks the client disconnected and, if the caller configured
// Options.OnReady and a reconnect campaign is not already running or the
// client is not closed, starts one. Without OnReady there is nobody to tell
// apart from the next failed Call, so no background respawn is started: the
// client simply stays disconnected until closed.
func (c *Client) onDisconnect() {
	c.mu.Lock()
	wasConnected := c.connected
	c.connected = false
	c.session = nil
	start := c.options.OnReady != nil && !c.reconnecting && !c.closed
	if start {
		c.reconnecting = true
	}
	c.mu.Unlock()

	if wasConnected {
		c.notifyReady(false)
	}
	if start {
		go c.reconnectLoop()
	}
}

// reconnectLoop retries connectChild with exponential backoff until it
// succeeds, the client is closed, or reconnectBudget elapses.
func (c *Client) reconnectLoop() {
	defer func() {
		c.mu.Lock()
		c.reconnecting = false
		c.mu.Unlock()
	}()

	deadline := time.Now().Add(reconnectBudget)
	backoff := reconnectInitialBackoff

	for {
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}

		attemptCtx, cancel := context.WithTimeout(context.Background(), remaining)
		session, err := connectChild(attemptCtx, c.options)
		cancel()

		if err == nil {
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				_ = session.Close()
				return
			}
			c.session = session
			c.connected = true
			c.mu.Unlock()
			c.notifyReady(true)
			return
		}

		remaining = time.Until(deadline)
		if remaining <= 0 {
			return
		}
		wait := backoff
		if wait > remaining {
			wait = remaining
		}
		time.Sleep(wait)
		backoff *= 2
		if backoff > reconnectMaxBackoff {
			backoff = reconnectMaxBackoff
		}
	}
}

func (c *Client) notifyReady(connected bool) {
	if c.options.OnReady == nil {
		return
	}
	go c.options.OnReady(connected)
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Close closes the client's session (if any) and stops any in-progress
// reconnect campaign from installing a new one. Close is idempotent and
// never logs to stdout.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		session := c.session
		c.session = nil
		c.connected = false
		c.mu.Unlock()

		if session != nil {
			c.closeErr = session.Close()
		}
	})
	return c.closeErr
}
