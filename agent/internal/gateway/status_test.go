package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/api"
	"github.com/mudler/hadron-desktop/agent/internal/control"
)

// TestStatusWriterSerializesElevenFields asserts the redacted status.json
// carries EXACTLY the eleven fields the hadron-status / first-run scripts read,
// with the correct names and values (a simulated in-flight computer_use ->
// computer_use_active true; session-down + not-paused -> degraded true & ready
// false), that it is written atomically (no temp file left behind), and that it
// never contains a bearer/token/secret.
func TestStatusWriterSerializesElevenFields(t *testing.T) {
	statusDir := t.TempDir()
	statusFile := filepath.Join(statusDir, "status.json")

	f := newFixture(t, func(cfg *Config, fx *fixture) {
		// Session probe DOWN, not paused: ready must be false and degraded true.
		fx.controller = control.New(control.Config{
			Version:        "v-test",
			SessionHealthy: func(context.Context) bool { return false },
			CuaHealthy:     func(context.Context) bool { return true },
		})
		cfg.Controller = fx.controller
		cfg.ListenAddr = "127.0.0.1:7443"
		cfg.CertFingerprint = "sha256:deadbeefcafe"
		cfg.MDNS = true
		cfg.StatusFile = statusFile
	})

	// Simulate a remote computer_use call currently executing.
	f.g.computerUse.Add(1)

	if err := f.g.writeStatusFile(context.Background()); err != nil {
		t.Fatalf("writeStatusFile: %v", err)
	}

	data, err := os.ReadFile(statusFile)
	if err != nil {
		t.Fatalf("read status.json: %v", err)
	}

	// Exactly the eleven contract field names, no more, no fewer.
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal status.json: %v (%s)", err, data)
	}
	want := []string{
		"version", "gateway", "session", "cua", "paused", "degraded",
		"computer_use_active", "ready", "cert_fingerprint", "mdns", "listen",
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Fatalf("status.json missing field %q: %s", k, data)
		}
	}
	if len(m) != len(want) {
		t.Fatalf("status.json field count = %d, want %d: %s", len(m), len(want), data)
	}

	// Values.
	var st RedactedStatus
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("unmarshal RedactedStatus: %v", err)
	}
	if st.Version != "v-test" {
		t.Errorf("version = %q, want v-test", st.Version)
	}
	if !st.Gateway {
		t.Error("gateway = false, want true")
	}
	if st.Session {
		t.Error("session = true, want false (probe down)")
	}
	if !st.Cua {
		t.Error("cua = false, want true")
	}
	if st.Paused {
		t.Error("paused = true, want false")
	}
	if st.Ready {
		t.Error("ready = true, want false (session down)")
	}
	if !st.Degraded {
		t.Error("degraded = false, want true (up, not ready, not paused)")
	}
	if !st.ComputerUseActive {
		t.Error("computer_use_active = false, want true (in-flight call simulated)")
	}
	if st.CertFingerprint != "sha256:deadbeefcafe" {
		t.Errorf("cert_fingerprint = %q, want sha256:deadbeefcafe", st.CertFingerprint)
	}
	if !st.MDNS {
		t.Error("mdns = false, want true")
	}
	if st.Listen != "127.0.0.1:7443" {
		t.Errorf("listen = %q, want 127.0.0.1:7443", st.Listen)
	}

	// Redaction: no secret material anywhere in the serialized JSON.
	lower := strings.ToLower(string(data))
	for _, bad := range []string{"bearer", "token", "hdn_", "authorization", "digest"} {
		if strings.Contains(lower, bad) {
			t.Fatalf("status.json leaked %q: %s", bad, data)
		}
	}

	// Atomic write: only the final status.json remains, no leftover temp file.
	entries, err := os.ReadDir(statusDir)
	if err != nil {
		t.Fatalf("read status dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "status.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("status dir = %v, want only [status.json] (atomic write leaves no temp)", names)
	}

	// The published file is world-readable (redacted) as the scripts expect.
	info, err := os.Stat(statusFile)
	if err != nil {
		t.Fatalf("stat status.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("status.json mode = %o, want 0644", perm)
	}
}

// TestStatusWriterReflectsPaused asserts a paused controller yields paused=true
// and, per the derivation, degraded=false (paused is a distinct state from
// degraded) with ready=false.
func TestStatusWriterReflectsPaused(t *testing.T) {
	statusFile := filepath.Join(t.TempDir(), "status.json")
	f := newFixture(t, func(cfg *Config, fx *fixture) {
		cfg.StatusFile = statusFile
	})

	if err := f.controller.Pause(context.Background()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := f.g.writeStatusFile(context.Background()); err != nil {
		t.Fatalf("writeStatusFile: %v", err)
	}

	var st RedactedStatus
	data, err := os.ReadFile(statusFile)
	if err != nil {
		t.Fatalf("read status.json: %v", err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !st.Paused {
		t.Error("paused = false, want true")
	}
	if st.Ready {
		t.Error("ready = true, want false while paused")
	}
	if st.Degraded {
		t.Error("degraded = true, want false while paused (paused != degraded)")
	}
}

// TestStatusWriterDisabledByEmptyPath asserts the writer is a no-op when no
// status file is configured -- so the contract and main tests never need a
// writable status directory.
func TestStatusWriterDisabledByEmptyPath(t *testing.T) {
	f := newFixture(t) // default fixture: StatusFile unset
	if f.g.statusFile != "" {
		t.Fatalf("default fixture statusFile = %q, want empty", f.g.statusFile)
	}
	if err := f.g.writeStatusFile(context.Background()); err != nil {
		t.Fatalf("writeStatusFile with no path must be a no-op, got %v", err)
	}
	// RunStatusWriter must also return immediately rather than block.
	done := make(chan struct{})
	go func() {
		f.g.RunStatusWriter(context.Background(), time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunStatusWriter did not return for an unset status file")
	}
}

// TestComputerUseActiveTracksInFlight drives a real computer_use call through
// the gateway with a blocking session broker and asserts ComputerUseActive
// flips true while the call is executing and false after it returns.
func TestComputerUseActiveTracksInFlight(t *testing.T) {
	entered := make(chan struct{}, 1)
	hold := make(chan struct{})
	f := newFixture(t, func(cfg *Config, fx *fixture) {
		fx.session.entered = entered
		fx.session.hold = hold
	})
	session := connectMCP(t, f.g, f.creds.userBearer)

	if f.g.ComputerUseActive() {
		t.Fatal("computer_use_active true before any call")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		callTool(t, session, api.ToolComputerUse, validArgs(api.ToolComputerUse))
	}()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(hold)
		t.Fatal("computer_use never reached the broker")
	}

	if !f.g.ComputerUseActive() {
		close(hold)
		t.Fatal("computer_use_active false while a computer_use call is executing")
	}

	close(hold)
	<-done

	// Once the call returns the counter must drop back to zero.
	deadline := time.Now().Add(2 * time.Second)
	for f.g.ComputerUseActive() {
		if time.Now().After(deadline) {
			t.Fatal("computer_use_active still true after the call returned")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNonComputerUseDoesNotFlipActive proves only computer_use is counted: an
// in-flight terminal call must NOT set computer_use_active.
func TestNonComputerUseDoesNotFlipActive(t *testing.T) {
	entered := make(chan struct{}, 1)
	hold := make(chan struct{})
	f := newFixture(t, func(cfg *Config, fx *fixture) {
		fx.session.entered = entered
		fx.session.hold = hold
	})
	session := connectMCP(t, f.g, f.creds.userBearer)

	done := make(chan struct{})
	go func() {
		defer close(done)
		callTool(t, session, api.ToolTerminal, validArgs(api.ToolTerminal))
	}()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(hold)
		t.Fatal("terminal call never reached the broker")
	}

	if f.g.ComputerUseActive() {
		close(hold)
		t.Fatal("computer_use_active flipped true for a terminal call")
	}

	close(hold)
	<-done
}
