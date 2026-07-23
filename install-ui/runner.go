package main

import (
	"bufio"
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"
)

// streamLines reads r line by line and delivers each as a lineMsg via send.
// Returns at EOF. Install logs can carry very long lines (device lists, kernel
// cmdlines), so the scanner buffer is enlarged well past bufio's 64KiB default.
//
// A scan error is reported as a doneMsg rather than swallowed. Ending the scan
// silently is not merely a stalled progress bar: the caller goes on to
// cmd.Wait() with the pipe undrained, so a child still writing blocks on a full
// pipe while Wait blocks on the child — an unrecoverable hang, and the progress
// screen has no quit key. Surfacing it makes it a visible halt the user can
// leave instead (see model.handleKey).
func streamLines(r io.Reader, send func(tea.Msg)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		send(lineMsg(sc.Text()))
	}
	if err := sc.Err(); err != nil {
		send(doneMsg{err: fmt.Errorf("reading installer output: %w", err)})
	}
}
