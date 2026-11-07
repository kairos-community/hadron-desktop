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

// userConfigPath is the cloud-config the wizard writes.
func userConfigPath() string { return oemDir() + "/99_hadron-user.yaml" }

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

// githubHandle is GitHub's own account-name shape: ASCII alphanumerics and
// hyphens, never leading or trailing, 39 characters at most.
//
// This matters more than the username check, not less. Github is the one field
// RenderCloudConfig writes into a *plain* YAML scalar with no quoting:
//
//   - github:<value>
//
// A newline there does not break the document, it extends it. The value
// "ada\nstages:\n  network:\n    - name: pwn" renders a cloud-config with two
// top-level `stages:` keys — still valid YAML, in which the injected block
// displaces the /etc/ly/save.ini stage and the machine boots to a login screen
// that was never configured. An invalid username fails loudly; this fails
// silently, and both happen after the disk has been wiped. RenderCloudConfig
// returns no error and so cannot refuse, so the check lives here at the input.
var githubHandle = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

const githubRule = "github username must be 1-39 chars: letters, digits or hyphens, not starting or ending with a hyphen"

// validateGithub accepts the empty string: importing SSH keys is optional and
// skipping the step is the documented way to decline.
func validateGithub(v string) error {
	if v == "" {
		return nil
	}
	if !githubHandle.MatchString(v) {
		return errors.New(githubRule)
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

		KeyboardLayout:  orDefault(os.Getenv("HADRON_KEYBOARD_LAYOUT"), defaultLayout),
		KeyboardVariant: os.Getenv("HADRON_KEYBOARD_VARIANT"),
		Timezone:        orDefault(os.Getenv("HADRON_TIMEZONE"), defaultTimezone),

		Encrypt:  os.Getenv("HADRON_ENCRYPT") == "1",
		Recovery: os.Getenv("HADRON_RECOVERY_PASSPHRASE"),
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
	// Same reasoning for the GitHub handle, which reaches an unquoted scalar.
	// Empty stays valid here: HADRON_GITHUB is genuinely optional.
	if err := validateGithub(c.Github); err != nil {
		return c, true, fmt.Errorf("HADRON_GITHUB: %w", err)
	}
	// Optional, defaulting to US English and UTC. The renderer would replace
	// a bad value with the default, but a typo here should fail the install,
	// not install a machine with a keyboard the operator did not ask for.
	if err := validateKeyboard(c.KeyboardLayout, c.KeyboardVariant); err != nil {
		return c, true, fmt.Errorf("HADRON_KEYBOARD_LAYOUT/HADRON_KEYBOARD_VARIANT: %w", err)
	}
	if err := validateTimezone(c.Timezone); err != nil {
		return c, true, fmt.Errorf("HADRON_TIMEZONE: %w", err)
	}
	// Encryption has no default passphrase: without HADRON_RECOVERY_PASSPHRASE
	// the only key would be the one in the TPM.
	if c.Encrypt {
		if err := validateRecovery(c.Recovery); err != nil {
			return c, true, fmt.Errorf("HADRON_RECOVERY_PASSPHRASE: %w", err)
		}
	} else {
		c.Recovery = ""
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

// The keyboard comes first: the console switches to the chosen keymap, so the
// password is typed with the layout the installed system will use for it.
const (
	stepKeyboard wizardStep = iota
	stepTimezone
	stepHostname
	stepUser
	stepPass
	stepGithub
	stepDisk
	stepEncrypt
	stepRecovery
	stepRecoveryConfirm
	stepReview
)

var stepName = map[wizardStep]string{
	stepKeyboard:        "keyboard",
	stepTimezone:        "timezone",
	stepHostname:        "hostname",
	stepUser:            "username",
	stepPass:            "password",
	stepGithub:          "ssh keys",
	stepDisk:            "target disk",
	stepEncrypt:         "encryption",
	stepRecovery:        "recovery passphrase",
	stepRecoveryConfirm: "recovery passphrase",
	stepReview:          "review",
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

	keyboards []Keyboard
	kbPick    picker
	zones     []string
	tzPick    picker
	// keymapsDir is where the console keymaps are looked up, and
	// applyKeymap loads one on the live console. newWizard makes it a no-op.
	keymapsDir  string
	applyKeymap func(string)

	// hasTPM shows the encryption step; without a TPM 2.0 the wizard goes
	// from the disk straight to the review. encIdx is the cursor on that
	// step, 0 for "no" (the default) and 1 for "yes".
	hasTPM bool
	encIdx int
	// recoveryFirst is the recovery passphrase as first typed, until the
	// second entry matches it.
	recoveryFirst string
}

// newWizard is the wizard with the built-in keyboard and timezone lists and
// no console side effects. runWizard uses newWizardWith and the image's lists.
func newWizard(disks []Disk) wizardModel {
	return newWizardWith(disks, curatedKeyboards, curatedTimezones, defaultTimezone)
}

func newWizardWith(disks []Disk, keyboards []Keyboard, zones []string, tz string) wizardModel {
	m := wizardModel{
		step:        stepKeyboard,
		disks:       disks,
		keyboards:   keyboards,
		zones:       zones,
		applyKeymap: func(string) {},
	}
	m.choice.KeyboardLayout = defaultLayout
	m.choice.Timezone = defaultTimezone
	if validateTimezone(tz) == nil && indexOf(zones, tz) >= 0 {
		m.choice.Timezone = tz
	}
	kl := make([]string, len(keyboards))
	for i, k := range keyboards {
		kl[i] = k.Description + "  " + k.Code()
	}
	m.kbPick = newPicker(kl)
	m.tzPick = newPicker(zones)
	m.in = textinput.New()
	m.enter(stepKeyboard)
	return m
}

func indexOf(list []string, v string) int {
	for i, s := range list {
		if s == v {
			return i
		}
	}
	return -1
}

func (m *wizardModel) keyboardIndex() int {
	for i, k := range m.keyboards {
		if k.Layout == m.choice.KeyboardLayout && k.Variant == m.choice.KeyboardVariant {
			return i
		}
	}
	return -1
}

func (m *wizardModel) isList() bool { return m.step == stepKeyboard || m.step == stepTimezone }

// enter re-seeds the input for step s from already-collected state, so values
// survive back-navigation instead of being silently cleared.
func (m *wizardModel) enter(s wizardStep) {
	m.step = s
	m.err = ""
	m.in.EchoMode = textinput.EchoNormal
	m.in.Prompt = "> "
	switch s {
	case stepKeyboard:
		// The list steps use the input as a search filter, empty on entry,
		// with the cursor on the current choice.
		m.in.Prompt = "search: "
		m.in.SetValue("")
		m.kbPick.filter("")
		m.kbPick.selectIndex(m.keyboardIndex())
	case stepTimezone:
		m.in.Prompt = "search: "
		m.in.SetValue("")
		m.tzPick.filter("")
		m.tzPick.selectIndex(indexOf(m.zones, m.choice.Timezone))
	case stepHostname:
		m.in.SetValue(orDefault(m.choice.Hostname, defaultHostname))
	case stepUser:
		m.in.SetValue(m.choice.Username)
	case stepPass:
		m.in.EchoMode = textinput.EchoPassword
		m.in.SetValue(m.choice.Password)
	case stepGithub:
		m.in.SetValue(m.choice.Github)
	case stepEncrypt:
		m.encIdx = 0
		if m.choice.Encrypt {
			m.encIdx = 1
		}
	case stepRecovery:
		m.in.EchoMode = textinput.EchoPassword
		m.in.SetValue(m.choice.Recovery)
	case stepRecoveryConfirm:
		m.in.EchoMode = textinput.EchoPassword
		m.in.SetValue("")
	}
	m.in.Focus()
	m.in.CursorEnd()
}

// installableDisks is ListDisks for the wizard's purposes. An unreadable
// sysBlock and an empty one mean the same thing to the user — there is nothing
// to install onto — and the wizard already has a clear screen for that case, so
// the error becomes an empty list rather than a bare syscall message printed
// over the branded UI.
func installableDisks(sysBlock string) []Disk {
	disks, err := ListDisks(sysBlock)
	if err != nil {
		return nil
	}
	return disks
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
		if m.step > stepKeyboard {
			m.enter(m.prevStep())
		}
		return m, nil
	}

	if m.isList() {
		return m.updateList(key)
	}
	if m.step == stepDisk {
		return m.updateDisk(key)
	}
	if m.step == stepEncrypt {
		return m.updateEncrypt(key)
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

// prevStep is where esc goes: the step before, skipping the encryption
// steps that were not shown.
func (m wizardModel) prevStep() wizardStep {
	switch m.step {
	case stepReview:
		switch {
		case m.choice.Encrypt:
			return stepRecoveryConfirm
		case m.hasTPM:
			return stepEncrypt
		}
		return stepDisk
	case stepEncrypt:
		return stepDisk
	}
	return m.step - 1
}

func (m wizardModel) commitField() (tea.Model, tea.Cmd) {
	v := strings.TrimSpace(m.in.Value())
	switch m.step {
	case stepRecovery:
		// Not trimmed: the passphrase is used byte for byte, and
		// validateRecovery refuses leading and trailing spaces instead.
		raw := m.in.Value()
		if err := validateRecovery(raw); err != nil {
			m.err = err.Error()
			return m, nil
		}
		m.recoveryFirst = raw
		m.enter(stepRecoveryConfirm)
	case stepRecoveryConfirm:
		if m.in.Value() != m.recoveryFirst {
			m.in.SetValue("")
			m.err = "the passphrases do not match: type it again, or esc to start over"
			return m, nil
		}
		m.choice.Recovery = m.recoveryFirst
		m.recoveryFirst = ""
		m.enter(stepReview)
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
		// Optional, but not arbitrary: an empty value skips key import, a
		// non-empty one must be a real handle before it can reach the renderer.
		if err := validateGithub(v); err != nil {
			m.err = err.Error()
			return m, nil
		}
		m.choice.Github = v
		if len(m.disks) == 0 {
			m.err = "no installable disk found"
			return m, nil
		}
		m.enter(stepDisk)
	}
	return m, nil
}

// updateList drives the keyboard and timezone steps: arrows move, enter
// picks the entry under the cursor, anything else edits the search filter.
func (m wizardModel) updateList(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := &m.kbPick
	if m.step == stepTimezone {
		p = &m.tzPick
	}
	switch key.String() {
	case "up":
		p.move(-1)
		return m, nil
	case "down":
		p.move(1)
		return m, nil
	case "pgup":
		p.move(-pickerRows)
		return m, nil
	case "pgdown":
		p.move(pickerRows)
		return m, nil
	case "enter":
		i, ok := p.selected()
		if !ok {
			m.err = "nothing matches the search"
			return m, nil
		}
		if m.step == stepKeyboard {
			k := m.keyboards[i]
			m.choice.KeyboardLayout, m.choice.KeyboardVariant = k.Layout, k.Variant
			m.choice.ConsoleKeymap = consoleKeymap(m.keymapsDir, k.Layout, k.Variant)
			m.applyKeymap(m.choice.ConsoleKeymap)
			m.enter(stepTimezone)
		} else {
			m.choice.Timezone = m.zones[i]
			m.enter(stepHostname)
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.in, cmd = m.in.Update(key)
	p.filter(m.in.Value())
	m.err = ""
	return m, cmd
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
		if m.hasTPM {
			m.enter(stepEncrypt)
		} else {
			m.enter(stepReview)
		}
	}
	return m, nil
}

func (m wizardModel) updateEncrypt(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "up", "k":
		m.encIdx = 0
	case "down", "j":
		m.encIdx = 1
	case "enter":
		if m.encIdx == 1 {
			m.choice.Encrypt = true
			m.enter(stepRecovery)
		} else {
			m.choice.Encrypt = false
			m.choice.Recovery = ""
			m.enter(stepReview)
		}
	}
	return m, nil
}

// encryptText is shown on the encryption step. It says what the TPM-held key
// protects and what it does not; see the design note for the reasoning.
var encryptText = []string{
	"Encrypt the partition that holds your files (/home) and /var.",
	"The key is kept in this computer's TPM chip, so the disk unlocks",
	"by itself at boot.",
	"",
	"Protects: your files when the disk is taken out of this computer.",
	"Does not protect: someone who has the whole computer. The key is not",
	"tied to the system that boots, so a person who starts another system",
	"on it (from a USB stick, say) can likely read the key from the TPM.",
	"",
	"You also set a recovery passphrase. If the TPM is cleared or reset,",
	"or the mainboard is replaced, it is the only way back to your files.",
	"Write it down and keep it away from the computer.",
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
	case stepKeyboard, stepTimezone:
		p, labels := m.kbPick, m.kbPick.labels
		if m.step == stepTimezone {
			p, labels = m.tzPick, m.tzPick.labels
		}
		b.WriteString("  " + prompt(m.step) + "\n")
		b.WriteString("  " + m.in.View() + "\n\n")
		from, to := p.window()
		for pos := from; pos < to; pos++ {
			line := labels[p.match[pos]]
			if pos == p.cursor {
				b.WriteString("  " + tnAcc.Render("> "+line) + "\n")
			} else {
				b.WriteString("    " + tnDim.Render(line) + "\n")
			}
		}
		if len(p.match) == 0 {
			b.WriteString("    " + tnDim.Render("no match") + "\n")
		}
		if m.err != "" {
			b.WriteString("\n  " + haltRed.Render(m.err) + "\n")
		}
		hint := "type to search · up/down select · enter confirm"
		if m.step > stepKeyboard {
			hint += " · esc back"
		}
		b.WriteString("\n  " + tnDim.Render(hint) + "\n")

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

	case stepEncrypt:
		b.WriteString("  " + tnHot.Render("Encrypt your files?") + "\n\n")
		for _, l := range encryptText {
			b.WriteString("  " + l + "\n")
		}
		b.WriteString("\n")
		for i, l := range []string{"no, do not encrypt", "yes, encrypt my files"} {
			if i == m.encIdx {
				b.WriteString("  " + tnAcc.Render("> "+l) + "\n")
			} else {
				b.WriteString("    " + tnDim.Render(l) + "\n")
			}
		}
		b.WriteString("\n  " + tnDim.Render("up/down select · enter confirm · esc back") + "\n")

	case stepReview:
		b.WriteString("  " + haltRed.Render("this ERASES "+m.choice.Disk+" and installs hadron-desktop") + "\n\n")
		b.WriteString("    keyboard   " + tnHot.Render(keyboardLabel(m.choice)) + "\n")
		b.WriteString("    timezone   " + tnHot.Render(m.choice.Timezone) + "\n")
		b.WriteString("    hostname   " + tnHot.Render(m.choice.Hostname) + "\n")
		b.WriteString("    username   " + tnHot.Render(m.choice.Username) + "\n")
		b.WriteString("    ssh keys   " + tnHot.Render(orDefault(githubLabel(m.choice.Github), "none")) + "\n")
		b.WriteString("    disk       " + haltRed.Render(m.choice.Disk) + "\n")
		if m.hasTPM {
			enc := "off"
			if m.choice.Encrypt {
				enc = "on (TPM key and recovery passphrase)"
			}
			b.WriteString("    encrypt    " + tnHot.Render(enc) + "\n")
		}
		b.WriteString("\n  " + tnDim.Render("enter to INSTALL · esc back") + "\n")

	default:
		b.WriteString("  " + prompt(m.step) + "\n")
		b.WriteString("  " + m.in.View() + "\n")
		if m.step == stepRecovery {
			if h := passphraseStrength(m.in.Value()); h != "" {
				b.WriteString("  " + tnDim.Render("strength: "+h) + "\n")
			}
		}
		if m.err != "" {
			b.WriteString("\n  " + haltRed.Render(m.err) + "\n")
		}
		b.WriteString("\n  " + tnDim.Render("enter continue · esc back") + "\n")
	}
	return b.String()
}

func keyboardLabel(c Choice) string {
	return Keyboard{Layout: c.KeyboardLayout, Variant: c.KeyboardVariant}.Code()
}

func githubLabel(g string) string {
	if g == "" {
		return ""
	}
	return "github:" + g
}

func prompt(s wizardStep) string {
	switch s {
	case stepKeyboard:
		return tnDim.Render("keyboard layout")
	case stepTimezone:
		return tnDim.Render("timezone")
	case stepHostname:
		return tnDim.Render("hostname for this machine")
	case stepUser:
		return tnDim.Render("username for the desktop account")
	case stepPass:
		return tnDim.Render("password for that account")
	case stepGithub:
		return tnDim.Render("GitHub username to import SSH keys from (blank to skip)")
	case stepRecovery:
		return tnDim.Render(fmt.Sprintf("recovery passphrase, %d characters or more (a few words work well)", minRecoveryLen))
	case stepRecoveryConfirm:
		return tnDim.Render("type the recovery passphrase again")
	}
	return ""
}

// --- entry point -----------------------------------------------------------

// runWizard collects the install configuration and writes it to /oem for
// `kairos-agent install` to merge. It is a no-op when a config is already present,
// preserving the shell installer's unattended behaviour for seeded installs.
// It returns what was chosen, or nil when it wrote nothing; the caller needs
// the choice to add the recovery passphrase after the install.
func runWizard() (*Choice, error) {
	out := userConfigPath()

	present, err := ConfigPresent(oemDir(), out)
	if err != nil {
		return nil, err
	}
	if present {
		// Someone already supplied a users/install config: install unattended.
		return nil, nil
	}

	c, nonInteractive, err := choiceFromEnv()
	if err != nil {
		return nil, err
	}
	hasTPM := tpmPresent(tpmDevice, "/sys/class/tpm")

	if nonInteractive {
		if err := zoneExists(zoneinfoDir, c.Timezone); err != nil {
			return nil, fmt.Errorf("HADRON_TIMEZONE: %w", err)
		}
		if c.Encrypt && !hasTPM {
			return nil, fmt.Errorf("HADRON_ENCRYPT: no TPM 2.0 (%s) on this machine", tpmDevice)
		}
		c.ConsoleKeymap = consoleKeymap(keymapsDir, c.KeyboardLayout, c.KeyboardVariant)
	} else {
		w := newWizardWith(installableDisks("/sys/block"),
			loadKeyboards(xkbRulesList), loadTimezones(zoneinfoDir), detectTimezone("/etc/localtime"))
		w.keymapsDir = keymapsDir
		w.applyKeymap = loadConsoleKeymap
		w.hasTPM = hasTPM
		final, err := tea.NewProgram(w, tea.WithAltScreen()).Run()
		if err != nil {
			return nil, err
		}
		wm, ok := final.(wizardModel)
		if !ok || wm.aborted || !wm.done {
			return nil, errors.New("install cancelled")
		}
		c = wm.choice
	}

	hash, err := HashPassword(c.Password)
	if err != nil {
		return nil, err
	}
	if err := writeChoice(out, c, hash); err != nil {
		return nil, err
	}
	return &c, nil
}
