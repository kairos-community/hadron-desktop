package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// State is where the daemon's one job stands. The D-Bus API sends it as a
// string, so the values are part of the interface.
type State string

const (
	StateIdle            State = "idle"
	StateChecking        State = "checking"
	StateUpToDate        State = "up-to-date"
	StateUpdateAvailable State = "update-available"
	StateInspecting      State = "inspecting"
	StateApplying        State = "applying"
	StateRebootPending   State = "reboot-pending"
	StateError           State = "error"
	StateUnmanaged       State = "unmanaged"
	// StateExtension: an extension job runs kairos-agent sysext.
	StateExtension State = "extension"
)

// Status is everything a client shows: the tray menu, the CLI's status line.
type Status struct {
	State                          State
	Installed, Latest, Pin, Detail string
	Advanced                       bool
	// Pending says how the install behind reboot-pending ended: one of the
	// pending* words, "" in every other state.
	Pending string
	// ExtResult and ExtDetail are the outcome of the last extension job:
	// one of the extResult* words and its sentence, "" until one ends.
	ExtResult, ExtDetail string
}

// The reboot marker's first word, and Status.Pending. Only "installed" means
// the new system is ready; the other two mean the slots are in an unknown
// state until the next boot.
const (
	pendingStarted   = "started"   // kairos-agent was started and has not finished
	pendingInstalled = "installed" // kairos-agent finished without an error
	pendingFailed    = "failed"    // kairos-agent returned an error
)

// pendingDetail is the sentence a client shows for a pending word.
func pendingDetail(word string) string {
	switch word {
	case pendingInstalled:
		return ""
	case pendingFailed:
		return "The update failed; restart before trying again."
	}
	return "The update was interrupted; restart before trying again."
}

// Backend installs an image and reboots. The real one is kairos-agent; tests
// use a fake, so the state machine, the pin and the refusals are tested
// without root.
type Backend interface {
	// Upgrade installs source ("oci:<repo>@<digest>") into the passive slot,
	// calling progress for each output line.
	Upgrade(ctx context.Context, source string, progress func(line string)) error
	Reboot() error
	// RebootPrevious selects the passive (previous) system for the next boot
	// only, then reboots. The boot after that is the active system again.
	RebootPrevious() error
}

var (
	// ErrBusy refuses a second job: two kairos-agent runs would race on the
	// same partitions.
	ErrBusy = errors.New("another update job is running")
	// ErrRebootPending refuses an install before the last one has booted: a
	// second upgrade would push the never-booted image into the passive slot
	// and overwrite the known-good one.
	ErrRebootPending = errors.New("an update is installed and waits for a reboot")
	// errUnfinishedPending is ErrRebootPending after an install that failed
	// or was interrupted: the slots are not known to be in order until a
	// restart.
	errUnfinishedPending = fmt.Errorf("%w: the last update did not finish; restart before trying again", ErrRebootPending)
	// ErrNotCancellable refuses to stop kairos-agent: a kill between the A/B
	// swap and the image write can leave no good slot.
	ErrNotCancellable = errors.New("the upgrade is writing the image and cannot be cancelled")
	// ErrRebooting refuses an install after Reboot was asked: kairos-agent
	// must not start under a system that systemd is shutting down.
	ErrRebooting = errors.New("the system is rebooting")

	errUnmanaged       = errors.New("this system has no hadron-desktop release file, so it is not updated from here")
	errNothingToCancel = errors.New("no job to cancel")
)

func runDir() string {
	if p := os.Getenv("HADRON_UPDATE_RUN_DIR"); p != "" {
		return p
	}
	return "/run/hadron-update"
}

// rebootMarker exists from the moment kairos-agent is about to start until
// the next boot, whatever the outcome: kairos-agent swaps the slots before
// its last step can fail. The daemon exits when idle, so the state cannot
// live in memory only; /run is a tmpfs, so the reboot itself clears it. The
// file is "<pending word> <source>".
func rebootMarker() string { return filepath.Join(runDir(), "reboot-pending") }

// latestFile keeps the newest published tag across the daemon's idle exits,
// so a service started again by D-Bus still reports it.
func latestFile() string { return filepath.Join(runDir(), "latest") }

// setLatestLocked records the newest tag in memory and in the run dir. A
// failed write only costs the tag after the next idle exit.
func (d *Daemon) setLatestLocked(tag string) {
	if tag == d.latest {
		return
	}
	d.latest = tag
	if err := writeRunFile(latestFile(), tag+"\n"); err != nil {
		fmt.Fprintln(os.Stderr, "hadron-update: could not record the newest release:", err)
	}
}

// Inhibitor blocks shutdown and sleep while kairos-agent writes the passive
// slot. The real one is logind; tests use a fake.
type Inhibitor interface {
	// Inhibit takes the lock; release gives it back.
	Inhibit() (release func(), err error)
}

// releasesTTL is how long a fetched release list is reused. Every check,
// inspection and release listing needs it, and GitHub allows 60 anonymous
// calls an hour per address, shared by everything on the machine. A variable
// so tests can expire it.
var releasesTTL = 60 * time.Second

// job is the running Check or Apply. prev and prevDetail are what a cancel
// returns to.
type job struct {
	cancel     context.CancelFunc
	prev       State
	prevDetail string
}

// event is a queued callback: a status change, or a progress line.
type event struct {
	status   Status
	line     string
	progress bool
}

// Daemon owns the one update job of the system. Every field below mu is
// guarded by it. Callbacks never run under mu: they are queued and delivered,
// in order, by a goroutine that holds no lock while calling them.
type Daemon struct {
	backend Backend
	rel     Release // read-only after NewDaemon
	managed bool

	mu         sync.Mutex
	state      State
	pending    string // the marker's word while state is reboot-pending
	latest     string
	detail     string
	job        *job
	onChange   []func(Status)
	onProgress []func(string)
	events     []event
	delivering bool
	// rebooting is set from a Reboot call until backend.Reboot fails: nothing
	// may start kairos-agent while systemd shuts the system down.
	rebooting bool

	wg sync.WaitGroup // running Apply jobs; tests wait on it

	// inhibitor, when set, is held while kairos-agent runs. Set before the
	// first job; nil means none.
	inhibitor Inhibitor

	// ext runs the extension commands; nil refuses every extension job.
	ext                  ExtBackend
	extResult, extDetail string // guarded by mu
	extCat               extCache

	relMu    sync.Mutex
	relCache []Latest
	relOK    bool
	relAt    time.Time
}

// NewDaemon reads the release file and the reboot marker. Without a release
// file the system is not a Hadron Desktop install this daemon may update, and
// every mutating call is refused.
func NewDaemon(b Backend) *Daemon {
	d := &Daemon{backend: b, state: StateIdle}
	if e, ok := b.(ExtBackend); ok {
		d.ext = e
	}
	rel, err := LoadRelease(releaseFile())
	if err != nil {
		d.state, d.detail = StateUnmanaged, err.Error()
		return d
	}
	d.rel, d.managed = rel, true
	if b, err := os.ReadFile(latestFile()); err == nil {
		if tag := strings.TrimSpace(string(b)); strictTag.MatchString(tag) {
			d.latest = tag
		}
	}
	// Any answer but "does not exist" counts as pending: refusing an install
	// is recoverable, overwriting the known-good slot is not.
	if word, ok := readRebootMarker(); ok {
		d.state, d.pending, d.detail = StateRebootPending, word, pendingDetail(word)
	}
	return d
}

// OnChange registers f to receive every state change.
func (d *Daemon) OnChange(f func(Status)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onChange = append(d.onChange, f)
}

// OnProgress registers f to receive kairos-agent's output lines.
func (d *Daemon) OnProgress(f func(string)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onProgress = append(d.onProgress, f)
}

// Status is a snapshot. The pin and the advanced switch are read from disk
// each time, so an edit by hand shows up without a restart.
func (d *Daemon) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.statusLocked()
}

func (d *Daemon) statusLocked() Status {
	pin, _ := LoadPin()
	return Status{
		State:     d.state,
		Installed: d.rel.Version,
		Latest:    d.latest,
		Pin:       pin,
		Detail:    d.detail,
		Advanced:  AdvancedEnabled(),
		Pending:   d.pendingLocked(),
		ExtResult: d.extResult,
		ExtDetail: d.extDetail,
	}
}

func (d *Daemon) pendingLocked() string {
	if d.state != StateRebootPending {
		return ""
	}
	return d.pending
}

// rebootPendingErrLocked is the refusal while reboot-pending holds.
func (d *Daemon) rebootPendingErrLocked() error {
	if d.pending == pendingInstalled {
		return ErrRebootPending
	}
	return errUnfinishedPending
}

// setLocked changes the state and queues an OnChange event.
func (d *Daemon) setLocked(s State, detail string) {
	d.state, d.detail = s, detail
	d.queueLocked(event{status: d.statusLocked()})
}

func (d *Daemon) queueLocked(ev event) {
	d.events = append(d.events, ev)
	if !d.delivering {
		d.delivering = true
		go d.deliver()
	}
}

// deliver calls the callbacks for queued events until the queue is empty.
// Only one deliver runs at a time, so callbacks see events in order, and a
// callback may call back into the daemon.
func (d *Daemon) deliver() {
	for {
		d.mu.Lock()
		if len(d.events) == 0 {
			d.delivering = false
			d.mu.Unlock()
			return
		}
		evs := d.events
		d.events = nil
		onChange := append([]func(Status){}, d.onChange...)
		onProgress := append([]func(string){}, d.onProgress...)
		d.mu.Unlock()

		for _, ev := range evs {
			if ev.progress {
				for _, f := range onProgress {
					f(ev.line)
				}
				continue
			}
			for _, f := range onChange {
				f(ev.status)
			}
		}
	}
}

// Busy says whether a job runs. The bus service never exits while one does:
// the job would die with it.
func (d *Daemon) Busy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.busyLocked() || d.job != nil
}

func (d *Daemon) busyLocked() bool {
	return d.state == StateChecking || d.state == StateInspecting || d.state == StateApplying || d.state == StateExtension
}

// newest is the newest published tag; FetchReleases sorts newest first.
func newest(published []Latest) string {
	if len(published) == 0 {
		return ""
	}
	return published[0].Tag
}

// fetchReleases is FetchReleases, cached for releasesTTL. Errors are not
// cached, so a failed check can be retried at once. Apply may use a cached
// list: it decides trust from it, but installs only the digest it resolves
// and compares itself.
func (d *Daemon) fetchReleases(ctx context.Context) ([]Latest, error) {
	d.relMu.Lock()
	if d.relOK && time.Since(d.relAt) < releasesTTL {
		c := append([]Latest(nil), d.relCache...)
		d.relMu.Unlock()
		return c, nil
	}
	d.relMu.Unlock()

	published, err := FetchReleases(ctx, d.rel.GitHubRepo)
	if err != nil {
		return nil, err
	}
	d.relMu.Lock()
	d.relCache, d.relOK, d.relAt = append([]Latest(nil), published...), true, time.Now()
	d.relMu.Unlock()
	return published, nil
}

// Check asks GitHub for the published releases. On an unmanaged system it
// does nothing, not even a request. While a reboot is pending it only updates
// Latest: the pending install matters more than any newer release.
func (d *Daemon) Check(ctx context.Context) error {
	d.mu.Lock()
	if !d.managed {
		d.mu.Unlock()
		return nil
	}
	if d.busyLocked() {
		d.mu.Unlock()
		return ErrBusy
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pending := d.state == StateRebootPending
	j := &job{cancel: cancel, prev: d.state, prevDetail: d.detail}
	if !pending {
		d.job = j
		d.setLocked(StateChecking, "")
	}
	d.mu.Unlock()

	published, err := d.fetchReleases(ctx)

	d.mu.Lock()
	defer d.mu.Unlock()
	if pending {
		if err != nil {
			return err
		}
		d.setLatestLocked(newest(published))
		d.queueLocked(event{status: d.statusLocked()})
		return nil
	}
	if d.job != j {
		// Cancel already restored the previous state.
		return fmt.Errorf("check cancelled: %w", context.Canceled)
	}
	d.job = nil
	if err != nil {
		d.setLocked(StateError, "could not check for updates: "+err.Error())
		return err
	}
	d.setLatestLocked(newest(published))
	pin, _ := LoadPin()
	// A pin means the user chose this image on purpose; "latest" stays in
	// Status for the menu, but no update is announced.
	if pin == "" && Newer(d.latest, d.rel.Version) {
		d.setLocked(StateUpdateAvailable, "")
	} else {
		d.setLocked(StateUpToDate, "")
	}
	return nil
}

// Releases lists the published releases, newest first.
func (d *Daemon) Releases(ctx context.Context) ([]Latest, error) {
	if !d.managed {
		return nil, errUnmanaged
	}
	return d.fetchReleases(ctx)
}

// evaluate resolves r and decides whether it may be installed. Only a
// published release is installed without advanced mode. ErrNotHadron and
// ErrNotRelease (labelled, but not trusted by name) yield to advanced mode,
// and only when the caller asked for it and update.conf allows it; an agent
// image, a wrong platform or a private image is refused whatever the caller
// says. info and the verdict are returned whenever Resolve succeeded, so a
// refusal can still show what the image is.
func (d *Daemon) evaluate(ctx context.Context, r ImageRef, advanced bool) (ImageInfo, Verdict, []Latest, error) {
	info, err := Resolve(ctx, r)
	if err != nil {
		return ImageInfo{}, Verdict{}, nil, err
	}
	published, err := d.fetchReleases(ctx)
	if err != nil {
		return info, Verdict{}, nil, fmt.Errorf("could not list the published releases: %w", err)
	}
	v, err := Classify(info, d.rel, published)
	if err == nil && !v.Trusted {
		err = ErrNotRelease
	}
	overridable := errors.Is(err, ErrNotHadron) || errors.Is(err, ErrNotRelease)
	if overridable && advanced && AdvancedEnabled() {
		return info, v, published, nil
	}
	if err != nil {
		return info, v, published, fmt.Errorf("%s: %w", r, err)
	}
	return info, v, published, nil
}

// Inspect says what ref is and whether Apply would install it. It changes no
// state and needs no authorization.
func (d *Daemon) Inspect(ctx context.Context, ref string, advanced bool) (ImageInfo, Verdict, error) {
	if !d.managed {
		return ImageInfo{}, Verdict{}, errUnmanaged
	}
	r, err := ParseRef(ref)
	if err != nil {
		return ImageInfo{}, Verdict{}, err
	}
	info, v, _, err := d.evaluate(ctx, r, advanced)
	return info, v, err
}

// Apply starts installing ref and returns at once; the outcome shows up as
// the reboot-pending or error state. It trusts nothing the caller says: it
// resolves and classifies the image again, and installs only if ref still
// resolves to digest, the one the user reviewed. Bad input is refused here,
// before a job starts.
func (d *Daemon) Apply(ctx context.Context, ref, digest string, advanced bool) error {
	r, err := ParseRef(ref)
	if err != nil {
		return err
	}
	if !sha256Digest.MatchString(digest) {
		return fmt.Errorf("%q is not a sha256 digest", digest)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case !d.managed:
		return errUnmanaged
	case d.rebooting:
		return ErrRebooting
	case d.state == StateRebootPending:
		return d.rebootPendingErrLocked()
	case d.busyLocked():
		return ErrBusy
	}
	// The job outlives the call (a D-Bus method returns at once), so it gets
	// its own context; Cancel is the only way to stop it.
	jctx, cancel := context.WithCancel(context.Background())
	j := &job{cancel: cancel, prev: d.state, prevDetail: d.detail}
	d.job = j
	d.setLocked(StateInspecting, "")
	d.wg.Add(1)
	go d.run(jctx, j, r, ref, digest, advanced)
	return nil
}

func (d *Daemon) run(ctx context.Context, j *job, r ImageRef, ref, digest string, advanced bool) {
	defer d.wg.Done()
	defer j.cancel()

	info, v, published, err := d.evaluate(ctx, r, advanced)
	if err == nil && info.Digest != digest {
		err = fmt.Errorf("%s now resolves to digest %s, not the reviewed %s; inspect it again", ref, info.Digest, digest)
	}

	d.mu.Lock()
	if d.job != j {
		d.mu.Unlock() // cancelled; Cancel restored the state
		return
	}
	if err == nil && d.rebooting {
		err = ErrRebooting
	}
	// Install the digest, never the tag: the tag may have moved since Inspect.
	source := "oci:" + info.Ref.Repo() + "@" + info.Digest
	// The marker goes down before kairos-agent starts, so a daemon that dies
	// or restarts mid-install comes back refusing a second install over a
	// half-written or never-booted slot. No marker, no install.
	if err == nil {
		if werr := writeRebootMarker(pendingStarted, source); werr != nil {
			err = fmt.Errorf("could not record the pending install, so it was not started: %w", werr)
		}
	}
	if err != nil {
		d.job = nil
		d.setLocked(StateError, err.Error())
		d.mu.Unlock()
		return
	}
	if l := newest(published); l != "" {
		d.setLatestLocked(l)
	}
	// From here on Cancel refuses, so the job cannot be abandoned mid-write.
	d.setLocked(StateApplying, "")
	d.mu.Unlock()

	release := d.inhibitShutdown()
	err = d.backend.Upgrade(context.Background(), source, d.progress)
	release()
	if err != nil {
		// No retry before a restart. kairos-agent (kairos-io/kairos
		// agent/pkg/action/upgrade.go, UpgradeAction.Run) renames
		// active.img to passive.img and the new image to active.img, and
		// only then runs the after-upgrade hook, whose error it returns. So
		// an error can come after the swap: the passive slot then holds the
		// system that runs now, and a second install would push the
		// never-booted image over it. The marker stays, and so does
		// reboot-pending, on every failure.
		fmt.Fprintf(os.Stderr, "hadron-update: the upgrade to %s failed: %v (last line: %q)\n", source, err, d.lastLine())
		if werr := writeRebootMarker(pendingFailed, source); werr != nil {
			// The marker still says started: reboot-pending all the same.
			fmt.Fprintln(os.Stderr, "hadron-update: could not record the failure in the marker:", werr)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.job = nil
		d.pending = pendingFailed
		d.setLocked(StateRebootPending, pendingDetail(pendingFailed))
		return
	}
	if werr := writeRebootMarker(pendingInstalled, source); werr != nil {
		// The marker still says started; a restarted daemon words the
		// state as interrupted. The install itself succeeded.
		fmt.Fprintln(os.Stderr, "hadron-update: could not record the finished install in the marker:", werr)
	}

	var notes []string
	// A pin keeps "update available" quiet after a deliberate choice. The
	// newest published release of any flavor is not a choice to keep: it is
	// "install latest" or "switch desktop".
	if v.Trusted && v.Version == newest(published) {
		err = ClearPin()
	} else {
		err = SavePin(ref)
	}
	if err != nil {
		notes = append(notes, "could not update the pin: "+err.Error())
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.job = nil
	detail := d.detail // kairos-agent's last line
	if len(notes) > 0 {
		detail = strings.Join(notes, "; ")
	}
	d.pending = pendingInstalled
	d.setLocked(StateRebootPending, detail)
}

// inhibitShutdown takes the shutdown lock for the write. Without it the
// upgrade still runs: refusing an update because logind is missing would
// leave the system on the old image for no gain.
func (d *Daemon) inhibitShutdown() (release func()) {
	if d.inhibitor == nil {
		return func() {}
	}
	release, err := d.inhibitor.Inhibit()
	if err != nil {
		fmt.Fprintln(os.Stderr, "hadron-update: could not block shutdown during the upgrade:", err)
		return func() {}
	}
	return release
}

func (d *Daemon) progress(line string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.detail = line
	d.queueLocked(event{line: line, progress: true})
}

// lastLine is kairos-agent's last progress line, for the log.
func (d *Daemon) lastLine() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.detail
}

// writeRebootMarker writes "<word> <source>" through a temp file and a
// rename, so the marker is never empty or half-written.
func writeRebootMarker(word, source string) error {
	return writeRunFile(rebootMarker(), word+" "+source+"\n")
}

func writeRunFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// readRebootMarker says whether a marker exists and its word. Any answer but
// "does not exist" counts as pending: refusing an install is recoverable,
// overwriting the known-good slot is not. A marker that cannot be read or
// names no known word is an install of unknown outcome: started.
func readRebootMarker() (word string, ok bool) {
	b, err := os.ReadFile(rebootMarker())
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err == nil {
		w, _, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
		if w == pendingInstalled || w == pendingFailed {
			return w, true
		}
	}
	return pendingStarted, true
}

// Cancel stops a check or the inspecting phase of an install and returns to
// the state before it. Once kairos-agent runs it refuses.
func (d *Daemon) Cancel() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch d.state {
	case StateChecking, StateInspecting:
		j := d.job
		d.job = nil
		j.cancel()
		prev, detail := j.prev, j.prevDetail
		if prev == StateError {
			// The error was the last job's; restoring it would show it as
			// news. Nothing is wrong now.
			prev, detail = StateIdle, ""
		}
		d.setLocked(prev, detail)
		return nil
	case StateApplying:
		return ErrNotCancellable
	case StateExtension:
		return errExtRunning
	default:
		return errNothingToCancel
	}
}

// Reboot restarts the system. It refuses while an install job runs: during
// inspecting the job could still start kairos-agent under a shutting-down
// system, and an extension job runs it. Once it is asked, no install starts
// until the reboot fails.
func (d *Daemon) Reboot() error {
	d.mu.Lock()
	if d.state == StateInspecting || d.state == StateApplying || d.state == StateExtension {
		d.mu.Unlock()
		return ErrBusy
	}
	d.rebooting = true
	d.mu.Unlock()

	if err := d.backend.Reboot(); err != nil {
		d.mu.Lock()
		d.rebooting = false
		d.mu.Unlock()
		return err
	}
	return nil
}

// errPreviousAfterInstall is RebootPrevious's refusal while an install waits
// for its reboot: the passive slot then holds the system the user runs now,
// and the active slot the new one, so "previous" would boot neither the
// system before this one nor the update.
var errPreviousAfterInstall = fmt.Errorf("%w; restart into it first, then the system you run now is the previous one", ErrRebootPending)

// RebootPrevious boots the previous system once. It refuses what Reboot
// refuses, and also while a reboot was already asked (the selection would
// change what that reboot boots) and while an install waits for its reboot.
func (d *Daemon) RebootPrevious() error {
	d.mu.Lock()
	switch {
	case d.rebooting:
		d.mu.Unlock()
		return ErrRebooting
	case d.state == StateInspecting || d.state == StateApplying || d.state == StateExtension:
		d.mu.Unlock()
		return ErrBusy
	case d.state == StateRebootPending && d.pending == pendingInstalled:
		d.mu.Unlock()
		return errPreviousAfterInstall
	case d.state == StateRebootPending:
		d.mu.Unlock()
		return errUnfinishedPending
	}
	d.rebooting = true
	d.mu.Unlock()

	if err := d.backend.RebootPrevious(); err != nil {
		d.mu.Lock()
		d.rebooting = false
		d.mu.Unlock()
		return err
	}
	return nil
}

// kairosBackend is the real Backend.
type kairosBackend struct{}

// Upgrade runs kairos-agent with stdout and stderr merged into one pipe, so
// progress lines keep their order. ctx is ignored on purpose: the upgrade is
// not cancellable once started (see ErrNotCancellable).
func (kairosBackend) Upgrade(_ context.Context, source string, progress func(string)) error {
	if err := streamCommand(kairosAgent(), []string{"upgrade", "--source", source}, progress); err != nil {
		return fmt.Errorf("kairos-agent upgrade: %w", err)
	}
	return nil
}

// scanLinesOrCR splits on \n and on \r, so a progress bar that redraws one
// line with \r still yields its updates.
func scanLinesOrCR(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func (kairosBackend) Reboot() error {
	out, err := exec.Command("systemctl", "reboot").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl reboot: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// bootModeDir holds immucore's boot-mode sentinels (active_mode,
// passive_mode, recovery_mode). A variable so tests can point it elsewhere.
var bootModeDir = "/run/cos"

// previousEntry is the boot entry of the passive slot, the same name on GRUB
// and on systemd-boot. kairos-agent resolves it, see RebootPrevious.
const previousEntry = "fallback"

// RebootPrevious runs `kairos-agent bootentry --select fallback`, then
// `systemctl reboot`. kairos-agent does the selection the way its own
// upgrade does (kairos-io/kairos agent/pkg/action/upgrade.go, which selects
// "cos" after an upgrade), and picks the boot loader itself:
//
//   - agent/pkg/cmd/root.go newBootentryCmd: --select calls
//     action.SelectBootEntry with no prompt.
//   - agent/pkg/action/bootentries.go SelectBootEntry: utils.IsUkiWithFs
//     chooses systemd-boot (Trusted Boot) or GRUB.
//   - GRUB (selectBootEntryGrub): checks "fallback" is a menu entry id, then
//     writes next_entry=fallback to /oem/grubenv (to the STATE grubenv when
//     OEM is encrypted). kairos-init's grub.cfg (kairos-init/pkg/bundled/
//     bundled.go) boots next_entry once and clears it with save_env; the
//     passive entry is `menuentry ... --id fallback`.
//   - systemd-boot (selectBootEntrySystemd): "fallback" resolves to
//     passive.conf (bootNameToSystemdConf) and is written to the
//     LoaderEntryOneShot EFI variable, which systemd-boot uses once.
//
// So the selection is one-shot on both: the boot after the previous system is
// the current one again. If the selection fails nothing reboots: the restart
// would only boot this same system.
func (kairosBackend) RebootPrevious() error {
	// Booted from passive (a fallback after a failed boot, or a rollback not
	// restarted since), "fallback" is this very system and the restart would
	// change nothing.
	if _, err := os.Stat(filepath.Join(bootModeDir, "passive_mode")); err == nil {
		return errors.New("this system already runs the previous system; a normal restart returns to the current one")
	}
	out, err := exec.Command(kairosAgent(), "bootentry", "--select", previousEntry).CombinedOutput()
	if err != nil {
		return fmt.Errorf("kairos-agent bootentry --select %s: %w: %s", previousEntry, err, strings.TrimSpace(string(out)))
	}
	if err := (kairosBackend{}).Reboot(); err != nil {
		// The one-shot selection stays: the next boot, whenever it comes,
		// starts the previous system. The user must know that.
		return fmt.Errorf("The previous system is selected for the next boot; restarting failed: %w", err)
	}
	return nil
}
