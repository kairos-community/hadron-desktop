package process

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// testConfig returns a Manager config suited to tests: process-group fallback
// allowed (CI has no delegated cgroup), a short teardown grace so escalation
// paths run fast, and a deterministic incrementing id generator.
func testConfig() Config {
	var n atomic.Uint64
	// Seed the id prefix with the pid and start time so a leaf orphaned by a
	// crashed run can never collide with a fresh run's deterministic ids.
	seed := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	cfg := DefaultConfig()
	cfg.RequireCgroup = false
	cfg.AllowPgroupFallback = true
	cfg.TerminateGrace = 100 * time.Millisecond
	// Keep the final teardown-join bound short so the fallback-survivor escape
	// hatch (I1) is exercised quickly; the normal paths never reach it.
	cfg.ReapTimeout = 300 * time.Millisecond
	cfg.NewID = func() (string, error) {
		return fmt.Sprintf("test-%s-%016x", seed, n.Add(1)), nil
	}
	return cfg
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m := New(testConfig())
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------------------
// terminal
// ---------------------------------------------------------------------------

func TestTerminalExitStatusAndStreams(t *testing.T) {
	m := newTestManager(t)
	out, err := m.Bash(context.Background(), api.BashInput{
		Command: "echo out; echo err 1>&2; exit 7",
	})
	if err != nil {
		t.Fatalf("Terminal returned error: %v", err)
	}
	if out.Code != "" {
		t.Fatalf("expected success code, got %q (%s)", out.Code, out.Message)
	}
	if got := strings.TrimSpace(out.Stdout); got != "out" {
		t.Errorf("stdout = %q, want %q", got, "out")
	}
	if got := strings.TrimSpace(out.Stderr); got != "err" {
		t.Errorf("stderr = %q, want %q", got, "err")
	}
	if out.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", out.ExitCode)
	}
}

func TestBashHasNoTimeoutButHonoursContext(t *testing.T) {
	m := newTestManager(t)

	// bash imposes no deadline of its own, so the ONLY thing that can cut a long
	// command short is the caller's context. That is what keeps the emergency
	// pause and shutdown working now that the tool promises to run unbounded.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	out, err := m.Bash(ctx, api.BashInput{Command: "sleep 30"})
	if err != nil {
		t.Fatalf("Bash returned error: %v", err)
	}
	if out.Code != api.CodeDeadlineExceeded {
		t.Fatalf("code = %q, want DEADLINE_EXCEEDED once the context was cancelled", out.Code)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation took too long: %v", elapsed)
	}
}

func TestBashRunsPastTheOldTimeoutCeiling(t *testing.T) {
	// The old terminal tool clamped every call to MaxTimeout. bash must ignore
	// that entirely: a command running longer than the old ceiling is the whole
	// point of removing the bound.
	m := New(func() Config {
		c := testConfig()
		c.MaxTimeout = 150 * time.Millisecond
		return c
	}())
	t.Cleanup(func() { _ = m.Close() })

	out, err := m.Bash(context.Background(), api.BashInput{Command: "sleep 0.6; echo survived"})
	if err != nil {
		t.Fatalf("Bash returned error: %v", err)
	}
	if out.Code != "" {
		t.Fatalf("code = %q, want success: bash must outlive the old MaxTimeout", out.Code)
	}
	if !strings.Contains(out.Stdout, "survived") {
		t.Errorf("stdout = %q, want the command to have completed", out.Stdout)
	}
}

func TestBashOutputIsUnbounded(t *testing.T) {
	m := newTestManager(t)
	// ~4KiB, which the old terminal tool would have capped and flagged as
	// truncated. bash promises no output limit, so every byte must come back.
	const chunks = 200
	out, err := m.Bash(context.Background(), api.BashInput{
		Command: "for i in $(seq 1 200); do printf '0123456789abcdef1234'; done",
	})
	if err != nil {
		t.Fatalf("Bash returned error: %v", err)
	}
	if out.Code != "" {
		t.Fatalf("code = %q, want success: bash does not truncate", out.Code)
	}
	if want := chunks * 20; len(out.Stdout) != want {
		t.Errorf("captured %d bytes, want all %d", len(out.Stdout), want)
	}
}

func TestTerminalEnvAndCwd(t *testing.T) {
	m := newTestManager(t)
	dir := t.TempDir()
	out, err := m.Bash(context.Background(), api.BashInput{
		Command: "printf '%s|%s' \"$PWD\" \"$HADRON_TEST\"",
		Cwd:     dir,
		Env:     []string{"HADRON_TEST=xyzzy"},
	})
	if err != nil {
		t.Fatalf("Terminal returned error: %v", err)
	}
	// macOS-style /private symlinks are not a concern on linux; compare via
	// EvalSymlinks to be safe.
	wantDir, _ := filepath.EvalSymlinks(dir)
	parts := strings.SplitN(out.Stdout, "|", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected output %q", out.Stdout)
	}
	gotDir, _ := filepath.EvalSymlinks(strings.TrimSpace(parts[0]))
	if gotDir != wantDir {
		t.Errorf("cwd = %q, want %q", gotDir, wantDir)
	}
	if parts[1] != "xyzzy" {
		t.Errorf("env HADRON_TEST = %q, want xyzzy", parts[1])
	}
}

func TestTerminalInvalidArgument(t *testing.T) {
	m := newTestManager(t)
	out, err := m.Bash(context.Background(), api.BashInput{Command: ""})
	if err != nil {
		t.Fatalf("Terminal returned error: %v", err)
	}
	if out.Code != api.CodeInvalidArgument {
		t.Errorf("code = %q, want INVALID_ARGUMENT", out.Code)
	}
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// TestTerminalKillsGrandchildren asserts a backgrounded grandchild that
// outlives the shell is reaped by the tracked teardown: after Terminal returns
// (teardown has run), the grandchild must have stopped producing output. A
// growth check is used rather than a marker file because a SIGTERM-first
// sequence legitimately lets a process act during the grace window; what must
// not happen is the grandchild surviving.
func TestTerminalKillsGrandchildren(t *testing.T) {
	m := newTestManager(t)
	marker := filepath.Join(t.TempDir(), "ticks")
	// A grandchild loop that keeps appending until killed; the shell itself
	// returns immediately.
	_, err := m.Bash(context.Background(), api.BashInput{
		Command: fmt.Sprintf("(while true; do echo tick >> %q; sleep 0.2; done) & echo started", marker),
	})
	if err != nil {
		t.Fatalf("Terminal returned error: %v", err)
	}
	// Teardown has run. If any grandchild survived it would keep growing the
	// file; confirm it has stopped.
	time.Sleep(1 * time.Second)
	s1 := fileSize(marker)
	time.Sleep(1 * time.Second)
	s2 := fileSize(marker)
	if s2 != s1 {
		t.Fatalf("grandchild still writing after teardown: %d -> %d bytes", s1, s2)
	}
}

// ---------------------------------------------------------------------------
// process: start / poll / write / terminate
// ---------------------------------------------------------------------------
