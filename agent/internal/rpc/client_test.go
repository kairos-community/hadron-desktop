package rpc

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestClientCallHealthPauseResumeRoundTrip(t *testing.T) {
	h := newFakeHandler()
	sockPath, _ := newTestServer(t, h, false)
	c := NewClient(sockPath)
	t.Cleanup(c.Close)
	ctx := context.Background()

	resp, err := c.Call(ctx, CallRequest{RequestID: "1", Tool: "terminal"}, "")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp == nil || len(resp.Content) == 0 {
		t.Fatalf("unexpected response: %+v", resp)
	}

	status, err := c.Health(ctx, "")
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !status.OK {
		t.Fatal("expected OK=true")
	}

	if err := c.Pause(ctx, ""); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := c.Pause(ctx, ""); err != nil {
		t.Fatalf("second Pause: %v", err)
	}
	if h.pauseCount != 2 {
		t.Fatalf("pauseCount = %d, want 2", h.pauseCount)
	}

	if err := c.Resume(ctx, ""); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := c.Resume(ctx, ""); err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	if h.resumeCount != 2 {
		t.Fatalf("resumeCount = %d, want 2", h.resumeCount)
	}
}

func TestClientForwardsAuthorizationHeaderVerbatim(t *testing.T) {
	h := newFakeHandler()
	sockPath, _ := newTestServer(t, h, true) // root-style server
	c := NewClient(sockPath)
	t.Cleanup(c.Close)

	_, err := c.Call(context.Background(), CallRequest{RequestID: "1", Tool: "terminal"}, "Bearer hdn_a_secret")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := h.lastAuthHeader(); got != "Bearer hdn_a_secret" {
		t.Fatalf("handler saw authHeader %q, want %q", got, "Bearer hdn_a_secret")
	}
}

func TestClientMissingAuthorizationOnRootSocketIsRejected(t *testing.T) {
	h := newFakeHandler()
	sockPath, _ := newTestServer(t, h, true)
	c := NewClient(sockPath)
	t.Cleanup(c.Close)

	_, err := c.Call(context.Background(), CallRequest{RequestID: "1", Tool: "terminal"}, "")
	if err == nil {
		t.Fatal("expected an error when Authorization is required but not supplied")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if se.StatusCode != 401 {
		t.Fatalf("status code = %d, want 401", se.StatusCode)
	}
	if h.callCount() != 0 {
		t.Fatalf("handler.Call invoked %d times, want 0", h.callCount())
	}
}

func TestClientDialingMissingSocketFailsCleanly(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.sock")
	c := NewClient(missing)
	t.Cleanup(c.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.Call(ctx, CallRequest{RequestID: "1", Tool: "terminal"}, "")
	if err == nil {
		t.Fatal("expected an error dialing a missing socket")
	}

	if _, err := c.Health(ctx, ""); err == nil {
		t.Fatal("expected an error dialing a missing socket (Health)")
	}
	if err := c.Pause(ctx, ""); err == nil {
		t.Fatal("expected an error dialing a missing socket (Pause)")
	}
	if err := c.Resume(ctx, ""); err == nil {
		t.Fatal("expected an error dialing a missing socket (Resume)")
	}
}

func TestClientCallCancellationIsClean(t *testing.T) {
	h := newFakeHandler()
	started := make(chan struct{})
	h.callFn = func(ctx context.Context, _ CallRequest, _ string) (*CallResponse, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	sockPath, _ := newTestServer(t, h, false)
	c := NewClient(sockPath)
	t.Cleanup(c.Close)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Call(ctx, CallRequest{RequestID: "1", Tool: "terminal"}, "")
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
			t.Fatal("expected Call to return an error after context cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Call never returned after cancellation")
	}
}

func TestClientUnsupportedToolIsHandlerLevelNotTransportLevel(t *testing.T) {
	h := newFakeHandler()
	h.callFn = func(_ context.Context, req CallRequest, _ string) (*CallResponse, error) {
		return &CallResponse{IsError: true}, nil
	}
	sockPath, _ := newTestServer(t, h, false)
	c := NewClient(sockPath)
	t.Cleanup(c.Close)

	resp, err := c.Call(context.Background(), CallRequest{RequestID: "1", Tool: "no_such_tool"}, "")
	if err != nil {
		t.Fatalf("Call: %v (should succeed at the transport level)", err)
	}
	if !resp.IsError {
		t.Fatal("expected IsError=true from the handler-reported failure")
	}
}
