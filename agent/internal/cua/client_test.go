package cua

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const helperProcessEnv = "GO_WANT_CUA_HELPER"

type echoInput struct {
	Value string `json:"value"`
	Exit  bool   `json:"exit,omitempty"`
}

type blockInput struct {
	DelayMilliseconds int    `json:"delayMilliseconds"`
	StartedPath       string `json:"startedPath,omitempty"`
}

type imageInput struct {
	Bytes int `json:"bytes"`
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperProcessEnv) != "1" {
		return
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "cua-test-helper",
		Version: "v0.0.1",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(
		_ context.Context,
		_ *mcp.CallToolRequest,
		input echoInput,
	) (*mcp.CallToolResult, any, error) {
		if input.Exit {
			os.Exit(42)
		}
		return textResult(input.Value), nil, nil
	})

	var active atomic.Int64
	var maximum atomic.Int64
	mcp.AddTool(server, &mcp.Tool{Name: "block"}, func(
		ctx context.Context,
		_ *mcp.CallToolRequest,
		input blockInput,
	) (*mcp.CallToolResult, any, error) {
		current := active.Add(1)
		defer active.Add(-1)
		observeMaximum(&maximum, current)

		if input.StartedPath != "" {
			if err := os.WriteFile(input.StartedPath, []byte("started"), 0o600); err != nil {
				return nil, nil, fmt.Errorf("signalling block start: %w", err)
			}
		}

		timer := time.NewTimer(time.Duration(input.DelayMilliseconds) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-timer.C:
			return textResult(strconv.FormatInt(maximum.Load(), 10)), nil, nil
		}
	})

	// A screenshot-sized reply. Cua's capture tools answer with ImageContent,
	// which is orders of magnitude larger than every other reply the driver
	// sends, so it is the one payload shape worth exercising repeatedly.
	mcp.AddTool(server, &mcp.Tool{Name: "image"}, func(
		_ context.Context,
		_ *mcp.CallToolRequest,
		input imageInput,
	) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.ImageContent{Data: make([]byte, input.Bytes), MIMEType: "image/png"},
			},
		}, nil, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Cua helper server failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestClientStartsCallsAndCloses(t *testing.T) {
	client := startHelper(t)

	result, err := client.Call(t.Context(), "echo", echoInput{Value: "hello from Cua"})
	if err != nil {
		t.Fatalf("Call(echo) returned an error: %v", err)
	}
	if got := resultText(t, result); got != "hello from Cua" {
		t.Fatalf("Call(echo) returned %q, want %q", got, "hello from Cua")
	}

	if err := client.Close(); err != nil {
		t.Fatalf("first Close returned an error: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close returned an error: %v", err)
	}
}

func TestClientCancelsInflightCall(t *testing.T) {
	client := startHelper(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	startedPath := t.TempDir() + "/started"
	done := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, "block", blockInput{
			DelayMilliseconds: 10_000,
			StartedPath:       startedPath,
		})
		done <- err
	}()

	waitForPath(t, startedPath)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Call(block) returned %v, want context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call(block) did not return after context cancellation")
	}

	result, err := client.Call(t.Context(), "echo", echoInput{Value: "still connected"})
	if err != nil {
		t.Fatalf("Call(echo) after cancellation returned an error: %v", err)
	}
	if got := resultText(t, result); got != "still connected" {
		t.Fatalf("Call(echo) after cancellation returned %q, want %q", got, "still connected")
	}
}

func TestClientReportsChildExit(t *testing.T) {
	client := startHelper(t)

	_, err := client.Call(t.Context(), "echo", echoInput{Exit: true})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Call(echo) returned %v after the child exited, want EOF", err)
	}
}

func TestClientFailsFastAfterDisconnectWithoutReconnect(t *testing.T) {
	// No OnReady configured: the client must not spend any effort respawning
	// on its own, and every Call after the disconnect must fail immediately
	// with ErrDisconnected rather than touching the dead transport again.
	client := startHelper(t)

	if _, err := client.Call(t.Context(), "echo", echoInput{Exit: true}); !errors.Is(err, io.EOF) {
		t.Fatalf("Call(echo) returned %v after the child exited, want EOF", err)
	}

	if client.Connected() {
		t.Fatal("Connected() reported true immediately after the child exited")
	}

	_, err := client.Call(t.Context(), "echo", echoInput{Value: "should not run"})
	if !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Call(echo) after disconnection returned %v, want ErrDisconnected", err)
	}
}

func TestClientReconnectsAfterChildExit(t *testing.T) {
	events := make(chan bool, 8)
	client := startHelperWithOptions(t, func(connected bool) {
		events <- connected
	})

	if _, err := client.Call(t.Context(), "echo", echoInput{Exit: true}); !errors.Is(err, io.EOF) {
		t.Fatalf("Call(echo) returned %v after the child exited, want EOF", err)
	}

	select {
	case connected := <-events:
		if connected {
			t.Fatal("first OnReady event reported connected=true, want false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe a disconnected OnReady event")
	}

	select {
	case connected := <-events:
		if !connected {
			t.Fatal("second OnReady event reported connected=false, want true")
		}
	case <-time.After(reconnectBudget + 2*time.Second):
		t.Fatal("did not observe a reconnected OnReady event within the reconnect budget")
	}

	if !client.Connected() {
		t.Fatal("Connected() reported false after a reconnected OnReady event")
	}

	result, err := client.Call(t.Context(), "echo", echoInput{Value: "reconnected"})
	if err != nil {
		t.Fatalf("Call(echo) after reconnect returned an error: %v", err)
	}
	if got := resultText(t, result); got != "reconnected" {
		t.Fatalf("Call(echo) after reconnect returned %q, want %q", got, "reconnected")
	}
}

func TestClientCloseDuringReconnectDoesNotHang(t *testing.T) {
	events := make(chan bool, 8)
	client := startHelperWithOptions(t, func(connected bool) {
		events <- connected
	})

	if _, err := client.Call(t.Context(), "echo", echoInput{Exit: true}); !errors.Is(err, io.EOF) {
		t.Fatalf("Call(echo) returned %v after the child exited, want EOF", err)
	}

	// Race Close against the background reconnect campaign; it must return
	// promptly either way, and never leave a respawned child dangling.
	if err := client.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}
}

func TestClientSerializesCalls(t *testing.T) {
	client := startHelper(t)

	const callCount = 4
	start := make(chan struct{})
	results := make(chan *mcp.CallToolResult, callCount)
	errs := make(chan error, callCount)
	var calls sync.WaitGroup
	for range callCount {
		calls.Add(1)
		go func() {
			defer calls.Done()
			<-start
			result, err := client.Call(t.Context(), "block", blockInput{
				DelayMilliseconds: 100,
			})
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}

	close(start)
	calls.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("Call(block) returned an error: %v", err)
	}
	if t.Failed() {
		return
	}

	var observedMaximum int64
	for result := range results {
		observed, err := strconv.ParseInt(resultText(t, result), 10, 64)
		if err != nil {
			t.Fatalf("parsing block concurrency: %v", err)
		}
		observedMaximum = max(observedMaximum, observed)
	}
	if observedMaximum != 1 {
		t.Fatalf("maximum concurrent CallTool operations = %d, want 1", observedMaximum)
	}
}

// TestClientRepeatsImageCalls guards the appliance's capture path: on a live
// appliance every computer_use capture after the FIRST one failed with
// SESSION_UNAVAILABLE while text-returning calls kept working, and the driver
// process never died. A screenshot is the only large reply the driver sends, so
// this replays that shape -- several image calls in a row on one client, with a
// text call interleaved to show the session is still usable.
func TestClientRepeatsImageCalls(t *testing.T) {
	client := startHelper(t)

	// Roughly the size of a real appliance screenshot.
	const screenshotBytes = 22_000

	for attempt := range 5 {
		result, err := client.Call(t.Context(), "image", imageInput{Bytes: screenshotBytes})
		if err != nil {
			t.Fatalf("image call %d returned an error: %v", attempt+1, err)
		}
		if len(result.Content) != 1 {
			t.Fatalf("image call %d returned %d content items, want 1", attempt+1, len(result.Content))
		}
		image, ok := result.Content[0].(*mcp.ImageContent)
		if !ok {
			t.Fatalf("image call %d returned content type %T, want *mcp.ImageContent", attempt+1, result.Content[0])
		}
		if len(image.Data) != screenshotBytes {
			t.Fatalf("image call %d returned %d bytes, want %d", attempt+1, len(image.Data), screenshotBytes)
		}

		result, err = client.Call(t.Context(), "echo", echoInput{Value: "still alive"})
		if err != nil {
			t.Fatalf("echo after image call %d returned an error: %v", attempt+1, err)
		}
		if got := resultText(t, result); got != "still alive" {
			t.Fatalf("echo after image call %d returned %q", attempt+1, got)
		}
	}
}

func startHelper(t *testing.T) *Client {
	t.Helper()
	return startHelperWithOptions(t, nil)
}

func startHelperWithOptions(t *testing.T, onReady func(bool)) *Client {
	t.Helper()

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("finding test executable: %v", err)
	}
	client, err := Start(t.Context(), Options{
		Binary:  executable,
		Args:    []string{"-test.run=^TestHelperProcess$"},
		Env:     []string{helperProcessEnv + "=1"},
		OnReady: onReady,
	})
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
	})
	return client
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
	}
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()

	if len(result.Content) != 1 {
		t.Fatalf("tool returned %d content items, want 1", len(result.Content))
	}
	content, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("tool returned content type %T, want *mcp.TextContent", result.Content[0])
	}
	return content.Text
}

func observeMaximum(maximum *atomic.Int64, value int64) {
	for {
		current := maximum.Load()
		if value <= current || maximum.CompareAndSwap(current, value) {
			return
		}
	}
}

func waitForPath(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if err == nil {
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("checking block start signal: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("block tool did not start")
}
