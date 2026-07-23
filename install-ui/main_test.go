package main

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestInstallCommandDefault(t *testing.T) {
	t.Setenv("HADRON_INSTALL_CMD", "")
	cmd := installCommand()
	got := strings.Join(cmd.Args, " ")
	if !strings.Contains(got, "kairos-agent") || !strings.Contains(got, "install") {
		t.Errorf("default command = %q, want kairos-agent install", got)
	}
}

func TestInstallCommandHonoursOverride(t *testing.T) {
	t.Setenv("HADRON_INSTALL_CMD", "/bin/true --fake")
	cmd := installCommand()
	got := strings.Join(cmd.Args, " ")
	if !strings.Contains(got, "/bin/true --fake") {
		t.Errorf("override command = %q, want it to carry /bin/true --fake", got)
	}
}

var errFake = errors.New("boom")

// sized drives a fresh model through a window-size message and the given
// messages, returning the resulting model.
func sized(t *testing.T, height int, msgs ...tea.Msg) model {
	t.Helper()
	var m tea.Model = newModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: height})
	for _, msg := range msgs {
		m, _ = m.Update(msg)
	}
	return m.(model)
}

// TestViewsFitTerminalHeight guards the halt banner. bubbletea's renderer keeps
// only the *last* terminal-height lines, so a view even one row too tall loses
// its top line — which on the failure screen is "install halted", the sole
// signal that the install died. A too-tall view is silent breakage, not a crash.
func TestViewsFitTerminalHeight(t *testing.T) {
	const height = 25

	cases := []struct {
		name string
		msgs []tea.Msg
		want string
		// exact pins a view that is sized to fill the screen exactly: too tall
		// loses its top line to the renderer, too short stops covering the
		// screen and leaves the previous frame's rows visible underneath.
		exact bool
	}{
		{
			name:  "failed",
			msgs:  []tea.Msg{lineMsg("Partitioning device /dev/vda"), doneMsg{err: errFake}},
			want:  "install halted",
			exact: true,
		},
		{
			name: "logs shown",
			msgs: []tea.Msg{lineMsg("Partitioning device /dev/vda"), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}},
			want: "to hide logs",
		},
		{
			name: "progress",
			msgs: []tea.Msg{lineMsg("Partitioning device /dev/vda")},
			want: "do not power off",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The *unclamped* layout, deliberately: View()'s clamp is a
			// last-resort net for tiny terminals, and measuring through it
			// would hide exactly the over-tall layout this test exists to
			// catch. The layout has to fit on its own.
			view := sized(t, height, tc.msgs...).view()
			got := len(strings.Split(view, "\n"))
			if got > height {
				t.Errorf("%s view is %d lines, terminal is %d: bubbletea will drop the top %d",
					tc.name, got, height, got-height)
			}
			if tc.exact && got != height {
				t.Errorf("%s view is %d lines, want exactly %d to fill the screen", tc.name, got, height)
			}
			if !strings.Contains(view, tc.want) {
				t.Errorf("%s view does not contain %q; got:\n%s", tc.name, tc.want, view)
			}
		})
	}
}

// TestFailureScreenHasASecondExit: bubbletea takes the tty into raw mode, so
// ctrl+c arrives as a key, not SIGINT, and v1.3.10 binds no default quit. If
// 's' were the only exit, a failed install on hardware with flaky VT input
// would strand the machine until a power cycle. The quit must not clear
// m.failed — runInstall's non-zero exit hangs off it.
func TestFailureScreenHasASecondExit(t *testing.T) {
	var m tea.Model = newModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 25})
	m, _ = m.Update(doneMsg{err: errFake})

	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c on the failure screen returned no command; expected tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("ctrl+c returned %T, want tea.QuitMsg", cmd())
	}
	got := m2.(model)
	if !got.failed {
		t.Error("quitting cleared m.failed; runInstall would then report success")
	}
	if got.action == actionShell {
		t.Error("ctrl+c must not exec a rescue shell; that is what 's' is for")
	}
}

// TestFailureScreenAdvertisesBothExits: the second exit above only helps a user
// who knows it is there. ctrl+c exists precisely for the case where 's' does not
// get through on a real VT, so the screen that appears in that case has to name
// it — an undiscoverable escape hatch is the same as no escape hatch.
func TestFailureScreenAdvertisesBothExits(t *testing.T) {
	var m tea.Model = newModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 25})
	m, _ = m.Update(doneMsg{err: errFake})

	view := m.(model).view()
	for _, want := range []string{"'s'", "rescue shell", "ctrl+c"} {
		if !strings.Contains(view, want) {
			t.Errorf("failure screen does not mention %q:\n%s", want, view)
		}
	}
}

// TestNoQuitKeyWhileInstalling is the other half: the disk is being written, so
// no key may end the program before the install does.
func TestNoQuitKeyWhileInstalling(t *testing.T) {
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyCtrlC},
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyRunes, Runes: []rune{'s'}},
	} {
		var m tea.Model = newModel()
		m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 25})
		m2, cmd := m.Update(k)
		if cmd != nil {
			if _, isQuit := cmd().(tea.QuitMsg); isQuit {
				t.Errorf("key %q quits during an install", k.String())
			}
		}
		if m2.(model).action == actionShell {
			t.Errorf("key %q execs a rescue shell during an install", k.String())
		}
	}
}

// errReader fails partway through, the way a pipe to a dying child does.
type errReader struct{ done bool }

func (e *errReader) Read(p []byte) (int, error) {
	if e.done {
		return 0, errors.New("read: broken pipe")
	}
	e.done = true
	return copy(p, "Partitioning device /dev/vda\n"), nil
}

// TestStreamLinesReportsScanErrors: if the scan stops silently, runInstall goes
// on to cmd.Wait() with the pipe undrained — the child blocks writing, Wait
// blocks on the child, and the progress screen has no quit key. That is a hang,
// not a stall, so the error has to reach the UI.
func TestStreamLinesReportsScanErrors(t *testing.T) {
	var msgs []tea.Msg
	streamLines(&errReader{}, func(m tea.Msg) { msgs = append(msgs, m) })

	if len(msgs) == 0 {
		t.Fatal("streamLines sent nothing at all")
	}
	last, ok := msgs[len(msgs)-1].(doneMsg)
	if !ok {
		t.Fatalf("last message is %T, want a doneMsg carrying the scan error", msgs[len(msgs)-1])
	}
	if last.err == nil {
		t.Fatal("doneMsg carries a nil error; the failure would render as success")
	}
	if !strings.Contains(last.err.Error(), "broken pipe") {
		t.Errorf("error %q does not carry the underlying cause", last.err)
	}
}

// TestStreamLinesSendsNoDoneMsgAtCleanEOF: a clean EOF is runInstall's job to
// report via cmd.Wait(); a spurious doneMsg here would flag a good install as
// failed.
func TestStreamLinesSendsNoDoneMsgAtCleanEOF(t *testing.T) {
	var msgs []tea.Msg
	streamLines(strings.NewReader("one\ntwo\n"), func(m tea.Msg) { msgs = append(msgs, m) })

	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 lines and nothing else: %v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if _, ok := m.(doneMsg); ok {
			t.Errorf("clean EOF produced a doneMsg: %v", m)
		}
	}
}

// TestViewNeverExceedsTerminalHeight: m.height is not decoration. bubbletea
// keeps the last height lines of a frame, so a view that outgrows a short
// terminal loses its top rows; clamping from the bottom keeps them.
func TestViewNeverExceedsTerminalHeight(t *testing.T) {
	for _, height := range []int{4, 6, 10, 25} {
		view := sized(t, height, lineMsg("Partitioning device /dev/vda")).View()
		if got := len(strings.Split(view, "\n")); got > height {
			t.Errorf("at height %d the view is %d lines", height, got)
		}
	}
}

func TestClampHeight(t *testing.T) {
	const s = "a\nb\nc\nd"
	if got := clampHeight(s, 2); got != "a\nb" {
		t.Errorf("clampHeight(_, 2) = %q, want %q", got, "a\nb")
	}
	if got := clampHeight(s, 4); got != s {
		t.Errorf("clampHeight(_, 4) = %q, want it unchanged", got)
	}
	if got := clampHeight(s, 9); got != s {
		t.Errorf("clampHeight(_, 9) = %q, want it unchanged", got)
	}
	// Height 0 means the terminal size is not known yet; truncating there would
	// blank the screen.
	if got := clampHeight(s, 0); got != s {
		t.Errorf("clampHeight(_, 0) = %q, want it unchanged", got)
	}
}
