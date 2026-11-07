package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Disk encryption. See docs/superpowers/specs/2026-11-07-encrypt-home-design.md.
//
// With encryption on, the rendered cloud-config asks kairos-agent to encrypt
// COS_PERSISTENT (/home and /var). On a GRUB install kcrypt makes a LUKS2
// container whose only keyslot holds a random passphrase kept in TPM NV
// memory, and immucore unlocks it at boot with that passphrase.
//
// The recovery passphrase is added here, after kairos-agent exits:
// kairos-agent locks the container again at the end of its encryption hook,
// before the after-install stage, and a yip stage cannot report a failure
// (non-strict hooks drop their errors). So the encrypted install runs with
// reboot: false (see installCommand), then the installer reads the TPM
// passphrase back (`kairos-agent kcrypt readnv`), gives both passphrases to
// `cryptsetup luksAddKey` through pipes, checks the result and reboots.
// Neither passphrase is written to a file, not even on tmpfs.

// tpmDevice is the TPM 2.0 resource manager kcrypt opens. The kernel makes a
// tpmrm device only for a TPM 2.0 chip, and kcrypt refuses to encrypt without
// this path (sdk/kcrypt validateTPMDevice), so its presence is the test.
const tpmDevice = "/dev/tpmrm0"

// tpmPresent reports whether the machine has a TPM 2.0 that kcrypt can use.
// When sysClassTPM can be read, a TPM there must also report major version 2.
func tpmPresent(dev, sysClassTPM string) bool {
	if _, err := os.Stat(dev); err != nil {
		return false
	}
	entries, err := os.ReadDir(sysClassTPM)
	if err != nil || len(entries) == 0 {
		return true // the device node is the authoritative signal
	}
	for _, e := range entries {
		v, err := os.ReadFile(sysClassTPM + "/" + e.Name() + "/tpm_version_major")
		if err != nil || strings.TrimSpace(string(v)) == "2" {
			return true
		}
	}
	return false
}

// encryptionMarker is the file the installed system keeps to say how its
// disk is encrypted. It holds no secret. The boot-time passphrase prompt
// reads it.
const encryptionMarker = "/etc/hadron-desktop/encryption"

// encryptedPartition is the one partition the installer encrypts: it holds
// /home and /var. COS_OEM stays readable (GRUB reads it); kcrypt refuses the
// state and recovery partitions.
const encryptedPartition = "COS_PERSISTENT"

// recoveryTokenType is the type of the LUKS2 token that names the recovery
// keyslot. The token holds no key material, only {"type", "keyslots"}.
const recoveryTokenType = "hadron-recovery"

// minRecoveryLen is the shortest recovery passphrase the installer takes.
// The passphrase is the only way back in after a TPM reset and an attacker
// with the disk can guess offline, so it is longer than the login password.
const minRecoveryLen = 12

func validateRecovery(p string) error {
	if len([]rune(p)) < minRecoveryLen {
		return fmt.Errorf("recovery passphrase must be at least %d characters", minRecoveryLen)
	}
	if strings.TrimSpace(p) != p {
		return errors.New("recovery passphrase must not start or end with a space")
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return errors.New("recovery passphrase must not contain control characters")
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
	if n < minRecoveryLen {
		return fmt.Sprintf("too short: %d of %d characters", n, minRecoveryLen)
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

// runner runs one command with stdin and, when extra is set, extra pipes
// that the child sees as fd 3, 4 and so on. It returns stdout. It is the seam
// the tests replace; nothing else in the enrollment executes a program.
type runner func(name string, args []string, stdin []byte, extra ...[]byte) ([]byte, error)

func execRunner(name string, args []string, stdin []byte, extra ...[]byte) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	var writers []*os.File
	for _, data := range extra {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		cmd.ExtraFiles = append(cmd.ExtraFiles, r)
		writers = append(writers, w)
		// A passphrase is far below the pipe buffer, so this cannot block.
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		w.Close()
	}
	out, err := cmd.Output()
	for _, f := range cmd.ExtraFiles {
		f.Close()
	}
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
// enrollment reads.
type luksMeta struct {
	Keyslots map[string]struct {
		Type string `json:"type"`
		KDF  struct {
			Type string `json:"type"`
		} `json:"kdf"`
	} `json:"keyslots"`
	Tokens map[string]struct {
		Type     string   `json:"type"`
		Keyslots []string `json:"keyslots"`
	} `json:"tokens"`
}

func parseLUKSMeta(out []byte) (luksMeta, error) {
	var m luksMeta
	if err := json.Unmarshal(out, &m); err != nil {
		return m, fmt.Errorf("reading the LUKS2 header: %w", err)
	}
	return m, nil
}

func (m luksMeta) slots() []int {
	var s []int
	for k := range m.Keyslots {
		if n, err := strconv.Atoi(k); err == nil {
			s = append(s, n)
		}
	}
	sort.Ints(s)
	return s
}

// lastLine is the passphrase in `kairos-agent kcrypt readnv` output.
// kairos-agent writes its start-up log lines (version, boot mode) to stdout
// too, and readnv prints the passphrase after them with no newline.
func lastLine(out []byte) []byte {
	out = bytes.TrimRight(out, "\r\n")
	if i := bytes.LastIndexByte(out, '\n'); i >= 0 {
		out = out[i+1:]
	}
	return bytes.TrimSpace(out)
}

// recoveryTokenJSON is the LUKS2 token that marks slot as the recovery
// keyslot.
func recoveryTokenJSON(slot int) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":     recoveryTokenType,
		"keyslots": []string{strconv.Itoa(slot)},
	})
	return b
}

// enrollRecovery adds recovery as a LUKS2 passphrase keyslot on the
// COS_PERSISTENT container of disk, marks it with a hadron-recovery token
// and proves the new keyslot opens the container. log receives progress
// lines, never a secret.
func enrollRecovery(run runner, disk, recovery string, log func(string)) error {
	if err := validateRecovery(recovery); err != nil {
		return err
	}
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

	tpmKey, err := run("kairos-agent", []string{"kcrypt", "readnv"}, nil)
	if err != nil {
		return fmt.Errorf("reading the TPM-held passphrase: %w", err)
	}
	tpmKey = lastLine(tpmKey)
	if len(tpmKey) == 0 {
		return errors.New("the TPM returned an empty passphrase")
	}

	dump := []string{"luksDump", "--dump-json-metadata", dev}
	out, err = run("cryptsetup", dump, nil)
	if err != nil {
		return err
	}
	before, err := parseLUKSMeta(out)
	if err != nil {
		return err
	}

	log("hadron: adding the recovery passphrase (argon2id)")
	// --force-password: the installer has checked the passphrase itself, and
	// a pwquality policy refusing it here would leave a half-done install.
	// No --iter-time: cryptsetup benchmarks argon2id for its default 2 s.
	_, err = run("cryptsetup", []string{
		"luksAddKey", "--batch-mode", "--pbkdf", "argon2id", "--force-password",
		"--key-file", "-", "--new-keyfile", "/dev/fd/3", dev,
	}, tpmKey, []byte(recovery))
	if err != nil {
		return err
	}

	out, err = run("cryptsetup", dump, nil)
	if err != nil {
		return err
	}
	after, err := parseLUKSMeta(out)
	if err != nil {
		return err
	}
	slot := -1
	for _, s := range after.slots() {
		if _, ok := before.Keyslots[strconv.Itoa(s)]; !ok {
			slot = s
		}
	}
	if slot < 0 {
		return errors.New("cryptsetup added no keyslot")
	}
	if kdf := after.Keyslots[strconv.Itoa(slot)].KDF.Type; kdf != "argon2id" {
		return fmt.Errorf("recovery keyslot %d uses %q, want argon2id", slot, kdf)
	}

	if _, err := run("cryptsetup", []string{"token", "import", "--json-file", "-", dev},
		recoveryTokenJSON(slot)); err != nil {
		return err
	}

	if _, err := run("cryptsetup", []string{
		"open", "--test-passphrase", "--key-slot", strconv.Itoa(slot), "--key-file", "-", dev,
	}, []byte(recovery)); err != nil {
		return fmt.Errorf("the recovery passphrase does not open keyslot %d: %w", slot, err)
	}
	log(fmt.Sprintf("hadron: recovery passphrase is keyslot %d", slot))
	return nil
}
