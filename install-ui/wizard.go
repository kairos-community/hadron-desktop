package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

const defaultHostname = "hadron"

func oemDir() string {
	if d := os.Getenv("HADRON_OEM_DIR"); d != "" {
		return d
	}
	return "/oem"
}

// posixUsername is the conservative intersection of what useradd accepts and
// what is safe to interpolate: lowercase start (letter or underscore), then
// letters, digits, dash or underscore, 32 characters at most.
//
// This is not cosmetic. RenderCloudConfig writes Username raw into the
// /etc/ly/save.ini block scalar and returns no error, so it cannot defend
// itself: a newline there closes the YAML block early and produces a
// cloud-config that fails to parse on a machine whose disk has already been
// wiped. The check has to live here, before the value can reach the renderer.
var posixUsername = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

const usernameRule = "username must be 1-32 chars: lowercase letters, digits, - or _, starting with a letter or _"

func validateUsername(v string) error {
	if v == "" {
		return errors.New("username is required")
	}
	if !posixUsername.MatchString(v) {
		return errors.New(usernameRule)
	}
	return nil
}

// choiceFromEnv reads the non-interactive configuration. The second return value
// reports whether non-interactive mode is active at all. These variable names are
// a stable contract with existing tests and automation.
func choiceFromEnv() (Choice, bool, error) {
	if os.Getenv("HADRON_INSTALL_NONINTERACTIVE") != "1" {
		return Choice{}, false, nil
	}
	c := Choice{
		Hostname: os.Getenv("HADRON_HOSTNAME"),
		Username: os.Getenv("HADRON_USER"),
		Password: os.Getenv("HADRON_PASS"),
		Github:   os.Getenv("HADRON_GITHUB"),
		Disk:     os.Getenv("HADRON_DISK"),
	}
	if c.Hostname == "" {
		c.Hostname = defaultHostname
	}
	var missing []string
	if c.Username == "" {
		missing = append(missing, "HADRON_USER")
	}
	if c.Password == "" {
		missing = append(missing, "HADRON_PASS")
	}
	if c.Disk == "" {
		missing = append(missing, "HADRON_DISK")
	}
	if len(missing) > 0 {
		return c, true, fmt.Errorf("non-interactive mode requires %s", strings.Join(missing, ", "))
	}
	// The interactive path validates at the input step; the env is the other way
	// a username can reach RenderCloudConfig, so it is checked here too.
	if err := validateUsername(c.Username); err != nil {
		return c, true, fmt.Errorf("HADRON_USER: %w", err)
	}
	return c, true, nil
}

// writeChoice hashes the password and writes the cloud-config with owner-only
// permissions — the file carries a password hash.
func writeChoice(path string, c Choice, hash string) error {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(RenderCloudConfig(c, hash)), 0o600)
}

func dirOf(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return "."
}

// --- wizard steps ----------------------------------------------------------

type wizardStep int

const (
	stepHostname wizardStep = iota
	stepUser
	stepPass
	stepGithub
	stepDisk
	stepReview
)

var stepName = map[wizardStep]string{
	stepHostname: "hostname",
	stepUser:     "username",
	stepPass:     "password",
	stepGithub:   "ssh keys",
	stepDisk:     "target disk",
	stepReview:   "review",
}

type wizardModel struct {
	step    wizardStep
	in      textinput.Model
	choice  Choice
	disks   []Disk
	diskIdx int
	err     string
	done    bool
	aborted bool
}

func newWizard(disks []Disk) wizardModel {
	m := wizardModel{step: stepHostname, disks: disks}
	m.in = textinput.New()
	m.enter(stepHostname)
	return m
}

// enter re-seeds the input for step s from already-collected state, so values
// survive back-navigation instead of being silently cleared.
func (m *wizardModel) enter(s wizardStep) {
	m.step = s
	m.err = ""
	m.in.EchoMode = textinput.EchoNormal
	m.in.Prompt = "> "
	switch s {
	case stepHostname:
		m.in.SetValue(orDefault(m.choice.Hostname, defaultHostname))
	case stepUser:
		m.in.SetValue(m.choice.Username)
	case stepPass:
		m.in.EchoMode = textinput.EchoPassword
		m.in.SetValue(m.choice.Password)
	case stepGithub:
		m.in.SetValue(m.choice.Github)
	}
	m.in.Focus()
	m.in.CursorEnd()
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (m wizardModel) Init() tea.Cmd { return textinput.Blink }

func (m wizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.in, cmd = m.in.Update(msg)
		return m, cmd
	}

	switch key.String() {
	case "ctrl+c":
		m.aborted = true
		return m, tea.Quit
	case "esc":
		if m.step > stepHostname {
			m.enter(m.step - 1)
		}
		return m, nil
	}

	if m.step == stepDisk {
		return m.updateDisk(key)
	}
	if m.step == stepReview {
		return m.updateReview(key)
	}
	if key.String() == "enter" {
		return m.commitField()
	}
	var cmd tea.Cmd
	m.in, cmd = m.in.Update(msg)
	return m, cmd
}

func (m wizardModel) commitField() (tea.Model, tea.Cmd) {
	v := strings.TrimSpace(m.in.Value())
	switch m.step {
	case stepHostname:
		m.choice.Hostname = orDefault(v, defaultHostname)
		m.enter(stepUser)
	case stepUser:
		if err := validateUsername(v); err != nil {
			m.err = err.Error()
			return m, nil
		}
		m.choice.Username = v
		m.enter(stepPass)
	case stepPass:
		if v == "" {
			m.err = "password is required"
			return m, nil
		}
		m.choice.Password = v
		m.enter(stepGithub)
	case stepGithub:
		m.choice.Github = v // optional
		if len(m.disks) == 0 {
			m.err = "no installable disk found"
			return m, nil
		}
		m.enter(stepDisk)
	}
	return m, nil
}

func (m wizardModel) updateDisk(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "up", "k":
		if m.diskIdx > 0 {
			m.diskIdx--
		}
	case "down", "j":
		if m.diskIdx < len(m.disks)-1 {
			m.diskIdx++
		}
	case "enter":
		m.choice.Disk = m.disks[m.diskIdx].Path
		m.enter(stepReview)
	}
	return m, nil
}

func (m wizardModel) updateReview(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.String() == "enter" {
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m wizardModel) View() string {
	var b strings.Builder
	b.WriteString("\n  " + tnHot.Render("HADRON") +
		tnDim.Render(" // desktop install · "+stepName[m.step]) + "\n\n")

	switch m.step {
	case stepDisk:
		b.WriteString("  " + tnDim.Render("the selected disk will be ERASED") + "\n\n")
		for i, d := range m.disks {
			line := fmt.Sprintf("%s  %d GB", d.Path, d.SizeGB)
			if i == m.diskIdx {
				b.WriteString("  " + tnAcc.Render("> "+line) + "\n")
			} else {
				b.WriteString("    " + tnDim.Render(line) + "\n")
			}
		}
		b.WriteString("\n  " + tnDim.Render("up/down select · enter confirm · esc back") + "\n")

	case stepReview:
		b.WriteString("  " + haltRed.Render("this ERASES "+m.choice.Disk+" and installs hadron-desktop") + "\n\n")
		b.WriteString("    hostname   " + tnHot.Render(m.choice.Hostname) + "\n")
		b.WriteString("    username   " + tnHot.Render(m.choice.Username) + "\n")
		b.WriteString("    ssh keys   " + tnHot.Render(orDefault(githubLabel(m.choice.Github), "none")) + "\n")
		b.WriteString("    disk       " + haltRed.Render(m.choice.Disk) + "\n")
		b.WriteString("\n  " + tnDim.Render("enter to INSTALL · esc back") + "\n")

	default:
		b.WriteString("  " + prompt(m.step) + "\n")
		b.WriteString("  " + m.in.View() + "\n")
		if m.err != "" {
			b.WriteString("\n  " + haltRed.Render(m.err) + "\n")
		}
		hint := "enter continue"
		if m.step > stepHostname {
			hint += " · esc back"
		}
		b.WriteString("\n  " + tnDim.Render(hint) + "\n")
	}
	return b.String()
}

func githubLabel(g string) string {
	if g == "" {
		return ""
	}
	return "github:" + g
}

func prompt(s wizardStep) string {
	switch s {
	case stepHostname:
		return tnDim.Render("hostname for this machine")
	case stepUser:
		return tnDim.Render("username for the desktop account")
	case stepPass:
		return tnDim.Render("password for that account")
	case stepGithub:
		return tnDim.Render("GitHub username to import SSH keys from (blank to skip)")
	}
	return ""
}

// --- entry point -----------------------------------------------------------

// runWizard collects the install configuration and writes it to /oem for
// `kairos-agent install` to merge. It is a no-op when a config is already present,
// preserving the shell installer's unattended behaviour for seeded installs.
func runWizard() error {
	out := oemDir() + "/99_hadron-user.yaml"

	present, err := ConfigPresent(oemDir(), out)
	if err != nil {
		return err
	}
	if present {
		// Someone already supplied a users/install config: install unattended.
		return nil
	}

	c, nonInteractive, err := choiceFromEnv()
	if err != nil {
		return err
	}

	if !nonInteractive {
		disks, err := ListDisks("/sys/block")
		if err != nil {
			return err
		}
		final, err := tea.NewProgram(newWizard(disks), tea.WithAltScreen()).Run()
		if err != nil {
			return err
		}
		wm, ok := final.(wizardModel)
		if !ok || wm.aborted || !wm.done {
			return errors.New("install cancelled")
		}
		c = wm.choice
	}

	hash, err := HashPassword(c.Password)
	if err != nil {
		return err
	}
	return writeChoice(out, c, hash)
}
