package main

import (
	"bufio"
	"io"

	tea "github.com/charmbracelet/bubbletea"
)

// streamLines reads r line by line and delivers each as a lineMsg via send.
// Returns at EOF. Install logs can carry very long lines (device lists, kernel
// cmdlines), so the scanner buffer is enlarged well past bufio's 64KiB default —
// otherwise a single long line ends the scan early and the progress bar stalls.
func streamLines(r io.Reader, send func(tea.Msg)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		send(lineMsg(sc.Text()))
	}
}
