package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Recovery keys of the disk encryption, run as root by hadron-updated for
// `hadron-recovery`. See the Trusted Boot design note,
// docs/superpowers/specs/2026-11-07-trusted-boot-edition-design.md.
//
// A recovery key is a LUKS2 keyslot with a systemd-recovery token next to it,
// the way systemd-cryptenroll --recovery-key makes one and the installer
// does (install-ui/recoverykey.go). On Trusted Boot the containers are
// COS_OEM_LUKS and COS_PERSISTENT_LUKS, each with a TPM keyslot
// (systemd-tpm2 token); on the normal edition COS_PERSISTENT_LUKS has the
// passphrase keyslot.
//
// No secret is ever an argument or an environment variable of a command:
// they reach cryptsetup and systemd-cryptenroll through 0600 files in a 0700
// directory under /run, deleted when the operation ends.

const (
	tpmTokenType      = "systemd-tpm2"
	recoveryTokenType = "systemd-recovery"
)

// The files kcrypt's TPM enrollment uses on a UKI boot (sdk/kcrypt/lock.go):
// the public half of the PCR signing key and the signatures, both from the
// booted UKI, and the TPM's storage root key. The reseal passes the same.
var (
	tpmPCRPublicKey = "/run/systemd/tpm2-pcr-public-key.pem"
	tpmPCRSignature = "/run/systemd/tpm2-pcr-signature.json"
	tpmSRKPublicKey = "/run/systemd/tpm2-srk-public-key.tpm2b_public"
	recoveryRunDir  = "/run/hadron-recovery"
	procCmdline     = "/proc/cmdline"
)

// Editions, as `hadron-recovery status` names them.
const (
	editionTrustedBoot = "trusted-boot"
	editionNormal      = "normal"
)

// cmdRunner runs one command with stdin and returns stdout. The seam the
// tests replace.
type cmdRunner func(name string, args []string, stdin []byte) ([]byte, error)

func execCmd(name string, args []string, stdin []byte) ([]byte, error) {
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

// recovery is the root side of hadron-recovery.
type recovery struct {
	run cmdRunner
}

// ContainerStatus describes one encrypted container. It holds no secret.
type ContainerStatus struct {
	Label      string
	Device     string
	TPM        int  // keyslots with a systemd-tpm2 token
	Recovery   int  // keyslots with a systemd-recovery token
	Passphrase int  // keyslots no token claims: a typed passphrase
	TPMWorks   bool // the TPM unseals the key now (only when TPM > 0)
}

// RecoveryStatus is what `hadron-recovery status` shows.
type RecoveryStatus struct {
	Edition    string
	Containers []ContainerStatus
}

var errNoContainers = errors.New("this system has no encrypted disk")

// edition reads the kernel command line: only a UKI boot has
// rd.immucore.uki, and the UKI's command line is signed.
func edition() string {
	b, err := os.ReadFile(procCmdline)
	if err == nil && bytes.Contains(b, []byte("rd.immucore.uki")) {
		return editionTrustedBoot
	}
	return editionNormal
}

// containers lists the Kairos LUKS containers: crypto_LUKS partitions whose
// label starts with COS_.
func (r recovery) containers() ([]ContainerStatus, error) {
	out, err := r.run("lsblk", []string{"-P", "-p", "-o", "NAME,FSTYPE,LABEL"}, nil)
	if err != nil {
		return nil, err
	}
	var cs []ContainerStatus
	for _, line := range strings.Split(string(out), "\n") {
		kv := lsblkPairs(line)
		if kv["FSTYPE"] == "crypto_LUKS" && strings.HasPrefix(kv["LABEL"], "COS_") {
			cs = append(cs, ContainerStatus{Label: kv["LABEL"], Device: kv["NAME"]})
		}
	}
	if len(cs) == 0 {
		return nil, errNoContainers
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Label < cs[j].Label })
	return cs, nil
}

// lsblkPairs reads one line of lsblk -P: KEY="value" KEY="value".
func lsblkPairs(line string) map[string]string {
	kv := map[string]string{}
	for line = strings.TrimSpace(line); line != ""; line = strings.TrimSpace(line) {
		eq := strings.Index(line, `="`)
		if eq < 0 {
			break
		}
		key, rest := line[:eq], line[eq+2:]
		end := strings.Index(rest, `"`)
		if end < 0 {
			break
		}
		kv[key] = rest[:end]
		line = rest[end+1:]
	}
	return kv
}

// luksHeader is the part of `cryptsetup luksDump --dump-json-metadata` read here.
type luksHeader struct {
	Keyslots map[string]json.RawMessage `json:"keyslots"`
	Tokens   map[string]struct {
		Type     string   `json:"type"`
		Keyslots []string `json:"keyslots"`
	} `json:"tokens"`
}

func (r recovery) header(dev string) (luksHeader, error) {
	out, err := r.run("cryptsetup", []string{"luksDump", "--dump-json-metadata", dev}, nil)
	if err != nil {
		return luksHeader{}, err
	}
	var h luksHeader
	if err := json.Unmarshal(out, &h); err != nil {
		return luksHeader{}, fmt.Errorf("reading the LUKS2 header of %s: %w", dev, err)
	}
	return h, nil
}

// slotsOf returns the keyslots claimed by tokens of type typ, and the ids of
// those tokens.
func (h luksHeader) slotsOf(typ string) (slots, tokens []string) {
	for id, t := range h.Tokens {
		if t.Type == typ {
			slots = append(slots, t.Keyslots...)
			tokens = append(tokens, id)
		}
	}
	sort.Strings(slots)
	sort.Strings(tokens)
	return slots, tokens
}

func (h luksHeader) unclaimed() int {
	claimed := map[string]bool{}
	for _, t := range h.Tokens {
		for _, s := range t.Keyslots {
			claimed[s] = true
		}
	}
	n := 0
	for s := range h.Keyslots {
		if !claimed[s] {
			n++
		}
	}
	return n
}

// tpmWorks asks cryptsetup to unseal the key through the TPM token and test
// it, without opening anything.
func (r recovery) tpmWorks(dev string) bool {
	_, err := r.run("cryptsetup", []string{"open", "--test-passphrase", "--token-only",
		"--token-type", tpmTokenType, dev}, nil)
	return err == nil
}

// Status reports the edition and every container's keyslots. No secrets.
func (r recovery) Status() (RecoveryStatus, error) {
	st := RecoveryStatus{Edition: edition()}
	cs, err := r.containers()
	if err != nil {
		return st, err
	}
	for i := range cs {
		h, err := r.header(cs[i].Device)
		if err != nil {
			return st, err
		}
		tpm, _ := h.slotsOf(tpmTokenType)
		rec, _ := h.slotsOf(recoveryTokenType)
		cs[i].TPM, cs[i].Recovery, cs[i].Passphrase = len(tpm), len(rec), h.unclaimed()
		if cs[i].TPM > 0 {
			cs[i].TPMWorks = r.tpmWorks(cs[i].Device)
		}
	}
	st.Containers = cs
	return st, nil
}

// secretFiles writes secrets to 0600 files in a fresh 0700 directory under
// recoveryRunDir. The returned func deletes them.
func secretFiles(secrets ...string) ([]string, func(), error) {
	if err := os.MkdirAll(recoveryRunDir, 0o700); err != nil {
		return nil, func() {}, err
	}
	dir, err := os.MkdirTemp(recoveryRunDir, "op-")
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	var paths []string
	for i, s := range secrets {
		p := filepath.Join(dir, fmt.Sprintf("%d", i))
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		paths = append(paths, p)
	}
	return paths, cleanup, nil
}

// testSecret reports whether secret opens dev, typed or, when it reads as a
// recovery key, in the key's canonical form. It returns the form that works.
func (r recovery) testSecret(dev, secret string) (string, bool) {
	try := []string{secret}
	if k := canonicalRecoveryKey(secret); k != "" && k != secret {
		try = append(try, k)
	}
	for _, s := range try {
		if _, err := r.run("cryptsetup", []string{"open", "--test-passphrase", "--disable-external-tokens",
			"--key-file", "-", dev}, []byte(s)); err == nil {
			return s, true
		}
	}
	return "", false
}

// Reseal puts a new TPM keyslot on every container of a Trusted Boot system,
// sealed to this machine's TPM the way kcrypt seals at install, in place of
// the stale one. secret is the recovery key: a container the TPM still
// unlocks is left alone, and one it does not needs the key.
//
// The stale TPM keyslot goes first. systemd-cryptenroll skips an enrollment
// whose PCR set is already enrolled ("executing no operation"), and a stale
// slot has the same PCR set as the new one. The recovery key is checked to
// open the container before the wipe, so the container always keeps a key.
func (r recovery) Reseal(secret string) error {
	if edition() != editionTrustedBoot {
		return errors.New("reseal is for Trusted Boot: this system's disk is not sealed to a TPM")
	}
	cs, err := r.containers()
	if err != nil {
		return err
	}
	resealed := 0
	for _, c := range cs {
		if r.tpmWorks(c.Device) {
			continue
		}
		if secret == "" {
			return fmt.Errorf("the TPM does not unlock %s: type the recovery key", c.Label)
		}
		s, ok := r.testSecret(c.Device, secret)
		if !ok {
			return fmt.Errorf("the recovery key does not open %s", c.Label)
		}
		if err := r.resealOne(c, s); err != nil {
			return err
		}
		resealed++
	}
	if resealed == 0 {
		return errNothingToReseal
	}
	return nil
}

// errNothingToReseal: the TPM unlocks every container already.
var errNothingToReseal = errors.New("the TPM already unlocks the disk; there is nothing to reseal")

func (r recovery) resealOne(c ContainerStatus, key string) error {
	paths, cleanup, err := secretFiles(key)
	if err != nil {
		return err
	}
	defer cleanup()
	unlock := "--unlock-key-file=" + paths[0]
	if _, err := r.run("systemd-cryptenroll", []string{unlock, "--wipe-slot=tpm2", c.Device}, nil); err != nil {
		return fmt.Errorf("removing the stale TPM keyslot of %s: %w", c.Label, err)
	}
	args := append([]string{unlock}, resealArgs()...)
	if _, err := r.run("systemd-cryptenroll", append(args, c.Device), nil); err != nil {
		return fmt.Errorf("sealing %s to the TPM (the recovery key still opens it): %w", c.Label, err)
	}
	h, err := r.header(c.Device)
	if err != nil {
		return err
	}
	if slots, _ := h.slotsOf(tpmTokenType); len(slots) != 1 {
		return fmt.Errorf("%s has %d TPM keyslots after the reseal, want 1", c.Label, len(slots))
	}
	if !r.tpmWorks(c.Device) {
		return fmt.Errorf("the new TPM keyslot of %s does not unlock it", c.Label)
	}
	return nil
}

// resealArgs are kcrypt's TPM enrollment options (sdk/kcrypt/lock.go) with
// Hadron's defaults: the PCR policy signed with the UKI's key over PCR 11,
// and no PCR bound by value.
//
// With the TPM's storage root key on disk (systemd-tpm2-setup writes it at
// every boot, for the TPM as it is now) it seals to that key, as kcrypt does;
// systemd-cryptenroll refuses --tpm2-device next to --tpm2-device-key.
// Without it, it talks to the TPM.
func resealArgs() []string {
	args := []string{"--tpm2-device=auto"}
	if _, err := os.Stat(tpmSRKPublicKey); err == nil {
		args = []string{"--tpm2-device-key=" + tpmSRKPublicKey}
	}
	return append(args,
		"--tpm2-public-key="+tpmPCRPublicKey,
		"--tpm2-public-key-pcrs=11",
		"--tpm2-pcrs=",
		"--tpm2-signature="+tpmPCRSignature,
	)
}

// NewKey adds newKey as the recovery key of every container, then removes
// the recovery keyslots it replaces, only once newKey has opened its own
// slot. secret authorizes it: the passphrase or the current recovery key, or
// empty to use the TPM on Trusted Boot.
func (r recovery) NewKey(secret, newKey string) error {
	if !recoveryKeyShape.MatchString(newKey) {
		return errors.New("the new recovery key is not 8 groups of 8 modhex letters")
	}
	cs, err := r.containers()
	if err != nil {
		return err
	}
	// Check every container opens before changing any.
	resolved := map[string]string{}
	for _, c := range cs {
		if secret == "" {
			if !r.tpmWorks(c.Device) {
				return fmt.Errorf("the TPM does not unlock %s: type the passphrase or recovery key", c.Label)
			}
			continue
		}
		s, ok := r.testSecret(c.Device, secret)
		if !ok {
			return fmt.Errorf("what you typed does not open %s", c.Label)
		}
		resolved[c.Device] = s
	}
	for _, c := range cs {
		if err := r.replaceRecoveryKey(c, resolved[c.Device], newKey); err != nil {
			return fmt.Errorf("%s: %w", c.Label, err)
		}
	}
	return nil
}

func (r recovery) replaceRecoveryKey(c ContainerStatus, secret, newKey string) error {
	before, err := r.header(c.Device)
	if err != nil {
		return err
	}
	oldSlots, oldTokens := before.slotsOf(recoveryTokenType)

	paths, cleanup, err := secretFiles(newKey, secret)
	if err != nil {
		return err
	}
	defer cleanup()
	unlock := []string{"--token-type", tpmTokenType}
	if secret != "" {
		unlock = []string{"--key-file", paths[1]}
	}
	args := append([]string{"luksAddKey", "--batch-mode"}, unlock...)
	args = append(args, "--pbkdf", "pbkdf2", "--hash", "sha512", "--pbkdf-force-iterations", "1000",
		c.Device, paths[0])
	if _, err := r.run("cryptsetup", args, nil); err != nil {
		return fmt.Errorf("adding the new recovery key: %w", err)
	}
	after, err := r.header(c.Device)
	if err != nil {
		return err
	}
	slot, err := addedSlot(before, after)
	if err != nil {
		return err
	}
	token, _ := json.Marshal(map[string]any{"type": recoveryTokenType, "keyslots": []string{slot}})
	if _, err := r.run("cryptsetup", []string{"token", "import", c.Device}, token); err != nil {
		return fmt.Errorf("marking keyslot %s as the recovery key: %w", slot, err)
	}
	if _, err := r.run("cryptsetup", []string{"open", "--test-passphrase", "--disable-external-tokens",
		"--key-slot", slot, "--key-file", "-", c.Device}, []byte(newKey)); err != nil {
		return fmt.Errorf("the new recovery key does not open keyslot %s; the old one is kept: %w", slot, err)
	}
	// Only now does the old key go: its tokens first, so no token ever
	// points at a slot that is gone.
	for _, id := range oldTokens {
		if _, err := r.run("cryptsetup", []string{"token", "remove", "--token-id", id, c.Device}, nil); err != nil {
			return fmt.Errorf("removing the old recovery key token %s: %w", id, err)
		}
	}
	for _, s := range oldSlots {
		if s == slot {
			continue
		}
		if _, err := r.run("cryptsetup", []string{"luksKillSlot", "--batch-mode", c.Device, s}, nil); err != nil {
			return fmt.Errorf("removing the old recovery keyslot %s: %w", s, err)
		}
	}
	return nil
}

func addedSlot(before, after luksHeader) (string, error) {
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

// The recovery key format, as in install-ui/recoverykey.go and systemd: 32
// random bytes, each nibble one of 16 modhex letters, 8 groups of 8.
const modhexAlphabet = "cbdefghijklnrtuv"

var recoveryKeyShape = regexp.MustCompile(`^([cbdefghijklnrtuv]{8}-){7}[cbdefghijklnrtuv]{8}$`)

func generateRecoveryKey(r io.Reader) (string, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("reading random bytes for the recovery key: %w", err)
	}
	var b strings.Builder
	for i, c := range buf {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(modhexAlphabet[c>>4])
		b.WriteByte(modhexAlphabet[c&0x0f])
	}
	return b.String(), nil
}

func newRecoveryKey() (string, error) { return generateRecoveryKey(rand.Reader) }

// canonicalRecoveryKey returns s as a recovery key in its canonical form
// (lower case, dashes), or "" when s is not 64 modhex letters once spaces
// and dashes are dropped.
func canonicalRecoveryKey(s string) string {
	var chars []rune
	for _, r := range strings.ToLower(s) {
		switch {
		case r == '-' || r == ' ' || r == '\t':
		case strings.ContainsRune(modhexAlphabet, r):
			chars = append(chars, r)
		default:
			return ""
		}
	}
	if len(chars) != 64 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < 64; i += 8 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(string(chars[i : i+8]))
	}
	return b.String()
}
