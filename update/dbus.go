package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

// The bus API. The names are part of the interface: the tray, the CLI, the
// D-Bus policy, the activation file and the polkit files all spell them.
const (
	busName      = "io.kairos.HadronUpdate1"
	busPath      = dbus.ObjectPath("/io/kairos/HadronUpdate1")
	busInterface = "io.kairos.HadronUpdate1"

	actionApply  = "io.kairos.hadron-update.apply"
	actionReboot = "io.kairos.hadron-update.reboot"
	// actionExtensions covers installing, enabling, disabling and removing
	// system extensions: each one changes what runs as root.
	actionExtensions = "io.kairos.hadron-update.extensions"
	// actionRecovery covers the disk recovery key: reading which keyslots
	// exist, adding a new key and resealing to the TPM.
	actionRecovery = "io.kairos.hadron-update.recovery"
)

// busIntrospection describes the interface for busctl, d-feet and friends.
const busIntrospection = introspect.IntrospectDeclarationString + `<node>
  <interface name="io.kairos.HadronUpdate1">
    <method name="Status"><arg name="status" type="a{sv}" direction="out"/></method>
    <method name="Check"/>
    <method name="Releases"><arg name="tags" type="as" direction="out"/></method>
    <method name="Inspect">
      <arg name="ref" type="s" direction="in"/>
      <arg name="advanced" type="b" direction="in"/>
      <arg name="info" type="a{sv}" direction="out"/>
    </method>
    <method name="Apply">
      <arg name="ref" type="s" direction="in"/>
      <arg name="digest" type="s" direction="in"/>
      <arg name="advanced" type="b" direction="in"/>
    </method>
    <method name="Cancel"/>
    <method name="Reboot"/>
    <method name="RebootPrevious"/>
    <method name="Extensions"><arg name="extensions" type="aa{sv}" direction="out"/></method>
    <method name="Catalog"><arg name="entries" type="aa{sv}" direction="out"/></method>
    <method name="ExtensionFiles">
      <arg name="name" type="s" direction="in"/>
      <arg name="files" type="as" direction="out"/>
    </method>
    <method name="InstallExtension">
      <arg name="ref" type="s" direction="in"/>
      <arg name="version" type="s" direction="in"/>
      <arg name="force" type="b" direction="in"/>
    </method>
    <method name="SetExtensionEnabled">
      <arg name="name" type="s" direction="in"/>
      <arg name="on" type="b" direction="in"/>
      <arg name="force" type="b" direction="in"/>
    </method>
    <method name="RemoveExtension"><arg name="name" type="s" direction="in"/></method>
    <method name="RecoveryStatus"><arg name="status" type="a{sv}" direction="out"/></method>
    <method name="RecoveryReseal"><arg name="secret" type="h" direction="in"/></method>
    <method name="RecoveryNewKey"><arg name="secrets" type="h" direction="in"/></method>
    <signal name="StateChanged"><arg name="status" type="a{sv}"/></signal>
    <signal name="Progress"><arg name="line" type="s"/></signal>
  </interface>` + introspect.IntrospectDataString + `</node>`

// idleTimeout is how long the service stays up with no method call other
// than Status, no state change and no job. D-Bus activation starts it again on the next
// call, so a desktop that never updates does not keep a root process around.
// Variables so tests can shorten them.
var (
	idleTimeout = 10 * time.Minute
	idleTick    = 30 * time.Second
)

// Authorizer decides whether the bus client sender may do action. Any error
// is a refusal: the caller never treats an error as a yes.
type Authorizer interface {
	Allowed(sender dbus.Sender, action string) (bool, error)
}

// polkitSubject is polkit's Subject, (sa{sv}).
type polkitSubject struct {
	Kind    string
	Details map[string]dbus.Variant
}

// polkitResult is polkit's AuthorizationResult, (bba{ss}).
type polkitResult struct {
	IsAuthorized bool
	IsChallenge  bool
	Details      map[string]string
}

// polkitAuthorizer asks polkit over the system bus.
type polkitAuthorizer struct{ conn *dbus.Conn }

// polkitTimeout bounds a CheckAuthorization call. Without interaction polkit
// answers at once; a hung polkit must not hold a method call forever.
const polkitTimeout = 25 * time.Second

// Allowed checks action for the bus name sender. polkit looks the name up
// itself, so the uid and process are the ones the bus vouches for, not ones
// the client claims. Flags are 0: no authentication dialog, since the rules
// file answers yes or no for every action and no agent runs. A challenge
// ("would be allowed after authenticating") is therefore a no.
func (p polkitAuthorizer) Allowed(sender dbus.Sender, action string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), polkitTimeout)
	defer cancel()
	subject := polkitSubject{
		Kind:    "system-bus-name",
		Details: map[string]dbus.Variant{"name": dbus.MakeVariant(string(sender))},
	}
	var res polkitResult
	err := p.conn.Object("org.freedesktop.PolicyKit1", "/org/freedesktop/PolicyKit1/Authority").
		CallWithContext(ctx, "org.freedesktop.PolicyKit1.Authority.CheckAuthorization", 0,
			subject, action, map[string]string{}, uint32(0), "").
		Store(&res)
	if err != nil {
		return false, fmt.Errorf("polkit: %w", err)
	}
	return res.IsAuthorized && !res.IsChallenge, nil
}

// logindInhibitor takes a logind "block" inhibitor lock on shutdown and
// sleep. logind keeps it until the returned fd is closed, so a reboot asked
// from the session waits for kairos-agent instead of cutting the write.
type logindInhibitor struct{ conn *dbus.Conn }

func (l logindInhibitor) Inhibit() (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), polkitTimeout)
	defer cancel()
	var fd dbus.UnixFD
	err := l.conn.Object("org.freedesktop.login1", "/org/freedesktop/login1").
		CallWithContext(ctx, "org.freedesktop.login1.Manager.Inhibit", 0,
			"shutdown:sleep", "Hadron Desktop update", "An update is being written to disk", "block").
		Store(&fd)
	if err != nil {
		return nil, fmt.Errorf("logind: %w", err)
	}
	f := os.NewFile(uintptr(fd), "logind-inhibitor")
	if f == nil {
		return nil, fmt.Errorf("logind returned an invalid descriptor %d", fd)
	}
	return func() { f.Close() }, nil
}

// busError maps a daemon error to a D-Bus error whose name a client can
// switch on; the message is the error's own sentence.
func busError(err error) *dbus.Error {
	if err == nil {
		return nil
	}
	var name string
	switch {
	case errors.Is(err, ErrBusy):
		name = "Busy"
	case errors.Is(err, ErrRebootPending):
		name = "RebootPending"
	case errors.Is(err, ErrNotCancellable):
		name = "NotCancellable"
	case errors.Is(err, ErrRebooting):
		name = "Rebooting"
	case errors.Is(err, ErrAgentImage), errors.Is(err, ErrNotHadron), errors.Is(err, ErrNotRelease),
		errors.Is(err, ErrNeedsCredentials), errors.Is(err, ErrWrongPlatform),
		errors.Is(err, errUnmanaged), errors.Is(err, errExtNeedsAdvanced):
		name = "Refused"
	default:
		name = "Failed"
	}
	return dbus.NewError(busInterface+".Error."+name, []any{err.Error()})
}

var errNotAuthorized = dbus.NewError(busInterface+".Error.NotAuthorized",
	[]any{"not authorized: only members of the admin group may install updates or extensions, or reboot"})

var errSandboxed = dbus.NewError(busInterface+".Error.NotAuthorized",
	[]any{"not authorized: applications in a Flatpak sandbox may not install updates or extensions, or reboot"})

// procRoot is where the caller's process is looked up, and selfFdinfo
// where this process's own fds are described. Variables so tests can point
// them at a fake /proc.
var (
	procRoot   = "/proc"
	selfFdinfo = "/proc/self/fdinfo"
)

// callerCreds is what the bus says about the process behind a bus name.
// pidfd is -1 when the bus sent none (dbus-daemon before 1.15.x); close
// releases it.
type callerCreds struct {
	pid   uint32
	pidfd int
	close func()
}

// busCreds asks the bus for sender's credentials. The bus vouches for them;
// the caller cannot claim another process. ProcessFD, a pidfd, pins the
// process itself, so a reused pid cannot stand in for it. Without a pidfd
// the check is weaker: the start time catches a pid reused during the
// check, but a pid reused before the first start-time read cannot be
// detected.
func busCreds(conn *dbus.Conn) func(dbus.Sender) (callerCreds, error) {
	return func(sender dbus.Sender) (callerCreds, error) {
		ctx, cancel := context.WithTimeout(context.Background(), polkitTimeout)
		defer cancel()
		c := callerCreds{pidfd: -1}
		var m map[string]dbus.Variant
		err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetConnectionCredentials", 0, string(sender)).Store(&m)
		if err == nil {
			if fd, ok := m["ProcessFD"].Value().(dbus.UnixFD); ok {
				c.pidfd = int(fd)
				c.close = func() { syscall.Close(int(fd)) }
			}
			c.pid, _ = m["ProcessID"].Value().(uint32)
		}
		if c.pidfd < 0 && c.pid == 0 {
			err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetConnectionUnixProcessID", 0, string(sender)).Store(&c.pid)
		}
		return c, err
	}
}

// readPidfdPid and readStartTime are the two reads that tie a check to one
// process, and statFlatpakInfo is the check. Variables so tests can make the
// process change between them.
var (
	readPidfdPid    = pidfdPid
	readStartTime   = procStartTime
	statFlatpakInfo = os.Stat
)

// pidfdPid is the pid a pidfd refers to, from the Pid: line of its fdinfo:
// -1 once that process has exited (and been reaped).
func pidfdPid(fd int) (int, error) {
	b, err := os.ReadFile(filepath.Join(selfFdinfo, strconv.Itoa(fd)))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "Pid:"); ok {
			return strconv.Atoi(strings.TrimSpace(v))
		}
	}
	return 0, errors.New("the pidfd's fdinfo has no Pid line")
}

// procStartTime is field 22 of /proc/<pid>/stat, the start time in clock
// ticks since boot: a pid reused by another process has another one. The
// command name (field 2) may hold spaces and parentheses, so the fields are
// counted after its last ')'.
func procStartTime(pid uint32) (string, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.FormatUint(uint64(pid), 10), "stat"))
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return "", errors.New("malformed stat")
	}
	fields := strings.Fields(string(b)[end+1:]) // starts at field 3
	if len(fields) < 20 {
		return "", errors.New("malformed stat")
	}
	return fields[19], nil
}

// flatpakInfo reports whether pid's root holds .flatpak-info, the way
// xdg-desktop-portal tells a sandbox. For a zombie the root does not
// resolve, and every path under it reads as missing, which would make a
// sandboxed zombie look like a host process. So the root must resolve before
// the Stat and again after it. Exit clears the root (exit_fs) and is
// irreversible, so a root that still resolves after the Stat proves the Stat
// saw the live root, even if the process entered exit just before. The start
// time and the pidfd's Pid do not show that: both stay until it is reaped.
// (os.OpenRoot would pin the root as well; the second Readlink needs no
// directory fd and checks the same thing.)
func flatpakInfo(pid uint32) (bool, error) {
	root := filepath.Join(procRoot, strconv.FormatUint(uint64(pid), 10), "root")
	if _, err := os.Readlink(root); err != nil {
		return true, fmt.Errorf("the root of pid %d does not resolve: %w", pid, err)
	}
	_, err := statFlatpakInfo(filepath.Join(root, ".flatpak-info"))
	if _, lerr := os.Readlink(root); lerr != nil {
		return true, fmt.Errorf("pid %d exited during the check: %w", pid, lerr)
	}
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return true, err
}

// sandboxed reports whether the caller runs in a Flatpak sandbox. It fails
// closed: any error, or any sign that the process changed during the check,
// counts as sandboxed. With a pidfd the pid is read from it before and after
// the check; without one the process start time is.
func sandboxed(c callerCreds) (bool, error) {
	if c.pidfd >= 0 {
		pid, err := readPidfdPid(c.pidfd)
		if err != nil {
			return true, err
		}
		if pid <= 0 {
			return true, errors.New("the calling process has exited")
		}
		sb, err := flatpakInfo(uint32(pid))
		if err != nil {
			return true, err
		}
		after, err := readPidfdPid(c.pidfd)
		if err != nil {
			return true, err
		}
		if after != pid {
			return true, errors.New("the calling process exited during the check")
		}
		return sb, nil
	}
	if c.pid == 0 {
		return true, errors.New("the bus named no process")
	}
	before, err := readStartTime(c.pid)
	if err != nil {
		return true, err
	}
	sb, err := flatpakInfo(c.pid)
	if err != nil {
		return true, err
	}
	after, err := readStartTime(c.pid)
	if err != nil {
		return true, err
	}
	if after != before {
		return true, fmt.Errorf("pid %d changed hands during the check", c.pid)
	}
	return sb, nil
}

func statusDict(s Status) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"state":      dbus.MakeVariant(string(s.State)),
		"installed":  dbus.MakeVariant(s.Installed),
		"latest":     dbus.MakeVariant(s.Latest),
		"pin":        dbus.MakeVariant(s.Pin),
		"detail":     dbus.MakeVariant(s.Detail),
		"advanced":   dbus.MakeVariant(s.Advanced),
		"pending":    dbus.MakeVariant(s.Pending),
		"ext_result": dbus.MakeVariant(s.ExtResult),
		"ext_detail": dbus.MakeVariant(s.ExtDetail),
	}
}

// service is the state serve keeps besides the daemon: who is talking to it
// and when anything last happened, for the idle exit.
type service struct {
	d    *Daemon
	auth Authorizer
	// credsOf names the process behind a bus name; nil refuses every
	// mutating call.
	credsOf func(dbus.Sender) (callerCreds, error)

	// netSlot admits one network-bound call (Check, Releases, Inspect) at
	// a time. They are open to every local user, so without it any caller
	// could make root run fetches without limit and spend the machine's
	// GitHub quota.
	netSlot chan struct{}

	mu      sync.Mutex
	active  int       // method calls in progress
	last    time.Time // end of the last call or the last daemon event
	closing bool      // decided to exit; no call may start anything now
}

// errExiting is its own name so a client can retry exactly once on it: the
// next call activates a new instance.
var errExiting = dbus.NewError(busInterface+".Error.Exiting",
	[]any{"the update service is exiting; call again to start a new one"})

var errNetBusy = dbus.NewError(busInterface+".Error.Busy",
	[]any{"another check or inspection is running; try again when it ends"})

// maxRefLen bounds a reference before any work; real ones are far shorter.
const maxRefLen = 512

func checkRef(ref string) *dbus.Error {
	if len(ref) > maxRefLen {
		return busError(fmt.Errorf("the image reference is longer than %d bytes", maxRefLen))
	}
	return nil
}

// network takes the network slot, or refuses at once: a caller waiting in
// line would still hold a goroutine and a bus call open.
func (s *service) network() (func(), *dbus.Error) {
	select {
	case s.netSlot <- struct{}{}:
		return func() { <-s.netSlot }, nil
	default:
		return nil, errNetBusy
	}
}

func (s *service) touch() {
	s.mu.Lock()
	s.last = time.Now()
	s.mu.Unlock()
}

// enter marks a method call in progress; the returned func ends it, and
// counts as activity for the idle exit. Once the service has decided to exit
// it refuses, so no job starts in a process that is about to go away.
func (s *service) enter() (func(), *dbus.Error) { return s.enterCall(true) }

// enterQuiet is enter for Status: every tray reads it, so counting it as
// activity would keep the service up for as long as any tray runs.
func (s *service) enterQuiet() (func(), *dbus.Error) { return s.enterCall(false) }

func (s *service) enterCall(activity bool) (func(), *dbus.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, errExiting
	}
	s.active++
	return func() {
		s.mu.Lock()
		s.active--
		if activity {
			s.last = time.Now()
		}
		s.mu.Unlock()
	}, nil
}

// tryClose decides to exit when nothing happened for quiet, no call runs and
// no job runs. The decision and the checks happen under one lock, so a call
// either counted as active or is refused by enter.
func (s *service) tryClose(quiet time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active > 0 || time.Since(s.last) < quiet || s.d.Busy() {
		return false
	}
	s.closing = true
	return true
}

func newService(d *Daemon, a Authorizer) *service {
	return &service{d: d, auth: a, last: time.Now(), netSlot: make(chan struct{}, 1)}
}

// authorize asks before any argument is looked at, so an unauthorized
// client learns nothing about the system from a mutating call. A caller in
// a Flatpak sandbox is refused before polkit is asked: the app runs as the
// user, so the admin-group rule would let any app with system-bus access
// install an image or reboot.
func (s *service) authorize(sender dbus.Sender, action string) *dbus.Error {
	if s.credsOf == nil {
		fmt.Fprintf(os.Stderr, "hadron-update: refusing %s for %s: no way to look up the caller\n", action, sender)
		return errNotAuthorized
	}
	creds, err := s.credsOf(sender)
	if creds.close != nil {
		defer creds.close()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "hadron-update: refusing %s for %s: %v\n", action, sender, err)
		return errNotAuthorized
	}
	if sb, err := sandboxed(creds); sb {
		if err != nil {
			fmt.Fprintf(os.Stderr, "hadron-update: refusing %s for %s: %v\n", action, sender, err)
			return errNotAuthorized
		}
		fmt.Fprintf(os.Stderr, "hadron-update: refusing %s for %s: it runs in a Flatpak sandbox\n", action, sender)
		return errSandboxed
	}
	ok, err := s.auth.Allowed(sender, action)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hadron-update: refusing %s for %s: %v\n", action, sender, err)
		return errNotAuthorized
	}
	if !ok {
		return errNotAuthorized
	}
	return nil
}

// busAPI is the exported object. Every exported method is a D-Bus method,
// so it has no others.
type busAPI struct{ s *service }

func (a busAPI) Status() (map[string]dbus.Variant, *dbus.Error) {
	done, derr := a.s.enterQuiet()
	if derr != nil {
		return nil, derr
	}
	defer done()
	return statusDict(a.s.d.Status()), nil
}

// Check is unauthenticated: it only refreshes the state, and the tray calls
// it on its own schedule.
func (a busAPI) Check() *dbus.Error {
	done, derr := a.s.enter()
	if derr != nil {
		return derr
	}
	defer done()
	free, derr := a.s.network()
	if derr != nil {
		return derr
	}
	defer free()
	return busError(a.s.d.Check(context.Background()))
}

func (a busAPI) Releases() ([]string, *dbus.Error) {
	done, derr := a.s.enter()
	if derr != nil {
		return nil, derr
	}
	defer done()
	free, derr := a.s.network()
	if derr != nil {
		return nil, derr
	}
	defer free()
	rels, err := a.s.d.Releases(context.Background())
	if err != nil {
		return nil, busError(err)
	}
	tags := make([]string, 0, len(rels))
	for _, r := range rels {
		tags = append(tags, r.Tag)
	}
	return tags, nil
}

// Inspect reports a policy refusal in the reply, not as an error, so the
// client can show what the image is and why it would be refused before it
// asks to Apply. An image that is not a published release (unlabelled, or
// labelled but from another repository) is refused unless advanced is set. "overridable" says whether asking again with advanced=true
// would make it installable. Everything else (a bad reference, the network,
// a private image, a wrong platform) is an error.
func (a busAPI) Inspect(ref string, advanced bool) (map[string]dbus.Variant, *dbus.Error) {
	done, derr := a.s.enter()
	if derr != nil {
		return nil, derr
	}
	defer done()
	if derr := checkRef(ref); derr != nil {
		return nil, derr
	}
	free, derr := a.s.network()
	if derr != nil {
		return nil, derr
	}
	defer free()
	info, v, err := a.s.d.Inspect(context.Background(), ref, advanced)
	refusal, overridable := "", false
	switch {
	case errors.Is(err, ErrNotHadron):
		refusal, overridable = ErrNotHadron.Error(), AdvancedEnabled()
	case errors.Is(err, ErrNotRelease):
		refusal, overridable = ErrNotRelease.Error(), AdvancedEnabled()
	case errors.Is(err, ErrAgentImage):
		refusal = ErrAgentImage.Error()
	case err != nil:
		return nil, busError(err)
	}
	return map[string]dbus.Variant{
		"ref":         dbus.MakeVariant(info.Ref.String()),
		"digest":      dbus.MakeVariant(info.Digest),
		"os":          dbus.MakeVariant(info.OS),
		"arch":        dbus.MakeVariant(info.Arch),
		"flavor":      dbus.MakeVariant(v.Flavor),
		"version":     dbus.MakeVariant(v.Version),
		"gpu":         dbus.MakeVariant(v.GPU),
		"trusted":     dbus.MakeVariant(v.Trusted),
		"hadron":      dbus.MakeVariant(v.Hadron),
		"refusal":     dbus.MakeVariant(refusal),
		"overridable": dbus.MakeVariant(overridable),
	}, nil
}

func (a busAPI) Apply(sender dbus.Sender, ref, digest string, advanced bool) *dbus.Error {
	done, derr := a.s.enter()
	if derr != nil {
		return derr
	}
	defer done()
	if err := a.s.authorize(sender, actionApply); err != nil {
		return err
	}
	if derr := checkRef(ref); derr != nil {
		return derr
	}
	return busError(a.s.d.Apply(context.Background(), ref, digest, advanced))
}

// Cancel shares Apply's action: whoever may start an install may stop one.
func (a busAPI) Cancel(sender dbus.Sender) *dbus.Error {
	done, derr := a.s.enter()
	if derr != nil {
		return derr
	}
	defer done()
	if err := a.s.authorize(sender, actionApply); err != nil {
		return err
	}
	return busError(a.s.d.Cancel())
}

func (a busAPI) Reboot(sender dbus.Sender) *dbus.Error {
	done, derr := a.s.enter()
	if derr != nil {
		return derr
	}
	defer done()
	if err := a.s.authorize(sender, actionReboot); err != nil {
		return err
	}
	return busError(a.s.d.Reboot())
}

// RebootPrevious shares Reboot's action: it is a restart, into the other
// installed system, and installs nothing.
func (a busAPI) RebootPrevious(sender dbus.Sender) *dbus.Error {
	done, derr := a.s.enter()
	if derr != nil {
		return derr
	}
	defer done()
	if err := a.s.authorize(sender, actionReboot); err != nil {
		return err
	}
	return busError(a.s.d.RebootPrevious())
}

// serve exports d on conn as io.kairos.HadronUpdate1 and returns when the
// service has been idle for idleTimeout, or with an error when the name is
// taken or the bus goes away. It never returns while a job runs.
func serve(conn *dbus.Conn, d *Daemon, a Authorizer) error {
	s := newService(d, a)
	s.credsOf = busCreds(conn)
	d.OnChange(func(st Status) {
		s.touch()
		if err := conn.Emit(busPath, busInterface+".StateChanged", statusDict(st)); err != nil {
			fmt.Fprintln(os.Stderr, "hadron-update: emitting StateChanged:", err)
		}
	})
	d.OnProgress(func(line string) {
		s.touch()
		if err := conn.Emit(busPath, busInterface+".Progress", line); err != nil {
			fmt.Fprintln(os.Stderr, "hadron-update: emitting Progress:", err)
		}
	})

	// Export before taking the name: the bus delivers an activating call as
	// soon as the name is owned.
	if err := conn.Export(busAPI{s}, busPath, busInterface); err != nil {
		return err
	}
	if err := conn.Export(introspect.Introspectable(busIntrospection), busPath,
		"org.freedesktop.DBus.Introspectable"); err != nil {
		return err
	}
	reply, err := conn.RequestName(busName, dbus.NameFlagDoNotQueue)
	if err != nil {
		return fmt.Errorf("requesting %s: %w", busName, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("%s is already owned by another process", busName)
	}

	tick := time.NewTicker(idleTick)
	defer tick.Stop()
	for {
		select {
		case <-conn.Context().Done():
			// Returning ends the process, and systemd then kills what is
			// left in the unit's cgroup: kairos-agent too. Let the job end.
			for d.Busy() {
				time.Sleep(idleTick)
			}
			return errors.New("lost the connection to the bus")
		case <-tick.C:
		}
		if s.tryClose(idleTimeout) {
			// Release the name before exiting, so the next call activates a
			// new instance. Calls that still reach this one are refused.
			if _, err := conn.ReleaseName(busName); err != nil {
				return fmt.Errorf("releasing %s: %w", busName, err)
			}
			return nil
		}
	}
}

// cmdDaemon is hadron-updated: the root service that owns the update job.
func cmdDaemon(args []string, stderr io.Writer) int {
	fs := newFlags("daemon", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "hadron-update daemon: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	// It runs kairos-agent and reboots; as any other user it could do
	// neither, and would only mislead clients by owning the name.
	if needRoot() {
		fmt.Fprintln(stderr, "hadron-update daemon: must run as root; D-Bus activation starts it through hadron-updated.service.")
		return exitError
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		fmt.Fprintln(stderr, "hadron-update daemon: cannot connect to the system bus:", err)
		return exitError
	}
	defer conn.Close()
	d := NewDaemon(kairosBackend{})
	d.inhibitor = logindInhibitor{conn: conn}
	if err := serve(conn, d, polkitAuthorizer{conn: conn}); err != nil {
		fmt.Fprintln(stderr, "hadron-update daemon:", err)
		return exitError
	}
	return exitOK
}
