package process

import (
	"os"
	"testing"
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
