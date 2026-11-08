package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testPassphrase = "correct horse battery staple"

func TestValidatePassphrase(t *testing.T) {
	for _, c := range []struct {
		in string
		ok bool
	}{
		{"", false},
		{"elevenchars", false},
		{"twelve chars", true},
		{testPassphrase, true},
		{" leading space ok?", false},
		{"trailing space    ", false},
		{"tab\tinside passphrase", false},
		{"line\nbreak passphrase", false},
		{"àèìòù àèìòù àè", true},
	} {
		err := validatePassphrase(c.in)
		if (err == nil) != c.ok {
			t.Errorf("validatePassphrase(%q) = %v, want ok=%v", c.in, err, c.ok)
		}
	}
}

func TestPassphraseStrength(t *testing.T) {
	for _, c := range []struct{ in, prefix string }{
		{"", ""},
		{"short", "too short"},
		{"aaaaaaaaaaaa", "weak"},
		{"abcdefgh1234", "fair"},
		{testPassphrase, "strong"},
	} {
		if got := passphraseStrength(c.in); !strings.HasPrefix(got, c.prefix) || (c.prefix == "" && got != "") {
			t.Errorf("passphraseStrength(%q) = %q, want prefix %q", c.in, got, c.prefix)
		}
	}
}

// --- cloud-config ----------------------------------------------------------

func TestRenderCloudConfigEncrypted(t *testing.T) {
	c := Choice{Hostname: "box", Username: "ada", Disk: "/dev/vda", Encrypt: true, Passphrase: testPassphrase,
		KeyboardLayout: "us", Timezone: "UTC"}
	out := RenderCloudConfig(c, "$6$fake")

	for _, want := range []string{
		"install:\n  device: \"/dev/vda\"\n  auto: true\n  reboot: false\n  encrypted_partitions:\n    - COS_PERSISTENT\n" +
			"kcrypt:\n  method: passphrase\n  passphrase_file: /run/hadron-install/passphrase\nhostname:",
		"        - path: /etc/hadron-desktop/encryption\n          permissions: 0644\n          content: |\n            mode=passphrase\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config is missing\n%s\nin\n%s", want, out)
		}
	}
	// The marker belongs to the initramfs stage, before the boot stage.
	if strings.Index(out, encryptionMarker) > strings.Index(out, "  boot:\n") {
		t.Errorf("marker is not in the initramfs stage:\n%s", out)
	}
	// kairos-agent copies this file to the unencrypted OEM partition.
	if strings.Contains(out, testPassphrase) || strings.Contains(out, "correct horse") {
		t.Errorf("the disk passphrase reached the cloud-config:\n%s", out)
	}
	for _, bad := range []string{"reboot: true", "mode=tpm", "recovery"} {
		if strings.Contains(out, bad) {
			t.Errorf("encrypted config contains %q:\n%s", bad, out)
		}
	}
}

func TestRenderCloudConfigUnencryptedIsUnchanged(t *testing.T) {
	c := Choice{Hostname: "box", Username: "ada", Disk: "/dev/vda", Passphrase: testPassphrase}
	out := RenderCloudConfig(c, "$6$fake")
	for _, bad := range []string{"encrypted_partitions", "kcrypt", encryptionMarker, "reboot: false", testPassphrase} {
		if strings.Contains(out, bad) {
			t.Errorf("unencrypted config contains %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "  reboot: true\n") {
		t.Errorf("unencrypted config lost reboot: true:\n%s", out)
	}
}

// writeChoice is what puts the config in /oem: the passphrase must not be in
// that file either.
func TestWriteChoiceKeepsThePassphraseOutOfOEM(t *testing.T) {
	out := filepath.Join(t.TempDir(), "99_hadron-user.yaml")
	c := Choice{Hostname: "box", Username: "ada", Disk: "/dev/vda", Encrypt: true, Passphrase: testPassphrase,
		KeyboardLayout: "us", Timezone: "UTC"}
	if err := writeChoice(out, c, "$6$fake"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "correct horse") {
		t.Errorf("the passphrase is in %s:\n%s", out, data)
	}
}

// --- the passphrase file ---------------------------------------------------

func TestPassphraseFileHandOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hadron-install", "passphrase")
	if err := writePassphraseFile(path, testPassphrase); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", st.Mode().Perm())
	}
	if dst, _ := os.Stat(filepath.Dir(path)); dst.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v, want 0700", dst.Mode().Perm())
	}
	if data, _ := os.ReadFile(path); string(data) != testPassphrase {
		t.Errorf("file = %q, want the passphrase with no newline", data)
	}

	if err := removePassphraseFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("the directory is still there: %v", err)
	}
	if err := removePassphraseFile(path); err != nil {
		t.Errorf("a second remove must be a no-op: %v", err)
	}
}

// --- non-interactive -------------------------------------------------------

func TestChoiceFromEnvEncryption(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("HADRON_INSTALL_NONINTERACTIVE", "1")
		t.Setenv("HADRON_USER", "ada")
		t.Setenv("HADRON_PASS", "hunter2")
		t.Setenv("HADRON_DISK", "/dev/vda")
	}
	t.Run("off by default", func(t *testing.T) {
		base(t)
		t.Setenv("HADRON_DISK_PASSPHRASE", testPassphrase)
		c, _, err := choiceFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.Encrypt || c.Passphrase != "" {
			t.Errorf("encryption off must drop the passphrase: %+v", c)
		}
	})
	t.Run("needs a passphrase", func(t *testing.T) {
		base(t)
		t.Setenv("HADRON_ENCRYPT", "1")
		t.Setenv("HADRON_DISK_PASSPHRASE", "short")
		if _, _, err := choiceFromEnv(); err == nil || !strings.Contains(err.Error(), "HADRON_DISK_PASSPHRASE") {
			t.Errorf("err = %v, want a HADRON_DISK_PASSPHRASE error", err)
		}
	})
	t.Run("on", func(t *testing.T) {
		base(t)
		t.Setenv("HADRON_ENCRYPT", "1")
		t.Setenv("HADRON_DISK_PASSPHRASE", testPassphrase)
		c, _, err := choiceFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !c.Encrypt || c.Passphrase != testPassphrase {
			t.Errorf("choice = %+v", c)
		}
	})
}

// The agent appliance installs with --progress-only: no wizard, no choice,
// so there is no passphrase to hand over. A choice without encryption is the
// same.
func TestEncryptionStepsOnlyForEncryptedChoice(t *testing.T) {
	if pre, cleanup, post := encryptionSteps(nil); pre != nil || cleanup != nil || post != nil {
		t.Error("no choice (--progress-only, seeded config) must not touch encryption")
	}
	if pre, _, _ := encryptionSteps(&Choice{Disk: "/dev/vda", Passphrase: testPassphrase}); pre != nil {
		t.Error("an unencrypted choice must not write a passphrase file")
	}
	pre, cleanup, post := encryptionSteps(&Choice{Disk: "/dev/vda", Encrypt: true, Passphrase: testPassphrase})
	if pre == nil || cleanup == nil || post == nil {
		t.Error("an encrypted choice needs all three steps")
	}
}

// --- wizard ----------------------------------------------------------------

// atDisk is the wizard on the disk step.
func atDisk(t *testing.T) wizardModel {
	t.Helper()
	m := atHostname(t, testDisks())
	m = keys(t, m, "box", "enter", "ada", "enter", "hunter2", "enter", "enter")
	if m.step != stepDisk {
		t.Fatalf("step = %v, want stepDisk", m.step)
	}
	return m
}

// The step is on every machine: it needs no TPM.
func TestWizardEncryptionDefaultsToOff(t *testing.T) {
	m := keys(t, atDisk(t), "enter")
	if m.step != stepEncrypt {
		t.Fatalf("step = %v, want stepEncrypt", m.step)
	}
	v := m.View()
	for _, want := range []string{"Encrypt your files?", "> no, do not encrypt", "every time it starts",
		"stolen", "If you forget the passphrase, your files are gone"} {
		if !strings.Contains(v, want) {
			t.Errorf("encryption step is missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "TPM") {
		t.Errorf("encryption step still talks about a TPM:\n%s", v)
	}
	if n := len(strings.Split(v, "\n")); n > 25 {
		t.Errorf("encryption step is %d lines, more than a 25-row console", n)
	}
	m = keys(t, m, "enter")
	if m.step != stepReview || m.choice.Encrypt {
		t.Fatalf("enter on the default = step %v, encrypt %v; want review, off", m.step, m.choice.Encrypt)
	}
	if !strings.Contains(m.View(), "encrypt    off") {
		t.Errorf("review does not say encryption is off:\n%s", m.View())
	}
	if m = keys(t, m, "esc"); m.step != stepEncrypt {
		t.Errorf("esc from review = %v, want stepEncrypt", m.step)
	}
	if m = keys(t, m, "esc"); m.step != stepDisk {
		t.Errorf("esc from encryption = %v, want stepDisk", m.step)
	}
}

func TestWizardDiskPassphrase(t *testing.T) {
	m := keys(t, atDisk(t), "enter", "down", "enter")
	if m.step != stepPassphrase || !m.choice.Encrypt {
		t.Fatalf("step = %v encrypt = %v, want stepPassphrase, on", m.step, m.choice.Encrypt)
	}
	if !strings.Contains(m.View(), "12 characters or more") {
		t.Errorf("passphrase step does not state the minimum:\n%s", m.View())
	}

	m = keys(t, m, "short", "enter")
	if m.step != stepPassphrase || m.err == "" {
		t.Fatalf("a short passphrase was accepted (step %v)", m.step)
	}
	if !strings.Contains(m.View(), "strength: too short") {
		t.Errorf("no strength hint:\n%s", m.View())
	}
	if strings.Contains(m.in.View(), "short") {
		t.Errorf("the passphrase is echoed:\n%s", m.in.View())
	}

	m = clear(t, m, testPassphrase)
	if !strings.Contains(m.View(), "strength: strong") {
		t.Errorf("no strong hint for a four-word passphrase:\n%s", m.View())
	}
	m = keys(t, m, "enter")
	if m.step != stepPassphraseConfirm {
		t.Fatalf("step = %v, want stepPassphraseConfirm", m.step)
	}
	if m.in.Value() != "" {
		t.Error("the confirmation field must start empty")
	}

	m = keys(t, m, "correct horse battery stapler", "enter")
	if m.step != stepPassphraseConfirm || !strings.Contains(m.err, "do not match") {
		t.Fatalf("a mismatch was accepted: step %v err %q", m.step, m.err)
	}
	if m.choice.Passphrase != "" {
		t.Error("a mismatched passphrase reached the choice")
	}

	m = keys(t, m, testPassphrase, "enter")
	// Then the recovery key question, off by default.
	if m.step != stepRecovery || !strings.Contains(m.View(), "Also create a recovery key?") {
		t.Fatalf("step = %v, want stepRecovery", m.step)
	}
	m = keys(t, m, "enter")
	if m.step != stepReview {
		t.Fatalf("step = %v, want stepReview", m.step)
	}
	if m.choice.Passphrase != testPassphrase || !m.choice.Encrypt {
		t.Errorf("choice = %+v", m.choice)
	}
	if v := m.View(); !strings.Contains(v, "on (passphrase at every start)") || strings.Contains(v, testPassphrase) {
		t.Errorf("review is wrong or shows the passphrase:\n%s", v)
	}

	// Back from review goes to the recovery key question, then the
	// passphrase from its first entry.
	if m = keys(t, m, "esc"); m.step != stepRecovery {
		t.Errorf("esc from review = %v, want stepRecovery", m.step)
	}
	if m = keys(t, m, "esc"); m.step != stepPassphrase {
		t.Errorf("esc from the recovery key = %v, want stepPassphrase", m.step)
	}

	// Turning encryption off again drops the passphrase.
	m = keys(t, m, "esc")
	if m.step != stepEncrypt {
		t.Fatalf("step = %v, want stepEncrypt", m.step)
	}
	m = keys(t, m, "up", "enter")
	if m.choice.Encrypt || m.choice.Passphrase != "" {
		t.Errorf("choice after turning encryption off = %+v", m.choice)
	}
}

// --- the check after the install --------------------------------------------

type call struct {
	name  string
	args  []string
	stdin string
}

// fakeDisk answers the commands verifyEncryption runs like a target disk
// that kcrypt's passphrase method has just encrypted.
type fakeDisk struct {
	calls     []call
	subsystem string
	kdf       string
	failOn    string
}

const lsblkOut = `NAME="/dev/vda" FSTYPE="" LABEL=""
NAME="/dev/vda1" FSTYPE="vfat" LABEL="COS_GRUB"
NAME="/dev/vda2" FSTYPE="ext4" LABEL="COS_OEM"
NAME="/dev/vda5" FSTYPE="crypto_LUKS" LABEL="COS_PERSISTENT_LUKS"
`

func (f *fakeDisk) run(name string, args []string, stdin []byte) ([]byte, error) {
	f.calls = append(f.calls, call{name: name, args: args, stdin: string(stdin)})
	if f.failOn != "" && len(args) > 0 && (name == f.failOn || args[0] == f.failOn) {
		return nil, errors.New("boom")
	}
	switch {
	case name == "lsblk":
		return []byte(lsblkOut), nil
	case name == "blkid":
		return []byte(f.subsystem + "\n"), nil
	case name == "cryptsetup" && args[0] == "luksDump":
		return []byte(`{"keyslots":{"0":{"type":"luks2","kdf":{"type":"` + f.kdf + `"}}},"tokens":{}}`), nil
	}
	return nil, nil
}

func goodDisk() *fakeDisk { return &fakeDisk{subsystem: kcryptSubsystem, kdf: "argon2id"} }

func (f *fakeDisk) find(name, sub string) *call {
	for i := range f.calls {
		if f.calls[i].name == name && len(f.calls[i].args) > 0 && f.calls[i].args[0] == sub {
			return &f.calls[i]
		}
	}
	return nil
}

func TestVerifyEncryption(t *testing.T) {
	f := goodDisk()
	var logs []string
	if err := verifyEncryption(f.run, "/dev/vda", testPassphrase, func(l string) { logs = append(logs, l) }); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		for _, a := range c.args {
			if strings.Contains(a, "correct horse") {
				t.Errorf("%s %v carries the passphrase in its arguments", c.name, c.args)
			}
		}
	}
	for _, l := range logs {
		if strings.Contains(l, "correct horse") {
			t.Errorf("log line carries the passphrase: %q", l)
		}
	}
	open := f.find("cryptsetup", "open")
	if open == nil || !strings.Contains(strings.Join(open.args, " "), "--test-passphrase --key-file - /dev/vda5") || open.stdin != testPassphrase {
		t.Errorf("passphrase test = %+v", open)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "hadron: checking the disk passphrase") {
		t.Errorf("the progress step marker was not logged: %q", logs)
	}
}

func TestVerifyEncryptionFailures(t *testing.T) {
	for name, f := range map[string]*fakeDisk{
		// An agent without kairos-io/kairos#6022 ignores kcrypt.method and
		// encrypts against the TPM, or fails without one.
		"not the passphrase method": {subsystem: "", kdf: "argon2id"},
		"weak KDF":                  {subsystem: kcryptSubsystem, kdf: "pbkdf2"},
		"passphrase does not open":  {subsystem: kcryptSubsystem, kdf: "argon2id", failOn: "open"},
		"no LUKS container":         {subsystem: kcryptSubsystem, kdf: "argon2id", failOn: "lsblk"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyEncryption(f.run, "/dev/vda", testPassphrase, func(string) {}); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestFindLUKSDevice(t *testing.T) {
	if dev, err := findLUKSDevice(lsblkOut); err != nil || dev != "/dev/vda5" {
		t.Errorf("= %q, %v", dev, err)
	}
	old := `NAME="/dev/sda4" FSTYPE="crypto_LUKS" LABEL="COS_PERSISTENT"`
	if dev, err := findLUKSDevice(old); err != nil || dev != "/dev/sda4" {
		t.Errorf("pre-#4403 label: = %q, %v", dev, err)
	}
	if _, err := findLUKSDevice(`NAME="/dev/sda4" FSTYPE="ext4" LABEL="COS_PERSISTENT"`); err == nil {
		t.Error("an unencrypted partition was taken for the LUKS container")
	}
}

// A Trusted Boot install medium encrypts with its TPM-sealed key (#61): the
// passphrase step is not offered there.
func TestWizardHidesEncryptionOnTrustedBoot(t *testing.T) {
	m := atDisk(t)
	m.trustedBoot = true
	// The disk leads to the recovery key step, not to the passphrase.
	m = keys(t, m, "enter")
	if m.step != stepRecovery {
		t.Fatalf("step = %v, want stepRecovery", m.step)
	}
	m = keys(t, m, "enter")
	if m.step != stepReview {
		t.Fatalf("step = %v, want stepReview", m.step)
	}
	if strings.Contains(m.View(), "encrypt") {
		t.Errorf("review mentions the passphrase choice on Trusted Boot:\n%s", m.View())
	}
	if m = keys(t, m, "esc"); m.step != stepRecovery {
		t.Errorf("esc from review = %v, want stepRecovery", m.step)
	}
	if m = keys(t, m, "esc"); m.step != stepDisk {
		t.Errorf("esc from the recovery key step = %v, want stepDisk", m.step)
	}
}

func TestTrustedBootMedium(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "uki_install_mode")
	if trustedBootMedium(marker) {
		t.Error("no marker, yet Trusted Boot")
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !trustedBootMedium(marker) {
		t.Error("marker present, yet not Trusted Boot")
	}
}
