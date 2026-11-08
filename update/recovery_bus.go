package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// The recovery methods take their secrets as a file descriptor (D-Bus type
// h), never as a string argument. The CLI writes them to a pipe and passes
// the read end, so the message on the bus carries only an fd number:
// dbus-monitor, the bus's own logs and the D-Bus policy never see a key or a
// passphrase. Asking with systemd-ask-password on the root side was the
// other choice, but a root service with no terminal has no agent to answer
// in a desktop session.

// recoveryOps is the root side; a variable so the tests can fake it.
var recoveryOps interface {
	Status() (RecoveryStatus, error)
	Reseal(secret string) error
	NewKey(secret, newKey string) error
} = recovery{run: execCmd}

// recoveryMu admits one recovery operation at a time: two could race on
// the same LUKS2 header.
var recoveryMu sync.Mutex

// secretReadTimeout bounds the wait for the client to close its pipe.
var secretReadTimeout = 10 * time.Second

const maxSecretBytes = 4096

var errSecretTooLong = errors.New("the secret is longer than 4096 bytes")

// readSecrets reads newline-separated secrets from fd and closes it.
func readSecrets(fd dbus.UnixFD) ([]string, error) {
	f := os.NewFile(uintptr(fd), "secret")
	if f == nil {
		return nil, errors.New("no secret file descriptor")
	}
	defer f.Close()
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
		done <- result{b, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(secretReadTimeout):
		return nil, errors.New("timed out reading the secret")
	}
	if r.err != nil {
		return nil, r.err
	}
	if len(r.b) > maxSecretBytes {
		return nil, errSecretTooLong
	}
	s := strings.TrimSuffix(string(r.b), "\n")
	clear(r.b)
	return strings.Split(s, "\n"), nil
}

func recoveryDict(st RecoveryStatus) map[string]dbus.Variant {
	cs := make([]map[string]dbus.Variant, 0, len(st.Containers))
	for _, c := range st.Containers {
		cs = append(cs, map[string]dbus.Variant{
			"label":      dbus.MakeVariant(c.Label),
			"device":     dbus.MakeVariant(c.Device),
			"tpm":        dbus.MakeVariant(int32(c.TPM)),
			"recovery":   dbus.MakeVariant(int32(c.Recovery)),
			"passphrase": dbus.MakeVariant(int32(c.Passphrase)),
			"tpm_works":  dbus.MakeVariant(c.TPMWorks),
		})
	}
	return map[string]dbus.Variant{
		"edition":    dbus.MakeVariant(st.Edition),
		"containers": dbus.MakeVariant(cs),
	}
}

func (a busAPI) RecoveryStatus(sender dbus.Sender) (map[string]dbus.Variant, *dbus.Error) {
	done, derr := a.s.enter()
	if derr != nil {
		return nil, derr
	}
	defer done()
	if err := a.s.authorize(sender, actionRecovery); err != nil {
		return nil, err
	}
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	st, err := recoveryOps.Status()
	if err != nil {
		return nil, busError(err)
	}
	return recoveryDict(st), nil
}

// RecoveryReseal reads one line from fd: the recovery key, or nothing to
// let a still-working TPM authorize the reseal.
func (a busAPI) RecoveryReseal(sender dbus.Sender, fd dbus.UnixFD) *dbus.Error {
	done, derr := a.s.enter()
	if derr != nil {
		closeFD(fd)
		return derr
	}
	defer done()
	if err := a.s.authorize(sender, actionRecovery); err != nil {
		closeFD(fd)
		return err
	}
	secrets, err := readSecrets(fd)
	if err != nil {
		return busError(err)
	}
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	return busError(recoveryOps.Reseal(secrets[0]))
}

// RecoveryNewKey reads two lines from fd: what unlocks the disk now (empty
// for the TPM) and the new recovery key, which the CLI generated and showed.
func (a busAPI) RecoveryNewKey(sender dbus.Sender, fd dbus.UnixFD) *dbus.Error {
	done, derr := a.s.enter()
	if derr != nil {
		closeFD(fd)
		return derr
	}
	defer done()
	if err := a.s.authorize(sender, actionRecovery); err != nil {
		closeFD(fd)
		return err
	}
	secrets, err := readSecrets(fd)
	if err != nil {
		return busError(err)
	}
	if len(secrets) != 2 {
		return busError(errors.New("want two lines: the current secret and the new recovery key"))
	}
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	return busError(recoveryOps.NewKey(secrets[0], secrets[1]))
}

func closeFD(fd dbus.UnixFD) {
	if f := os.NewFile(uintptr(fd), "secret"); f != nil {
		f.Close()
	}
}
