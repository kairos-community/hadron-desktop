package main

import (
	"strings"
	"testing"
)

// --- keyboard and timezone steps ---------------------------------------------

func TestWizardStartsOnKeyboardWithDefaults(t *testing.T) {
	m := newWizard(testDisks())
	if m.step != stepKeyboard {
		t.Fatalf("step = %v, want stepKeyboard first, so the password is typed with the chosen layout", m.step)
	}
	if !strings.Contains(m.View(), "> English (US)  us") {
		t.Errorf("cursor is not on the default layout:\n%s", m.View())
	}
	m = keys(t, m, "enter")
	if m.choice.KeyboardLayout != "us" || m.choice.KeyboardVariant != "" {
		t.Errorf("keyboard = %q/%q, want us with no variant", m.choice.KeyboardLayout, m.choice.KeyboardVariant)
	}
	if m.step != stepTimezone {
		t.Fatalf("step = %v, want stepTimezone", m.step)
	}
	m = keys(t, m, "enter")
	if m.choice.Timezone != defaultTimezone {
		t.Errorf("timezone = %q, want %q", m.choice.Timezone, defaultTimezone)
	}
}

func TestWizardSearchesAndPicksKeyboardAndTimezone(t *testing.T) {
	dir := t.TempDir()
	writeZone(t, dir, "i386/qwertz/de-latin1-nodeadkeys.map.gz")
	m := newWizard(testDisks())
	m.keymapsDir = dir
	var loaded []string
	m.applyKeymap = func(k string) { loaded = append(loaded, k) }

	m = keys(t, m, "german dead", "enter")
	if m.choice.KeyboardLayout != "de" || m.choice.KeyboardVariant != "nodeadkeys" {
		t.Fatalf("keyboard = %q/%q, want de/nodeadkeys", m.choice.KeyboardLayout, m.choice.KeyboardVariant)
	}
	if m.choice.ConsoleKeymap != "de-latin1-nodeadkeys" {
		t.Errorf("console keymap = %q, want de-latin1-nodeadkeys", m.choice.ConsoleKeymap)
	}
	if len(loaded) != 1 || loaded[0] != "de-latin1-nodeadkeys" {
		t.Errorf("live console keymap loads = %v, want [de-latin1-nodeadkeys]", loaded)
	}

	m = keys(t, m, "rome", "enter")
	if m.choice.Timezone != "Europe/Rome" {
		t.Fatalf("timezone = %q, want Europe/Rome", m.choice.Timezone)
	}
	if m.step != stepHostname {
		t.Fatalf("step = %v, want stepHostname", m.step)
	}

	// Going back finds the cursor on the earlier choices, with the filter
	// cleared.
	m = keys(t, m, "esc")
	if !strings.Contains(m.View(), "> Europe/Rome") {
		t.Errorf("timezone cursor lost after going back:\n%s", m.View())
	}
	m = keys(t, m, "esc")
	if !strings.Contains(m.View(), "> German (no dead keys)  de(nodeadkeys)") {
		t.Errorf("keyboard cursor lost after going back:\n%s", m.View())
	}

	m = keys(t, m, "enter", "enter")
	m = clear(t, m, "")
	m = keys(t, m, "box", "enter", "ada", "enter", "pw", "enter", "enter", "enter", "enter")
	if m.step != stepReview {
		t.Fatalf("step = %v, want stepReview", m.step)
	}
	for _, want := range []string{"de(nodeadkeys)", "Europe/Rome"} {
		if !strings.Contains(m.View(), want) {
			t.Errorf("review does not show %q:\n%s", want, m.View())
		}
	}
}

func TestWizardListStepRefusesAnEmptyMatch(t *testing.T) {
	m := keys(t, newWizard(testDisks()), "no such layout", "enter")
	if m.step != stepKeyboard {
		t.Fatalf("step = %v, want to stay on stepKeyboard", m.step)
	}
	if !strings.Contains(m.View(), "nothing matches") {
		t.Errorf("no error shown for an empty match:\n%s", m.View())
	}
}

func TestWizardListStepMovesWithArrows(t *testing.T) {
	m := keys(t, newWizard(testDisks()), "down", "enter")
	want := curatedKeyboards[1]
	if m.choice.KeyboardLayout != want.Layout || m.choice.KeyboardVariant != want.Variant {
		t.Errorf("keyboard = %q/%q, want %+v", m.choice.KeyboardLayout, m.choice.KeyboardVariant, want)
	}
}

// A detected zone that the list does not have must not become the choice:
// the user would see the cursor on UTC and get something else.
func TestNewWizardWithIgnoresAnUnlistedDetectedZone(t *testing.T) {
	m := newWizardWith(testDisks(), curatedKeyboards, []string{"UTC", "Europe/Rome"}, "Mars/Olympus")
	if m.choice.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", m.choice.Timezone)
	}
	m = newWizardWith(testDisks(), curatedKeyboards, []string{"UTC", "Europe/Rome"}, "Europe/Rome")
	if m.choice.Timezone != "Europe/Rome" {
		t.Errorf("timezone = %q, want the detected Europe/Rome", m.choice.Timezone)
	}
}

func TestWizardListStepsFitTheConsole(t *testing.T) {
	m := newWizardWith(testDisks(), curatedKeyboards, curatedTimezones, "UTC")
	for _, s := range []wizardStep{stepKeyboard, stepTimezone} {
		if m.step != s {
			t.Fatalf("step = %v, want %v", m.step, s)
		}
		if n := len(strings.Split(m.View(), "\n")); n > 25 {
			t.Errorf("%s view is %d lines, more than a 25-row console", stepName[s], n)
		}
		m = keys(t, m, "enter")
	}
}

func TestChoiceFromEnvLocaleDefaultsAndValidation(t *testing.T) {
	t.Setenv("HADRON_INSTALL_NONINTERACTIVE", "1")
	t.Setenv("HADRON_USER", "ada")
	t.Setenv("HADRON_PASS", "hunter2")
	t.Setenv("HADRON_DISK", "/dev/vda")
	t.Setenv("HADRON_KEYBOARD_LAYOUT", "")
	t.Setenv("HADRON_KEYBOARD_VARIANT", "")
	t.Setenv("HADRON_TIMEZONE", "")
	c, _, err := choiceFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.KeyboardLayout != "us" || c.KeyboardVariant != "" || c.Timezone != "UTC" {
		t.Errorf("defaults = %q/%q/%q, want us, no variant, UTC", c.KeyboardLayout, c.KeyboardVariant, c.Timezone)
	}

	t.Setenv("HADRON_KEYBOARD_LAYOUT", "de")
	t.Setenv("HADRON_KEYBOARD_VARIANT", "nodeadkeys")
	t.Setenv("HADRON_TIMEZONE", "Europe/Berlin")
	c, _, err = choiceFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.KeyboardLayout != "de" || c.KeyboardVariant != "nodeadkeys" || c.Timezone != "Europe/Berlin" {
		t.Errorf("choice = %+v", c)
	}

	for _, tc := range []struct{ env, value string }{
		{"HADRON_KEYBOARD_LAYOUT", "de\nstages:"},
		{"HADRON_KEYBOARD_VARIANT", "a b"},
		{"HADRON_TIMEZONE", "../../etc/shadow"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			if _, _, err := choiceFromEnv(); err == nil || !strings.Contains(err.Error(), tc.env) {
				t.Errorf("err = %v, want an error naming %s", err, tc.env)
			}
		})
	}
}

// --- rendering -----------------------------------------------------------------

func TestRenderCloudConfigWritesKeyboardAndTimezone(t *testing.T) {
	c := Choice{Hostname: "h", Username: "u", Disk: "/dev/sda",
		KeyboardLayout: "de", KeyboardVariant: "nodeadkeys", ConsoleKeymap: "de-latin1-nodeadkeys", Timezone: "Europe/Rome"}
	out := RenderCloudConfig(c, "x")
	for _, want := range []string{
		"        - path: /etc/hadron-desktop/keyboard\n",
		"            XKB_LAYOUT=de\n            XKB_VARIANT=nodeadkeys\n",
		"        - path: /etc/X11/xorg.conf.d/00-keyboard.conf\n",
		"                Option \"XkbLayout\" \"de\"\n",
		"                Option \"XkbVariant\" \"nodeadkeys\"\n",
		"        - \"ln -sfn /usr/share/zoneinfo/Europe/Rome /etc/localtime\"\n",
		"echo 'KEYMAP=de-latin1-nodeadkeys' >> /etc/vconsole.conf",
		// The initramfs, where the disk passphrase is typed, never sees that
		// vconsole.conf; it gets the keymap from the command line.
		"  grub_options:\n    extra_cmdline: \"rd.vconsole.keymap=de-latin1-nodeadkeys\"\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered config missing %q\n---\n%s", want, out)
		}
	}
	// In the initramfs stage, like save.ini, so /etc has it before
	// systemd-vconsole-setup and the display manager start.
	kb, boot := strings.Index(out, "/etc/hadron-desktop/keyboard"), strings.Index(out, "\n  boot:")
	if kb < 0 || boot < kb {
		t.Errorf("keyboard file must be written in the initramfs stage:\n%s", out)
	}
	// grub_options belongs to install:, which ends where hostname: starts.
	if g, h := strings.Index(out, "  grub_options:"), strings.Index(out, "\nhostname:"); g < 0 || h < g {
		t.Errorf("grub_options must be under install:\n%s", out)
	}
	if n := topLevelStages(out); n != 1 {
		t.Errorf("%d top-level stages: keys, want 1", n)
	}
}

// An encrypted install is the one that needs the keymap in the initramfs,
// where the passphrase is typed. grub_options must stay under install:, not
// under the kcrypt: key that follows.
func TestRenderCloudConfigEncryptedKeepsTheKeymapUnderInstall(t *testing.T) {
	c := Choice{Hostname: "h", Username: "u", Disk: "/dev/vda", Encrypt: true, Passphrase: "correct horse battery",
		KeyboardLayout: "de", ConsoleKeymap: "de-latin1", Timezone: "UTC"}
	out := RenderCloudConfig(c, "x")
	want := "install:\n  device: \"/dev/vda\"\n  auto: true\n" +
		"  grub_options:\n    extra_cmdline: \"rd.vconsole.keymap=de-latin1\"\n  reboot: false\n"
	if !strings.Contains(out, want) {
		t.Errorf("config is missing\n%s\nin\n%s", want, out)
	}
}

func TestRenderCloudConfigLocaleDefaults(t *testing.T) {
	out := RenderCloudConfig(Choice{Hostname: "h", Username: "u", Disk: "/dev/sda"}, "x")
	for _, want := range []string{
		"            XKB_LAYOUT=us\n            XKB_VARIANT=\n",
		"Option \"XkbLayout\" \"us\"\n",
		"/usr/share/zoneinfo/UTC /etc/localtime",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered config missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "XkbVariant") {
		t.Errorf("an empty variant must not be written to xorg.conf.d:\n%s", out)
	}
	if strings.Contains(out, "KEYMAP=") {
		t.Errorf("no console keymap was resolved, so vconsole.conf must not be touched:\n%s", out)
	}
	if strings.Contains(out, "grub_options") || strings.Contains(out, "vconsole.keymap") {
		t.Errorf("no console keymap was resolved, so the command line must not change:\n%s", out)
	}
}

// The renderer returns no error. A value the input steps should have refused
// must still not reach the YAML or the shell commands.
func TestRenderCloudConfigReplacesInvalidLocaleValues(t *testing.T) {
	c := Choice{Hostname: "h", Username: "u", Disk: "/dev/sda",
		KeyboardLayout: "de\nstages:\n  network:", KeyboardVariant: "x",
		ConsoleKeymap: "de'; rm -rf / #", Timezone: "../../etc/shadow"}
	out := RenderCloudConfig(c, "x")
	for _, bad := range []string{"network:", "rm -rf", "shadow"} {
		if strings.Contains(out, bad) {
			t.Errorf("invalid value %q reached the config:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "XKB_LAYOUT=us\n") || !strings.Contains(out, "zoneinfo/UTC ") {
		t.Errorf("invalid values were not replaced by the defaults:\n%s", out)
	}
	if n := topLevelStages(out); n != 1 {
		t.Errorf("%d top-level stages: keys, want 1", n)
	}
}
