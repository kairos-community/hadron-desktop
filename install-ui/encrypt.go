package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

// Disk encryption. See docs/superpowers/specs/2026-11-07-encrypt-home-design.md.
//
// With encryption on, the rendered cloud-config asks kairos-agent to encrypt
// COS_PERSISTENT (/home and /var) with kcrypt's passphrase method
// (kairos-io/kairos#6022): a LUKS2 container with one argon2id keyslot that
// holds the passphrase the user typed. The machine asks for it at every boot.
//
// The passphrase never goes into the cloud-config, because kairos-agent copies
// that to the unencrypted OEM partition. kcrypt reads it from
// kcrypt.passphrase_file instead: a 0600 file on /run (tmpfs) that the
// installer writes just before kairos-agent runs and deletes as soon as it
// exits. Then the installer checks the result and reboots.

// encryptionMarker is the file the installed system keeps to say how its
// disk is encrypted. It holds no secret.
const encryptionMarker = "/etc/hadron-desktop/encryption"

// encryptedPartition is the one partition the installer encrypts: it holds
// /home and /var. COS_OEM stays readable (GRUB reads it); kcrypt refuses the
// state and recovery partitions.
const encryptedPartition = "COS_PERSISTENT"

// kcryptPassphraseMethod and kcryptSubsystem are the kcrypt.method value that
// selects the passphrase encryptor, and the LUKS2 subsystem that encryptor
// writes. The boot prompt (hadron-unlock in the initramfs) keys on the
// subsystem.
const (
	kcryptPassphraseMethod = "passphrase"
	kcryptSubsystem        = "kcrypt:passphrase"
)

// ukiInstallMarker is the file immucore writes when it boots a Trusted Boot
// (UKI) install medium. That edition encrypts with a key sealed to the TPM
// and the measured boot (#61), so the installer does not offer the passphrase
// method there: kcrypt.method: passphrase would replace the sealed key.
const ukiInstallMarker = "/run/cos/uki_install_mode"

// trustedBootMedium reports whether the installer runs on a Trusted Boot
// install medium.
func trustedBootMedium(marker string) bool {
	_, err := os.Stat(marker)
	return err == nil
}

// passphraseFile is the hand-off to kcrypt (kcrypt.passphrase_file). /run is
// a tmpfs on the live system, so the file never touches a disk.
const passphraseFile = "/run/hadron-install/passphrase"

// minPassphraseLen is the shortest disk passphrase the installer takes. The
// passphrase is the only key, and anyone with the disk can guess offline, so
// it is longer than the login password.
const minPassphraseLen = 12

func validatePassphrase(p string) error {
	if len([]rune(p)) < minPassphraseLen {
		return fmt.Errorf("the passphrase must be at least %d characters", minPassphraseLen)
	}
	if strings.TrimSpace(p) != p {
		return errors.New("the passphrase must not start or end with a space")
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return errors.New("the passphrase must not contain control characters")
		}
	}
	return nil
}

// passphraseStrength is a rough hint for the person typing, not a policy:
// length times the bits of the character classes used. Four or five random
// words in lower case come out "strong" by length alone, which is right.
func passphraseStrength(p string) string {
	n := len([]rune(p))
	if n == 0 {
		return ""
	}
	if n < minPassphraseLen {
		return fmt.Sprintf("too short: %d of %d characters", n, minPassphraseLen)
	}
	var lower, upper, digit, other bool
	for _, r := range p {
		switch {
		case unicode.IsLower(r):
			lower = true
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsDigit(r):
			digit = true
		default:
			other = true
		}
	}
	pool := 0
	for _, c := range []struct {
		on   bool
		size int
	}{{lower, 26}, {upper, 26}, {digit, 10}, {other, 33}} {
		if c.on {
			pool += c.size
		}
	}
	bits := float64(n) * math.Log2(float64(pool))
	switch {
	case bits < 60:
		return "weak: make it longer"
	case bits < 80:
		return "fair: a few more words make it strong"
	default:
		return "strong"
	}
}

// writePassphraseFile puts the passphrase where kcrypt reads it: a directory
// only root can enter, a file only root can read, no trailing newline.
func writePassphraseFile(path, passphrase string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(passphrase); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// removePassphraseFile deletes the hand-off file and its directory.
func removePassphraseFile(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(filepath.Dir(path))
	return nil
}

// runner runs one command with stdin and returns stdout. It is the seam the
// tests replace; nothing else in the check executes a program.
type runner func(name string, args []string, stdin []byte) ([]byte, error)

func execRunner(name string, args []string, stdin []byte) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%s %s: %w: %s", name, args[0], err, msg)
		}
		return out, fmt.Errorf("%s %s: %w", name, args[0], err)
	}
	return out, nil
}

// luksLabels are the labels kcrypt gives the LUKS container of
// COS_PERSISTENT: COS_PERSISTENT_LUKS since kairos-io/kairos#4403, the plain
// label before it.
var luksLabels = []string{encryptedPartition + "_LUKS", encryptedPartition}

// findLUKSDevice picks the COS_PERSISTENT LUKS container out of
// `lsblk -P -p -o NAME,FSTYPE,LABEL` output.
func findLUKSDevice(lsblk string) (string, error) {
	byLabel := map[string]string{}
	for _, line := range strings.Split(lsblk, "\n") {
		kv := parsePairs(line)
		if kv["FSTYPE"] == "crypto_LUKS" && kv["NAME"] != "" {
			byLabel[kv["LABEL"]] = kv["NAME"]
		}
	}
	for _, l := range luksLabels {
		if dev, ok := byLabel[l]; ok {
			return dev, nil
		}
	}
	return "", fmt.Errorf("no LUKS container labelled %s on the target disk", strings.Join(luksLabels, " or "))
}

// parsePairs reads one line of lsblk -P output: KEY="value" KEY="value".
func parsePairs(line string) map[string]string {
	kv := map[string]string{}
	for line = strings.TrimSpace(line); line != ""; line = strings.TrimSpace(line) {
		eq := strings.Index(line, `="`)
		if eq < 0 {
			break
		}
		key := line[:eq]
		rest := line[eq+2:]
		end := strings.Index(rest, `"`)
		if end < 0 {
			break
		}
		kv[key] = rest[:end]
		line = rest[end+1:]
	}
	return kv
}

// luksMeta is the part of `cryptsetup luksDump --dump-json-metadata` the
// check reads.
type luksMeta struct {
	Keyslots map[string]struct {
		KDF struct {
			Type string `json:"type"`
		} `json:"kdf"`
	} `json:"keyslots"`
}

// verifyEncryption checks what kairos-agent left on disk before the installer
// reboots into it: one COS_PERSISTENT LUKS2 container, marked for the boot
// prompt, whose keyslots all use argon2id, and that the passphrase opens.
// log receives progress lines, never a secret.
func verifyEncryption(run runner, disk, passphrase string, log func(string)) error {
	log("hadron: checking the disk passphrase")
	// kairos-agent has just closed the container; let udev catch up so lsblk
	// reports its label.
	_, _ = run("udevadm", []string{"settle"}, nil)
	out, err := run("lsblk", []string{"-P", "-p", "-o", "NAME,FSTYPE,LABEL", disk}, nil)
	if err != nil {
		return err
	}
	dev, err := findLUKSDevice(string(out))
	if err != nil {
		return err
	}
	log("hadron: encrypted partition " + dev)

	out, err = run("blkid", []string{"-o", "value", "-s", "SUBSYSTEM", dev}, nil)
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != kcryptSubsystem {
		return fmt.Errorf("%s has LUKS2 subsystem %q, want %q: this kairos-agent does not support kcrypt method passphrase", dev, got, kcryptSubsystem)
	}

	out, err = run("cryptsetup", []string{"luksDump", "--dump-json-metadata", dev}, nil)
	if err != nil {
		return err
	}
	var meta luksMeta
	if err := json.Unmarshal(out, &meta); err != nil {
		return fmt.Errorf("reading the LUKS2 header: %w", err)
	}
	if len(meta.Keyslots) == 0 {
		return fmt.Errorf("%s has no keyslot", dev)
	}
	for slot, k := range meta.Keyslots {
		if k.KDF.Type != "argon2id" {
			return fmt.Errorf("keyslot %s uses %q, want argon2id", slot, k.KDF.Type)
		}
	}

	if _, err := run("cryptsetup", []string{"open", "--test-passphrase", "--key-file", "-", dev},
		[]byte(passphrase)); err != nil {
		return fmt.Errorf("the passphrase does not open %s: %w", dev, err)
	}
	log("hadron: the passphrase opens " + dev)
	return nil
}
