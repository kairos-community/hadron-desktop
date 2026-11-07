package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// InspectInfo is what the daemon's Inspect answers: which image a reference
// really names, and whether the daemon will install it.
type InspectInfo struct {
	Ref, Digest, OS, Arch, Flavor, Version string
	// GPU is the image line, full or vm, or "" when the image does not say.
	GPU string
	// Trusted: a published release; Hadron: trusted, or labelled as a
	// Hadron Desktop image by its publisher.
	Trusted, Hadron bool
	// Refusal is why the daemon will not install the image; Overridable says
	// an explicit second answer from the user can lift it.
	Refusal     string
	Overridable bool
}

// Event is one StateChanged or Progress signal: Status is set for the first,
// Line for the second.
type Event struct {
	Status *Status
	Line   string
}

// daemonClient is the CLI's view of the root service. The real one speaks
// D-Bus; tests inject a fake through dialDaemon, so no bus is needed.
type daemonClient interface {
	Status() (Status, error)
	Check() error
	Releases() ([]string, error)
	Inspect(ref string, advanced bool) (InspectInfo, error)
	Apply(ref, digest string, advanced bool) error
	Cancel() error
	Reboot() error
	// RebootPrevious boots the previous system once.
	RebootPrevious() error
	// Watch subscribes to StateChanged and Progress. Subscribe before Apply,
	// so a fast job cannot finish unseen. stop ends the subscription and
	// closes the channel.
	Watch() (events <-chan Event, stop func(), err error)
	Close()
}

// dialDaemon connects to the root service. A variable so tests can inject a fake.
var dialDaemon = func() (daemonClient, error) { return dial() }

// daemonError is a D-Bus error from the service, reduced to the short name
// after "io.kairos.HadronUpdate1.Error." (Busy, Refused, ...).
type daemonError struct{ Name, Msg string }

func (e *daemonError) Error() string { return e.Msg }

func daemonErrName(err error) string {
	var de *daemonError
	if errors.As(err, &de) {
		return de.Name
	}
	return ""
}

const errPrefix = busInterface + ".Error."

func wrapBusErr(err error) error {
	var be dbus.Error
	if errors.As(err, &be) && strings.HasPrefix(be.Name, errPrefix) {
		msg := be.Name
		if len(be.Body) > 0 {
			if s, ok := be.Body[0].(string); ok {
				msg = s
			}
		}
		return &daemonError{Name: strings.TrimPrefix(be.Name, errPrefix), Msg: msg}
	}
	var pbe *dbus.Error
	if errors.As(err, &pbe) && pbe != nil && strings.HasPrefix(pbe.Name, errPrefix) {
		return wrapBusErr(*pbe)
	}
	return err
}

// client is the D-Bus implementation of daemonClient.
type client struct {
	conn *dbus.Conn
	obj  dbus.BusObject
}

// dial opens a private connection to the system bus. The service itself is
// started by D-Bus activation on the first call.
func dial() (*client, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	return &client{conn: conn, obj: conn.Object(busName, busPath)}, nil
}

func (c *client) Close() { c.conn.Close() }

// call retries once on Exiting: the service decided to quit, and the next
// call activates a fresh one.
func (c *client) call(method string, out []any, args ...any) error {
	return c.callFlags(method, 0, out, args...)
}

func (c *client) callFlags(method string, flags dbus.Flags, out []any, args ...any) error {
	var err error
	for i := 0; i < 2; i++ {
		err = wrapBusErr(c.obj.Call(busInterface+"."+method, flags, args...).Store(out...))
		if daemonErrName(err) != "Exiting" {
			return err
		}
	}
	return err
}

func (c *client) Status() (Status, error) { return c.status(0) }

// pollStatus is Status for the watcher's backstop poll. It never starts the
// service: a poll after a crash must not quietly bring it back. Watch polls
// only while a job runs, and the service does not count Status as activity,
// so a watcher never keeps an idle service from exiting.
func (c *client) pollStatus() (Status, error) { return c.status(dbus.FlagNoAutoStart) }

func (c *client) status(flags dbus.Flags) (Status, error) {
	var m map[string]dbus.Variant
	if err := c.callFlags("Status", flags, []any{&m}); err != nil {
		return Status{}, err
	}
	return statusFromDict(m), nil
}

func (c *client) Check() error { return c.call("Check", nil) }

func (c *client) Releases() ([]string, error) {
	var tags []string
	err := c.call("Releases", []any{&tags})
	return tags, err
}

func (c *client) Inspect(ref string, advanced bool) (InspectInfo, error) {
	var m map[string]dbus.Variant
	if err := c.call("Inspect", []any{&m}, ref, advanced); err != nil {
		return InspectInfo{}, err
	}
	return InspectInfo{
		Ref: str(m, "ref"), Digest: str(m, "digest"), OS: str(m, "os"), Arch: str(m, "arch"),
		Flavor: str(m, "flavor"), Version: str(m, "version"), GPU: str(m, "gpu"), Hadron: boolean(m, "hadron"),
		Trusted: boolean(m, "trusted"), Refusal: str(m, "refusal"), Overridable: boolean(m, "overridable"),
	}, nil
}

func (c *client) Apply(ref, digest string, advanced bool) error {
	return c.call("Apply", nil, ref, digest, advanced)
}
func (c *client) Cancel() error { return c.call("Cancel", nil) }
func (c *client) Reboot() error { return c.call("Reboot", nil) }
func (c *client) RebootPrevious() error {
	return c.call("RebootPrevious", nil)
}

// statusPoll is the backstop for a missed signal: while the last state seen
// is a job (or not known yet), Watch also asks Status this often. A variable
// so tests can shorten it.
var statusPoll = 5 * time.Second

// eventFromSignal decodes a StateChanged or Progress signal. ok is false for
// any other signal or a malformed body.
func eventFromSignal(s *dbus.Signal) (ev Event, ok bool) {
	if len(s.Body) != 1 {
		return ev, false
	}
	switch s.Name {
	case busInterface + ".StateChanged":
		m, isMap := s.Body[0].(map[string]dbus.Variant)
		if !isMap {
			return ev, false
		}
		st := statusFromDict(m)
		ev.Status = &st
		return ev, true
	case busInterface + ".Progress":
		line, isStr := s.Body[0].(string)
		return Event{Line: line}, isStr
	}
	return ev, false
}

// ownerLost reports whether s says the service's bus name has no owner any
// more: the daemon died, and no further signal will come.
func ownerLost(s *dbus.Signal) bool {
	if s.Name != "org.freedesktop.DBus.NameOwnerChanged" || len(s.Body) != 3 {
		return false
	}
	name, _ := s.Body[0].(string)
	newOwner, _ := s.Body[2].(string)
	return name == busName && newOwner == ""
}

// Watch also closes the channel when the service's name loses its owner, and
// polls Status as a backstop, so a caller never waits on a dead job.
func (c *client) Watch() (<-chan Event, func(), error) {
	out := make(chan Event, 64)
	sigs := make(chan *dbus.Signal, 64)
	opts := []dbus.MatchOption{dbus.WithMatchSender(busName), dbus.WithMatchObjectPath(busPath), dbus.WithMatchInterface(busInterface)}
	ownerOpts := []dbus.MatchOption{
		dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, busName),
	}
	if err := c.conn.AddMatchSignal(opts...); err != nil {
		return nil, nil, fmt.Errorf("subscribing to the update service: %w", err)
	}
	if err := c.conn.AddMatchSignal(ownerOpts...); err != nil {
		_ = c.conn.RemoveMatchSignal(opts...)
		return nil, nil, fmt.Errorf("watching the update service: %w", err)
	}
	c.conn.Signal(sigs)
	done := make(chan struct{})
	send := func(ev Event) bool {
		select {
		case out <- ev:
			return true
		case <-done:
			return false
		}
	}
	go func() {
		defer close(out)
		tick := time.NewTicker(statusPoll)
		defer tick.Stop()
		// last is the last state seen, "" until the first answer. The poll
		// runs only while it is a job: a missed signal then would leave the
		// caller waiting on a finished job. Outside a job the next change
		// comes with a signal anyway.
		var last State
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if last != "" && !busyState(last) {
					continue
				}
				st, err := c.pollStatus()
				if err != nil {
					if last == "" {
						// Not running: no job to miss a signal of.
						last = StateIdle
					}
					continue
				}
				last = st.State
				if !send(Event{Status: &st}) {
					return
				}
			case s, ok := <-sigs:
				if !ok || ownerLost(s) {
					return
				}
				ev, ok := eventFromSignal(s)
				if !ok {
					continue
				}
				if ev.Status != nil {
					last = ev.Status.State
				}
				if !send(ev) {
					return
				}
			}
		}
	}()
	var once sync.Once
	return out, func() {
		once.Do(func() {
			close(done)
			c.conn.RemoveSignal(sigs)
			_ = c.conn.RemoveMatchSignal(opts...)
			_ = c.conn.RemoveMatchSignal(ownerOpts...)
		})
	}, nil
}

func str(m map[string]dbus.Variant, k string) string {
	s, _ := m[k].Value().(string)
	return s
}

func boolean(m map[string]dbus.Variant, k string) bool {
	b, _ := m[k].Value().(bool)
	return b
}

func statusFromDict(m map[string]dbus.Variant) Status {
	return Status{
		State: State(str(m, "state")), Installed: str(m, "installed"), Latest: str(m, "latest"),
		Pin: str(m, "pin"), Detail: str(m, "detail"), Advanced: boolean(m, "advanced"),
		Pending: str(m, "pending"), ExtResult: str(m, "ext_result"), ExtDetail: str(m, "ext_detail"),
	}
}

// describeDaemonError turns a daemon error into the sentence the user reads.
func describeDaemonError(err error) string {
	switch daemonErrName(err) {
	case "NotAuthorized":
		return "Only members of the admin group can install updates or restart the system."
	case "RebootPending":
		// Installed, failed or interrupted: a restart comes first in all three.
		return "The last update waits for a restart; restart first."
	case "Busy":
		return "Another update is running."
	}
	return fmt.Sprint(err)
}

// describeRollbackError is describeDaemonError for RebootPrevious, whose
// refusals mean something else: a pending install makes "previous" ambiguous.
// pending is Status.Pending at the time of the refusal: after a failed or
// interrupted install, which slot holds what is unknown until a restart.
func describeRollbackError(err error, pending string) string {
	switch daemonErrName(err) {
	case "RebootPending":
		if pending == pendingFailed || pending == pendingStarted {
			return "The last update did not finish; restart before choosing a system."
		}
		return "An update is installed and waits for a reboot; restart into it first. The system you run now is then the previous one."
	case "Rebooting":
		return "The system is already restarting."
	}
	return describeDaemonError(err)
}
