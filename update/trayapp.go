package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// trayView is everything the tray shows at one moment.
type trayView struct {
	Icon    string // a key of iconColors
	Tooltip string
	Menu    []MenuItem
}

// trayUI shows a view. The systray glue implements it; `tray --no-icon`
// (and so `notify`) uses a UI that shows nothing, and tests record the views.
type trayUI interface{ Show(trayView) }

type noUI struct{}

func (noUI) Show(trayView) {}

// trayRetry is how long the tray waits before dialling an unreachable
// update service again.
const trayRetry = 30 * time.Second

// unavailableText is the tooltip and menu line while the service cannot be
// reached.
const unavailableText = "Update service not available"

var errServiceUnavailable = errors.New("the update service is not available")

// trayApp is the tray without the tray library: it follows the daemon,
// checks on a schedule, announces new releases and acts on menu clicks. All
// its state lives in the one goroutine that runs run.
type trayApp struct {
	rel  Release
	ui   trayUI
	dial func() (daemonClient, error)
	log  *log.Logger
	// clicks carries menu item IDs from the tray library to run.
	clicks chan string
	// notify and launch are seams: a desktop notification, and a terminal
	// running `hadron-update <args>`.
	notify func(summary, body string) error
	launch func(args ...string) error
	// openMenu opens an entry of hadron-menu, the same one Super+Escape
	// shows: "Extensions" is one list on every variant.
	openMenu func(entry string) error
	// noIcon makes the notification point at the CLI: there is no menu.
	noIcon bool

	firstCheck, interval, retry time.Duration

	st       Status
	releases []Latest
	line     string    // the last progress line of the running job
	down     bool      // the service was unreachable at the last try
	due      time.Time // when the next Check runs
}

func newTrayApp(rel Release, ui trayUI, dial func() (daemonClient, error), logOut io.Writer) *trayApp {
	a := &trayApp{
		rel: rel, ui: ui, dial: dial,
		log:        log.New(logOut, "hadron-update: ", 0),
		clicks:     make(chan string, 8),
		notify:     Notify,
		firstCheck: notifyDelay, interval: notifyInterval, retry: trayRetry,
	}
	a.launch = func(args ...string) error { return openTerminal(args, logOut) }
	a.openMenu = openHadronMenu
	return a
}

// run follows the service until ctx ends. It never gives up: an unreachable
// service shows the error icon and is dialled again every a.retry.
func (a *trayApp) run(ctx context.Context) {
	for ctx.Err() == nil {
		c, err := a.dial()
		if err == nil {
			err = a.session(ctx, c)
			c.Close()
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// Log the loss once, not at every retry.
			if !a.down {
				a.log.Printf("the update service is not available: %v", err)
			}
			a.down = true
			a.ui.Show(trayView{Icon: "error", Tooltip: unavailableText, Menu: []MenuItem{{ID: "status", Label: unavailableText}}})
		}
		a.idle(ctx, a.retry)
	}
}

// idle waits d, still answering clicks: a terminal item needs no service.
func (a *trayApp) idle(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			return
		case id := <-a.clicks:
			a.click(nil, id)
		}
	}
}

// session follows one connection. It returns nil when ctx ends and an error
// when the service cannot be reached, so run shows the error and dials again.
func (a *trayApp) session(ctx context.Context, c daemonClient) error {
	st, err := c.Status()
	if err != nil {
		return err
	}
	events, stop, err := c.Watch()
	if err != nil {
		return err
	}
	a.down = false
	a.setStatus(st)
	defer func() { stop() }()

	// An unmanaged system has nothing to check against, so no timer runs.
	var timer <-chan time.Time
	var t *time.Timer
	if st.State != StateUnmanaged {
		if a.due.IsZero() {
			a.due = time.Now().Add(a.firstCheck)
		}
		t = time.NewTimer(time.Until(a.due))
		defer t.Stop()
		timer = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case id := <-a.clicks:
			a.click(c, id)
		case <-timer:
			if busyState(a.st.State) {
				// The service is busy with a job: Check would only answer
				// Busy, after retryBusy held this goroutine for seconds.
				t.Reset(a.retry)
				continue
			}
			if err := a.check(c); err != nil && daemonErrName(err) == "" {
				// The bus or the activation failed, not the check: keep
				// the schedule, and try again once the service is back.
				return err
			}
			a.due = time.Now().Add(a.interval)
			t.Reset(a.interval)
		case ev, ok := <-events:
			if !ok {
				a.serviceGone()
				stop()
				if events, stop, err = c.Watch(); err != nil {
					return err
				}
				continue
			}
			if ev.Status != nil {
				a.setStatus(*ev.Status)
			} else {
				a.line = ev.Line
				a.render()
			}
		}
	}
}

// serviceGone handles the service's name losing its owner. After an idle
// exit nothing changes. During a job it means the service died with it: the
// job is lost, and the menu must not keep promising it.
func (a *trayApp) serviceGone() {
	if busyState(a.st.State) {
		st := a.st
		st.State, st.Detail = StateError, "the update service stopped during the update"
		a.setStatus(st)
	}
}

// check runs one scheduled check, refreshes the menu and announces a new
// release once.
func (a *trayApp) check(c daemonClient) error {
	if err := retryBusy(c.Check); err != nil {
		a.log.Printf("checking for updates: %v", err)
		return err
	}
	st, err := c.Status()
	if err != nil {
		return err
	}
	var tags []string
	if err := retryBusy(func() (err error) { tags, err = c.Releases(); return }); err == nil {
		a.releases = a.releases[:0]
		for _, t := range tags {
			a.releases = append(a.releases, Latest{Tag: t})
		}
	} else {
		a.log.Printf("listing releases: %v", err)
	}
	a.setStatus(st)
	if err := announce(c, a.rel, st, a.noIcon, a.notify); err != nil {
		a.log.Print(err)
	}
	return nil
}

func (a *trayApp) setStatus(st Status) {
	if st.State != StateInspecting && st.State != StateApplying {
		a.line = ""
	}
	a.st = st
	a.render()
}

func (a *trayApp) render() {
	tip := statusLine(a.st)
	if a.line != "" {
		tip = a.line
	}
	a.ui.Show(trayView{Icon: iconFor(a.st.State), Tooltip: tip, Menu: BuildMenu(a.st, a.releases, a.rel.Flavor, a.rel.Line())})
}

// click acts on a menu item. Anything that needs details or an answer opens
// a terminal running the CLI, which waits for Enter before closing over an
// error (--pause-on-error); Cancel, Restart and Restart into the previous
// system are the click itself, and polkit decides. c is nil while the service is unreachable.
func (a *trayApp) click(c daemonClient, id string) {
	var err error
	switch {
	case id == "install-latest":
		err = a.launch("upgrade", "--pause-on-error")
	case strings.HasPrefix(id, "releases/"):
		err = a.launch("upgrade", "--pause-on-error", "--version", strings.TrimPrefix(id, "releases/"))
	case strings.HasPrefix(id, "switch/"):
		err = a.launch("switch", strings.TrimPrefix(id, "switch/"), "--pause-on-error")
	case strings.HasPrefix(id, "gpu/"):
		err = a.launch("switch", "--gpu", strings.TrimPrefix(id, "gpu/"), "--pause-on-error")
	case id == "advanced":
		err = a.launch("upgrade", "--pause-on-error", "--image", "-")
	case id == "extensions":
		err = a.openMenu("Extensions")
	case id == "recovery":
		err = a.openMenu("Recovery key")
	case c == nil && (id == "cancel" || id == "restart" || id == "rollback"):
		err = errServiceUnavailable
	case id == "cancel":
		err = c.Cancel()
	case id == "restart":
		err = c.Reboot()
	case id == "rollback":
		err = c.RebootPrevious()
	default:
		return
	}
	if err != nil {
		a.log.Printf("%s: %v", id, err)
		msg := describeDaemonError(err)
		if id == "rollback" {
			msg = describeRollbackError(err, a.st.Pending)
		}
		if nerr := a.notify("Hadron Desktop update", msg); nerr != nil {
			a.log.Printf("showing a notification: %v", nerr)
		}
	}
}

// announce shows one notification per new release, only while the daemon
// says an update is available (a pinned system says up-to-date), and only
// once the release's image can be installed: a release published before its
// image, or one whose package is not public yet, is announced on a later
// check instead.
func announce(c daemonClient, rel Release, st Status, noIcon bool, notify func(summary, body string) error) error {
	if st.State != StateUpdateAvailable || st.Latest == "" || alreadyNotified(st.Latest) {
		return nil
	}
	ref, err := ReleaseRef(rel, rel.Flavor, st.Latest)
	if err != nil {
		return err
	}
	var info InspectInfo
	err = retryBusy(func() (err error) { info, err = c.Inspect(ref.String(), false); return })
	if err == nil && info.Refusal != "" {
		err = errors.New(info.Refusal)
	}
	if err != nil {
		return fmt.Errorf("%s is out but not installable yet: %w", st.Latest, err)
	}
	body := fmt.Sprintf("You are on %s. Install it from the update icon's menu in the panel.", orUnknown(st.Installed))
	if noIcon {
		body = fmt.Sprintf("You are on %s. Run `hadron-update upgrade` in a terminal to install it.", orUnknown(st.Installed))
	}
	if err := notify(fmt.Sprintf("Hadron Desktop %s is available", st.Latest), body); err != nil {
		return err
	}
	if err := markNotified(st.Latest); err != nil {
		// The user was told; only the record failed, so the next session
		// announces it again.
		return fmt.Errorf("recording that %s was announced: %w", st.Latest, err)
	}
	return nil
}

// busyState reports whether the service is running a job.
func busyState(s State) bool {
	return s == StateChecking || s == StateInspecting || s == StateApplying || s == StateExtension
}

// defaultSessionFile names the variant's session settings, among them
// TERMINAL_EXEC, the terminal the tray opens the CLI in.
const defaultSessionFile = "/etc/hadron-desktop/session"

func sessionFile() string {
	if p := os.Getenv("HADRON_UPDATE_SESSION_FILE"); p != "" {
		return p
	}
	return defaultSessionFile
}

// terminalArgv is the command that runs `self args...` in a terminal window.
// TERMINAL_EXEC is split on spaces, never through a shell. Without it, foot
// and then xterm are tried, and the fallback is logged.
func terminalArgv(self string, args []string, logOut io.Writer) ([]string, error) {
	kv, _ := readKV(sessionFile())
	if te := strings.Fields(kv["TERMINAL_EXEC"]); len(te) > 0 {
		return append(append(te, self), args...), nil
	}
	for _, t := range []struct {
		bin string
		pre []string
	}{{"foot", nil}, {"xterm", []string{"-e"}}} {
		if p, err := exec.LookPath(t.bin); err == nil {
			fmt.Fprintf(logOut, "hadron-update: %s sets no TERMINAL_EXEC; using %s\n", sessionFile(), p)
			return append(append(append([]string{p}, t.pre...), self), args...), nil
		}
	}
	return nil, fmt.Errorf("no terminal to open: %s sets no TERMINAL_EXEC, and neither foot nor xterm is installed", sessionFile())
}

// openHadronMenu runs `hadron-menu ENTRY` and does not wait for it.
func openHadronMenu(entry string) error {
	cmd := exec.Command("hadron-menu", entry)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// openTerminal starts the CLI in a terminal and does not wait for it.
func openTerminal(args []string, logOut io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		self = "hadron-update"
	}
	argv, err := terminalArgv(self, args, logOut)
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap it; the terminal's exit status is not ours to report
	return nil
}
