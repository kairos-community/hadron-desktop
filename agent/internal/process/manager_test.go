package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	out, err := m.Terminal(context.Background(), api.TerminalInput{
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
	if out.Truncated {
		t.Errorf("unexpected truncation")
	}
}

func TestTerminalTimeout(t *testing.T) {
	m := newTestManager(t)
	start := time.Now()
	out, err := m.Terminal(context.Background(), api.TerminalInput{
		Command:   "sleep 30",
		TimeoutMs: ptr(200),
	})
	if err != nil {
		t.Fatalf("Terminal returned error: %v", err)
	}
	if out.Code != api.CodeDeadlineExceeded {
		t.Fatalf("code = %q, want DEADLINE_EXCEEDED", out.Code)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}

func TestTerminalTruncation(t *testing.T) {
	m := newTestManager(t)
	out, err := m.Terminal(context.Background(), api.TerminalInput{
		// Emit ~4KiB but cap capture at 100 bytes.
		Command:        "for i in $(seq 1 200); do printf '0123456789abcdef1234'; done",
		MaxOutputBytes: ptr(100),
	})
	if err != nil {
		t.Fatalf("Terminal returned error: %v", err)
	}
	if !out.Truncated {
		t.Fatalf("expected Truncated=true")
	}
	if out.Code != api.CodeOutputTruncated {
		t.Errorf("code = %q, want OUTPUT_TRUNCATED", out.Code)
	}
	if len(out.Stdout) != 100 {
		t.Errorf("captured %d bytes, want exactly 100", len(out.Stdout))
	}
}

func TestTerminalEnvAndCwd(t *testing.T) {
	m := newTestManager(t)
	dir := t.TempDir()
	out, err := m.Terminal(context.Background(), api.TerminalInput{
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
	out, err := m.Terminal(context.Background(), api.TerminalInput{Command: ""})
	if err != nil {
		t.Fatalf("Terminal returned error: %v", err)
	}
	if out.Code != api.CodeInvalidArgument {
		t.Errorf("code = %q, want INVALID_ARGUMENT", out.Code)
	}
}

func TestTerminalTimeoutClampedToMax(t *testing.T) {
	m := New(func() Config {
		c := testConfig()
		c.MaxTimeout = 150 * time.Millisecond
		return c
	}())
	t.Cleanup(func() { _ = m.Close() })
	out, _ := m.Terminal(context.Background(), api.TerminalInput{
		Command:   "sleep 30",
		TimeoutMs: ptr(3_600_000), // 1 hour requested; clamped to 150ms
	})
	if out.Code != api.CodeDeadlineExceeded {
		t.Fatalf("code = %q, want DEADLINE_EXCEEDED (max-clamp)", out.Code)
	}
}

// fileSize returns the size of path, or 0 if it does not exist.
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
	_, err := m.Terminal(context.Background(), api.TerminalInput{
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

func TestProcessStartPollExit(t *testing.T) {
	m := newTestManager(t)
	start, err := m.Process(context.Background(), api.ProcessInput{
		Action:  api.ProcessStart,
		Command: "echo hello; exit 3",
	})
	if err != nil {
		t.Fatalf("start error: %v", err)
	}
	if start.Code != "" {
		t.Fatalf("start failed: %q %s", start.Code, start.Message)
	}
	if start.ProcessID == "" {
		t.Fatal("empty process id")
	}
	if start.PID != 0 {
		t.Errorf("PID must never be exposed, got %d", start.PID)
	}

	// Poll until exit.
	deadline := time.Now().Add(5 * time.Second)
	var last api.ProcessOutput
	var stdout strings.Builder
	for time.Now().Before(deadline) {
		p, err := m.Process(context.Background(), api.ProcessInput{
			Action:    api.ProcessPoll,
			ProcessID: start.ProcessID,
			TimeoutMs: ptr(200),
		})
		if err != nil {
			t.Fatalf("poll error: %v", err)
		}
		stdout.WriteString(p.Stdout)
		last = p
		if !p.Running {
			break
		}
	}
	if last.Running {
		t.Fatal("process still running after deadline")
	}
	if !strings.Contains(stdout.String(), "hello") {
		t.Errorf("stdout %q missing 'hello'", stdout.String())
	}
	if last.ExitCode == nil || *last.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", last.ExitCode)
	}
}

func TestProcessWrite(t *testing.T) {
	m := newTestManager(t)
	start, err := m.Process(context.Background(), api.ProcessInput{
		Action:  api.ProcessStart,
		Command: "while read line; do echo \"got:$line\"; done",
	})
	if err != nil || start.Code != "" {
		t.Fatalf("start failed: %v %q", err, start.Code)
	}
	if _, err := m.Process(context.Background(), api.ProcessInput{
		Action:    api.ProcessWrite,
		ProcessID: start.ProcessID,
		Input:     "ping\n",
	}); err != nil {
		t.Fatalf("write error: %v", err)
	}

	got := pollUntil(t, m, start.ProcessID, func(s string) bool {
		return strings.Contains(s, "got:ping")
	})
	if !strings.Contains(got, "got:ping") {
		t.Errorf("stdout %q missing echo of input", got)
	}
	_, _ = m.Process(context.Background(), api.ProcessInput{
		Action: api.ProcessTerminate, ProcessID: start.ProcessID,
	})
}

func TestProcessPTYEcho(t *testing.T) {
	m := newTestManager(t)
	start, err := m.Process(context.Background(), api.ProcessInput{
		Action:  api.ProcessStart,
		Command: "cat",
		PTY:     true,
	})
	if err != nil || start.Code != "" {
		t.Fatalf("start failed: %v %q", err, start.Code)
	}
	if _, err := m.Process(context.Background(), api.ProcessInput{
		Action:    api.ProcessWrite,
		ProcessID: start.ProcessID,
		Input:     "hi-pty\n",
	}); err != nil {
		t.Fatalf("write error: %v", err)
	}
	// A pty echoes input back on the master, so we should see it even though
	// cat only re-emits after a newline.
	got := pollUntil(t, m, start.ProcessID, func(s string) bool {
		return strings.Contains(s, "hi-pty")
	})
	if !strings.Contains(got, "hi-pty") {
		t.Errorf("pty output %q missing echoed input", got)
	}
	_, _ = m.Process(context.Background(), api.ProcessInput{
		Action: api.ProcessTerminate, ProcessID: start.ProcessID,
	})
}

func TestProcessUnknownID(t *testing.T) {
	m := newTestManager(t)
	for _, action := range []api.ProcessAction{api.ProcessPoll, api.ProcessWrite, api.ProcessTerminate} {
		in := api.ProcessInput{Action: action, ProcessID: "does-not-exist"}
		if action == api.ProcessWrite {
			in.Input = "x"
		}
		out, err := m.Process(context.Background(), in)
		if err != nil {
			t.Fatalf("%s error: %v", action, err)
		}
		if out.Code != api.CodeNotFound {
			t.Errorf("%s code = %q, want NOT_FOUND", action, out.Code)
		}
	}
}

func TestProcessTerminate(t *testing.T) {
	m := newTestManager(t)
	start, _ := m.Process(context.Background(), api.ProcessInput{
		Action: api.ProcessStart, Command: "sleep 100",
	})
	out, err := m.Process(context.Background(), api.ProcessInput{
		Action: api.ProcessTerminate, ProcessID: start.ProcessID,
	})
	if err != nil {
		t.Fatalf("terminate error: %v", err)
	}
	if out.Running {
		t.Errorf("process still running after terminate")
	}
}

// TestProcessLiveLimit asserts the 16-live cap returns RESOURCE_EXHAUSTED and
// that terminating processes frees slots.
func TestProcessLiveLimit(t *testing.T) {
	m := newTestManager(t)
	var ids []string
	for i := 0; i < DefaultMaxLive; i++ {
		out, err := m.Process(context.Background(), api.ProcessInput{
			Action: api.ProcessStart, Command: "sleep 100",
		})
		if err != nil || out.Code != "" {
			t.Fatalf("start %d failed: %v %q", i, err, out.Code)
		}
		ids = append(ids, out.ProcessID)
	}
	// 17th must be rejected.
	over, _ := m.Process(context.Background(), api.ProcessInput{
		Action: api.ProcessStart, Command: "sleep 100",
	})
	if over.Code != api.CodeResourceExhausted {
		t.Fatalf("17th start code = %q, want RESOURCE_EXHAUSTED", over.Code)
	}
	// Terminate one and confirm a slot frees.
	_, _ = m.Process(context.Background(), api.ProcessInput{
		Action: api.ProcessTerminate, ProcessID: ids[0],
	})
	again, err := m.Process(context.Background(), api.ProcessInput{
		Action: api.ProcessStart, Command: "sleep 100",
	})
	if err != nil || again.Code != "" {
		t.Fatalf("start after terminate failed: %v %q", err, again.Code)
	}
}

// TestProcessConcurrentStartCap hammers start from many goroutines and asserts
// the live cap is never exceeded (atomic reservation).
func TestProcessConcurrentStartCap(t *testing.T) {
	m := newTestManager(t)
	const attempts = 40
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := m.Process(context.Background(), api.ProcessInput{
				Action: api.ProcessStart, Command: "sleep 100",
			})
			if err == nil && out.Code == "" {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := ok.Load(); got > int32(DefaultMaxLive) {
		t.Fatalf("started %d processes, cap is %d", got, DefaultMaxLive)
	}
}

func TestManagerCloseKillsGrandchildren(t *testing.T) {
	m := New(testConfig())
	marker := filepath.Join(t.TempDir(), "ticks")
	// A process tree: sh runs a foreground sleep and backgrounds a grandchild
	// loop that appends until killed. Close must reap the whole tree.
	start, err := m.Process(context.Background(), api.ProcessInput{
		Action:  api.ProcessStart,
		Command: fmt.Sprintf("(while true; do echo tick >> %q; sleep 0.2; done) & sleep 100", marker),
	})
	if err != nil || start.Code != "" {
		t.Fatalf("start failed: %v %q", err, start.Code)
	}
	// Give the tree a moment to fork its grandchildren.
	time.Sleep(300 * time.Millisecond)
	if err := m.Close(); err != nil {
		t.Fatalf("close error: %v", err)
	}
	// After Close, the grandchild loop must have stopped growing the file.
	time.Sleep(1 * time.Second)
	s1 := fileSize(marker)
	time.Sleep(1 * time.Second)
	s2 := fileSize(marker)
	if s2 != s1 {
		t.Fatalf("grandchild survived Close, still writing: %d -> %d bytes", s1, s2)
	}
}

// TestFallbackTeardownDoesNotHang exercises the I1 escape hatch. In
// pgroup-fallback mode (no cgroup) a setsid grandchild escapes the process
// group AND keeps the inherited stdout/stderr pipe write-ends open. Without a
// bounded final join, waitChild's readersWG.Wait() — and thus reap's wait on
// t.done — would block forever because the pipes never EOF and the escapee is
// out of reach of the process-group SIGKILL. Teardown must still return in
// bounded time by force-closing the child IO.
func TestFallbackTeardownDoesNotHang(t *testing.T) {
	// Force genuine pgroup-fallback (leaf == nil) regardless of whether the
	// host has a usable delegated cgroup: point at a bogus root so cgroupProbe
	// fails and beginCgroup takes the AllowPgroupFallback branch. In real
	// cgroup mode cgroup.kill would reap the escapee and the bound would never
	// be needed — this test must exercise the escape hatch itself.
	cfg := testConfig()
	cfg.CgroupRoot = filepath.Join(t.TempDir(), "no-such-cgroup")
	m := New(cfg)
	t.Cleanup(func() { _ = m.Close() })
	if m.cgroupOK {
		t.Fatal("expected cgroup placement to be unavailable (forcing pgroup fallback)")
	}
	start, err := m.Process(context.Background(), api.ProcessInput{
		Action: api.ProcessStart,
		// The shell replaces itself with a short sleep (exec) while a setsid
		// grandchild leaves the process group and holds fds 1/2 open for a
		// long time.
		Command: `setsid sh -c "sleep 60" & exec sleep 0.1`,
	})
	if err != nil || start.Code != "" {
		t.Fatalf("start failed: %v %q", err, start.Code)
	}
	// Let the shell exit and the grandchild detach.
	time.Sleep(300 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		_, _ = m.Process(context.Background(), api.ProcessInput{
			Action: api.ProcessTerminate, ProcessID: start.ProcessID,
		})
		close(done)
	}()
	select {
	case <-done:
		// Returned in bounded time: success.
	case <-time.After(15 * time.Second):
		t.Fatal("terminate hung: fallback grandchild held the pipes open")
	}

	// Close must also return promptly even though the escapee survives.
	closed := make(chan struct{})
	go func() { _ = m.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(15 * time.Second):
		t.Fatal("Close hung after a fallback grandchild survived teardown")
	}
}

// TestProcessStartIDGenerationFailure covers M3: when the opaque-id generator
// fails (crypto/rand error, modelled via the NewID seam) the start must return
// INTERNAL and mint no handle — never a guessable/PID-derived fallback id — and
// must not leak the reserved live slot.
func TestProcessStartIDGenerationFailure(t *testing.T) {
	cfg := testConfig()
	cfg.NewID = func() (string, error) { return "", errors.New("rand unavailable") }
	m := New(cfg)
	t.Cleanup(func() { _ = m.Close() })

	// Repeat past the live cap: if the reserved slot leaked on failure, later
	// calls would report RESOURCE_EXHAUSTED instead of INTERNAL.
	for i := 0; i < DefaultMaxLive+2; i++ {
		out, err := m.Process(context.Background(), api.ProcessInput{
			Action: api.ProcessStart, Command: "echo hi",
		})
		if err != nil {
			t.Fatalf("iter %d: unexpected error: %v", i, err)
		}
		if out.Code != api.CodeInternal {
			t.Fatalf("iter %d: code = %q, want INTERNAL (live slot leaked?)", i, out.Code)
		}
		if out.ProcessID != "" {
			t.Errorf("iter %d: a handle was minted on id failure: %q", i, out.ProcessID)
		}
	}

	// Terminal shares the seam and must also fail cleanly.
	tout, err := m.Terminal(context.Background(), api.TerminalInput{Command: "echo hi"})
	if err != nil {
		t.Fatalf("terminal unexpected error: %v", err)
	}
	if tout.Code != api.CodeInternal {
		t.Fatalf("terminal code = %q, want INTERNAL", tout.Code)
	}
}

// pollUntil polls a process, accumulating stdout, until pred is satisfied, the
// process exits, or a deadline elapses.
func pollUntil(t *testing.T, m *Manager, id string, pred func(string) bool) string {
	t.Helper()
	var sb strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := m.Process(context.Background(), api.ProcessInput{
			Action: api.ProcessPoll, ProcessID: id, TimeoutMs: ptr(200),
		})
		if err != nil {
			t.Fatalf("poll error: %v", err)
		}
		sb.WriteString(p.Stdout)
		if pred(sb.String()) {
			return sb.String()
		}
		if !p.Running {
			return sb.String()
		}
	}
	return sb.String()
}
