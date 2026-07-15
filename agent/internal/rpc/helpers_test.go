package rpc

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeHandler is a scriptable, concurrency-safe Handler used by every test
// in this package.
type fakeHandler struct {
	mu sync.Mutex

	callFn func(ctx context.Context, req CallRequest, authHeader string) (*CallResponse, error)

	calls          []CallRequest
	authHeaders    []string
	pauseCount     int
	resumeCount    int
	paused         bool
	healthErr      error
	pauseErr       error
	resumeErr      error
	callErrDefault error
}

func newFakeHandler() *fakeHandler {
	return &fakeHandler{}
}

func (f *fakeHandler) Call(ctx context.Context, req CallRequest, authHeader string) (*CallResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.authHeaders = append(f.authHeaders, authHeader)
	fn := f.callFn
	defaultErr := f.callErrDefault
	f.mu.Unlock()

	if fn != nil {
		return fn(ctx, req, authHeader)
	}
	if defaultErr != nil {
		return nil, defaultErr
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "ok:" + req.Tool}},
	}, nil
}

func (f *fakeHandler) Health(ctx context.Context) (HealthStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.healthErr != nil {
		return HealthStatus{}, f.healthErr
	}
	return HealthStatus{OK: true, Paused: f.paused}, nil
}

func (f *fakeHandler) Pause(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pauseErr != nil {
		return f.pauseErr
	}
	f.pauseCount++
	f.paused = true
	return nil
}

func (f *fakeHandler) Resume(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resumeErr != nil {
		return f.resumeErr
	}
	f.resumeCount++
	f.paused = false
	return nil
}

func (f *fakeHandler) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeHandler) lastAuthHeader() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.authHeaders) == 0 {
		return ""
	}
	return f.authHeaders[len(f.authHeaders)-1]
}

var errBoom = errors.New("boom")

// startTestServer starts s on a fresh Unix socket under t.TempDir() and
// arranges for it to be shut down when the test ends. It returns the socket
// path.
func startTestServer(t *testing.T, s *Server) string {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- s.Serve(l)
	}()

	t.Cleanup(func() {
		_ = s.Shutdown(context.Background())
		if err := <-done; err != nil {
			t.Errorf("Serve returned error: %v", err)
		}
	})

	return sockPath
}
