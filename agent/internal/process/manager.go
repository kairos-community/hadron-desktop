// Package process implements the executor behind the terminal and process MCP
// tools defined in package api. It is invoked by both the unprivileged session
// broker and the root helper (later Phase-2 tasks): the OS process identity the
// executor runs under IS the privilege boundary, so this package spawns only
// under the identity of the calling process and never elevates.
//
// Two shapes of execution are offered:
//
//   - terminal: run one "/bin/sh -lc <command>" to completion under a timeout,
//     capturing bounded, SEPARATE stdout and stderr, and reporting exit status,
//     timeout, and truncation.
//
//   - process: start a long-running "/bin/sh -lc <command>" (optionally under a
//     pseudo-terminal), tracked by an opaque 128-bit handle — never an OS pid —
//     that the caller polls, writes to, and terminates. Live output is kept in
//     fixed-capacity ring buffers; poll returns only bytes produced since the
//     previous poll.
//
// Every spawned process is confined two ways so that grandchildren cannot
// escape cleanup: it is made a process-group leader (killed via the negative
// pgid) and, when a delegated cgroup-v2 is available, placed in a private leaf
// whose cgroup.kill reaches the entire subtree. terminal, Process(terminate),
// and Close all funnel through the same teardown sequence.
package process

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// Bounds and defaults fixed by the task brief.
const (
	// DefaultTerminalTimeout is applied to a terminal call that does not set
	// timeout_ms.
	DefaultTerminalTimeout = 30 * time.Second
	// MaxTerminalTimeout is the hard ceiling on a terminal call's timeout; a
	// larger caller-supplied timeout_ms is clamped to it.
	MaxTerminalTimeout = 300 * time.Second

	// DefaultStreamCap is the per-stream capture ceiling for a terminal call
	// that does not set max_output_bytes.
	DefaultStreamCap = 1 << 20 // 1 MiB
	// MaxStreamCap is the hard per-stream capture ceiling for terminal.
	MaxStreamCap = 8 << 20 // 8 MiB

	// DefaultRingSize is the per-stream ring-buffer capacity for a tracked
	// process.
	DefaultRingSize = 1 << 20 // 1 MiB

	// DefaultMaxLive is the cap on concurrently live tracked processes per
	// Manager.
	DefaultMaxLive = 16

	// DefaultTerminateGrace is how long teardown waits after the initial
	// signal before escalating to cgroup.kill + SIGKILL.
	DefaultTerminateGrace = 2 * time.Second

	// DefaultReapTimeout bounds the FINAL join in teardown (the wait for
	// cmd.Wait after the kill escalation, and the reader-goroutine join). In
	// the production require-cgroup path this join always completes promptly
	// because cgroup.kill kills every fd holder, so the readers EOF and
	// cmd.Wait returns. It exists only as an escape hatch for the documented
	// pgroup-fallback path, where a setsid grandchild can escape both the
	// process group and (absent a cgroup) the kill, holding an inherited
	// pipe/pty write-end open so the readers never EOF: without a bound the
	// join would hang forever. On timeout teardown force-closes the child IO
	// to unblock the readers, guaranteeing reap/Close return in bounded time.
	DefaultReapTimeout = 5 * time.Second

	// DefaultPidsMax and DefaultMemoryMax are the cgroup limits applied to a
	// tracked process's leaf when the controllers are delegated.
	DefaultPidsMax   int64 = 128
	DefaultMemoryMax int64 = 1 << 30 // 1 GiB (1073741824 bytes)
)

// Clock abstracts time so tests can drive timeouts and the teardown grace
// period deterministically.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Config configures a Manager. The zero value is not usable; start from
// DefaultConfig and override.
type Config struct {
	DefaultTimeout   time.Duration
	MaxTimeout       time.Duration
	DefaultStreamCap int
	MaxStreamCap     int
	RingSize         int
	MaxLive          int
	TerminateGrace   time.Duration
	// ReapTimeout bounds the final teardown join (see DefaultReapTimeout). It
	// is only ever reached on the degraded pgroup-fallback survivor path;
	// production teardown completes well within it.
	ReapTimeout time.Duration

	// RequireCgroup makes cgroup-leaf placement mandatory: a start whose
	// placement fails returns an error instead of degrading. Production sets
	// this true.
	RequireCgroup bool
	// AllowPgroupFallback permits a start to proceed with process-group
	// tracking only when cgroup placement is unavailable. Tests and
	// non-systemd environments set this true.
	AllowPgroupFallback bool
	// CgroupRoot is the delegated parent cgroup under which per-process leaves
	// are created. Empty means autodetect from /proc/self/cgroup.
	CgroupRoot string
	PidsMax    int64
	MemoryMax  int64

	// Clock and NewID are test seams. NewID must return a fresh opaque handle
	// per call, or an error (e.g. crypto/rand failure) which the start path
	// surfaces as INTERNAL rather than minting a guessable/PID-derived handle.
	Clock Clock
	NewID func() (string, error)
}

// DefaultConfig returns the production defaults: cgroup placement required, no
// process-group fallback, autodetected cgroup root.
func DefaultConfig() Config {
	return Config{
		DefaultTimeout:      DefaultTerminalTimeout,
		MaxTimeout:          MaxTerminalTimeout,
		DefaultStreamCap:    DefaultStreamCap,
		MaxStreamCap:        MaxStreamCap,
		RingSize:            DefaultRingSize,
		MaxLive:             DefaultMaxLive,
		TerminateGrace:      DefaultTerminateGrace,
		ReapTimeout:         DefaultReapTimeout,
		RequireCgroup:       true,
		AllowPgroupFallback: false,
		PidsMax:             DefaultPidsMax,
		MemoryMax:           DefaultMemoryMax,
		Clock:               realClock{},
		NewID:               newOpaqueID,
	}
}

func (c *Config) fillDefaults() {
	if c.DefaultTimeout <= 0 {
		c.DefaultTimeout = DefaultTerminalTimeout
	}
	if c.MaxTimeout <= 0 {
		c.MaxTimeout = MaxTerminalTimeout
	}
	if c.DefaultStreamCap <= 0 {
		c.DefaultStreamCap = DefaultStreamCap
	}
	if c.MaxStreamCap <= 0 {
		c.MaxStreamCap = MaxStreamCap
	}
	if c.RingSize <= 0 {
		c.RingSize = DefaultRingSize
	}
	if c.MaxLive <= 0 {
		c.MaxLive = DefaultMaxLive
	}
	if c.TerminateGrace <= 0 {
		c.TerminateGrace = DefaultTerminateGrace
	}
	if c.ReapTimeout <= 0 {
		c.ReapTimeout = DefaultReapTimeout
	}
	if c.PidsMax <= 0 {
		c.PidsMax = DefaultPidsMax
	}
	if c.MemoryMax <= 0 {
		c.MemoryMax = DefaultMemoryMax
	}
	if c.Clock == nil {
		c.Clock = realClock{}
	}
	if c.NewID == nil {
		c.NewID = newOpaqueID
	}
}

// Manager owns every process this executor spawns. It is safe for concurrent
// use. A Manager created for the session broker and one created for the root
// helper are independent, each enforcing its own live-process cap.
type Manager struct {
	cfg        Config
	clock      Clock
	cgroupRoot string
	cgroupOK   bool

	mu     sync.Mutex
	procs  map[string]*procHandle
	live   int
	closed bool
}

// New constructs a Manager from cfg (missing fields are filled with defaults).
// It resolves the cgroup root once: on failure, placement is simply
// unavailable and every start relies on the configured fallback policy.
func New(cfg Config) *Manager {
	cfg.fillDefaults()
	m := &Manager{
		cfg:   cfg,
		clock: cfg.Clock,
		procs: make(map[string]*procHandle),
	}
	root := cfg.CgroupRoot
	if root == "" {
		if detected, err := detectCgroupRoot(); err == nil {
			root = detected
		}
	}
	m.cgroupRoot = root
	m.cgroupOK = root != "" && cgroupProbe(root) == nil
	return m
}

// Close terminates every tracked process (grandchildren included) and releases
// resources. It is idempotent.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	handles := make([]*procHandle, 0, len(m.procs))
	for _, h := range m.procs {
		handles = append(handles, h)
	}
	m.procs = make(map[string]*procHandle)
	m.mu.Unlock()

	for _, h := range handles {
		m.reap(h.tracked, syscall.SIGTERM)
		h.readersWG.Wait()
		h.closeIO()
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func okMeta() api.ResultMeta { return api.ResultMeta{} }

func failMeta(code api.ErrorCode, msg string, retryable bool) api.ResultMeta {
	return api.ResultMeta{Code: code, Message: msg, Retryable: retryable}
}

// buildCommand constructs the "/bin/sh -lc <command>" invocation shared by
// terminal and process. Extra args become the shell's positional parameters.
func buildCommand(ctx context.Context, command string, args, env []string, cwd string) *exec.Cmd {
	// Everything runs through a login shell so the process inherits the same
	// environment an interactive session would (PATH, XDG_DATA_DIRS -- flatpak
	// in particular is unusable without them).
	//
	// How args are handed over matters. `sh -lc <command> a b c` binds a/b/c to
	// $0/$1/$2 -- positional parameters that <command> never references -- so
	// arguments were accepted by the API and then silently dropped: `process`
	// with command="flatpak", args=["install", ...] ran a bare `flatpak` and
	// failed with "No command specified". Pass them through explicitly instead,
	// so ProcessInput.Args reaches the executable as documented.
	//
	// With no args the command stays a plain shell command line, which is what
	// callers passing a whole pipeline in `command` rely on.
	var shArgs []string
	if len(args) == 0 {
		shArgs = []string{"-lc", command}
	} else {
		shArgs = append([]string{"-lc", `exec "$0" "$@"`, command}, args...)
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", shArgs...)
	cmd.Cancel = func() error { return nil } // teardown is handled explicitly
	if cwd != "" {
		cmd.Dir = cwd
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd
}

// resolveSignal maps a caller-supplied signal name to a syscall.Signal,
// defaulting to SIGTERM.
func resolveSignal(name string) syscall.Signal {
	switch strings.ToUpper(strings.TrimPrefix(strings.ToUpper(name), "SIG")) {
	case "KILL":
		return syscall.SIGKILL
	case "INT":
		return syscall.SIGINT
	case "HUP":
		return syscall.SIGHUP
	case "QUIT":
		return syscall.SIGQUIT
	case "TERM", "":
		return syscall.SIGTERM
	default:
		return syscall.SIGTERM
	}
}

// exitCodeOf extracts a numeric exit code from a cmd.Wait error. It returns
// (code, wasSignaled). A process killed by a signal reports code -1.
func exitCodeOf(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if status, ok := ee.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return -1, true
		}
		return ee.ExitCode(), false
	}
	return -1, false
}

// ---------------------------------------------------------------------------
// tracked teardown (shared by terminal, process terminate, and Close)
// ---------------------------------------------------------------------------

// tracked is the kill boundary for one spawned process: its process group and,
// when available, its cgroup leaf. reap drives the escalation exactly once.
type tracked struct {
	pgid int
	leaf *cgroupLeaf
	done chan struct{} // closed once cmd.Wait has returned
	once sync.Once
	// forceClose, when set, closes EVERY child IO endpoint (including the
	// reader-owned pipe read ends and the pty master). reap invokes it only on
	// the bounded fallback-survivor timeout path, to interrupt a reader
	// goroutine that is stuck on an inherited fd a setsid grandchild kept open.
	// nil for tracked trees with no reader goroutines to unblock (e.g.
	// terminal, which joins its readers itself).
	forceClose func()
}

// reap performs the fixed teardown sequence, at most once per tracked:
//
//  1. send sig to the negative process group;
//  2. wait up to TerminateGrace for the process to exit;
//  3. write cgroup.kill (SIGKILLs the whole subtree, escapees included);
//  4. SIGKILL the process group as a final fallback;
//  5. wait (bounded) for reaping, then remove the empty leaf.
//
// Step 5's wait is bounded by ReapTimeout so teardown cannot hang forever. In
// the production require-cgroup path it always completes promptly: cgroup.kill
// kills every fd holder, the reader goroutines EOF, and cmd.Wait returns. The
// bound only ever fires on the documented pgroup-fallback path, where a setsid
// grandchild can escape both the process group and (absent a cgroup) the kill
// and hold an inherited pipe/pty write-end open — so the readers never EOF and
// t.done never closes. On timeout reap force-closes the child IO (interrupting
// the stuck readers) and bound-waits once more; if a grandchild somehow still
// pins cmd.Wait we return rather than hang, leaving that already-degraded tree
// detached. The normal path always joins cleanly.
func (m *Manager) reap(t *tracked, sig syscall.Signal) {
	t.once.Do(func() {
		if t.pgid > 0 {
			_ = syscall.Kill(-t.pgid, sig)
		}
		select {
		case <-t.done:
		case <-m.clock.After(m.cfg.TerminateGrace):
		}
		_ = t.leaf.kill()
		if t.pgid > 0 {
			_ = syscall.Kill(-t.pgid, syscall.SIGKILL)
		}
		// Final, bounded wait for cmd.Wait (the waiter closes t.done).
		select {
		case <-t.done:
		case <-m.clock.After(m.cfg.ReapTimeout):
			// Fallback survivor: unblock the readers stuck on the escapee's
			// inherited fd, then give cmd.Wait a bounded moment to return.
			if t.forceClose != nil {
				t.forceClose()
			}
			select {
			case <-t.done:
			case <-m.clock.After(m.cfg.ReapTimeout):
			}
		}
		_ = t.leaf.remove()
	})
}

// waitWG waits for wg, bounded by timeout via the injected clock. It reports
// whether wg completed (true) or the bound fired first (false). The helper
// goroutine it spawns always exits once wg completes, so a caller that
// force-closes the readers' fds after a false return will not leak it.
func waitWG(wg *sync.WaitGroup, clock Clock, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-clock.After(timeout):
		return false
	}
}

// joinReaders joins wg, bounded by timeout. If the readers do not finish in
// time — only possible on the pgroup-fallback path, where a surviving setsid
// grandchild holds a stream's write end open so the read end never EOFs — it
// runs forceClose (which closes the read ends, interrupting the stuck readers)
// and joins once more, bounded again, so the caller never blocks forever. In
// the production require-cgroup path cgroup.kill has already killed every fd
// holder before this is reached, so the first join returns immediately.
func joinReaders(wg *sync.WaitGroup, clock Clock, timeout time.Duration, forceClose func()) {
	if waitWG(wg, clock, timeout) {
		return
	}
	forceClose()
	waitWG(wg, clock, timeout)
}

// ---------------------------------------------------------------------------
// opaque ids
// ---------------------------------------------------------------------------

// clampTimeout resolves a caller-supplied timeout in ms against defaults and
// the max ceiling.
func clampTimeout(ms *int, def, max time.Duration) time.Duration {
	if ms == nil {
		return def
	}
	d := time.Duration(*ms) * time.Millisecond
	if d <= 0 {
		return def
	}
	if d > max {
		return max
	}
	return d
}

// clampCap resolves a caller-supplied byte cap against defaults and the max
// ceiling.
func clampCap(n *int, def, max int) int {
	if n == nil {
		return def
	}
	if *n <= 0 {
		return def
	}
	if *n > max {
		return max
	}
	return *n
}

// newOpaqueID returns a 128-bit handle as a 32-character hex string, drawn
// from crypto/rand. It never encodes or exposes an OS pid. If crypto/rand
// fails it returns an error rather than falling back to a guessable,
// time/pid-derived value: the start path surfaces that as INTERNAL.
func newOpaqueID() (string, error) {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate opaque id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ---------------------------------------------------------------------------
// ring buffer
// ---------------------------------------------------------------------------

// ring is a fixed-capacity byte ring with a monotonic total-written counter
// that doubles as the read cursor. It keeps only the most recent cap bytes;
// once total exceeds cap, the oldest bytes are overwritten. It is not
// goroutine-safe on its own — procHandle guards it with a mutex.
type ring struct {
	buf   []byte
	cap   int
	total int64 // total bytes ever written; the head cursor
}

func newRing(capacity int) *ring {
	if capacity <= 0 {
		capacity = DefaultRingSize
	}
	return &ring{buf: make([]byte, capacity), cap: capacity}
}

// write appends p, overwriting the oldest bytes when full.
func (r *ring) write(p []byte) {
	for len(p) > 0 {
		off := int(r.total % int64(r.cap))
		n := copy(r.buf[off:], p)
		p = p[n:]
		r.total += int64(n)
	}
}

// readFrom returns the bytes written since cursor and the next cursor to pass
// back. lost is true when cursor had fallen behind the ring's oldest retained
// byte, meaning some bytes between cursor and the returned data were
// overwritten and are gone. A cursor at or beyond the head returns no data
// with lost=false (caught up). A cursor ahead of the head (impossible under
// monotonic use, but defended against) is treated as caught up.
func (r *ring) readFrom(cursor int64) (data []byte, next int64, lost bool) {
	if cursor >= r.total {
		return nil, r.total, false
	}
	oldest := r.total - int64(r.cap)
	if oldest < 0 {
		oldest = 0
	}
	start := cursor
	if start < oldest {
		start = oldest
		lost = true
	}
	n := int(r.total - start)
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = r.buf[int((start+int64(i))%int64(r.cap))]
	}
	return out, r.total, lost
}

// ---------------------------------------------------------------------------
// tracked process handle
// ---------------------------------------------------------------------------

// procHandle is one live/finished tracked process.
type procHandle struct {
	id      string
	cmd     *exec.Cmd
	pty     bool
	ptmx    *os.File       // pty master; nil when not a pty
	stdin   io.WriteCloser // stdin pipe, or the pty master
	stdoutR *os.File       // stdout pipe read end (nil in pty mode); retained
	stderrR *os.File       // stderr pipe read end (nil in pty mode); so teardown
	tracked *tracked       // can force them closed to unblock a stuck reader

	readersWG sync.WaitGroup
	notify    chan struct{} // buffered(1) wakeup for a blocked poll

	mu        sync.Mutex
	stdout    *ring
	stderr    *ring // nil in pty mode (stdout and stderr are merged)
	outCursor int64
	errCursor int64
	exited    bool
	exitCode  *int
}

// signalOutput wakes a poll that is blocked waiting for new output.
func (h *procHandle) signalOutput() {
	select {
	case h.notify <- struct{}{}:
	default:
	}
}

// streamWriter drains one child stream (pipe or pty master) into a ring,
// waking any blocked poll after each write. closeAfter, when non-nil, is
// closed once the stream reaches EOF (used to release pipe read-ends; the pty
// master is closed elsewhere because it doubles as the stdin fd).
func (h *procHandle) streamWriter(src io.Reader, r *ring, closeAfter io.Closer) {
	defer h.readersWG.Done()
	if closeAfter != nil {
		defer closeAfter.Close()
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			h.mu.Lock()
			r.write(buf[:n])
			h.mu.Unlock()
			h.signalOutput()
		}
		if err != nil {
			return
		}
	}
}

// closeIO closes the process's I/O endpoints. Safe to call more than once.
func (h *procHandle) closeIO() {
	if h.stdin != nil {
		_ = h.stdin.Close()
	}
	if h.ptmx != nil {
		_ = h.ptmx.Close()
	}
}

// forceCloseIO closes every IO endpoint, INCLUDING the reader-owned pipe read
// ends (which closeIO leaves to the reader goroutines). Closing a read end
// interrupts a reader blocked in Read on that fd (the poller returns an error),
// letting it exit even while a surviving grandchild still holds the write end.
// Used only on reap's bounded fallback-survivor timeout path. Safe to call
// concurrently with the readers' own Close (os.File.Close is concurrency-safe)
// and more than once.
func (h *procHandle) forceCloseIO() {
	h.closeIO()
	if h.stdoutR != nil {
		_ = h.stdoutR.Close()
	}
	if h.stderrR != nil {
		_ = h.stderrR.Close()
	}
}

// snapshotExit returns the finished state under the lock.
func (h *procHandle) snapshotExit() (exited bool, code *int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.exitCode != nil {
		c := *h.exitCode
		return h.exited, &c
	}
	return h.exited, nil
}

// ---------------------------------------------------------------------------
// capWriter: bounded terminal capture
// ---------------------------------------------------------------------------

// capWriter stores at most limit bytes and records whether more arrived. It
// never returns a short write or error, so os/exec keeps draining the child
// (preventing a stalled pipe / SIGPIPE) even after the cap is hit.
type capWriter struct {
	limit     int
	buf       []byte
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if len(w.buf) < w.limit {
		room := w.limit - len(w.buf)
		if room >= len(p) {
			w.buf = append(w.buf, p...)
		} else {
			w.buf = append(w.buf, p[:room]...)
			w.truncated = true
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}

func (w *capWriter) String() string { return string(w.buf) }

// drainInto copies src into w until EOF, then closes src and marks the wait
// group done. Used by terminal to capture a stream without exec's copier
// (which would tie cmd.Wait to inherited-fd lifetime).
func drainInto(wg *sync.WaitGroup, src io.ReadCloser, w *capWriter) {
	defer wg.Done()
	defer src.Close()
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// cgroup placement policy
// ---------------------------------------------------------------------------

// beginCgroup applies the Manager's require-cgroup / pgroup-fallback policy
// BEFORE a process is spawned. On success with a cgroup it returns the freshly
// created (empty) leaf and an open directory fd the caller passes to
// SysProcAttr.CgroupFD so the child is born inside the leaf; the caller must
// close fd after Start. In permitted fallback mode it returns (nil, nil, nil).
// When a cgroup is required but unavailable it returns an error and the caller
// must not spawn.
//
// Being born into the cgroup (clone3 CLONE_INTO_CGROUP) is what makes teardown
// escape-proof: every descendant is created inside the leaf, so cgroup.kill
// reaches grandchildren that started their own session or process group.
func (m *Manager) beginCgroup(id string) (*cgroupLeaf, *os.File, error) {
	if m.cgroupOK {
		leaf, err := createLeaf(m.cgroupRoot, id, m.cfg.PidsMax, m.cfg.MemoryMax)
		if err == nil {
			fd, ferr := os.Open(leaf.path)
			if ferr != nil {
				// The leaf exists but we cannot get a directory fd; fall back
				// to moving the pid in after start (still confines the tree
				// root; grandchildren rely on the process group).
				return leaf, nil, nil
			}
			return leaf, fd, nil
		}
		if m.cfg.RequireCgroup {
			return nil, nil, err
		}
		if m.cfg.AllowPgroupFallback {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if m.cfg.RequireCgroup {
		return nil, nil, errors.New("cgroup placement required but no delegated cgroup is available")
	}
	if m.cfg.AllowPgroupFallback {
		return nil, nil, nil
	}
	return nil, nil, errors.New("cgroup placement unavailable and process-group fallback not permitted")
}

// applyCgroupFD wires a born-into-cgroup file descriptor into a SysProcAttr.
func applyCgroupFD(attr *syscall.SysProcAttr, fd *os.File) {
	if fd != nil {
		attr.UseCgroupFD = true
		attr.CgroupFD = int(fd.Fd())
	}
}

// ---------------------------------------------------------------------------
// terminal
// ---------------------------------------------------------------------------

// Terminal runs a single "/bin/sh -lc <command>" to completion under a
// timeout, capturing bounded and SEPARATE stdout/stderr. The child is a
// process-group leader placed (when possible) in a cgroup leaf; whichever way
// the command ends, teardown runs so backgrounded grandchildren are killed.
func (m *Manager) Terminal(ctx context.Context, in api.TerminalInput) (api.TerminalOutput, error) {
	if err := in.Validate(); err != nil {
		return api.TerminalOutput{ResultMeta: failMeta(api.CodeInvalidArgument, err.Error(), false)}, nil
	}
	timeout := clampTimeout(in.TimeoutMs, m.cfg.DefaultTimeout, m.cfg.MaxTimeout)
	streamCap := clampCap(in.MaxOutputBytes, m.cfg.DefaultStreamCap, m.cfg.MaxStreamCap)

	id, idErr := m.cfg.NewID()
	if idErr != nil {
		return api.TerminalOutput{ResultMeta: failMeta(api.CodeInternal, "failed to generate process id", false)}, nil
	}

	// Create the cgroup leaf (if any) before spawning so the shell and every
	// child it forks are born inside it.
	leaf, cgFD, cerr := m.beginCgroup(id)
	if cerr != nil {
		return api.TerminalOutput{ResultMeta: failMeta(api.CodeInternal, "sandbox unavailable", false)}, nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := buildCommand(runCtx, in.Command, nil, in.Env, in.Cwd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	applyCgroupFD(cmd.SysProcAttr, cgFD)

	// Use explicit pipes (not cmd.Stdout = writer): with an exec-managed
	// copier, cmd.Wait would block until every inherited fd closed, so a
	// backgrounded grandchild holding stdout would hang the call until the
	// timeout. With explicit *os.File ends, cmd.Wait returns when the shell
	// exits; the reader goroutines are joined only after teardown.
	outCap := &capWriter{limit: streamCap}
	errCap := &capWriter{limit: streamCap}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		if cgFD != nil {
			_ = cgFD.Close()
		}
		_ = leaf.remove()
		return api.TerminalOutput{ResultMeta: failMeta(api.CodeInternal, "failed to start command", false)}, nil
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		if cgFD != nil {
			_ = cgFD.Close()
		}
		_ = leaf.remove()
		return api.TerminalOutput{ResultMeta: failMeta(api.CodeInternal, "failed to start command", false)}, nil
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	startErr := cmd.Start()
	if cgFD != nil {
		_ = cgFD.Close()
	}
	if startErr != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		_ = leaf.remove()
		return api.TerminalOutput{ResultMeta: failMeta(api.CodeInternal, "failed to start command", false)}, nil
	}
	// Close our copies of the child-side write ends so the read ends see EOF
	// once every writer (the shell and any survivor) is gone.
	stdoutW.Close()
	stderrW.Close()

	var readers sync.WaitGroup
	readers.Add(2)
	go drainInto(&readers, stdoutR, outCap)
	go drainInto(&readers, stderrR, errCap)

	pid := cmd.Process.Pid
	if cgFD == nil && leaf != nil {
		// Directory fd was unavailable: best-effort move into the leaf.
		_ = leaf.placeExisting(pid)
	}

	t := &tracked{pgid: pid, leaf: leaf, done: make(chan struct{})}
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(t.done)
	}()

	timedOut := false
	select {
	case <-t.done:
	case <-m.clock.After(timeout):
		timedOut = true
	case <-ctx.Done():
		timedOut = true
	}

	// Always tear down the tracked tree so grandchildren cannot survive.
	m.reap(t, syscall.SIGTERM)
	// With the whole tree dead, the write ends are closed; join the readers so
	// captured output is complete and no goroutine leaks. Bounded so a
	// fallback-mode grandchild that escaped teardown and still holds a write
	// end cannot hang the call: on timeout the read ends are force-closed to
	// interrupt the stuck readers.
	joinReaders(&readers, m.clock, m.cfg.ReapTimeout, func() {
		_ = stdoutR.Close()
		_ = stderrR.Close()
	})

	// t.done is closed (reap waited for it), so waitErr is safely visible.
	code, _ := exitCodeOf(waitErr)
	out := api.TerminalOutput{
		Stdout:   outCap.String(),
		Stderr:   errCap.String(),
		ExitCode: code,
	}
	truncated := outCap.truncated || errCap.truncated
	out.Truncated = truncated
	switch {
	case timedOut:
		out.ResultMeta = failMeta(api.CodeDeadlineExceeded, "command exceeded its timeout", false)
	case truncated:
		out.ResultMeta = api.ResultMeta{Code: api.CodeOutputTruncated, Message: "output truncated at the size limit"}
	default:
		out.ResultMeta = okMeta()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// process
// ---------------------------------------------------------------------------

// Process dispatches a process tool call on its action.
func (m *Manager) Process(ctx context.Context, in api.ProcessInput) (api.ProcessOutput, error) {
	if err := in.Validate(); err != nil {
		return api.ProcessOutput{ResultMeta: failMeta(api.CodeInvalidArgument, err.Error(), false)}, nil
	}
	switch in.Action {
	case api.ProcessStart:
		return m.processStart(in)
	case api.ProcessPoll:
		return m.processPoll(ctx, in)
	case api.ProcessWrite:
		return m.processWrite(in)
	case api.ProcessTerminate:
		return m.processTerminate(in)
	default:
		return api.ProcessOutput{ResultMeta: failMeta(api.CodeInvalidArgument, "unknown action", false)}, nil
	}
}

func (m *Manager) lookup(id string) *procHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.procs[id]
}

func (m *Manager) releaseLive() {
	m.mu.Lock()
	if m.live > 0 {
		m.live--
	}
	m.mu.Unlock()
}

// processStart spawns and tracks a long-running process.
func (m *Manager) processStart(in api.ProcessInput) (api.ProcessOutput, error) {
	// Reserve a live slot atomically so the 16-process cap holds under
	// concurrent starts.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return api.ProcessOutput{ResultMeta: failMeta(api.CodeInternal, "manager is closed", false)}, nil
	}
	if m.live >= m.cfg.MaxLive {
		m.mu.Unlock()
		return api.ProcessOutput{ResultMeta: failMeta(api.CodeResourceExhausted, "too many live processes", true)}, nil
	}
	m.live++
	m.mu.Unlock()

	id, idErr := m.cfg.NewID()
	if idErr != nil {
		m.releaseLive()
		return api.ProcessOutput{ResultMeta: failMeta(api.CodeInternal, "failed to generate process id", false)}, nil
	}

	// Create the leaf before spawning so the child is born inside it.
	leaf, cgFD, cerr := m.beginCgroup(id)
	if cerr != nil {
		m.releaseLive()
		return api.ProcessOutput{ResultMeta: failMeta(api.CodeInternal, "sandbox unavailable", false)}, nil
	}

	cmd := buildCommand(context.Background(), in.Command, in.Args, in.Env, in.Cwd)
	h := &procHandle{id: id, cmd: cmd, pty: in.PTY, notify: make(chan struct{}, 1)}

	if err := m.startChild(h, in.PTY, cgFD); err != nil {
		if cgFD != nil {
			_ = cgFD.Close()
		}
		_ = leaf.remove()
		m.releaseLive()
		return api.ProcessOutput{ResultMeta: failMeta(api.CodeInternal, "failed to start process", false)}, nil
	}
	if cgFD != nil {
		_ = cgFD.Close()
	}
	pid := cmd.Process.Pid
	if cgFD == nil && leaf != nil {
		_ = leaf.placeExisting(pid)
	}

	h.tracked = &tracked{pgid: pid, leaf: leaf, done: make(chan struct{}), forceClose: h.forceCloseIO}
	go m.waitChild(h)

	m.mu.Lock()
	m.procs[id] = h
	m.mu.Unlock()

	return api.ProcessOutput{ResultMeta: okMeta(), ProcessID: id, Running: true}, nil
}

// startChild wires up stdio and starts the process, placing it in its process
// group and (when cgFD is non-nil) in its cgroup leaf at birth. On return the
// process is running, or an error is returned and nothing leaks.
func (m *Manager) startChild(h *procHandle, usePTY bool, cgFD *os.File) error {
	if usePTY {
		h.cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		applyCgroupFD(h.cmd.SysProcAttr, cgFD)
		ptmx, err := pty.Start(h.cmd) // adds Setsid + Setctty; pgid == pid
		if err != nil {
			return err
		}
		h.ptmx = ptmx
		h.stdin = ptmx
		h.stdout = newRing(m.cfg.RingSize)
		h.readersWG.Add(1)
		// Do not close ptmx from the reader: it is also the stdin fd.
		go h.streamWriter(ptmx, h.stdout, nil)
		return nil
	}

	h.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	applyCgroupFD(h.cmd.SysProcAttr, cgFD)
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return err
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return err
	}
	h.cmd.Stdout = stdoutW
	h.cmd.Stderr = stderrW
	h.cmd.Stdin = stdinR
	if err := h.cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		stdinR.Close()
		stdinW.Close()
		return err
	}
	// The child holds its own dups; close our copies of the child-side ends.
	stdoutW.Close()
	stderrW.Close()
	stdinR.Close()

	h.stdin = stdinW
	h.stdoutR = stdoutR
	h.stderrR = stderrR
	h.stdout = newRing(m.cfg.RingSize)
	h.stderr = newRing(m.cfg.RingSize)
	h.readersWG.Add(2)
	go h.streamWriter(stdoutR, h.stdout, stdoutR)
	go h.streamWriter(stderrR, h.stderr, stderrR)
	return nil
}

// waitChild joins the reader goroutines, reaps the process, records its exit
// status, wakes any blocked poll, and signals teardown that the process is
// gone.
func (m *Manager) waitChild(h *procHandle) {
	h.readersWG.Wait() // readers see EOF only after the child has exited
	err := h.cmd.Wait()
	code, _ := exitCodeOf(err)
	h.mu.Lock()
	h.exited = true
	h.exitCode = &code
	h.mu.Unlock()
	m.releaseLive()
	h.signalOutput()
	// The subtree is gone; drop the now-empty leaf. A later terminate/Close
	// funnels through reap, whose leaf.kill/remove are idempotent no-ops on an
	// already-removed leaf.
	_ = h.tracked.leaf.remove()
	close(h.tracked.done)
}

// processPoll returns output produced since the previous poll and the current
// run/exit state, optionally blocking up to timeout_ms for new output.
func (m *Manager) processPoll(ctx context.Context, in api.ProcessInput) (api.ProcessOutput, error) {
	h := m.lookup(in.ProcessID)
	if h == nil {
		return api.ProcessOutput{ResultMeta: failMeta(api.CodeNotFound, "unknown process id", false)}, nil
	}
	timeout := clampTimeout(in.TimeoutMs, 0, m.cfg.MaxTimeout)
	var timer <-chan time.Time
	if timeout > 0 {
		timer = m.clock.After(timeout)
	}

	timedOut := false
	for {
		h.mu.Lock()
		outData, outNext, outLost := h.stdout.readFrom(h.outCursor)
		h.outCursor = outNext
		var errData []byte
		var errLost bool
		if h.stderr != nil {
			errData, h.errCursor, errLost = h.stderr.readFrom(h.errCursor)
		}
		exited := h.exited
		var code *int
		if h.exitCode != nil {
			c := *h.exitCode
			code = &c
		}
		h.mu.Unlock()

		if len(outData) > 0 || len(errData) > 0 || exited || timeout == 0 || timedOut {
			out := api.ProcessOutput{
				ProcessID: h.id,
				Running:   !exited,
				ExitCode:  code,
				Stdout:    string(outData),
				Stderr:    string(errData),
			}
			if outLost || errLost {
				out.Truncated = true
				out.ResultMeta = api.ResultMeta{Code: api.CodeOutputTruncated, Message: "output buffer overran; some bytes were dropped"}
			} else {
				out.ResultMeta = okMeta()
			}
			return out, nil
		}

		select {
		case <-h.notify:
		case <-timer:
			timedOut = true
		case <-ctx.Done():
			timedOut = true
		}
	}
}

// processWrite sends bytes to the process's stdin.
func (m *Manager) processWrite(in api.ProcessInput) (api.ProcessOutput, error) {
	h := m.lookup(in.ProcessID)
	if h == nil {
		return api.ProcessOutput{ResultMeta: failMeta(api.CodeNotFound, "unknown process id", false)}, nil
	}
	if exited, _ := h.snapshotExit(); exited {
		return api.ProcessOutput{ProcessID: h.id, Running: false, ResultMeta: failMeta(api.CodeInvalidArgument, "cannot write to an exited process", false)}, nil
	}
	if h.stdin == nil {
		return api.ProcessOutput{ProcessID: h.id, Running: true, ResultMeta: failMeta(api.CodeInternal, "process has no stdin", false)}, nil
	}
	if _, err := io.WriteString(h.stdin, in.Input); err != nil {
		return api.ProcessOutput{ProcessID: h.id, Running: true, ResultMeta: failMeta(api.CodeInternal, "failed to write to process", true)}, nil
	}
	return api.ProcessOutput{ProcessID: h.id, Running: true, ResultMeta: okMeta()}, nil
}

// processTerminate tears the process (and its subtree) down and returns its
// final state.
func (m *Manager) processTerminate(in api.ProcessInput) (api.ProcessOutput, error) {
	h := m.lookup(in.ProcessID)
	if h == nil {
		return api.ProcessOutput{ResultMeta: failMeta(api.CodeNotFound, "unknown process id", false)}, nil
	}
	m.reap(h.tracked, resolveSignal(in.Signal))
	h.readersWG.Wait()
	h.closeIO()
	exited, code := h.snapshotExit()
	return api.ProcessOutput{
		ProcessID:  h.id,
		Running:    !exited,
		ExitCode:   code,
		ResultMeta: okMeta(),
	}, nil
}
