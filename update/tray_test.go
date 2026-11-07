package main

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// trayFake is a daemonClient safe to use from the tray's goroutine and the
// test at once. Check moves the status to afterCheck, as the daemon would.
type trayFake struct {
	mu          sync.Mutex
	st          Status
	afterCheck  *Status
	tags        []string
	inspectErr  error
	refusal     string
	rebootErr   error
	previousErr error
	calls       []string
	events      chan Event
	watches     int
}

func (f *trayFake) rec(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *trayFake) count(s string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == s {
			n++
		}
	}
	return n
}
func (f *trayFake) Status() (Status, error) {
	f.rec("Status")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st, nil
}
func (f *trayFake) Check() error {
	f.rec("Check")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.afterCheck != nil {
		f.st = *f.afterCheck
	}
	return nil
}
func (f *trayFake) Releases() ([]string, error) { f.rec("Releases"); return f.tags, nil }
func (f *trayFake) Inspect(ref string, adv bool) (InspectInfo, error) {
	f.rec("Inspect " + ref)
	if f.inspectErr != nil {
		return InspectInfo{}, f.inspectErr
	}
	return InspectInfo{Ref: ref, Digest: digestA, Trusted: true, Refusal: f.refusal}, nil
}
func (f *trayFake) Apply(ref, digest string, adv bool) error { f.rec("Apply"); return nil }
func (f *trayFake) Cancel() error                            { f.rec("Cancel"); return nil }
func (f *trayFake) Reboot() error                            { f.rec("Reboot"); return f.rebootErr }
func (f *trayFake) RebootPrevious() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "RebootPrevious")
	return f.previousErr
}
func (f *trayFake) Watch() (<-chan Event, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = make(chan Event, 16)
	f.watches++
	return f.events, func() {}, nil
}
func (f *trayFake) Close() {}

// push sends ev on the current watch channel; lose closes it, as when the
// service's name loses its owner.
func (f *trayFake) push(ev Event) { f.mu.Lock(); ch := f.events; f.mu.Unlock(); ch <- ev }
func (f *trayFake) lose()         { f.mu.Lock(); close(f.events); f.mu.Unlock() }

// recUI keeps every view the tray showed.
type recUI struct {
	mu    sync.Mutex
	views []trayView
}

func (u *recUI) Show(v trayView) { u.mu.Lock(); u.views = append(u.views, v); u.mu.Unlock() }

// waitView waits until the latest view satisfies ok.
func (u *recUI) waitView(t *testing.T, what string, ok func(trayView) bool) trayView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		u.mu.Lock()
		var last trayView
		n := len(u.views)
		if n > 0 {
			last = u.views[n-1]
		}
		u.mu.Unlock()
		if n > 0 && ok(last) {
			return last
		}
		time.Sleep(2 * time.Millisecond)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	t.Fatalf("never showed %s; views %+v", what, u.views)
	return trayView{}
}

// syncBuf is a log sink the tray goroutine and the test share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// trayHarness runs a trayApp against f until the test ends.
type trayHarness struct {
	app      *trayApp
	ui       *recUI
	log      *syncBuf
	mu       sync.Mutex
	notes    []string
	launches [][]string
	menus    []string
}

func (h *trayHarness) notified() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.notes...)
}

func (h *trayHarness) launched() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]string(nil), h.launches...)
}

func startTray(t *testing.T, dial func() (daemonClient, error), first, every time.Duration) *trayHarness {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h := &trayHarness{ui: &recUI{}, log: &syncBuf{}}
	rel := Release{Flavor: "sway", Version: "v0.1.0", ImageRepo: "ghcr.io/kairos-community/hadron-desktop", GitHubRepo: "kairos-community/hadron-desktop"}
	h.app = newTrayApp(rel, h.ui, dial, h.log)
	h.app.firstCheck, h.app.interval, h.app.retry = first, every, 5*time.Millisecond
	h.app.notify = func(summary, body string) error {
		h.mu.Lock()
		h.notes = append(h.notes, summary+" | "+body)
		h.mu.Unlock()
		return nil
	}
	h.app.launch = func(args ...string) error {
		h.mu.Lock()
		h.launches = append(h.launches, args)
		h.mu.Unlock()
		return nil
	}
	h.app.openMenu = func(entry string) error {
		h.mu.Lock()
		h.menus = append(h.menus, entry)
		h.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.app.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return h
}

func dialFake(f *trayFake) func() (daemonClient, error) {
	return func() (daemonClient, error) { return f, nil }
}

func available() *Status {
	return &Status{State: StateUpdateAvailable, Installed: "v0.1.0", Latest: "v0.2.0", Advanced: true}
}

func TestTrayChecksThenNotifiesOncePerTag(t *testing.T) {
	f := &trayFake{st: Status{State: StateIdle, Installed: "v0.1.0", Advanced: true}, afterCheck: available(), tags: []string{"v0.2.0", "v0.1.0"}}
	h := startTray(t, dialFake(f), time.Millisecond, 5*time.Millisecond)
	v := h.ui.waitView(t, "the update", func(v trayView) bool {
		_, ok := find(v.Menu, "releases/v0.2.0")
		return v.Icon == "update" && ok
	})
	if it, _ := find(v.Menu, "install-latest"); it.Label != "Install v0.2.0" {
		t.Fatalf("menu %+v", v.Menu)
	}
	for f.count("Check") < 3 {
		time.Sleep(time.Millisecond)
	}
	notes := h.notified()
	if len(notes) != 1 || !strings.Contains(notes[0], "Hadron Desktop v0.2.0 is available") || !strings.Contains(notes[0], "menu") {
		t.Fatalf("notifications %q, want one pointing at the tray menu", notes)
	}
	if strings.Contains(notes[0], "terminal") {
		t.Fatalf("notification still sends the user to a terminal: %q", notes[0])
	}
	if f.count("Inspect ghcr.io/kairos-community/hadron-desktop:sway-v0.2.0") != 1 {
		t.Fatalf("calls %v: the release image must be inspected once before announcing it", f.calls)
	}
}

func TestTrayWaitsBeforeTheFirstCheck(t *testing.T) {
	f := &trayFake{st: Status{State: StateIdle, Installed: "v0.1.0"}}
	h := startTray(t, dialFake(f), time.Hour, time.Hour)
	h.ui.waitView(t, "the idle icon", func(v trayView) bool { return v.Icon == "idle" })
	time.Sleep(20 * time.Millisecond)
	if f.count("Check") != 0 {
		t.Fatal("checked on the login path")
	}
}

func TestTrayQuietWhilePinned(t *testing.T) {
	pinned := &Status{State: StateUpToDate, Installed: "v0.1.0", Latest: "v0.2.0", Pin: "ghcr.io/x/y:z"}
	f := &trayFake{st: Status{State: StateIdle}, afterCheck: pinned}
	h := startTray(t, dialFake(f), time.Millisecond, 5*time.Millisecond)
	v := h.ui.waitView(t, "the pinned status", func(v trayView) bool { return strings.Contains(v.Tooltip, "pinned") })
	if it, _ := find(v.Menu, "install-latest"); it.Label != "Install latest (v0.2.0)" {
		t.Fatalf("menu %+v", v.Menu)
	}
	for f.count("Check") < 2 {
		time.Sleep(time.Millisecond)
	}
	if n := h.notified(); len(n) != 0 {
		t.Fatalf("notified while pinned: %q", n)
	}
}

func TestTrayQuietWhenTheImageIsNotThere(t *testing.T) {
	f := &trayFake{st: Status{State: StateIdle}, afterCheck: available(), inspectErr: errors.New("manifest unknown")}
	h := startTray(t, dialFake(f), time.Millisecond, 5*time.Millisecond)
	for f.count("Check") < 2 {
		time.Sleep(time.Millisecond)
	}
	if n := h.notified(); len(n) != 0 || alreadyNotified("v0.2.0") {
		t.Fatalf("announced an image that cannot be installed: %q", n)
	}
}

func TestTrayUnmanagedNeverChecks(t *testing.T) {
	f := &trayFake{st: Status{State: StateUnmanaged, Advanced: true}}
	h := startTray(t, dialFake(f), time.Millisecond, time.Millisecond)
	v := h.ui.waitView(t, "unmanaged", func(v trayView) bool { return len(v.Menu) > 0 })
	if got := strings.Join(ids(v.Menu), ","); got != "status" {
		t.Fatalf("menu %v", got)
	}
	time.Sleep(20 * time.Millisecond)
	if f.count("Check") != 0 || f.count("Releases") != 0 {
		t.Fatalf("calls %v on an unmanaged system", f.calls)
	}
}

func TestTrayRetriesAnUnavailableService(t *testing.T) {
	f := &trayFake{st: Status{State: StateIdle, Installed: "v0.1.0"}}
	var mu sync.Mutex
	fails := 3
	dial := func() (daemonClient, error) {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			return nil, errors.New("no bus")
		}
		return f, nil
	}
	h := startTray(t, dial, time.Hour, time.Hour)
	h.ui.waitView(t, "the error icon", func(v trayView) bool {
		return v.Icon == "error" && v.Tooltip == "Update service not available"
	})
	h.ui.waitView(t, "recovery", func(v trayView) bool { return v.Icon == "idle" })
	// Three failed dials, one line: the loss is logged, not every retry.
	if n := strings.Count(h.log.String(), "not available"); n != 1 {
		t.Fatalf("logged the outage %d times: %q", n, h.log.String())
	}
}

// A scheduled check while a job runs would only get Busy, after retryBusy
// held the tray for seconds; it waits for the job instead.
func TestTraySkipsTheCheckDuringAJob(t *testing.T) {
	f := &trayFake{st: Status{State: StateApplying, Installed: "v0.1.0"}}
	h := startTray(t, dialFake(f), time.Millisecond, time.Millisecond)
	h.ui.waitView(t, "busy", func(v trayView) bool { return v.Icon == "busy" })
	time.Sleep(30 * time.Millisecond)
	if f.count("Check") != 0 {
		t.Fatal("checked while a job runs")
	}
	f.push(Event{Status: &Status{State: StateRebootPending, Installed: "v0.1.0"}})
	deadline := time.Now().Add(5 * time.Second)
	for f.count("Check") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no check after the job ended")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTrayLogsAFailedNotificationRecord(t *testing.T) {
	f := &trayFake{st: Status{State: StateIdle}, afterCheck: available()}
	// The first check waits long enough for the state dir to be broken below.
	h := startTray(t, dialFake(f), 50*time.Millisecond, time.Hour)
	// A file where the state directory should be: markNotified cannot write.
	blocker := filepath.Join(t.TempDir(), "state")
	os.WriteFile(blocker, nil, 0o644)
	t.Setenv("XDG_STATE_HOME", blocker)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(h.log.String(), "recording that v0.2.0 was announced") {
		if time.Now().After(deadline) {
			t.Fatalf("log %q", h.log.String())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTrayProgressIsTheTooltip(t *testing.T) {
	f := &trayFake{st: Status{State: StateIdle, Installed: "v0.1.0", Latest: "v0.2.0"}}
	h := startTray(t, dialFake(f), time.Hour, time.Hour)
	h.ui.waitView(t, "idle", func(v trayView) bool { return v.Icon == "idle" })
	f.push(Event{Status: &Status{State: StateApplying, Installed: "v0.1.0", Latest: "v0.2.0"}})
	f.push(Event{Line: "pulling layer 3/7"})
	h.ui.waitView(t, "progress", func(v trayView) bool { return v.Icon == "busy" && v.Tooltip == "pulling layer 3/7" })
	f.push(Event{Status: &Status{State: StateRebootPending, Installed: "v0.1.0"}})
	v := h.ui.waitView(t, "reboot", func(v trayView) bool { return v.Icon == "reboot" })
	if _, ok := find(v.Menu, "restart"); !ok || strings.Contains(v.Tooltip, "pulling") {
		t.Fatalf("view %+v", v)
	}
}

func TestTrayServiceLostMidJob(t *testing.T) {
	f := &trayFake{st: Status{State: StateApplying, Installed: "v0.1.0"}}
	h := startTray(t, dialFake(f), time.Hour, time.Hour)
	h.ui.waitView(t, "busy", func(v trayView) bool { return v.Icon == "busy" })
	f.lose()
	h.ui.waitView(t, "the error", func(v trayView) bool {
		return v.Icon == "error" && strings.Contains(v.Tooltip, "stopped")
	})
	// It watches again, so a restarted service is seen.
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		w := f.watches
		f.mu.Unlock()
		if w >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("did not watch again")
		}
		time.Sleep(time.Millisecond)
	}
	f.push(Event{Status: &Status{State: StateIdle, Installed: "v0.1.0"}})
	h.ui.waitView(t, "idle again", func(v trayView) bool { return v.Icon == "idle" })
}

// An idle exit is normal: the menu stays as it was.
func TestTrayServiceIdleExitKeepsTheMenu(t *testing.T) {
	f := &trayFake{st: Status{State: StateUpToDate, Installed: "v0.1.0", Latest: "v0.1.0"}}
	h := startTray(t, dialFake(f), time.Hour, time.Hour)
	h.ui.waitView(t, "up to date", func(v trayView) bool { return v.Icon == "idle" })
	h.ui.mu.Lock()
	n := len(h.ui.views)
	h.ui.mu.Unlock()
	f.lose()
	time.Sleep(20 * time.Millisecond)
	h.ui.mu.Lock()
	defer h.ui.mu.Unlock()
	for _, v := range h.ui.views[n:] {
		if v.Icon != "idle" {
			t.Fatalf("view %+v after an idle exit", v)
		}
	}
}

func TestTrayClicks(t *testing.T) {
	f := &trayFake{st: Status{State: StateRebootPending, Installed: "v0.1.0"}}
	h := startTray(t, dialFake(f), time.Hour, time.Hour)
	h.ui.waitView(t, "reboot", func(v trayView) bool { return v.Icon == "reboot" })
	for _, id := range []string{"install-latest", "releases/v0.2.0", "switch/i3", "gpu/vm", "advanced", "cancel"} {
		h.app.clicks <- id
	}
	want := [][]string{
		{"upgrade", "--pause-on-error"},
		{"upgrade", "--pause-on-error", "--version", "v0.2.0"},
		{"switch", "i3", "--pause-on-error"},
		{"switch", "--gpu", "vm", "--pause-on-error"},
		{"upgrade", "--pause-on-error", "--image", "-"},
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.count("Cancel") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := h.launched(); !reflect.DeepEqual(got, want) {
		t.Fatalf("launched %q, want %q", got, want)
	}
	f.mu.Lock()
	f.rebootErr = &daemonError{Name: "NotAuthorized", Msg: "no"}
	f.mu.Unlock()
	h.app.clicks <- "restart"
	for len(h.notified()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.count("Reboot") != 1 || len(h.notified()) != 1 || !strings.Contains(h.notified()[0], "admin group") {
		t.Fatalf("reboot calls %d, notes %q", f.count("Reboot"), h.notified())
	}
}

// TestTrayExtensionsOpensTheHadronMenuEntry: "Extensions…" opens the same
// list as the Hadron menu, even while the service is unreachable.
func TestTrayExtensionsOpensTheHadronMenuEntry(t *testing.T) {
	h := startTray(t, func() (daemonClient, error) { return nil, errors.New("down") }, time.Hour, time.Hour)
	h.ui.waitView(t, "error", func(v trayView) bool { return v.Icon == "error" })
	h.app.clicks <- "extensions"
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		got := strings.Join(h.menus, ",")
		h.mu.Unlock()
		if got == "Extensions" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("menus opened %q", got)
		}
		time.Sleep(time.Millisecond)
	}
	if iconFor(StateExtension) != "busy" || !busyState(StateExtension) {
		t.Fatal("an extension job is not shown as busy")
	}
}

func TestTrayAndNotifyExitAtOnceOnAgent(t *testing.T) {
	setupAgent(t, "v0.1.0", "v0.2.0")
	dialDaemon = func() (daemonClient, error) {
		t.Error("the agent flavor dialled the update service")
		return nil, errors.New("no")
	}
	for _, args := range [][]string{{"tray"}, {"tray", "--no-icon"}, {"notify"}, {"notify", "--once"}} {
		if code, _, errOut := runCLI("", args...); code != exitOK {
			t.Errorf("%v: code %d, stderr %q", args, code, errOut)
		}
	}
}

func TestTerminalArgv(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "session")
	t.Setenv("HADRON_UPDATE_SESSION_FILE", session)
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	var log bytes.Buffer

	os.WriteFile(session, []byte("DESKTOP_NAME=i3\nTERMINAL_EXEC=\"st -c HadronFloat -e\"\n"), 0o644)
	got, err := terminalArgv("/usr/bin/hadron-update", []string{"switch", "niri"}, &log)
	if want := []string{"st", "-c", "HadronFloat", "-e", "/usr/bin/hadron-update", "switch", "niri"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("argv %q (%v), want %q", got, err, want)
	}

	os.WriteFile(session, []byte("DESKTOP_NAME=i3\n"), 0o644)
	if _, err := terminalArgv("hu", []string{"upgrade"}, &log); err == nil {
		t.Fatal("no terminal at all must be an error")
	}
	for _, name := range []string{"xterm", "foot"} {
		os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755)
		got, err = terminalArgv("hu", []string{"upgrade"}, &log)
		want := map[string][]string{
			"xterm": {filepath.Join(bin, "xterm"), "-e", "hu", "upgrade"},
			"foot":  {filepath.Join(bin, "foot"), "hu", "upgrade"},
		}[name]
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("with %s: argv %q (%v), want %q", name, got, err, want)
		}
	}
	if !strings.Contains(log.String(), "TERMINAL_EXEC") {
		t.Fatalf("the fallback is not logged: %q", log.String())
	}
}

func TestIcons(t *testing.T) {
	want := map[string][3]uint8{
		"idle": {0x7a, 0xa2, 0xf7}, "update": {0x9e, 0xce, 0x6a}, "busy": {0xe0, 0xaf, 0x68},
		"reboot": {0xbb, 0x9a, 0xf7}, "error": {0xf7, 0x76, 0x8e},
	}
	for kind, rgb := range want {
		for _, size := range []int{22, 44} {
			img, err := png.Decode(bytes.NewReader(iconPNG(kind, size)))
			if err != nil {
				t.Fatalf("%s/%d: %v", kind, size, err)
			}
			if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
				t.Fatalf("%s/%d: bounds %v", kind, size, b)
			}
			// Inside the disc, above every glyph.
			r, g, b, a := img.At(size/2, size/8+1).RGBA()
			if a>>8 < 0xf0 || uint8(r>>8) != rgb[0] || uint8(g>>8) != rgb[1] || uint8(b>>8) != rgb[2] {
				t.Errorf("%s/%d: disc colour %02x%02x%02x/%02x", kind, size, r>>8, g>>8, b>>8, a>>8)
			}
			if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
				t.Errorf("%s/%d: corner not transparent", kind, size)
			}
			// The middle of the three busy dots is glyph, in the dark
			// on_accent colour that stays readable on the light discs.
			if kind == "busy" {
				r, g, b, _ := img.At(size/2, size/2).RGBA()
				if uint8(r>>8) != 0x16 || uint8(g>>8) != 0x16 || uint8(b>>8) != 0x1e {
					t.Errorf("busy/%d: glyph colour %02x%02x%02x", size, r>>8, g>>8, b>>8)
				}
			}
		}
	}
}

func TestTrayRollbackClick(t *testing.T) {
	f := &trayFake{st: Status{State: StateUpToDate, Installed: "v0.1.0"}}
	h := startTray(t, dialFake(f), time.Hour, time.Hour)
	h.ui.waitView(t, "rollback", func(v trayView) bool { _, ok := find(v.Menu, "rollback"); return ok })
	h.app.clicks <- "rollback"
	deadline := time.Now().Add(5 * time.Second)
	for f.count("RebootPrevious") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.count("RebootPrevious") != 1 || f.count("Reboot") != 0 || len(h.launched()) != 0 {
		t.Fatalf("RebootPrevious %d, Reboot %d, launched %q", f.count("RebootPrevious"), f.count("Reboot"), h.launched())
	}

	f.mu.Lock()
	f.previousErr = &daemonError{Name: "RebootPending", Msg: "raw"}
	f.mu.Unlock()
	h.app.clicks <- "rollback"
	for len(h.notified()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := h.notified(); len(n) != 1 || !strings.Contains(n[0], "restart into it first") {
		t.Fatalf("notes %q", n)
	}
}

func TestTrayRollbackWithoutTheService(t *testing.T) {
	h := startTray(t, func() (daemonClient, error) { return nil, errors.New("no bus") }, time.Hour, time.Hour)
	h.ui.waitView(t, "unavailable", func(v trayView) bool { return v.Tooltip == unavailableText })
	h.app.clicks <- "rollback"
	deadline := time.Now().Add(5 * time.Second)
	for len(h.notified()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := h.notified(); len(n) != 1 || !strings.Contains(n[0], "not available") {
		t.Fatalf("notes %q", n)
	}
}
