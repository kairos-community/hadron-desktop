package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testRecovery = "correct horse battery staple"

// --- TPM detection ---------------------------------------------------------

func TestTPMPresent(t *testing.T) {
	dir := t.TempDir()
	dev := filepath.Join(dir, "tpmrm0")
	sys := filepath.Join(dir, "class")

	if tpmPresent(dev, sys) {
		t.Error("no device node: want no TPM")
	}
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !tpmPresent(dev, sys) {
		t.Error("device node and no sysfs: want a TPM")
	}
	if err := os.MkdirAll(filepath.Join(sys, "tpm0"), 0o755); err != nil {
		t.Fatal(err)
	}
	ver := filepath.Join(sys, "tpm0", "tpm_version_major")
	if err := os.WriteFile(ver, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if tpmPresent(dev, sys) {
		t.Error("sysfs reports TPM 1.2: want no TPM")
	}
	if err := os.WriteFile(ver, []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !tpmPresent(dev, sys) {
		t.Error("sysfs reports TPM 2.0: want a TPM")
	}
}

// --- passphrase checks -----------------------------------------------------

func TestValidateRecovery(t *testing.T) {
	for _, c := range []struct {
		in string
		ok bool
	}{
		{"", false},
		{"elevenchars", false},
		{"twelve chars", true},
		{testRecovery, true},
		{" leading space ok?", false},
		{"trailing space    ", false},
		{"tab\tinside passphrase", false},
		{"line\nbreak passphrase", false},
		{"àèìòù àèìòù àè", true},
	} {
		err := validateRecovery(c.in)
		if (err == nil) != c.ok {
			t.Errorf("validateRecovery(%q) = %v, want ok=%v", c.in, err, c.ok)
		}
	}
}

func TestPassphraseStrength(t *testing.T) {
	for _, c := range []struct{ in, prefix string }{
		{"", ""},
		{"short", "too short"},
		{"aaaaaaaaaaaa", "weak"},
		{"abcdefgh1234", "fair"},
		{testRecovery, "strong"},
	} {
		if got := passphraseStrength(c.in); !strings.HasPrefix(got, c.prefix) || (c.prefix == "" && got != "") {
			t.Errorf("passphraseStrength(%q) = %q, want prefix %q", c.in, got, c.prefix)
		}
	}
}

// --- cloud-config ----------------------------------------------------------

func TestRenderCloudConfigEncrypted(t *testing.T) {
	c := Choice{Hostname: "box", Username: "ada", Disk: "/dev/vda", Encrypt: true, Recovery: testRecovery,
		KeyboardLayout: "us", Timezone: "UTC"}
	out := RenderCloudConfig(c, "$6$fake")

	for _, want := range []string{
		"install:\n  device: \"/dev/vda\"\n  auto: true\n  reboot: false\n  encrypted_partitions:\n    - COS_PERSISTENT\nhostname:",
		"        - path: /etc/hadron-desktop/encryption\n          permissions: 0644\n          content: |\n            mode=tpm\n            recovery=1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config is missing\n%s\nin\n%s", want, out)
		}
	}
	// The marker belongs to the initramfs stage, before the boot stage.
	if strings.Index(out, encryptionMarker) > strings.Index(out, "  boot:\n") {
		t.Errorf("marker is not in the initramfs stage:\n%s", out)
	}
	if strings.Contains(out, testRecovery) || strings.Contains(out, "correct horse") {
		t.Errorf("the recovery passphrase reached the cloud-config:\n%s", out)
	}
	if strings.Contains(out, "reboot: true") {
		t.Errorf("an encrypted install must not let kairos-agent reboot first:\n%s", out)
	}
}

func TestRenderCloudConfigUnencryptedIsUnchanged(t *testing.T) {
	c := Choice{Hostname: "box", Username: "ada", Disk: "/dev/vda", Recovery: testRecovery}
	out := RenderCloudConfig(c, "$6$fake")
	for _, bad := range []string{"encrypted_partitions", encryptionMarker, "reboot: false", testRecovery} {
		if strings.Contains(out, bad) {
			t.Errorf("unencrypted config contains %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "  reboot: true\n") {
		t.Errorf("unencrypted config lost reboot: true:\n%s", out)
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
		t.Setenv("HADRON_RECOVERY_PASSPHRASE", testRecovery)
		c, _, err := choiceFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.Encrypt || c.Recovery != "" {
			t.Errorf("encryption off must drop the passphrase: %+v", c)
		}
	})
	t.Run("needs a passphrase", func(t *testing.T) {
		base(t)
		t.Setenv("HADRON_ENCRYPT", "1")
		t.Setenv("HADRON_RECOVERY_PASSPHRASE", "short")
		if _, _, err := choiceFromEnv(); err == nil || !strings.Contains(err.Error(), "HADRON_RECOVERY_PASSPHRASE") {
			t.Errorf("err = %v, want a HADRON_RECOVERY_PASSPHRASE error", err)
		}
	})
	t.Run("on", func(t *testing.T) {
		base(t)
		t.Setenv("HADRON_ENCRYPT", "1")
		t.Setenv("HADRON_RECOVERY_PASSPHRASE", testRecovery)
		c, _, err := choiceFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !c.Encrypt || c.Recovery != testRecovery {
			t.Errorf("choice = %+v", c)
		}
	})
}

// The agent appliance installs with --progress-only: no wizard, no choice,
// so nothing is enrolled after the install. A choice without encryption is
// the same.
func TestEnrollCommandOnlyForEncryptedChoice(t *testing.T) {
	if enrollCommand(nil) != nil {
		t.Error("no choice (--progress-only, seeded config) must not enroll")
	}
	if enrollCommand(&Choice{Disk: "/dev/vda", Recovery: testRecovery}) != nil {
		t.Error("an unencrypted choice must not enroll")
	}
	if enrollCommand(&Choice{Disk: "/dev/vda", Encrypt: true, Recovery: testRecovery}) == nil {
		t.Error("an encrypted choice must enroll")
	}
}

// --- wizard ----------------------------------------------------------------

// atDiskWithTPM is the wizard on the disk step of a machine with a TPM.
func atDiskWithTPM(t *testing.T) wizardModel {
	t.Helper()
	m := atHostname(t, testDisks())
	m.hasTPM = true
	m = keys(t, m, "box", "enter", "ada", "enter", "hunter2", "enter", "enter")
	if m.step != stepDisk {
		t.Fatalf("step = %v, want stepDisk", m.step)
	}
	return m
}

func TestWizardWithoutTPMSkipsEncryption(t *testing.T) {
	m := atHostname(t, testDisks())
	m = keys(t, m, "box", "enter", "ada", "enter", "hunter2", "enter", "enter", "enter")
	if m.step != stepReview {
		t.Fatalf("step = %v, want stepReview", m.step)
	}
	if strings.Contains(m.View(), "encrypt") {
		t.Errorf("review mentions encryption on a machine without a TPM:\n%s", m.View())
	}
	if m = keys(t, m, "esc"); m.step != stepDisk {
		t.Errorf("esc from review = %v, want stepDisk", m.step)
	}
}

func TestWizardEncryptionDefaultsToOff(t *testing.T) {
	m := keys(t, atDiskWithTPM(t), "enter")
	if m.step != stepEncrypt {
		t.Fatalf("step = %v, want stepEncrypt", m.step)
	}
	v := m.View()
	for _, want := range []string{"Encrypt your files?", "> no, do not encrypt", "taken out of this computer",
		"Does not protect", "USB stick", "only way back"} {
		if !strings.Contains(v, want) {
			t.Errorf("encryption step is missing %q:\n%s", want, v)
		}
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

func TestWizardRecoveryPassphrase(t *testing.T) {
	m := keys(t, atDiskWithTPM(t), "enter", "down", "enter")
	if m.step != stepRecovery || !m.choice.Encrypt {
		t.Fatalf("step = %v encrypt = %v, want stepRecovery, on", m.step, m.choice.Encrypt)
	}
	if !strings.Contains(m.View(), "12 characters or more") {
		t.Errorf("recovery step does not state the minimum:\n%s", m.View())
	}

	m = keys(t, m, "short", "enter")
	if m.step != stepRecovery || m.err == "" {
		t.Fatalf("a short passphrase was accepted (step %v)", m.step)
	}
	if !strings.Contains(m.View(), "strength: too short") {
		t.Errorf("no strength hint:\n%s", m.View())
	}
	if strings.Contains(m.View(), "short") && strings.Contains(m.in.View(), "short") {
		t.Errorf("the passphrase is echoed:\n%s", m.in.View())
	}

	m = clear(t, m, testRecovery)
	if !strings.Contains(m.View(), "strength: strong") {
		t.Errorf("no strong hint for a four-word passphrase:\n%s", m.View())
	}
	m = keys(t, m, "enter")
	if m.step != stepRecoveryConfirm {
		t.Fatalf("step = %v, want stepRecoveryConfirm", m.step)
	}
	if m.in.Value() != "" {
		t.Error("the confirmation field must start empty")
	}

	m = keys(t, m, "correct horse battery stapler", "enter")
	if m.step != stepRecoveryConfirm || !strings.Contains(m.err, "do not match") {
		t.Fatalf("a mismatch was accepted: step %v err %q", m.step, m.err)
	}
	if m.choice.Recovery != "" {
		t.Error("a mismatched passphrase reached the choice")
	}

	m = keys(t, m, testRecovery, "enter")
	if m.step != stepReview {
		t.Fatalf("step = %v, want stepReview", m.step)
	}
	if m.choice.Recovery != testRecovery || !m.choice.Encrypt {
		t.Errorf("choice = %+v", m.choice)
	}
	if v := m.View(); !strings.Contains(v, "on (TPM key and recovery passphrase)") || strings.Contains(v, testRecovery) {
		t.Errorf("review is wrong or shows the passphrase:\n%s", v)
	}

	// Back from review goes to the confirmation, then the first entry.
	if m = keys(t, m, "esc"); m.step != stepRecoveryConfirm {
		t.Errorf("esc from review = %v, want stepRecoveryConfirm", m.step)
	}

	// Turning encryption off again drops the passphrase.
	m = keys(t, m, "esc", "esc")
	if m.step != stepEncrypt {
		t.Fatalf("step = %v, want stepEncrypt", m.step)
	}
	m = keys(t, m, "up", "enter")
	if m.choice.Encrypt || m.choice.Recovery != "" {
		t.Errorf("choice after turning encryption off = %+v", m.choice)
	}
}

// --- enrollment ------------------------------------------------------------

type call struct {
	name  string
	args  []string
	stdin string
	extra []string
}

// fakeLUKS records the commands enrollRecovery runs and answers them like a
// LUKS2 container that has keyslot 0 and gains keyslot 1 on luksAddKey.
type fakeLUKS struct {
	calls    []call
	added    bool
	kdf      string
	failOn   string
	addsSlot bool
}

const lsblkOut = `NAME="/dev/vda" FSTYPE="" LABEL=""
NAME="/dev/vda1" FSTYPE="vfat" LABEL="COS_GRUB"
NAME="/dev/vda2" FSTYPE="ext4" LABEL="COS_OEM"
NAME="/dev/vda5" FSTYPE="crypto_LUKS" LABEL="COS_PERSISTENT_LUKS"
`

func (f *fakeLUKS) run(name string, args []string, stdin []byte, extra ...[]byte) ([]byte, error) {
	c := call{name: name, args: args, stdin: string(stdin)}
	for _, e := range extra {
		c.extra = append(c.extra, string(e))
	}
	f.calls = append(f.calls, c)
	if f.failOn != "" && len(args) > 0 && (name == f.failOn || args[0] == f.failOn) {
		return nil, errors.New("boom")
	}
	switch {
	case name == "lsblk":
		return []byte(lsblkOut), nil
	case name == "kairos-agent":
		// What kairos-agent v4.4 prints: log lines, then the passphrase.
		return []byte("\x1b[90m2026-11-07T18:22:55Z\x1b[0m INF Kairos Agent version=v4.4.0\n" +
			"2026-11-07T18:22:55Z INF Boot in uki mode result=false\nTPMSECRET"), nil
	case name == "cryptsetup" && args[0] == "luksDump":
		if f.added {
			return []byte(`{"keyslots":{"0":{"type":"luks2","kdf":{"type":"argon2id"}},"1":{"type":"luks2","kdf":{"type":"` + f.kdf + `"}}},"tokens":{}}`), nil
		}
		return []byte(`{"keyslots":{"0":{"type":"luks2","kdf":{"type":"argon2id"}}},"tokens":{}}`), nil
	case name == "cryptsetup" && args[0] == "luksAddKey":
		f.added = f.addsSlot
	}
	return nil, nil
}

func newFake() *fakeLUKS { return &fakeLUKS{kdf: "argon2id", addsSlot: true} }

func (f *fakeLUKS) find(name, sub string) *call {
	for i := range f.calls {
		if f.calls[i].name == name && len(f.calls[i].args) > 0 && f.calls[i].args[0] == sub {
			return &f.calls[i]
		}
	}
	return nil
}

func TestEnrollRecovery(t *testing.T) {
	f := newFake()
	var logs []string
	if err := enrollRecovery(f.run, "/dev/vda", testRecovery, func(l string) { logs = append(logs, l) }); err != nil {
		t.Fatal(err)
	}

	// No secret in any argument or log line.
	for _, c := range f.calls {
		for _, a := range c.args {
			if strings.Contains(a, "correct horse") || strings.Contains(a, "TPMSECRET") {
				t.Errorf("%s %v carries a secret in its arguments", c.name, c.args)
			}
		}
	}
	for _, l := range logs {
		if strings.Contains(l, "correct horse") || strings.Contains(l, "TPMSECRET") {
			t.Errorf("log line carries a secret: %q", l)
		}
	}

	add := f.find("cryptsetup", "luksAddKey")
	if add == nil {
		t.Fatal("no luksAddKey")
	}
	joined := strings.Join(add.args, " ")
	for _, want := range []string{"--pbkdf argon2id", "--key-file -", "--new-keyfile /dev/fd/3", "/dev/vda5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("luksAddKey %q lacks %q", joined, want)
		}
	}
	for _, bad := range []string{"--iter-time", "--pbkdf-memory", "--pbkdf-force-iterations"} {
		if strings.Contains(joined, bad) {
			t.Errorf("luksAddKey %q weakens the cryptsetup default with %s", joined, bad)
		}
	}
	if add.stdin != "TPMSECRET" {
		t.Errorf("luksAddKey stdin = %q, want the trimmed TPM passphrase", add.stdin)
	}
	if len(add.extra) != 1 || add.extra[0] != testRecovery {
		t.Errorf("luksAddKey fd 3 = %q, want the recovery passphrase exactly", add.extra)
	}

	tok := f.find("cryptsetup", "token")
	if tok == nil || tok.stdin != `{"keyslots":["1"],"type":"hadron-recovery"}` {
		t.Errorf("token import = %+v", tok)
	}
	open := f.find("cryptsetup", "open")
	if open == nil || !strings.Contains(strings.Join(open.args, " "), "--test-passphrase --key-slot 1") || open.stdin != testRecovery {
		t.Errorf("verification = %+v", open)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "hadron: adding the recovery passphrase") {
		t.Errorf("the progress step marker was not logged: %q", logs)
	}
}

func TestEnrollRecoveryFailures(t *testing.T) {
	for name, f := range map[string]*fakeLUKS{
		"luksAddKey fails": {kdf: "argon2id", addsSlot: true, failOn: "luksAddKey"},
		"no TPM key":       {kdf: "argon2id", addsSlot: true, failOn: "kairos-agent"},
		"no new keyslot":   {kdf: "argon2id"},
		"weak KDF":         {kdf: "pbkdf2", addsSlot: true},
		"verify fails":     {kdf: "argon2id", addsSlot: true, failOn: "open"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := enrollRecovery(f.run, "/dev/vda", testRecovery, func(string) {}); err == nil {
				t.Error("want an error")
			}
		})
	}
	if err := enrollRecovery(newFake().run, "/dev/vda", "short", func(string) {}); err == nil {
		t.Error("a short passphrase was enrolled")
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
