package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// rawClient issues requests directly against a Unix socket without going
// through package rpc's own Client, so server tests do not depend on the
// client implementation being correct.
type rawClient struct {
	http.Client
}

func newRawClient(sockPath string) *rawClient {
	return &rawClient{
		Client: http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
			},
		},
	}
}

func (c *rawClient) do(t *testing.T, ctx context.Context, method, path, contentType string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, r)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return data
}

func newTestServer(t *testing.T, h *fakeHandler, requireAuth bool) (string, *Server) {
	t.Helper()
	s := NewServer(h, Config{RequireAuth: requireAuth})
	sockPath := startTestServer(t, s)
	return sockPath, s
}

// ---------------------------------------------------------------------------
// Only the four endpoints exist
// ---------------------------------------------------------------------------

func TestServerOnlyExposesFourEndpoints(t *testing.T) {
	h := newFakeHandler()
	sockPath, _ := newTestServer(t, h, false)
	c := newRawClient(sockPath)
	ctx := context.Background()

	t.Run("unknown path is 404", func(t *testing.T) {
		resp := c.do(t, ctx, http.MethodGet, "/v1/nope", "", nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		resp.Body.Close()
	})

	t.Run("root path is 404", func(t *testing.T) {
		resp := c.do(t, ctx, http.MethodGet, "/", "", nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		resp.Body.Close()
	})

	t.Run("wrong method on /v1/call is 405", func(t *testing.T) {
		resp := c.do(t, ctx, http.MethodGet, PathCall, "", nil, nil)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
		resp.Body.Close()
	})

	t.Run("wrong method on /v1/health is 405", func(t *testing.T) {
		resp := c.do(t, ctx, http.MethodPost, PathHealth, "", nil, nil)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
		resp.Body.Close()
	})

	t.Run("wrong method on /v1/pause is 405", func(t *testing.T) {
		resp := c.do(t, ctx, http.MethodGet, PathPause, "", nil, nil)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
		resp.Body.Close()
	})

	t.Run("wrong method on /v1/resume is 405", func(t *testing.T) {
		resp := c.do(t, ctx, http.MethodGet, PathResume, "", nil, nil)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
		resp.Body.Close()
	})

	if h.callCount() != 0 {
		t.Fatalf("handler.Call invoked %d times, want 0", h.callCount())
	}
}

// ---------------------------------------------------------------------------
// Body parsing correctness
// ---------------------------------------------------------------------------

func TestServerRejectsTruncatedJSON(t *testing.T) {
	h := newFakeHandler()
	sockPath, _ := newTestServer(t, h, false)
	c := newRawClient(sockPath)

	resp := c.do(t, context.Background(), http.MethodPost, PathCall, "application/json",
		[]byte(`{"request_id":"1","tool":"term`), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body ErrorResponse
	if err := json.Unmarshal(readBody(t, resp), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error == "" {
		t.Fatal("expected a non-empty error message")
	}
	if h.callCount() != 0 {
		t.Fatalf("handler.Call invoked %d times, want 0", h.callCount())
	}
}

func TestServerRejectsEmptyBody(t *testing.T) {
	h := newFakeHandler()
	sockPath, _ := newTestServer(t, h, false)
	c := newRawClient(sockPath)

	resp := c.do(t, context.Background(), http.MethodPost, PathCall, "application/json", []byte(``), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestServerRejectsOversizedBodyWithoutUnboundedRead(t *testing.T) {
	h := newFakeHandler()
	sockPath, _ := newTestServer(t, h, false)
	c := newRawClient(sockPath)

	// Build a syntactically valid, but oversized, JSON body: a big padding
	// string inside "arguments" pushes it past MaxBodyBytes.
	padding := strings.Repeat("a", MaxBodyBytes+1024)
	body, err := json.Marshal(CallRequest{
		RequestID: "1",
		Tool:      "terminal",
		Arguments: json.RawMessage(`"` + padding + `"`),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(body) <= MaxBodyBytes {
		t.Fatalf("test body (%d bytes) must exceed MaxBodyBytes (%d)", len(body), MaxBodyBytes)
	}

	resp := c.do(t, context.Background(), http.MethodPost, PathCall, "application/json", body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 413 or 400", resp.StatusCode)
	}
	if h.callCount() != 0 {
		t.Fatalf("handler.Call invoked %d times, want 0", h.callCount())
	}
}

func TestServerRejectsUnknownField(t *testing.T) {
	h := newFakeHandler()
	sockPath, _ := newTestServer(t, h, false)
	c := newRawClient(sockPath)

	resp := c.do(t, context.Background(), http.MethodPost, PathCall, "application/json",
		[]byte(`{"request_id":"1","tool":"terminal","arguments":{},"admin":true}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if h.callCount() != 0 {
		t.Fatalf("handler.Call invoked %d times, want 0", h.callCount())
	}
}

func TestServerRejectsNonJSONContentType(t *testing.T) {
	h := newFakeHandler()
	sockPath, _ := newTestServer(t, h, false)
	c := newRawClient(sockPath)

	resp := c.do(t, context.Background(), http.MethodPost, PathCall, "text/plain",
		[]byte(`{"request_id":"1","tool":"terminal"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if h.callCount() != 0 {
		t.Fatalf("handler.Call invoked %d times, want 0", h.callCount())
	}
}

// ---------------------------------------------------------------------------
// Handler dispatch (unsupported tool names are the handler's job)
// ---------------------------------------------------------------------------

func TestServerRoutesUnsupportedToolNameToHandler(t *testing.T) {
	h := newFakeHandler()
	h.callFn = func(_ context.Context, req CallRequest, _ string) (*CallResponse, error) {
		return &CallResponse{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "unsupported tool: " + req.Tool}},
		}, nil
	}
	sockPath, _ := newTestServer(t, h, false)
	c := newRawClient(sockPath)

	body, _ := json.Marshal(CallRequest{RequestID: "1", Tool: "does_not_exist"})
	resp := c.do(t, context.Background(), http.MethodPost, PathCall, "application/json", body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (transport succeeds; handler reports the tool error)", resp.StatusCode)
	}
	var got CallResponse
	if err := json.Unmarshal(readBody(t, resp), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.IsError {
		t.Fatal("expected IsError=true")
	}
	if h.callCount() != 1 {
		t.Fatalf("handler.Call invoked %d times, want 1", h.callCount())
	}
	if h.calls[0].Tool != "does_not_exist" {
		t.Fatalf("handler saw tool %q, want %q", h.calls[0].Tool, "does_not_exist")
	}
}

// ---------------------------------------------------------------------------
// Pause / resume idempotence, health
// ---------------------------------------------------------------------------

func TestServerPauseResumeIdempotent(t *testing.T) {
	h := newFakeHandler()
	sockPath, _ := newTestServer(t, h, false)
	c := newRawClient(sockPath)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		resp := c.do(t, ctx, http.MethodPost, PathPause, "", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("pause #%d status = %d, want 200", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if h.pauseCount != 2 {
		t.Fatalf("handler.Pause invoked %d times, want 2", h.pauseCount)
	}

	for i := 0; i < 2; i++ {
		resp := c.do(t, ctx, http.MethodPost, PathResume, "", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("resume #%d status = %d, want 200", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if h.resumeCount != 2 {
		t.Fatalf("handler.Resume invoked %d times, want 2", h.resumeCount)
	}
}

func TestServerHealth(t *testing.T) {
	h := newFakeHandler()
	sockPath, _ := newTestServer(t, h, false)
	c := newRawClient(sockPath)

	resp := c.do(t, context.Background(), http.MethodGet, PathHealth, "", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var status HealthStatus
	if err := json.Unmarshal(readBody(t, resp), &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !status.OK {
		t.Fatal("expected OK=true")
	}
}

// ---------------------------------------------------------------------------
// Authorization: root socket requires it, session/control does not
// ---------------------------------------------------------------------------

func TestServerRootRequiresAuthorizationHeader(t *testing.T) {
	h := newFakeHandler()
	sockPath, _ := newTestServer(t, h, true) // requireAuth = true, simulating the root socket
	c := newRawClient(sockPath)

	body, _ := json.Marshal(CallRequest{RequestID: "1", Tool: "terminal"})

	t.Run("missing header is rejected", func(t *testing.T) {
		resp := c.do(t, context.Background(), http.MethodPost, PathCall, "application/json", body, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	if h.callCount() != 0 {
		t.Fatalf("handler.Call invoked %d times, want 0 (rejected before dispatch)", h.callCount())
	}

	t.Run("present header is forwarded and accepted", func(t *testing.T) {
		resp := c.do(t, context.Background(), http.MethodPost, PathCall, "application/json", body,
			map[string]string{"Authorization": "Bearer hdn_a_secret"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})

	if got := h.lastAuthHeader(); got != "Bearer hdn_a_secret" {
		t.Fatalf("handler saw authHeader %q, want %q", got, "Bearer hdn_a_secret")
	}
}

func TestServerSessionDoesNotRequireAuthorizationHeader(t *testing.T) {
	h := newFakeHandler()
	sockPath, _ := newTestServer(t, h, false) // requireAuth = false, simulating session/control
	c := newRawClient(sockPath)

	body, _ := json.Marshal(CallRequest{RequestID: "1", Tool: "terminal"})
	resp := c.do(t, context.Background(), http.MethodPost, PathCall, "application/json", body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no auth required on this socket)", resp.StatusCode)
	}
	if got := h.lastAuthHeader(); got != "" {
		t.Fatalf("handler saw authHeader %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

func TestServerHandlesClientCancellationCleanly(t *testing.T) {
	h := newFakeHandler()
	started := make(chan struct{})
	released := make(chan struct{})
	h.callFn = func(ctx context.Context, req CallRequest, _ string) (*CallResponse, error) {
		close(started)
		<-ctx.Done()
		close(released)
		return nil, ctx.Err()
	}
	sockPath, _ := newTestServer(t, h, false)
	c := newRawClient(sockPath)

	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(CallRequest{RequestID: "1", Tool: "terminal"})

	errCh := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix"+PathCall, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		_, err := c.Do(req)
		errCh <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}

	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected client request to fail after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client request never returned after cancellation")
	}

	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("server-side context was never canceled")
	}
}
