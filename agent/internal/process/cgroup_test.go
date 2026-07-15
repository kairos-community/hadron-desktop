package process

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// delegatedCgroupRoot returns a writable delegated cgroup-v2 root, or skips the
// test when the environment lacks one (as CI does). Detection is by attempting
// to create and remove a probe leaf; EACCES / EROFS / read-only mounts cause a
// graceful skip rather than a failure.
func delegatedCgroupRoot(t *testing.T) string {
	t.Helper()
	root, err := detectCgroupRoot()
	if err != nil {
		t.Skipf("no cgroup-v2 root: %v", err)
	}
	if err := cgroupProbe(root); err != nil {
		t.Skipf("cgroup root %q not writable/delegated: %v", root, err)
	}
	return root
}

// TestCgroupPlacementAndKill exercises the real cgroup path: a process is
// placed in a leaf whose cgroup.procs contains its pid, and cgroup.kill reaps
// the whole subtree. Skipped when no delegated cgroup is writable.
func TestCgroupPlacementAndKill(t *testing.T) {
	root := delegatedCgroupRoot(t)

	cfg := testConfig()
	cfg.RequireCgroup = true
	cfg.AllowPgroupFallback = false
	cfg.CgroupRoot = root
	m := New(cfg)
	t.Cleanup(func() { _ = m.Close() })
	if !m.cgroupOK {
		t.Skip("cgroup probe failed despite writable root")
	}

	start, err := m.Process(context.Background(), api.ProcessInput{
		Action:  api.ProcessStart,
		Command: "sleep 100",
	})
	if err != nil || start.Code != "" {
		t.Fatalf("start failed: %v %q %s", err, start.Code, start.Message)
	}

	h := m.lookup(start.ProcessID)
	if h == nil || h.tracked == nil || h.tracked.leaf == nil {
		t.Fatal("expected a cgroup leaf to be assigned")
	}
	leafPath := h.tracked.leaf.path

	// The leaf's cgroup.procs must contain a pid (we do not surface it, but it
	// must be non-empty on disk).
	procsFile := filepath.Join(leafPath, "cgroup.procs")
	data, err := os.ReadFile(procsFile)
	if err != nil {
		t.Fatalf("read cgroup.procs: %v", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		t.Fatalf("cgroup.procs empty; process was not placed in the leaf")
	}

	// Terminate drives cgroup.kill + leaf removal.
	if _, err := m.Process(context.Background(), api.ProcessInput{
		Action: api.ProcessTerminate, ProcessID: start.ProcessID,
	}); err != nil {
		t.Fatalf("terminate error: %v", err)
	}

	// The leaf directory must be gone.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(leafPath); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("cgroup leaf %q was not removed after terminate", leafPath)
}

// TestCgroupKillsPgroupEscapee proves the escape-proof guarantee: a grandchild
// that calls setsid leaves the process group entirely, so a negative-pgid kill
// cannot reach it — only cgroup.kill can. After terminate, the escapee must
// have stopped running. Skipped without a delegated cgroup (the process-group
// fallback genuinely cannot contain a setsid escapee, by design).
func TestCgroupKillsPgroupEscapee(t *testing.T) {
	root := delegatedCgroupRoot(t)
	cfg := testConfig()
	cfg.RequireCgroup = true
	cfg.AllowPgroupFallback = false
	cfg.CgroupRoot = root
	m := New(cfg)
	t.Cleanup(func() { _ = m.Close() })
	if !m.cgroupOK {
		t.Skip("cgroup probe failed despite writable root")
	}

	marker := filepath.Join(t.TempDir(), "ticks")
	// The grandchild escapes into its own session via setsid, then loops.
	start, err := m.Process(context.Background(), api.ProcessInput{
		Action: api.ProcessStart,
		Command: fmt.Sprintf(
			`setsid sh -c 'while true; do echo tick >> %q; sleep 0.2; done' & sleep 100`,
			marker),
	})
	if err != nil || start.Code != "" {
		t.Fatalf("start failed: %v %q", err, start.Code)
	}
	time.Sleep(400 * time.Millisecond)

	if _, err := m.Process(context.Background(), api.ProcessInput{
		Action: api.ProcessTerminate, ProcessID: start.ProcessID,
	}); err != nil {
		t.Fatalf("terminate error: %v", err)
	}

	// The setsid escapee must have stopped growing the file.
	time.Sleep(700 * time.Millisecond)
	s1 := fileSizeOf(marker)
	time.Sleep(700 * time.Millisecond)
	s2 := fileSizeOf(marker)
	if s2 != s1 {
		t.Fatalf("setsid grandchild escaped cgroup teardown: %d -> %d bytes", s1, s2)
	}
}

func fileSizeOf(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// TestRequireCgroupWithoutDelegationFails asserts that when cgroup placement is
// required but the configured root is unusable, start fails cleanly rather than
// silently running unconfined.
func TestRequireCgroupWithoutDelegationFails(t *testing.T) {
	cfg := testConfig()
	cfg.RequireCgroup = true
	cfg.AllowPgroupFallback = false
	// A path that certainly is not a writable delegated cgroup.
	cfg.CgroupRoot = filepath.Join(t.TempDir(), "not-a-cgroup")
	m := New(cfg)
	t.Cleanup(func() { _ = m.Close() })

	out, err := m.Process(context.Background(), api.ProcessInput{
		Action: api.ProcessStart, Command: "sleep 100",
	})
	if err != nil {
		t.Fatalf("start error: %v", err)
	}
	if out.Code != api.CodeInternal {
		t.Fatalf("code = %q, want INTERNAL when required cgroup is unavailable", out.Code)
	}
}

// TestPgroupFallback asserts that with fallback enabled and no cgroup, a
// process still starts and is tracked (leaf nil) and killed via its pgroup.
func TestPgroupFallback(t *testing.T) {
	cfg := testConfig() // RequireCgroup=false, AllowPgroupFallback=true
	cfg.CgroupRoot = filepath.Join(t.TempDir(), "not-a-cgroup")
	m := New(cfg)
	t.Cleanup(func() { _ = m.Close() })

	start, err := m.Process(context.Background(), api.ProcessInput{
		Action: api.ProcessStart, Command: "sleep 100",
	})
	if err != nil || start.Code != "" {
		t.Fatalf("start failed: %v %q", err, start.Code)
	}
	h := m.lookup(start.ProcessID)
	if h == nil || h.tracked == nil {
		t.Fatal("process not tracked")
	}
	if h.tracked.leaf != nil {
		t.Fatal("expected no cgroup leaf in fallback mode")
	}
	out, err := m.Process(context.Background(), api.ProcessInput{
		Action: api.ProcessTerminate, ProcessID: start.ProcessID,
	})
	if err != nil {
		t.Fatalf("terminate error: %v", err)
	}
	if out.Running {
		t.Fatal("process still running after pgroup terminate")
	}
}
