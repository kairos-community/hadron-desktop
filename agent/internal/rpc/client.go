package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

// baseURL is a fixed placeholder host: the Unix-socket DialContext below
// ignores the network address entirely and always dials the configured
// socket path, so the host/scheme here are never actually resolved or
// looked up -- they only need to be syntactically valid for net/http's URL
// parsing.
const baseURL = "http://unix"

// Client talks the fixed four-endpoint RPC protocol to a single server over
// a Unix domain socket.
type Client struct {
	httpClient *http.Client
}

// NewClient builds a Client that dials socketPath (via net.Dial("unix",
// ...) under the hood) for every request. socketPath is not required to
// exist yet; dialing happens lazily on the first request, and a missing or
// removed socket simply surfaces as an error from that request.
func NewClient(socketPath string) *Client {
	var dialer net.Dialer
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{httpClient: &http.Client{Transport: transport}}
}

// Close releases any idle connections held by the client. It does not
// cancel in-flight requests.
func (c *Client) Close() {
	c.httpClient.CloseIdleConnections()
}

// Call issues POST /v1/call. authHeader, if non-empty, is forwarded
// verbatim as the outgoing request's Authorization header -- Client never
// constructs, parses, or otherwise interprets it; it only ever forwards
// exactly what its caller supplied.
func (c *Client) Call(ctx context.Context, req CallRequest, authHeader string) (*CallResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("rpc: marshal call request: %w", err)
	}

	var resp CallResponse
	if err := c.do(ctx, http.MethodPost, PathCall, body, authHeader, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Health issues GET /v1/health.
func (c *Client) Health(ctx context.Context, authHeader string) (HealthStatus, error) {
	var status HealthStatus
	if err := c.do(ctx, http.MethodGet, PathHealth, nil, authHeader, &status); err != nil {
		return HealthStatus{}, err
	}
	return status, nil
}

// Pause issues POST /v1/pause. It is safe to call multiple times.
func (c *Client) Pause(ctx context.Context, authHeader string) error {
	return c.do(ctx, http.MethodPost, PathPause, nil, authHeader, nil)
}

// Resume issues POST /v1/resume. It is safe to call multiple times.
func (c *Client) Resume(ctx context.Context, authHeader string) error {
	return c.do(ctx, http.MethodPost, PathResume, nil, authHeader, nil)
}

// StatusError is returned by Client methods when the server responds with a
// non-2xx status. StatusCode lets a caller distinguish, e.g., 401
// (Authorization required/rejected) from 500 (handler failure).
type StatusError struct {
	StatusCode int
	Message    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("rpc: server responded %d: %s", e.StatusCode, e.Message)
}

// do issues one request and, on a 2xx response, decodes the body into out
// (when out is non-nil and the response body is non-empty). body, when
// non-nil, is sent with Content-Type: application/json.
func (c *Client) do(ctx context.Context, method, path string, body []byte, authHeader string, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("rpc: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("rpc: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// Read one byte past the limit so an oversized body is DETECTED rather than
	// silently truncated. A truncated body is worse than a rejected one: it
	// still decodes as "some JSON error" and the real cause (a response too big
	// for the hop) never reaches anyone.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("rpc: read response body: %w", err)
	}
	if len(respBody) > MaxResponseBytes {
		return fmt.Errorf("rpc: %s %s: %w", method, path, ErrResponseTooLarge)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := string(respBody)
		var errResp ErrorResponse
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			msg = errResp.Error
		}
		return &StatusError{StatusCode: resp.StatusCode, Message: msg}
	}

	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("rpc: decode response: %w", err)
	}
	return nil
}
