package watchdog

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// SystemDetector is the production SessionDetector. It reports the agent
// graphical session present only when BOTH hold:
//
//  1. logind lists an agent-owned graphical session (Type x11/wayland/mir), and
//  2. an i3 process owned by the agent uid is running.
//
// Both probes are cgo-free: logind is queried by shelling out to loginctl (its
// output is parsed), and the i3 process is found by scanning /proc. The seams
// (loginctl runner, /proc root, uid lookup) are fields so they can be pointed
// at fixtures in tests, but the watchdog's own state-machine tests use a plain
// fake SessionDetector instead.
type SystemDetector struct {
	// User is the guarded account (agent).
	User string
	// uid is the resolved numeric uid of User; -1 until resolved.
	uid int

	// runLoginctl runs `loginctl <args...>` and returns stdout. Injectable.
	runLoginctl func(ctx context.Context, args ...string) ([]byte, error)
	// procRoot is the /proc mount to scan. Empty means "/proc".
	procRoot string
	// lookupUID resolves a username to its numeric uid. Injectable.
	lookupUID func(name string) (int, error)
}

// NewSystemDetector builds a production detector for the given account.
func NewSystemDetector(username string) *SystemDetector {
	return &SystemDetector{
		User:        username,
		uid:         -1,
		runLoginctl: defaultLoginctl,
		procRoot:    "/proc",
		lookupUID:   lookupUID,
	}
}

// Present implements SessionDetector.
func (d *SystemDetector) Present(ctx context.Context) (bool, error) {
	uid, err := d.resolveUID()
	if err != nil {
		return false, err
	}
	graphical, err := d.hasGraphicalSession(ctx, uid)
	if err != nil {
		return false, err
	}
	if !graphical {
		return false, nil
	}
	i3, err := d.hasI3Process(uid)
	if err != nil {
		return false, err
	}
	return i3, nil
}

// resolveUID resolves and caches the guarded account's numeric uid.
func (d *SystemDetector) resolveUID() (int, error) {
	if d.uid >= 0 {
		return d.uid, nil
	}
	lookup := d.lookupUID
	if lookup == nil {
		lookup = lookupUID
	}
	uid, err := lookup(d.User)
	if err != nil {
		return -1, fmt.Errorf("resolve uid for %q: %w", d.User, err)
	}
	d.uid = uid
	return uid, nil
}

// hasGraphicalSession asks logind whether the given uid owns a graphical
// session. It lists sessions, then inspects each session's User and Type,
// treating x11/wayland/mir as graphical.
func (d *SystemDetector) hasGraphicalSession(ctx context.Context, uid int) (bool, error) {
	run := d.runLoginctl
	if run == nil {
		run = defaultLoginctl
	}
	out, err := run(ctx, "list-sessions", "--no-legend")
	if err != nil {
		return false, fmt.Errorf("loginctl list-sessions: %w", err)
	}
	ids := parseSessionIDs(out)
	for _, id := range ids {
		props, err := run(ctx, "show-session", id, "-p", "Name", "-p", "User", "-p", "Type")
		if err != nil {
			// A session can vanish between listing and inspection; skip it.
			continue
		}
		kv := parseKeyValues(props)
		if kv["User"] != strconv.Itoa(uid) {
			continue
		}
		if isGraphicalType(kv["Type"]) {
			return true, nil
		}
	}
	return false, nil
}

// hasI3Process scans /proc for an i3 process owned by uid.
func (d *SystemDetector) hasI3Process(uid int) (bool, error) {
	root := d.procRoot
	if root == "" {
		root = "/proc"
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a pid dir
		}
		pidDir := filepath.Join(root, e.Name())
		if procComm(pidDir) != "i3" {
			continue
		}
		if procUID(pidDir) == uid {
			return true, nil
		}
	}
	return false, nil
}

// procComm reads /proc/<pid>/comm and returns the trimmed command name, or ""
// if it cannot be read (the process may have exited).
func procComm(pidDir string) string {
	b, err := os.ReadFile(filepath.Join(pidDir, "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// procUID reads the real uid from /proc/<pid>/status, or -1 if unavailable.
func procUID(pidDir string) int {
	b, err := os.ReadFile(filepath.Join(pidDir, "status"))
	if err != nil {
		return -1
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return -1
		}
		uid, err := strconv.Atoi(fields[1]) // real uid
		if err != nil {
			return -1
		}
		return uid
	}
	return -1
}

// parseSessionIDs pulls the session id (first column) from `loginctl
// list-sessions --no-legend` output.
func parseSessionIDs(out []byte) []string {
	var ids []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		ids = append(ids, fields[0])
	}
	return ids
}

// parseKeyValues parses `KEY=VALUE` lines (loginctl show-session -p output).
func parseKeyValues(out []byte) map[string]string {
	kv := make(map[string]string)
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return kv
}

// isGraphicalType reports whether a logind session Type is a graphical one.
func isGraphicalType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "x11", "wayland", "mir":
		return true
	default:
		return false
	}
}

// defaultLoginctl runs the real loginctl binary.
func defaultLoginctl(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "loginctl", args...).Output()
}

// lookupUID resolves a username to its numeric uid. Under CGO_ENABLED=0 the
// os/user package parses /etc/passwd in pure Go, so this stays cgo-free.
func lookupUID(name string) (int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(u.Uid)
}

// SystemRestarter is the production Restarter: it restarts ly@tty1.service and
// nothing else. The run seam is injectable so a test can assert the EXACT
// command issued.
type SystemRestarter struct {
	// run executes the restart command. Injectable; nil uses systemctl.
	run func(ctx context.Context, name string, args ...string) error
}

// NewSystemRestarter builds a production restarter.
func NewSystemRestarter() *SystemRestarter { return &SystemRestarter{run: defaultRun} }

// RestartLy runs exactly `systemctl restart ly@tty1.service`.
func (r *SystemRestarter) RestartLy(ctx context.Context) error {
	run := r.run
	if run == nil {
		run = defaultRun
	}
	return run(ctx, "systemctl", "restart", "ly@tty1.service")
}

// defaultRun executes a command and waits for it.
func defaultRun(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}
