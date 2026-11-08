package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var recoveryKeyShape = regexp.MustCompile(`^([cbdefghijklnrtuv]{8}-){7}[cbdefghijklnrtuv]{8}$`)

func TestGenerateRecoveryKey(t *testing.T) {
	seq := make([]byte, 32)
	for i := range seq {
		seq[i] = byte(i * 8)
	}
	got, err := generateRecoveryKey(bytes.NewReader(seq))
	if err != nil {
		t.Fatal(err)
	}
	// 0x00 0x08 0x10 0x18 -> cc cj bc bj: high nibble then low nibble.
	if want := "cccjbcbj"; !strings.HasPrefix(got, want+"-") {
		t.Errorf("first group = %q, want %q", got[:8], want)
	}
	if !recoveryKeyShape.MatchString(got) {
		t.Errorf("%q is not 8 groups of 8 modhex characters", got)
	}

	a, err := newRecoveryKey()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newRecoveryKey()
	if a == b || !recoveryKeyShape.MatchString(a) {
		t.Errorf("two keys from crypto/rand: %q and %q", a, b)
	}
	if _, err := generateRecoveryKey(bytes.NewReader(make([]byte, 31))); err == nil {
		t.Error("a short random source must fail, not pad the key")
	}
}

func TestConfirmsRecoveryKey(t *testing.T) {
	key, _ := generateRecoveryKey(bytes.NewReader(bytes.Repeat([]byte{0x12, 0x34, 0x56, 0x78}, 8)))
	groups := recoveryKeyGroupsOf(key)
	for _, typed := range []string{groups[3], strings.ToUpper(groups[3]), " " + groups[3] + " ", key, strings.ReplaceAll(key, "-", "")} {
		if !confirmsRecoveryKey(key, 3, typed) {
			t.Errorf("%q should confirm group 4", typed)
		}
	}
	other, _ := generateRecoveryKey(rand.Reader)
	for _, typed := range []string{"", recoveryKeyGroupsOf(other)[3], groups[3][:7]} {
		if confirmsRecoveryKey(key, 3, typed) {
			t.Errorf("%q should not confirm group 4", typed)
		}
	}
}

func TestBigRecoveryKey(t *testing.T) {
	for _, r := range modhex {
		g, ok := bigGlyphs[r]
		if !ok {
			t.Fatalf("no glyph for %q", r)
		}
		for _, row := range g {
			if len(row) != 3 {
				t.Errorf("glyph %q row %q is not 3 wide", r, row)
			}
		}
	}
	key, _ := newRecoveryKey()
	lines := bigRecoveryKey(key)
	if len(lines) != 4*6 {
		t.Fatalf("%d lines, want 4 pairs of 5 rows and a blank", len(lines))
	}
	for _, l := range lines {
		if n := len([]rune(l)); n > 80 {
			t.Errorf("line is %d columns, over 80: %q", n, l)
		}
	}
}

// keyedWizard is at the recovery key step of a Trusted Boot install, with a
// known key and group.
func keyedWizard(t *testing.T) (wizardModel, string) {
	t.Helper()
	key, _ := generateRecoveryKey(bytes.NewReader(bytes.Repeat([]byte{0xab, 0xcd}, 16)))
	m := atDisk(t)
	m.trustedBoot = true
	m.newKey = func() (string, error) { return key, nil }
	m.pickGroup = func() int { return 2 }
	return keys(t, m, "enter"), key
}

func TestWizardRecoveryKeyDefaultsToOff(t *testing.T) {
	m, _ := keyedWizard(t)
	if m.step != stepRecovery {
		t.Fatalf("step = %v, want stepRecovery", m.step)
	}
	m = keys(t, m, "enter")
	if m.step != stepReview || m.choice.RecoveryKey || m.choice.RecoveryKeyValue != "" {
		t.Fatalf("enter on the default: step %v, choice %+v", m.step, m.choice)
	}
	cfg := RenderCloudConfig(m.choice, "$6$hash")
	if strings.Contains(cfg, "kcrypt:") || strings.Contains(cfg, "passphrase_fallback") || !strings.Contains(cfg, "reboot: true") {
		t.Errorf("a TPM-only install must not configure the fallback:\n%s", cfg)
	}
}

func TestWizardRecoveryKeyFlow(t *testing.T) {
	m, key := keyedWizard(t)
	m = keys(t, m, "down", "enter")
	if m.step != stepRecoveryShow {
		t.Fatalf("step = %v, want stepRecoveryShow", m.step)
	}
	if !strings.Contains(m.View(), key) {
		t.Fatal("the key screen does not show the key")
	}
	m = keys(t, m, "enter")
	if m.step != stepRecoveryConfirm || !strings.Contains(m.View(), "group 3 of 8") {
		t.Fatalf("step = %v, view:\n%s", m.step, m.View())
	}
	m = keys(t, m, "cccccccc", "enter")
	if m.step != stepRecoveryConfirm || m.err == "" {
		t.Fatalf("a wrong group was accepted: step %v", m.step)
	}
	m = keys(t, m, recoveryKeyGroupsOf(key)[2], "enter")
	if m.step != stepReview {
		t.Fatalf("the right group: step = %v, err %q", m.step, m.err)
	}
	if !m.choice.RecoveryKey || m.choice.RecoveryKeyValue != key {
		t.Fatalf("choice = %+v", m.choice)
	}
	if strings.Contains(m.View(), key) {
		t.Error("the review shows the key again")
	}
	// Back from the review goes to the yes/no step, not to the key.
	if m = keys(t, m, "esc"); m.step != stepRecovery {
		t.Errorf("esc from review = %v, want stepRecovery", m.step)
	}

	cfg := RenderCloudConfig(m.choice, "$6$hash")
	for _, g := range recoveryKeyGroupsOf(key) {
		if strings.Contains(cfg, g) {
			t.Fatalf("the cloud-config, which goes to OEM, carries the key:\n%s", cfg)
		}
	}
	for _, want := range []string{"reboot: false", "passphrase_fallback: true", "mode=tpm-recovery-key"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("cloud-config lacks %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "method:") || strings.Contains(cfg, "encrypted_partitions") {
		t.Errorf("the recovery key must not change how Trusted Boot encrypts:\n%s", cfg)
	}
}

// fakeTB answers like a Trusted Boot target disk: two TPM-sealed
// containers, each getting one new keyslot when luksAddKey runs.
type fakeTB struct {
	calls  []call
	added  map[string]bool
	failOn string
	keyAt  string // what the key file held when luksAddKey ran
}

func (f *fakeTB) run(name string, args []string, stdin []byte) ([]byte, error) {
	f.calls = append(f.calls, call{name: name, args: args, stdin: string(stdin)})
	if f.failOn != "" && len(args) > 0 && args[0] == f.failOn {
		return nil, errors.New("boom")
	}
	switch {
	case name == "lsblk":
		return []byte(`NAME="/dev/vda" FSTYPE="" LABEL=""
NAME="/dev/vda1" FSTYPE="vfat" LABEL="COS_GRUB"
NAME="/dev/vda3" FSTYPE="crypto_LUKS" LABEL="COS_PERSISTENT_LUKS"
NAME="/dev/vda2" FSTYPE="crypto_LUKS" LABEL="COS_OEM_LUKS"
`), nil
	case name == "cryptsetup" && args[0] == "luksDump":
		dev := args[len(args)-1]
		slots := `"1":{}`
		if f.added[dev] {
			slots += `,"0":{}`
		}
		return []byte(`{"keyslots":{` + slots + `},"tokens":{"0":{"type":"systemd-tpm2","keyslots":["1"]}}}`), nil
	case name == "cryptsetup" && args[0] == "luksAddKey":
		dev, file := args[len(args)-2], args[len(args)-1]
		data, _ := os.ReadFile(file)
		f.keyAt = string(data)
		f.added[dev] = true
	}
	return nil, nil
}

func TestEnrollRecoveryKey(t *testing.T) {
	old := recoveryKeyFile
	recoveryKeyFile = filepath.Join(t.TempDir(), "hadron-install", "recovery-key")
	t.Cleanup(func() { recoveryKeyFile = old })
	key, _ := newRecoveryKey()

	f := &fakeTB{added: map[string]bool{}}
	var logs []string
	if err := enrollRecoveryKey(f.run, "/dev/vda", key, func(l string) { logs = append(logs, l) }); err != nil {
		t.Fatal(err)
	}
	if f.keyAt != key {
		t.Errorf("luksAddKey read %q from the key file", f.keyAt)
	}
	if _, err := os.Stat(recoveryKeyFile); !os.IsNotExist(err) {
		t.Error("the key file is still there after the enrollment")
	}
	var adds, imports, tests int
	for _, c := range f.calls {
		joined := strings.Join(c.args, " ")
		if strings.Contains(joined, key) {
			t.Errorf("%s %v carries the key in its arguments", c.name, c.args)
		}
		switch c.args[0] {
		case "luksAddKey":
			adds++
			for _, want := range []string{"--token-type systemd-tpm2", "--pbkdf pbkdf2", "--pbkdf-force-iterations 1000"} {
				if !strings.Contains(joined, want) {
					t.Errorf("luksAddKey %q lacks %q", joined, want)
				}
			}
		case "token":
			imports++
			var tok struct {
				Type     string   `json:"type"`
				Keyslots []string `json:"keyslots"`
			}
			if err := json.Unmarshal([]byte(c.stdin), &tok); err != nil || tok.Type != "systemd-recovery" || len(tok.Keyslots) != 1 || tok.Keyslots[0] != "0" {
				t.Errorf("token import %q", c.stdin)
			}
		case "open":
			tests++
			if c.stdin != key || !strings.Contains(joined, "--disable-external-tokens --key-slot 0") {
				t.Errorf("key test %q with stdin of %d bytes", joined, len(c.stdin))
			}
		}
	}
	if adds != 2 || imports != 2 || tests != 2 {
		t.Errorf("luksAddKey %d, token import %d, key test %d; want 2 each (OEM and persistent)", adds, imports, tests)
	}
	for _, l := range logs {
		if strings.Contains(l, recoveryKeyGroupsOf(key)[0]) {
			t.Errorf("log line carries the key: %q", l)
		}
	}
}

func TestEnrollRecoveryKeyFailures(t *testing.T) {
	old := recoveryKeyFile
	recoveryKeyFile = filepath.Join(t.TempDir(), "hadron-install", "recovery-key")
	t.Cleanup(func() { recoveryKeyFile = old })
	key, _ := newRecoveryKey()
	for _, step := range []string{"luksAddKey", "token", "open"} {
		f := &fakeTB{added: map[string]bool{}, failOn: step}
		if err := enrollRecoveryKey(f.run, "/dev/vda", key, func(string) {}); err == nil {
			t.Errorf("%s failed, yet the enrollment succeeded", step)
		}
		if _, err := os.Stat(recoveryKeyFile); !os.IsNotExist(err) {
			t.Errorf("%s failed and the key file was left behind", step)
		}
	}
}

func TestEncryptionStepsRecoveryKey(t *testing.T) {
	pre, cleanup, post := encryptionSteps(&Choice{RecoveryKey: true, RecoveryKeyValue: "x", Disk: "/dev/vda"})
	if pre == nil || cleanup == nil || post == nil {
		t.Fatal("a recovery key install needs the manual install and the post step")
	}
	if pre, _, _ := encryptionSteps(&Choice{Disk: "/dev/vda"}); pre != nil {
		t.Error("a TPM-only install needs no post step")
	}
}
