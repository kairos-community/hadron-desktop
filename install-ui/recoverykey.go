package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Recovery key for the Trusted Boot edition. See the Trusted Boot design note
// (docs/superpowers/specs/2026-11-07-trusted-boot-edition-design.md).
//
// Trusted Boot seals the disk key to the TPM and the measured boot. That is
// all it has by default, so a cleared TPM, some firmware updates or the disk
// in another machine leave the files unreadable. The opt-in recovery key is
// a second keyslot on every TPM-sealed container that a person can type at
// the boot prompt when the TPM refuses.
//
// The key is systemd-cryptenroll's recovery key format: 32 random bytes as 64
// modhex characters in eight dash-separated groups. Modhex has no look-alike
// characters, and the keyslot holds the key with its dashes, as systemd's
// does, so the boot prompt can take it with or without them.

// modhex maps a nibble to a letter, as systemd and YubiKey do.
const modhex = "cbdefghijklnrtuv"

const (
	recoveryKeyBytes  = 32 // 256 bits
	recoveryGroupLen  = 8
	recoveryKeyGroups = 2 * recoveryKeyBytes / recoveryGroupLen
)

// recoveryKeyFile is the hand-off to cryptsetup: a 0600 file on /run
// (tmpfs), written just before the keyslot is added and deleted right after,
// like the disk passphrase file. The key is never in the cloud-config, which
// kairos-agent copies to OEM.
var recoveryKeyFile = "/run/hadron-install/recovery-key"

// recoveryTokenType marks the keyslot as a recovery key in the LUKS2 header.
// It is the type systemd-cryptenroll --recovery-key writes; kcrypt reads it
// to word its boot prompt "recovery key" and to offer the prompt at all.
const recoveryTokenType = "systemd-recovery"

// generateRecoveryKey reads 32 bytes from r (crypto/rand.Reader in the
// installer) and formats them as a recovery key.
func generateRecoveryKey(r io.Reader) (string, error) {
	buf := make([]byte, recoveryKeyBytes)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("reading random bytes for the recovery key: %w", err)
	}
	var b strings.Builder
	for i, c := range buf {
		if i > 0 && i%(recoveryGroupLen/2) == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(modhex[c>>4])
		b.WriteByte(modhex[c&0x0f])
	}
	return b.String(), nil
}

func newRecoveryKey() (string, error) { return generateRecoveryKey(rand.Reader) }

// recoveryKeyGroupsOf splits a key into its eight groups.
func recoveryKeyGroupsOf(key string) []string { return strings.Split(key, "-") }

// normalizeKeyInput forgives how a group or a key is typed back: case,
// spaces and dashes do not count.
func normalizeKeyInput(s string) string {
	return strings.NewReplacer("-", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(s)))
}

// confirmsRecoveryKey reports whether typed is group number group (0-based)
// of key, or the whole key.
func confirmsRecoveryKey(key string, group int, typed string) bool {
	t := normalizeKeyInput(typed)
	if t == "" {
		return false
	}
	if t == normalizeKeyInput(key) {
		return true
	}
	groups := recoveryKeyGroupsOf(key)
	return group >= 0 && group < len(groups) && t == groups[group]
}

// recoveryKeyLines is the key screen: the key in the console's own font,
// which keeps every letter distinct, two groups per line with their numbers
// and a space between letters, then the whole key on one line.
func recoveryKeyLines(key string) []string {
	g := recoveryKeyGroupsOf(key)
	var out []string
	for i := 0; i+1 < len(g); i += 2 {
		out = append(out, fmt.Sprintf("%d  %s    %s  %d", i+1, spaced(g[i]), spaced(g[i+1]), i+2))
	}
	return append(out, "", key)
}

// spaced puts a space between letters: easier to read and copy by hand.
func spaced(s string) string { return strings.Join(strings.Split(s, ""), " ") }

// luksDump is the part of `cryptsetup luksDump --dump-json-metadata` the
// enrollment reads.
type luksDump struct {
	Keyslots map[string]json.RawMessage `json:"keyslots"`
	Tokens   map[string]struct {
		Type     string   `json:"type"`
		Keyslots []string `json:"keyslots"`
	} `json:"tokens"`
}

func readLUKSDump(run runner, dev string) (luksDump, error) {
	out, err := run("cryptsetup", []string{"luksDump", "--dump-json-metadata", dev}, nil)
	if err != nil {
		return luksDump{}, err
	}
	var d luksDump
	if err := json.Unmarshal(out, &d); err != nil {
		return luksDump{}, fmt.Errorf("reading the LUKS2 header of %s: %w", dev, err)
	}
	return d, nil
}

// findLUKSDevices returns every LUKS container in `lsblk -P -p -o
// NAME,FSTYPE,LABEL` output, sorted by name. On Trusted Boot those are
// COS_OEM_LUKS and COS_PERSISTENT_LUKS, both sealed to the TPM.
func findLUKSDevices(lsblk string) []string {
	var devs []string
	for _, line := range strings.Split(lsblk, "\n") {
		kv := parsePairs(line)
		if kv["FSTYPE"] == "crypto_LUKS" && kv["NAME"] != "" {
			devs = append(devs, kv["NAME"])
		}
	}
	sort.Strings(devs)
	return devs
}

// newKeyslot returns the keyslot in after that is not in before.
func newKeyslot(before, after luksDump) (string, error) {
	var added []string
	for s := range after.Keyslots {
		if _, ok := before.Keyslots[s]; !ok {
			added = append(added, s)
		}
	}
	if len(added) != 1 {
		return "", fmt.Errorf("expected one new keyslot, found %d", len(added))
	}
	return added[0], nil
}

// enrollRecoveryKey adds key as a keyslot on every LUKS container of disk,
// right after kairos-agent installed and closed them, and marks the slot
// with a systemd-recovery token.
//
// cryptsetup unlocks each container with its systemd-tpm2 token, so the
// install medium's TPM unseals the key the installed system will use: the
// medium boots the same signed UKI, so PCR 11 matches the sealed policy.
// The key is 256 random bits, so the slot uses PBKDF2 at the minimum cost
// (1000 iterations): a slow KDF only protects a guessable secret, and
// argon2's memory cost would only make the boot prompt slower on a small
// machine. log receives progress lines, never the key.
func enrollRecoveryKey(run runner, disk, key string, log func(string)) error {
	return enrollRecoveryKeyWith(run, disk, key, nil, log)
}

// enrollRecoveryKeyWith is enrollRecoveryKey for a container unlocked by a
// passphrase: unlockFile holds it (the 0600 passphrase hand-off of the
// normal edition), and cryptsetup reads the existing key from there instead
// of the TPM token. nil keeps the TPM.
func enrollRecoveryKeyWith(run runner, disk, key string, unlockFile *string, log func(string)) error {
	log("hadron: adding the recovery key")
	_, _ = run("udevadm", []string{"settle"}, nil)
	out, err := run("lsblk", []string{"-P", "-p", "-o", "NAME,FSTYPE,LABEL", disk}, nil)
	if err != nil {
		return err
	}
	devs := findLUKSDevices(string(out))
	if len(devs) == 0 {
		return fmt.Errorf("no LUKS container on %s to add the recovery key to", disk)
	}
	if err := writePassphraseFile(recoveryKeyFile, key); err != nil {
		return err
	}
	defer removePassphraseFile(recoveryKeyFile)

	for _, dev := range devs {
		before, err := readLUKSDump(run, dev)
		if err != nil {
			return err
		}
		unlock := []string{"--key-file", ""}
		if unlockFile != nil {
			unlock[1] = *unlockFile
		} else {
			if !hasToken(before, "systemd-tpm2") {
				return fmt.Errorf("%s has no systemd-tpm2 token: is this a Trusted Boot install?", dev)
			}
			unlock = []string{"--token-type", "systemd-tpm2"}
		}
		args := append([]string{"luksAddKey", "--batch-mode"}, unlock...)
		args = append(args, "--pbkdf", "pbkdf2", "--hash", "sha512", "--pbkdf-force-iterations", "1000",
			dev, recoveryKeyFile)
		if _, err := run("cryptsetup", args, nil); err != nil {
			return fmt.Errorf("adding the recovery key to %s: %w", dev, err)
		}
		after, err := readLUKSDump(run, dev)
		if err != nil {
			return err
		}
		slot, err := newKeyslot(before, after)
		if err != nil {
			return fmt.Errorf("%s: %w", dev, err)
		}
		token, _ := json.Marshal(map[string]any{"type": recoveryTokenType, "keyslots": []string{slot}})
		if _, err := run("cryptsetup", []string{"token", "import", dev}, token); err != nil {
			return fmt.Errorf("marking keyslot %s of %s as the recovery key: %w", slot, dev, err)
		}
		// Only the typed key, no token: this is what the boot prompt does.
		if _, err := run("cryptsetup", []string{"open", "--test-passphrase", "--disable-external-tokens",
			"--key-slot", slot, "--key-file", "-", dev}, []byte(key)); err != nil {
			return fmt.Errorf("the recovery key does not open %s: %w", dev, err)
		}
		log("hadron: recovery key added to " + dev + " (keyslot " + slot + ")")
	}
	return nil
}

func hasToken(d luksDump, typ string) bool {
	for _, t := range d.Tokens {
		if t.Type == typ {
			return true
		}
	}
	return false
}

// recoveryGroupToConfirm picks the group the person types back, from r.
func recoveryGroupToConfirm(r io.Reader) int {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0
	}
	return int(b[0]) % recoveryKeyGroups
}

// errRecoveryKeyNonInteractive refuses HADRON_RECOVERY_KEY in the
// unattended installer: the key must be shown to a person, once.
var errRecoveryKeyNonInteractive = errors.New("HADRON_RECOVERY_KEY: the recovery key is shown once on screen, so it needs the interactive installer")

// ordinal is "1st", "2nd" ... for the confirmation prompt.
func ordinal(n int) string {
	suffix := "th"
	switch n % 10 {
	case 1:
		suffix = "st"
	case 2:
		suffix = "nd"
	case 3:
		suffix = "rd"
	}
	if n%100 >= 11 && n%100 <= 13 {
		suffix = "th"
	}
	return strconv.Itoa(n) + suffix
}
