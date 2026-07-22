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
	}{
		{
			name: "failed",
			msgs: []tea.Msg{lineMsg("Partitioning device /dev/vda"), doneMsg{err: errFake}},
			want: "install halted",
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
			view := sized(t, height, tc.msgs...).View()
			if got := len(strings.Split(view, "\n")); got > height {
				t.Errorf("%s view is %d lines, terminal is %d: bubbletea will drop the top %d",
					tc.name, got, height, got-height)
			}
			if !strings.Contains(view, tc.want) {
				t.Errorf("%s view does not contain %q; got:\n%s", tc.name, tc.want, view)
			}
		})
	}
}
