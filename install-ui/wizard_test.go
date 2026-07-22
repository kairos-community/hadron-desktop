package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestChoiceFromEnvRequiresMandatoryFields(t *testing.T) {
	t.Setenv("HADRON_INSTALL_NONINTERACTIVE", "1")
	t.Setenv("HADRON_USER", "")
	t.Setenv("HADRON_PASS", "")
	t.Setenv("HADRON_DISK", "")
	if _, _, err := choiceFromEnv(); err == nil {
		t.Fatal("expected an error when HADRON_USER/PASS/DISK are unset")
	}
}

// TestChoiceFromEnvRequiresEachFieldIndividually: checking only the all-empty
// case cannot tell that all three are enforced — with two checks in place the
// third could be dropped and the suite would stay green. An unenforced
// HADRON_DISK in particular would install to device "".
func TestChoiceFromEnvRequiresEachFieldIndividually(t *testing.T) {
	full := map[string]string{
		"HADRON_USER": "ada",
		"HADRON_PASS": "hunter2",
		"HADRON_DISK": "/dev/vda",
	}
	for blank := range full {
		t.Run(blank, func(t *testing.T) {
			t.Setenv("HADRON_INSTALL_NONINTERACTIVE", "1")
			for k, v := range full {
				if k == blank {
					v = ""
				}
				t.Setenv(k, v)
			}
			_, _, err := choiceFromEnv()
			if err == nil {
				t.Fatalf("expected an error when %s is unset", blank)
			}
			if !strings.Contains(err.Error(), blank) {
				t.Errorf("error %q does not name the missing variable %s", err, blank)
			}
		})
	}
}

func TestChoiceFromEnvPopulatesAndDefaultsHostname(t *testing.T) {
	t.Setenv("HADRON_INSTALL_NONINTERACTIVE", "1")
	t.Setenv("HADRON_USER", "ada")
	t.Setenv("HADRON_PASS", "hunter2")
	t.Setenv("HADRON_DISK", "/dev/vda")
	t.Setenv("HADRON_HOSTNAME", "")
	t.Setenv("HADRON_GITHUB", "adalovelace")

	c, ok, err := choiceFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected non-interactive mode to be active")
	}
	if c.Username != "ada" || c.Password != "hunter2" || c.Disk != "/dev/vda" {
		t.Errorf("unexpected choice: %+v", c)
	}
	if c.Hostname != "hadron" {
		t.Errorf("Hostname = %q, want default %q", c.Hostname, "hadron")
	}
	if c.Github != "adalovelace" {
		t.Errorf("Github = %q, want adalovelace", c.Github)
	}
}

func TestChoiceFromEnvInactiveWhenFlagUnset(t *testing.T) {
	t.Setenv("HADRON_INSTALL_NONINTERACTIVE", "0")
	if _, ok, _ := choiceFromEnv(); ok {
		t.Error("non-interactive mode should be inactive when the flag is not 1")
	}
}

// TestChoiceFromEnvRejectsInvalidUsername guards the one interpolation
// RenderCloudConfig cannot defend: Username goes raw into the /etc/ly/save.ini
// block scalar, so a newline there terminates the YAML block and yields an
// invalid cloud-config on a machine that has already been wiped.
func TestChoiceFromEnvRejectsInvalidUsername(t *testing.T) {
	t.Setenv("HADRON_INSTALL_NONINTERACTIVE", "1")
	t.Setenv("HADRON_PASS", "hunter2")
	t.Setenv("HADRON_DISK", "/dev/vda")
	t.Setenv("HADRON_USER", "ada\nsession_index = 9")
	if _, _, err := choiceFromEnv(); err == nil {
		t.Fatal("expected an error for a username containing a newline")
	}
}

func TestWriteChoiceProducesReadableConfig(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "99_hadron-user.yaml")
	c := Choice{Hostname: "box", Username: "ada", Disk: "/dev/vda"}

	if err := writeChoice(out, c, "$6$fake"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hostname: \"box\"") {
		t.Errorf("config missing hostname:\n%s", data)
	}
	// The file carries a password hash; it must not be world-readable.
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("permissions = %o, want no group/other access", perm)
	}
}

// TestWriteChoiceMatchesRenderCloudConfig pins that writeChoice writes the
// renderer's bytes unmodified — the shell installer's output shape is verified
// against RenderCloudConfig in config_test.go, and this is the only step
// between it and the disk.
func TestWriteChoiceMatchesRenderCloudConfig(t *testing.T) {
	out := filepath.Join(t.TempDir(), "nested", "99_hadron-user.yaml")
	c := Choice{Hostname: "box", Username: "ada", Github: "adalovelace", Disk: "/dev/vda"}

	if err := writeChoice(out, c, "$6$fake"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := RenderCloudConfig(c, "$6$fake"); string(data) != want {
		t.Errorf("written config differs from RenderCloudConfig\n got:\n%s\nwant:\n%s", data, want)
	}
}

// --- wizard flow -----------------------------------------------------------

// keys drives a wizard through a sequence of keystrokes. Runes are typed one
// character at a time; anything else is a named key ("enter", "esc", "down").
func keys(t *testing.T, m wizardModel, seq ...string) wizardModel {
	t.Helper()
	var tm tea.Model = m
	for _, s := range seq {
		var msg tea.KeyMsg
		switch s {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "ctrl+c":
			msg = tea.KeyMsg{Type: tea.KeyCtrlC}
		default:
			for _, r := range s {
				tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			}
			continue
		}
		tm, _ = tm.Update(msg)
	}
	return tm.(wizardModel)
}

func testDisks() []Disk {
	return []Disk{{Path: "/dev/sda", SizeGB: 120}, {Path: "/dev/vdb", SizeGB: 500}}
}

// clear replaces whatever the step seeded with the given text.
func clear(t *testing.T, m wizardModel, text string) wizardModel {
	t.Helper()
	m.in.SetValue(text)
	return m
}

func TestWizardHappyPathCollectsEveryField(t *testing.T) {
	m := clear(t, newWizard(testDisks()), "")
	m = keys(t, m, "box", "enter", "ada", "enter", "hunter2", "enter", "adalovelace", "enter")

	if m.step != stepDisk {
		t.Fatalf("step = %v, want stepDisk", m.step)
	}
	m = keys(t, m, "down", "enter")
	if m.step != stepReview {
		t.Fatalf("step = %v, want stepReview", m.step)
	}
	want := Choice{Hostname: "box", Username: "ada", Password: "hunter2", Github: "adalovelace", Disk: "/dev/vdb"}
	if m.choice != want {
		t.Errorf("choice = %+v, want %+v", m.choice, want)
	}
	if m.done {
		t.Error("wizard reported done before the review gate was confirmed")
	}

	m = keys(t, m, "enter")
	if !m.done {
		t.Error("enter on the review screen should finish the wizard")
	}
}

// TestWizardHostnameDefaults pins that an emptied hostname field falls back to
// the shell installer's default rather than writing an empty hostname.
func TestWizardHostnameDefaults(t *testing.T) {
	m := clear(t, newWizard(testDisks()), "")
	m = keys(t, m, "enter")
	if m.choice.Hostname != defaultHostname {
		t.Errorf("Hostname = %q, want %q", m.choice.Hostname, defaultHostname)
	}
}

// TestWizardEscRestoresPreviousValue is the whole point of back-navigation: a
// user who mistypes their username and goes back must find their hostname
// still there, not a blank field.
func TestWizardEscRestoresPreviousValue(t *testing.T) {
	m := clear(t, newWizard(testDisks()), "")
	m = keys(t, m, "box", "enter", "ada", "enter") // now on password
	m = keys(t, m, "esc")                          // back to username
	if m.step != stepUser {
		t.Fatalf("step = %v, want stepUser", m.step)
	}
	if got := m.in.Value(); got != "ada" {
		t.Errorf("username field = %q, want %q", got, "ada")
	}
	m = keys(t, m, "esc") // back to hostname
	if m.step != stepHostname {
		t.Fatalf("step = %v, want stepHostname", m.step)
	}
	if got := m.in.Value(); got != "box" {
		t.Errorf("hostname field = %q, want %q", got, "box")
	}
	// Esc on the first step must not underflow past stepHostname.
	m = keys(t, m, "esc")
	if m.step != stepHostname {
		t.Errorf("step = %v after esc on the first step, want stepHostname", m.step)
	}
}

func TestWizardEscFromReviewGoesBackToDisk(t *testing.T) {
	m := clear(t, newWizard(testDisks()), "")
	m = keys(t, m, "box", "enter", "ada", "enter", "pw", "enter", "enter", "enter")
	if m.step != stepReview {
		t.Fatalf("step = %v, want stepReview", m.step)
	}
	m = keys(t, m, "esc")
	if m.step != stepDisk {
		t.Errorf("step = %v, want stepDisk", m.step)
	}
	if m.done {
		t.Error("esc from review must not finish the wizard")
	}
}

func TestWizardPasswordIsMasked(t *testing.T) {
	m := clear(t, newWizard(testDisks()), "")
	m = keys(t, m, "enter", "ada", "enter", "hunter2")
	if m.step != stepPass {
		t.Fatalf("step = %v, want stepPass", m.step)
	}
	if strings.Contains(m.View(), "hunter2") {
		t.Errorf("password is echoed in plaintext:\n%s", m.View())
	}
	if m.in.Value() != "hunter2" {
		t.Errorf("password value = %q, want hunter2", m.in.Value())
	}
}

func TestWizardUsernameValidation(t *testing.T) {
	cases := []struct {
		name  string
		input string
		ok    bool
	}{
		{"plain", "ada", true},
		{"digits and dash", "ada-l0velace", true},
		{"leading underscore", "_ada", true},
		{"empty", "", false},
		{"newline breaks the save.ini block scalar", "ada\nsession_index = 9", false},
		{"space", "ada lovelace", false},
		{"uppercase", "Ada", false},
		{"leading digit", "1ada", false},
		{"colon", "ada:x", false},
		{"too long", strings.Repeat("a", 33), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := clear(t, newWizard(testDisks()), "")
			m = keys(t, m, "enter") // past hostname, now on username
			m = clear(t, m, tc.input)
			m = keys(t, m, "enter")

			if tc.ok {
				if m.step != stepPass {
					t.Fatalf("step = %v, want stepPass (input %q rejected: %q)", m.step, tc.input, m.err)
				}
				if m.choice.Username != tc.input {
					t.Errorf("Username = %q, want %q", m.choice.Username, tc.input)
				}
				return
			}
			if m.step != stepUser {
				t.Fatalf("step = %v, want to stay on stepUser for input %q", m.step, tc.input)
			}
			if m.err == "" {
				t.Errorf("no on-screen error shown for invalid username %q", tc.input)
			}
			if m.choice.Username != "" {
				t.Errorf("invalid username %q was stored anyway", m.choice.Username)
			}
			if !strings.Contains(m.View(), m.err) {
				t.Errorf("error %q is not rendered in the view:\n%s", m.err, m.View())
			}
		})
	}
}

func TestWizardPasswordIsRequired(t *testing.T) {
	m := clear(t, newWizard(testDisks()), "")
	m = keys(t, m, "enter", "ada", "enter")
	m = clear(t, m, "")
	m = keys(t, m, "enter")
	if m.step != stepPass {
		t.Fatalf("step = %v, want to stay on stepPass", m.step)
	}
	if m.err == "" {
		t.Error("no on-screen error shown for an empty password")
	}
}

func TestWizardGithubIsOptional(t *testing.T) {
	m := clear(t, newWizard(testDisks()), "")
	m = keys(t, m, "enter", "ada", "enter", "pw", "enter")
	if m.step != stepGithub {
		t.Fatalf("step = %v, want stepGithub", m.step)
	}
	m = keys(t, m, "enter")
	if m.step != stepDisk {
		t.Fatalf("step = %v, want stepDisk after skipping github", m.step)
	}
	if m.choice.Github != "" {
		t.Errorf("Github = %q, want empty", m.choice.Github)
	}
}

// TestWizardNoDisksHalts: with nothing installable there is no safe next step,
// so the wizard must say so rather than advance to an empty picker whose enter
// key would index an empty slice.
func TestWizardNoDisksHalts(t *testing.T) {
	m := clear(t, newWizard(nil), "")
	m = keys(t, m, "enter", "ada", "enter", "pw", "enter", "enter")
	if m.step != stepGithub {
		t.Fatalf("step = %v, want to stay on stepGithub when no disk exists", m.step)
	}
	if m.err == "" {
		t.Error("no on-screen error shown when no disk is installable")
	}
}

func TestWizardDiskSelectionClampsAtBothEnds(t *testing.T) {
	m := clear(t, newWizard(testDisks()), "")
	m = keys(t, m, "enter", "ada", "enter", "pw", "enter", "enter")
	if m.step != stepDisk {
		t.Fatalf("step = %v, want stepDisk", m.step)
	}
	m = keys(t, m, "up", "up") // already at the top
	if m.diskIdx != 0 {
		t.Errorf("diskIdx = %d after up at the top, want 0", m.diskIdx)
	}
	m = keys(t, m, "down", "down", "down") // only two disks
	if m.diskIdx != 1 {
		t.Errorf("diskIdx = %d after running off the bottom, want 1", m.diskIdx)
	}
	m = keys(t, m, "enter")
	if m.choice.Disk != "/dev/vdb" {
		t.Errorf("Disk = %q, want /dev/vdb", m.choice.Disk)
	}
}

// TestWizardReviewShowsTheTarget: the review gate is the last point before a
// disk is erased, so it must name the disk that is about to go.
func TestWizardReviewShowsTheTarget(t *testing.T) {
	m := clear(t, newWizard(testDisks()), "")
	m = keys(t, m, "box", "enter", "ada", "enter", "pw", "enter", "adalovelace", "enter", "enter")
	view := m.View()
	for _, want := range []string{"/dev/sda", "box", "ada", "github:adalovelace", "ERASES"} {
		if !strings.Contains(view, want) {
			t.Errorf("review view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "pw") {
		t.Errorf("review view leaks the password:\n%s", view)
	}
	// The target must appear in the labelled summary row, not only inside the
	// warning sentence — the summary is what a user scans before committing.
	var found bool
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "disk") && strings.Contains(line, "/dev/sda") {
			found = true
		}
	}
	if !found {
		t.Errorf("review view has no 'disk' summary row naming /dev/sda:\n%s", view)
	}
}

// TestWizardReviewOnlyConfirmsOnEnter: the review screen is the last gate
// before a disk is erased, so only the documented key may pass it. A stray
// keypress — someone leaning on the keyboard, a VT emitting junk — must not
// start an irreversible install.
func TestWizardReviewOnlyConfirmsOnEnter(t *testing.T) {
	base := clear(t, newWizard(testDisks()), "")
	base = keys(t, base, "box", "enter", "ada", "enter", "pw", "enter", "adalovelace", "enter", "enter")
	if base.step != stepReview {
		t.Fatalf("step = %v, want stepReview", base.step)
	}
	for _, k := range []string{"x", "y", "down", "up"} {
		if m := keys(t, base, k); m.done {
			t.Errorf("key %q confirmed the review gate; only enter may", k)
		}
	}
	if !keys(t, base, "enter").done {
		t.Error("enter did not confirm the review gate")
	}
}

func TestWizardCtrlCAborts(t *testing.T) {
	m := clear(t, newWizard(testDisks()), "")
	m = keys(t, m, "ctrl+c")
	if !m.aborted {
		t.Error("ctrl+c should abort the wizard")
	}
	if m.done {
		t.Error("an aborted wizard must not report done")
	}
}

// --- runWizard end to end --------------------------------------------------

func TestRunWizardNonInteractiveWritesConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HADRON_OEM_DIR", dir)
	t.Setenv("HADRON_INSTALL_NONINTERACTIVE", "1")
	t.Setenv("HADRON_USER", "ada")
	t.Setenv("HADRON_PASS", "hunter2")
	t.Setenv("HADRON_DISK", "/dev/vda")
	t.Setenv("HADRON_HOSTNAME", "box")
	t.Setenv("HADRON_GITHUB", "adalovelace")

	if err := runWizard(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "99_hadron-user.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"#cloud-config", "hostname: \"box\"", "name: \"ada\"", "$6$", "github:adalovelace", "/etc/ly/save.ini"} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "hunter2") {
		t.Errorf("plaintext password reached disk:\n%s", got)
	}
}

// TestRunWizardSkipsWhenConfigPresent preserves the shell installer's unattended
// behaviour: a seeded /oem must install without prompting. If this regressed the
// wizard would block a seeded install forever on a headless machine.
func TestRunWizardSkipsWhenConfigPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "10_seed.yaml"), []byte("users:\n  - name: seeded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HADRON_OEM_DIR", dir)
	// Non-interactive mode is deliberately NOT set: if the short-circuit failed,
	// runWizard would try to open a terminal here rather than return nil.
	t.Setenv("HADRON_INSTALL_NONINTERACTIVE", "")

	if err := runWizard(); err != nil {
		t.Fatalf("seeded /oem should install unattended, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "99_hadron-user.yaml")); !os.IsNotExist(err) {
		t.Errorf("wizard wrote its own config despite a seeded one being present (stat err: %v)", err)
	}
}

func TestRunWizardReportsMissingEnv(t *testing.T) {
	t.Setenv("HADRON_OEM_DIR", t.TempDir())
	t.Setenv("HADRON_INSTALL_NONINTERACTIVE", "1")
	t.Setenv("HADRON_USER", "")
	t.Setenv("HADRON_PASS", "")
	t.Setenv("HADRON_DISK", "")
	err := runWizard()
	if err == nil {
		t.Fatal("expected an error when the non-interactive env is incomplete")
	}
	if !strings.Contains(err.Error(), "HADRON_USER") {
		t.Errorf("error %q does not name the missing variable", err)
	}
}

func TestOemDirDefaultsToOem(t *testing.T) {
	t.Setenv("HADRON_OEM_DIR", "")
	if got := oemDir(); got != "/oem" {
		t.Errorf("oemDir() = %q, want /oem", got)
	}
	t.Setenv("HADRON_OEM_DIR", "/tmp/fake-oem")
	if got := oemDir(); got != "/tmp/fake-oem" {
		t.Errorf("oemDir() = %q, want /tmp/fake-oem", got)
	}
}
