package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type lineMsg string
type doneMsg struct{ err error }

type action int

const (
	actionNone  action = iota
	actionShell        // on exit, main() should exec a rescue shell
)

type model struct {
	spin     spinner.Model
	vp       viewport.Model
	logs     *ring
	step     int
	showLogs bool
	failed   bool
	done     bool
	action   action
	ready    bool
	height   int
}

func newModel() model {
	s := spinner.New()
	s.Spinner = spinner.Line // ASCII (|/-\) — safe on the VT console font
	return model{spin: s, logs: newRing(2000)}
}

func (m model) Init() tea.Cmd { return m.spin.Tick }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Reserve two rows of chrome, not one. The failure view is the tallest:
		// a halt banner above the viewport and a rescue hint below it. Sizing the
		// viewport to Height-1 makes that view Height+1 rows, and bubbletea's
		// renderer drops the *leading* overflow (standard_renderer.go: it keeps
		// the last r.height lines) — which silently ate the red "install halted"
		// banner, the one thing the user must see. See TestViewsFitTerminalHeight.
		m.height = msg.Height
		m.vp = viewport.New(msg.Width, msg.Height-2)
		m.vp.SetContent(m.logs.text())
		m.ready = true
		return m, nil

	case lineMsg:
		atBottom := m.ready && m.vp.AtBottom()
		m.logs.add(string(msg))
		m.step = AdvanceStep(string(msg), m.step)
		if m.ready {
			m.vp.SetContent(m.logs.text())
			if atBottom {
				m.vp.GotoBottom() // follow the tail unless the user scrolled up
			}
		}
		return m, nil

	case doneMsg:
		if msg.err != nil {
			m.failed = true
			m.showLogs = true // a failure without logs is useless
			if m.ready {
				m.vp.SetContent(m.logs.text())
				m.vp.GotoBottom()
			}
			return m, nil
		}
		m.done = true
		m.step = len(Steps) - 1
		return m, tea.Quit

	case tea.KeyMsg:
		return m.handleKey(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.failed {
		if msg.String() == "s" {
			m.action = actionShell
			return m, tea.Quit
		}
		// A second way off the failure screen. bubbletea puts the tty in raw
		// mode, so ctrl+c arrives here as a key rather than SIGINT, and v1.3.10
		// installs no default quit binding — without this, 's' is the only exit
		// and a flaky VT keyboard would strand a failed install until a power
		// cycle. m.failed stays set, so runInstall still exits non-zero.
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg) // allow scrolling the logs
		return m, cmd
	}
	if msg.String() == "l" {
		m.showLogs = !m.showLogs
		return m, nil
	}
	if m.showLogs {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
	// Deliberately no quit key while installing: the disk is being written.
	return m, nil
}

// Tokyo Night styles, matching the boot splash so the handoff reads as one
// continuous machine. 16-color VGA palette only: this runs on the kernel VT
// (TERM=linux), where truecolor and 256-color values round to the wrong hue.
// 12 = bright blue, 14 = bright cyan, 8 = dark grey, 15 = white-hot,
// 9 = red (reserved for a real halt).
var (
	tnLogo  = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	tnHot   = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)
	tnDim   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	tnAcc   = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	haltRed = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
)

// progressBar renders [####....] with a bright fill over a dim track.
func progressBar(pct, width int) string {
	if width < 1 {
		width = 1
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	fill := pct * width / 100
	return tnDim.Render("[") + tnAcc.Render(strings.Repeat("#", fill)) +
		tnDim.Render(strings.Repeat(".", width-fill)) + tnDim.Render("]")
}

// clampHeight drops trailing lines so s is at most height rows. bubbletea's
// renderer keeps the *last* height lines of whatever it is given, so an
// over-tall view loses its top — on the failure screen that is the halt banner.
// Truncating from the bottom instead keeps the banner and costs only a hint
// line. height <= 0 means we have not been told the terminal size yet.
func clampHeight(s string, height int) string {
	if height <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= height {
		return s
	}
	return strings.Join(lines[:height], "\n")
}

func (m model) View() string { return clampHeight(m.view(), m.height) }

func (m model) view() string {
	if !m.ready {
		return "\n  " + tnDim.Render("waking the machine...")
	}
	if m.failed {
		return haltRed.Render("  install halted") + "\n" + m.vp.View() +
			"\n  " + tnDim.Render("press 's' for a rescue shell")
	}
	if m.showLogs {
		return m.vp.View() + "\n  " + tnDim.Render("press 'l' to hide logs")
	}

	var b strings.Builder
	b.WriteString("\n")
	for _, l := range logo {
		b.WriteString("        " + tnLogo.Render(l) + "\n")
	}
	b.WriteString("\n")
	if m.done {
		b.WriteString("        " + tnHot.Render("system installed - rebooting...") + "\n")
	} else {
		b.WriteString("        " + tnHot.Render(m.spin.View()) + "  " +
			tnHot.Render(Steps[m.step].Name) + "...\n")
	}
	pct := Percent(m.step)
	b.WriteString("\n        " + progressBar(pct, 20) +
		tnDim.Render(fmt.Sprintf("  %3d%%", pct)) + "\n")
	b.WriteString("\n        " + tnDim.Render("press 'l' to show logs · do not power off") + "\n")
	return b.String()
}
