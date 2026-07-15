package control

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/rpc"
)

// fakeBroker records pause/resume calls so a test can assert the controller
// fanned out to every broker.
type fakeBroker struct {
	mu      sync.Mutex
	paused  int
	resumed int
}

func (b *fakeBroker) Pause(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.paused++
	return nil
}

func (b *fakeBroker) Resume(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resumed++
	return nil
}

func (b *fakeBroker) counts() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.paused, b.resumed
}

func TestEnterAdmitsWhenRunning(t *testing.T) {
	c := New(Config{Version: "v1"})
	ctx, release, ok := c.Enter(context.Background())
	if !ok {
		t.Fatal("Enter returned ok=false while running")
	}
	defer release()
	if ctx.Err() != nil {
		t.Fatalf("admitted context already cancelled: %v", ctx.Err())
	}
}

func TestPauseRejectsBothClasses(t *testing.T) {
	c := New(Config{Version: "v1"})
	if err := c.Pause(context.Background()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// Both credential classes reach Enter identically; a paused controller
	// rejects every call regardless of who is asking.
	for _, name := range []string{"user", "admin"} {
		if _, _, ok := c.Enter(context.Background()); ok {
			t.Fatalf("Enter admitted a %s call while paused", name)
		}
	}
	if !c.Paused() {
		t.Fatal("Paused() false after Pause")
	}
}

func TestPauseCancelsActiveGeneration(t *testing.T) {
	c := New(Config{Version: "v1"})
	ctx, release, ok := c.Enter(context.Background())
	if !ok {
		t.Fatal("Enter ok=false")
	}
	defer release()

	if err := c.Pause(context.Background()); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	select {
	case <-ctx.Done():
		// expected: pausing cancelled the in-flight call's context.
	case <-time.After(time.Second):
		t.Fatal("active call context not cancelled by Pause")
	}
}

func TestPauseFansOutToBrokers(t *testing.T) {
	b1, b2 := &fakeBroker{}, &fakeBroker{}
	c := New(Config{Version: "v1", Brokers: []Broker{b1, b2}})
	if err := c.Pause(context.Background()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	for i, b := range []*fakeBroker{b1, b2} {
		if p, _ := b.counts(); p != 1 {
			t.Fatalf("broker %d paused=%d, want 1", i, p)
		}
	}
}

func TestResumeInstallsFreshGenerationAndResumesBrokers(t *testing.T) {
	b := &fakeBroker{}
	c := New(Config{Version: "v1", Brokers: []Broker{b}})

	if err := c.Pause(context.Background()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := c.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// A call admitted after resume must NOT be born cancelled: it belongs to
	// the fresh generation, not the one Pause cancelled.
	ctx, release, ok := c.Enter(context.Background())
	if !ok {
		t.Fatal("Enter ok=false after resume")
	}
	defer release()
	if ctx.Err() != nil {
		t.Fatalf("post-resume call context cancelled: %v", ctx.Err())
	}

	if p, r := b.counts(); p != 1 || r != 1 {
		t.Fatalf("broker paused=%d resumed=%d, want 1,1", p, r)
	}
}

func TestPauseResumeIdempotent(t *testing.T) {
	b := &fakeBroker{}
	c := New(Config{Version: "v1", Brokers: []Broker{b}})
	for range 3 {
		if err := c.Pause(context.Background()); err != nil {
			t.Fatalf("Pause: %v", err)
		}
	}
	for range 3 {
		if err := c.Resume(context.Background()); err != nil {
			t.Fatalf("Resume: %v", err)
		}
	}
	// Idempotent calls must not error; broker tolerates repeats.
	if p, r := b.counts(); p != 3 || r != 3 {
		t.Fatalf("broker paused=%d resumed=%d, want 3,3", p, r)
	}
}

func TestStatusRedactedAndReady(t *testing.T) {
	c := New(Config{
		Version:        "v1.2.3",
		SessionHealthy: func(context.Context) bool { return true },
		CuaHealthy:     func(context.Context) bool { return true },
	})
	s := c.Status(context.Background())
	if !s.Ready() {
		t.Fatalf("expected ready, got %+v", s)
	}

	// Serialized status must carry only version + the four booleans.
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	want := map[string]bool{"version": true, "gateway": true, "session": true, "cua": true, "paused": true}
	for k := range m {
		if !want[k] {
			t.Fatalf("status leaked unexpected field %q: %s", k, data)
		}
	}
	if len(m) != len(want) {
		t.Fatalf("status field count = %d, want %d: %s", len(m), len(want), data)
	}
}

func TestStatusTransitions(t *testing.T) {
	sessionUp, cuaUp := true, true
	c := New(Config{
		Version:        "v1",
		SessionHealthy: func(context.Context) bool { return sessionUp },
		CuaHealthy:     func(context.Context) bool { return cuaUp },
	})

	if !c.Status(context.Background()).Ready() {
		t.Fatal("want ready initially")
	}

	cuaUp = false
	if c.Status(context.Background()).Ready() {
		t.Fatal("must not be ready when cua down")
	}
	cuaUp = true

	if err := c.Pause(context.Background()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if c.Status(context.Background()).Ready() {
		t.Fatal("must not be ready when paused")
	}
	if err := c.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !c.Status(context.Background()).Ready() {
		t.Fatal("want ready after resume")
	}
}

func TestPauseWritesRedactedStatusLine(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	c := New(Config{Version: "v9", Logger: logger})

	if err := c.Pause(context.Background()); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	line := buf.String()
	if !strings.Contains(line, "runtime paused") {
		t.Fatalf("pause did not log a status line: %q", line)
	}
	if !strings.Contains(line, `"paused":true`) {
		t.Fatalf("status line missing paused boolean: %q", line)
	}
}

func TestSocketHandlerCallUnsupported(t *testing.T) {
	c := New(Config{Version: "v1"})
	h := NewSocketHandler(c)
	if _, err := h.Call(context.Background(), rpc.CallRequest{Tool: "terminal"}, ""); err == nil {
		t.Fatal("control socket Call must fail")
	}
}

func TestSocketHandlerPauseResumeHealth(t *testing.T) {
	b := &fakeBroker{}
	c := New(Config{
		Version:        "v1",
		Brokers:        []Broker{b},
		SessionHealthy: func(context.Context) bool { return true },
		CuaHealthy:     func(context.Context) bool { return true },
	})
	h := NewSocketHandler(c)

	if err := h.Pause(context.Background()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	st, err := h.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if st.OK {
		t.Fatal("Health OK should be false while paused")
	}
	if !st.Paused {
		t.Fatal("Health Paused should be true while paused")
	}

	if err := h.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	st, err = h.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !st.OK || st.Paused {
		t.Fatalf("after resume Health=%+v, want OK & not paused", st)
	}
}
