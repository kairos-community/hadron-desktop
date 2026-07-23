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
func installCommand() *exec.Cmd {
	if c := os.Getenv("HADRON_INSTALL_CMD"); c != "" {
		return exec.Command("/bin/sh", "-c", c)
	}
	return exec.Command("/usr/bin/kairos-agent", "install")
}

// runInstall runs the installer as a subprocess, rendering the branded progress
// UI from its output. kairos-agent has no progress API, so its stdout is the
// only signal available (see steps.go).
func runInstall() error {
	cmd := installCommand()

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
		p.Send(doneMsg{err: cmd.Wait()})
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

	if !progressOnly {
		if err := runWizard(); err != nil {
			fmt.Fprintf(os.Stderr, "hadron-install-ui: %v\n", err)
			os.Exit(1)
		}
	}

	if err := runInstall(); err != nil {
		fmt.Fprintf(os.Stderr, "hadron-install-ui: %v\n", err)
		os.Exit(1)
	}
}
