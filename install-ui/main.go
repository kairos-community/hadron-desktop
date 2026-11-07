package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

// installCommand builds the child install process. HADRON_INSTALL_CMD is the
// single seam through which a disk wipe happens; tests inject a fake here to
// assert did/didn't install, so it must keep working exactly as in the shell
// installer this replaces.
//
// manualConfig, when set, is the installer's own cloud-config, and the install
// runs as `kairos-agent manual-install` on it. The encrypted install needs
// that: its config says reboot: false so the installer can delete the
// passphrase file and check the disk first, and `kairos-agent install`
// answers reboot: false with a
// "press enter" prompt and a getty on tty1, where this UI is drawing.
// manual-install returns instead. --use-default-dirs makes it read the same
// config directories `install` reads, so the two install the same system.
func installCommand(manualConfig string) *exec.Cmd {
	if c := os.Getenv("HADRON_INSTALL_CMD"); c != "" {
		return exec.Command("/bin/sh", "-c", c)
	}
	if manualConfig != "" {
		return exec.Command("/usr/bin/kairos-agent", "manual-install", "--use-default-dirs", manualConfig)
	}
	return exec.Command("/usr/bin/kairos-agent", "install")
}

// postInstall runs after a successful `kairos-agent install`, inside the
// progress UI; log adds a line to the install log. nil means nothing to do.
type postInstall func(log func(string)) error

// rebootCommand restarts the machine after an install that asked kairos-agent
// not to (an encrypted one). HADRON_REBOOT_CMD is the test seam.
func rebootCommand() *exec.Cmd {
	if c := os.Getenv("HADRON_REBOOT_CMD"); c != "" {
		return exec.Command("/bin/sh", "-c", c)
	}
	return exec.Command("reboot")
}

// encryptionSteps returns what an encrypted install does around kairos-agent:
// pre writes the passphrase where kcrypt reads it, cleanup deletes it again
// (whether or not the install worked), post checks the disk. All three are
// nil for a choice without encryption, for --progress-only (the agent
// appliance) and for a seeded config.
func encryptionSteps(c *Choice) (pre func() error, cleanup func() error, post postInstall) {
	if c == nil || !c.Encrypt {
		return nil, nil, nil
	}
	disk, passphrase := c.Disk, c.Passphrase
	pre = func() error { return writePassphraseFile(passphraseFile, passphrase) }
	cleanup = func() error { return removePassphraseFile(passphraseFile) }
	post = func(log func(string)) error {
		// The file goes before the check, so it is gone even when the
		// check fails and the person walks away from the failure screen.
		if err := cleanup(); err != nil {
			return fmt.Errorf("deleting %s: %w", passphraseFile, err)
		}
		return verifyEncryption(execRunner, disk, passphrase, log)
	}
	return pre, cleanup, post
}

// runInstall runs the installer as a subprocess, rendering the branded progress
// UI from its output. kairos-agent has no progress API, so its stdout is the
// only signal available (see steps.go). post, when set, runs after a
// successful install and its failure is an install failure.
func runInstall(manualConfig string, post postInstall) error {
	cmd := installCommand(manualConfig)

	// The child must never read the terminal: bubbletea owns it.
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd.Stdin = devnull

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout // merge, so failures show up in the log viewport

	p := tea.NewProgram(newModel(), tea.WithAltScreen())
	go func() {
		if err := cmd.Start(); err != nil {
			p.Send(doneMsg{err: err})
			return
		}
		streamLines(stdout, p.Send)
		err := cmd.Wait()
		if err == nil && post != nil {
			err = post(func(l string) { p.Send(lineMsg(l)) })
		}
		p.Send(doneMsg{err: err})
	}()

	final, err := p.Run()
	if err != nil {
		return err
	}
	if m, ok := final.(model); ok && m.action == actionShell {
		// Replace this process so the rescue shell owns tty1 outright.
		return syscall.Exec("/bin/sh", []string{"sh"}, os.Environ())
	}
	if m, ok := final.(model); ok && m.failed {
		return fmt.Errorf("install failed")
	}
	return nil
}

func main() {
	progressOnly := false
	for _, a := range os.Args[1:] {
		switch a {
		case "--progress-only":
			progressOnly = true
		default:
			fmt.Fprintf(os.Stderr, "hadron-install-ui: unknown argument: %s\n", a)
			os.Exit(2)
		}
	}

	// --progress-only (the agent appliance's installer) never runs the
	// wizard, so it never offers encryption.
	var choice *Choice
	if !progressOnly {
		c, err := runWizard()
		if err != nil {
			fmt.Fprintf(os.Stderr, "hadron-install-ui: %v\n", err)
			os.Exit(1)
		}
		choice = c
	}

	pre, cleanup, post := encryptionSteps(choice)
	manualConfig := ""
	if pre != nil {
		manualConfig = configOut()
		if err := pre(); err != nil {
			fmt.Fprintf(os.Stderr, "hadron-install-ui: %v\n", err)
			os.Exit(1)
		}
	}
	err := runInstall(manualConfig, post)
	if cleanup != nil {
		// post already deleted it after a successful install; this covers
		// a failed one.
		if cerr := cleanup(); cerr != nil && err == nil {
			err = cerr
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "hadron-install-ui: %v\n", err)
		os.Exit(1)
	}
	if post != nil {
		// The encrypted install told kairos-agent not to reboot.
		if err := rebootCommand().Run(); err != nil {
			fmt.Fprintf(os.Stderr, "hadron-install-ui: reboot: %v\n", err)
			os.Exit(1)
		}
	}
}
