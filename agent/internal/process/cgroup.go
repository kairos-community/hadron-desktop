// cgroup-v2 leaf management for tracked processes.
//
// Every process this package spawns is placed, when possible, into a private
// cgroup-v2 leaf created directly below the service's own (delegated) cgroup.
// The leaf is the authoritative kill boundary: writing "1" to its cgroup.kill
// atomically SIGKILLs every task in the subtree, so grandchildren that escape
// the process group (via setsid) still cannot escape teardown.
//
// Placement is best-effort by policy: a Manager configured with RequireCgroup
// treats a placement failure as fatal, while one configured with
// AllowPgroupFallback (the mode tests and non-systemd environments use)
// proceeds with process-group tracking only. The pids.max / memory.max limits
// are applied only when the corresponding controller files exist in the leaf,
// which requires the parent to have delegated those controllers via its
// cgroup.subtree_control.
package process

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cgroupMount is the well-known cgroup-v2 unified hierarchy mount point.
const cgroupMount = "/sys/fs/cgroup"

// cgroupLeaf is a single cgroup-v2 directory this package created and owns.
type cgroupLeaf struct {
	// path is the absolute directory of the leaf, e.g.
	// /sys/fs/cgroup/<service>/hadron-proc-<id>.
	path string
}

// detectCgroupRoot returns the absolute filesystem path of the current
// process's own cgroup, derived from /proc/self/cgroup. For the unified
// (v2) hierarchy the file holds a single line of the form "0::<path>".
func detectCgroupRoot() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("read /proc/self/cgroup: %w", err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		// Unified hierarchy entry: hierarchy-ID is 0 and controllers empty.
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "0" && parts[1] == "" {
			return filepath.Join(cgroupMount, parts[2]), nil
		}
	}
	return "", errors.New("no cgroup-v2 unified entry in /proc/self/cgroup")
}

// cgroupProbe reports whether root is a usable delegated cgroup-v2 directory:
// one under which this process may create and remove a leaf. It returns nil
// when a probe leaf could be created and removed, and a descriptive error
// (typically wrapping EACCES or EROFS) otherwise. Tests use it to skip
// cgroup-dependent assertions when the environment lacks a writable delegated
// cgroup.
func cgroupProbe(root string) error {
	if root == "" {
		return errors.New("empty cgroup root")
	}
	probe := filepath.Join(root, fmt.Sprintf("hadron-probe-%d", os.Getpid()))
	if err := os.Mkdir(probe, 0o755); err != nil {
		if os.IsExist(err) {
			_ = os.Remove(probe)
			return nil
		}
		return fmt.Errorf("mkdir probe leaf: %w", err)
	}
	_ = os.Remove(probe)
	return nil
}

// createLeaf creates an empty leaf below root and applies the pids.max /
// memory.max limits for whichever controllers the leaf exposes. The leaf is
// created BEFORE the process is spawned so the child can be born directly
// inside it (via clone3 CLONE_INTO_CGROUP), making cgroup.kill authoritative
// over the entire descendant tree — grandchildren cannot be created outside
// the leaf and so cannot escape teardown.
func createLeaf(root, id string, pidsMax, memoryMax int64) (*cgroupLeaf, error) {
	if root == "" {
		return nil, errors.New("empty cgroup root")
	}
	leaf := &cgroupLeaf{path: filepath.Join(root, "hadron-proc-"+id)}
	if err := os.Mkdir(leaf.path, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cgroup leaf: %w", err)
	}
	// Limits are best-effort: the control files exist only when the parent
	// delegated the controller. A missing file is not an error.
	leaf.applyLimit("pids.max", strconv.FormatInt(pidsMax, 10))
	leaf.applyLimit("memory.max", strconv.FormatInt(memoryMax, 10))
	return leaf, nil
}

// placeExisting moves an already-running pid into the leaf. It is the fallback
// used when the kernel does not support being born into a cgroup at clone
// time (CLONE_INTO_CGROUP, Linux < 5.7).
func (l *cgroupLeaf) placeExisting(pid int) error {
	return os.WriteFile(filepath.Join(l.path, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644)
}

// applyLimit writes value to the named controller file if it exists.
func (l *cgroupLeaf) applyLimit(name, value string) {
	file := filepath.Join(l.path, name)
	if _, err := os.Stat(file); err != nil {
		return // controller not delegated to this leaf
	}
	_ = os.WriteFile(file, []byte(value), 0o644)
}

// kill writes "1" to the leaf's cgroup.kill, which the kernel translates into
// a SIGKILL delivered to every task in the subtree — including descendants
// that started their own session/process group. It is a no-op if the leaf is
// nil or the file is absent (older kernels).
func (l *cgroupLeaf) kill() error {
	if l == nil {
		return nil
	}
	err := os.WriteFile(filepath.Join(l.path, "cgroup.kill"), []byte("1"), 0o644)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// populated reports whether the leaf still has member processes. Used to wait
// for the subtree to drain before rmdir (an rmdir of a non-empty cgroup fails
// with EBUSY).
func (l *cgroupLeaf) populated() bool {
	data, err := os.ReadFile(filepath.Join(l.path, "cgroup.procs"))
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(data))) > 0
}

// remove deletes the (now empty) leaf directory. Because task exit and cgroup
// drain are asynchronous with respect to process reaping, it retries briefly
// while the subtree empties. A leaf that is already gone is success.
func (l *cgroupLeaf) remove() error {
	if l == nil {
		return nil
	}
	var err error
	for i := 0; i < 100; i++ {
		if err = os.Remove(l.path); err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) {
			return err
		}
		time.Sleep(2 * time.Millisecond)
	}
	return err
}
