package main

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

const recoveryUsage = `Usage: hadron-recovery [command]

Manage the recovery key of the encrypted disk. Runs as root through the
update service; only members of the admin group may use it.

Commands:
  status    show the edition, the keyslots of each encrypted container and
            whether the TPM unlocks it now. Shows no secret.
  new-key   create a new recovery key, show it once, and replace the old one
            (the old key stops working only after the new one is checked)
  reseal    Trusted Boot: seal the disk to this computer's TPM again, after a
            boot that needed the recovery key, so the next boots need none

With no command it shows the status and asks what to do.
`

// recoveryClient is the CLI's view of the recovery half of the service.
type recoveryClient interface {
	RecoveryStatus() (RecoveryStatus, error)
	RecoveryReseal(secret string) error
	RecoveryNewKey(secret, newKey string) error
	Close()
}

var dialRecovery = func() (recoveryClient, error) { return dial() }

// withSecrets passes secrets to the service through a pipe: the D-Bus
// message carries the read end as an fd, never the text.
func withSecrets(secrets []string, call func(dbus.UnixFD) error) error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	payload := []byte(strings.Join(secrets, "\n") + "\n")
	if len(payload) > 4096 {
		w.Close()
		return errors.New("the secret is too long")
	}
	// A pipe buffer holds 4096 bytes, so this write never blocks.
	_, werr := w.Write(payload)
	clear(payload)
	w.Close()
	if werr != nil {
		return werr
	}
	return call(dbus.UnixFD(r.Fd()))
}

func (c *client) RecoveryStatus() (RecoveryStatus, error) {
	var m map[string]dbus.Variant
	if err := c.call("RecoveryStatus", []any{&m}); err != nil {
		return RecoveryStatus{}, err
	}
	st := RecoveryStatus{Edition: str(m, "edition")}
	cs, _ := m["containers"].Value().([]map[string]dbus.Variant)
	for _, x := range cs {
		n := func(k string) int { v, _ := x[k].Value().(int32); return int(v) }
		st.Containers = append(st.Containers, ContainerStatus{
			Label: str(x, "label"), Device: str(x, "device"),
			TPM: n("tpm"), Recovery: n("recovery"), Passphrase: n("passphrase"),
			TPMWorks: boolean(x, "tpm_works"),
		})
	}
	return st, nil
}

func (c *client) RecoveryReseal(secret string) error {
	return withSecrets([]string{secret}, func(fd dbus.UnixFD) error {
		return c.call("RecoveryReseal", nil, fd)
	})
}

func (c *client) RecoveryNewKey(secret, newKey string) error {
	return withSecrets([]string{secret, newKey}, func(fd dbus.UnixFD) error {
		return c.call("RecoveryNewKey", nil, fd)
	})
}

// terminal is where the CLI asks: lines with echo, and secrets without.
type terminal interface {
	Line(prompt string) (string, error)
	Secret(prompt string) (string, error)
}

// newTerminal opens the controlling terminal. A variable for the tests.
var newTerminal = func(out io.Writer) (terminal, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("hadron-recovery needs a terminal: run it in one")
	}
	return &ttyTerminal{f: f, r: bufio.NewReader(f), out: out}, nil
}

type ttyTerminal struct {
	f   *os.File
	r   *bufio.Reader
	out io.Writer
}

func (t *ttyTerminal) Line(prompt string) (string, error) {
	fmt.Fprint(t.out, prompt)
	s, err := t.r.ReadString('\n')
	if err != nil && s == "" {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// Secret reads a line with the terminal's echo off.
func (t *ttyTerminal) Secret(prompt string) (string, error) {
	fd := int(t.f.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", err
	}
	noEcho := *old
	noEcho.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &noEcho); err != nil {
		return "", err
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, old)
	s, err := t.Line(prompt)
	fmt.Fprintln(t.out)
	return s, err
}

// recoveryKeyLines is the key screen of the installer
// (install-ui/recoverykey.go): the key in the terminal's own font, two
// groups per line with their numbers, then the key on one line.
func recoveryKeyLines(key string) []string {
	g := strings.Split(key, "-")
	var out []string
	for i := 0; i+1 < len(g); i += 2 {
		out = append(out, fmt.Sprintf("  %d  %s    %s  %d", i+1, spaced(g[i]), spaced(g[i+1]), i+2))
	}
	return append(out, "", "  "+key)
}

// spaced puts a space between letters: easier to read and copy by hand.
func spaced(s string) string { return strings.Join(strings.Split(s, ""), " ") }

func confirmsGroup(key string, group int, typed string) bool {
	t := strings.NewReplacer("-", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(typed)))
	if t == "" {
		return false
	}
	if t == strings.ReplaceAll(key, "-", "") {
		return true
	}
	g := strings.Split(key, "-")
	return group >= 0 && group < len(g) && t == g[group]
}

// pickGroup chooses the group to type back. A variable for the tests.
var pickGroup = func() int {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return int(b[0]) % 8
}

// makeRecoveryKey generates the new key. A variable for the tests.
var makeRecoveryKey = newRecoveryKey

func cmdRecovery(args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, recoveryUsage)
		return exitOK
	case "", "status", "new-key", "reseal":
	default:
		fmt.Fprintf(stderr, "hadron-recovery: unknown command %q\n\n%s", sub, recoveryUsage)
		return exitUsage
	}
	c, err := dialRecovery()
	if err != nil {
		fmt.Fprintf(stderr, "hadron-recovery: cannot reach the update service: %v\n", err)
		return exitError
	}
	defer c.Close()
	st, err := c.RecoveryStatus()
	if err != nil {
		fmt.Fprintf(stderr, "hadron-recovery: %v\n", err)
		return exitError
	}
	if sub == "status" {
		printRecoveryStatus(stdout, st)
		return exitOK
	}
	t, err := newTerminal(stdout)
	if err != nil {
		fmt.Fprintf(stderr, "hadron-recovery: %v\n", err)
		return exitError
	}
	if sub == "" {
		printRecoveryStatus(stdout, st)
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "  1  create a new recovery key")
		if st.Edition == editionTrustedBoot {
			fmt.Fprintln(stdout, "  2  reseal the disk to this computer's TPM")
		}
		ans, err := t.Line("\nChoose, or press Enter to quit: ")
		if err != nil {
			return exitError
		}
		switch strings.TrimSpace(ans) {
		case "1":
			sub = "new-key"
		case "2":
			if st.Edition == editionTrustedBoot {
				sub = "reseal"
			}
		}
		if sub == "" {
			return exitOK
		}
	}
	var code int
	if sub == "reseal" {
		code = recoveryReseal(c, t, st, stdout, stderr)
	} else {
		code = recoveryNewKey(c, t, st, stdout, stderr)
	}
	if code == exitOK {
		if after, err := c.RecoveryStatus(); err == nil {
			fmt.Fprintln(stdout)
			printRecoveryStatus(stdout, after)
		}
	}
	return code
}

func printRecoveryStatus(w io.Writer, st RecoveryStatus) {
	name := "normal (disk passphrase)"
	if st.Edition == editionTrustedBoot {
		name = "Trusted Boot (TPM)"
	}
	fmt.Fprintf(w, "Edition: %s\n", name)
	for _, c := range st.Containers {
		var kinds []string
		if c.TPM > 0 {
			kinds = append(kinds, fmt.Sprintf("TPM %d", c.TPM))
		}
		if c.Passphrase > 0 {
			kinds = append(kinds, fmt.Sprintf("passphrase %d", c.Passphrase))
		}
		if c.Recovery > 0 {
			kinds = append(kinds, fmt.Sprintf("recovery key %d", c.Recovery))
		}
		line := fmt.Sprintf("  %s (%s): keyslots: %s", c.Label, c.Device, strings.Join(kinds, ", "))
		if c.TPM > 0 {
			if c.TPMWorks {
				line += "; TPM unlock: works"
			} else {
				line += "; TPM unlock: FAILS (run `hadron-recovery reseal`)"
			}
		}
		fmt.Fprintln(w, line)
	}
}

func tpmWorksEverywhere(st RecoveryStatus) bool {
	if len(st.Containers) == 0 {
		return false
	}
	for _, c := range st.Containers {
		if c.TPM == 0 || !c.TPMWorks {
			return false
		}
	}
	return true
}

func recoveryReseal(c recoveryClient, t terminal, st RecoveryStatus, stdout, stderr io.Writer) int {
	if st.Edition != editionTrustedBoot {
		fmt.Fprintln(stderr, "hadron-recovery: reseal is for Trusted Boot; this disk is not sealed to a TPM")
		return exitError
	}
	if tpmWorksEverywhere(st) {
		fmt.Fprintln(stdout, "The TPM unlocks the disk already: there is nothing to reseal.")
		return exitOK
	}
	secret, err := t.Secret("Type the recovery key (8 groups of 8 letters; dashes optional): ")
	if err != nil {
		return exitError
	}
	if strings.TrimSpace(secret) == "" {
		fmt.Fprintln(stderr, "hadron-recovery: nothing typed; nothing changed")
		return exitError
	}
	fmt.Fprintln(stdout, "Sealing the disk to this computer's TPM...")
	if err := c.RecoveryReseal(secret); err != nil {
		fmt.Fprintf(stderr, "hadron-recovery: %v\n", err)
		return exitError
	}
	fmt.Fprintln(stdout, "Done. The next start unlocks the disk without the recovery key.")
	return exitOK
}

func recoveryNewKey(c recoveryClient, t terminal, st RecoveryStatus, stdout, stderr io.Writer) int {
	secret := ""
	var err error
	if st.Edition == editionTrustedBoot && tpmWorksEverywhere(st) {
		fmt.Fprintln(stdout, "The TPM unlocks the disk, so it authorizes the new key.")
	} else {
		p := "Type the disk passphrase or the current recovery key: "
		if st.Edition == editionTrustedBoot {
			p = "The TPM does not unlock the disk. Type the current recovery key: "
		}
		if secret, err = t.Secret(p); err != nil {
			return exitError
		}
		if secret == "" {
			fmt.Fprintln(stderr, "hadron-recovery: nothing typed; nothing changed")
			return exitError
		}
	}

	key, err := makeRecoveryKey()
	if err != nil {
		fmt.Fprintf(stderr, "hadron-recovery: %v\n", err)
		return exitError
	}
	fmt.Fprint(stdout, "\nYour new recovery key\n\n")
	for _, l := range recoveryKeyLines(key) {
		fmt.Fprintln(stdout, l)
	}
	fmt.Fprintln(stdout, "\n  Write it down or take a photo of it now, and keep it away from")
	fmt.Fprintln(stdout, "  this computer. It is shown only this once and saved nowhere.")
	fmt.Fprintf(stdout, "  The letters are only these: %s\n\n", spaced(modhexAlphabet))
	if _, err := t.Line("Press Enter when you have saved it."); err != nil {
		return exitError
	}
	// Push the key off the screen before asking for it back.
	fmt.Fprint(stdout, strings.Repeat("\n", 40))
	group := pickGroup()
	for tries := 0; ; tries++ {
		typed, err := t.Line(fmt.Sprintf("To check you saved it, type group %d of 8 of the new key: ", group+1))
		if err != nil {
			return exitError
		}
		if confirmsGroup(key, group, typed) {
			break
		}
		if tries == 2 {
			fmt.Fprintln(stderr, "hadron-recovery: that does not match; nothing changed, your old key still works")
			return exitError
		}
		fmt.Fprintln(stdout, "That is not it. Check what you wrote down.")
	}
	fmt.Fprintln(stdout, "Adding the new recovery key...")
	if err := c.RecoveryNewKey(secret, key); err != nil {
		fmt.Fprintf(stderr, "hadron-recovery: %v\n", err)
		return exitError
	}
	fmt.Fprintln(stdout, "Done. The new recovery key works; the old one does not any more.")
	return exitOK
}
