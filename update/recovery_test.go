package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/godbus/dbus/v5"
)

const testKey = "cbdefghi-jklnrtuv-cbdefghi-jklnrtuv-cbdefghi-jklnrtuv-cbdefghi-jklnrtuv"

// fakeDisk is a system with LUKS containers. Each one has keyslots and
// tokens; luksAddKey adds a slot, token import/remove and luksKillSlot edit
// them. open --test-passphrase accepts the slots' secrets.
type fakeLUKS struct {
	slots  map[string]string // slot -> secret ("" for a TPM slot)
	tokens map[string]tok
	tpmOK  bool
}

type tok struct {
	Type     string   `json:"type"`
	Keyslots []string `json:"keyslots"`
}

type fakeRecoveryDisk struct {
	mu     sync.Mutex
	devs   map[string]*fakeLUKS
	labels map[string]string
	calls  []string
	failOn string
	files  map[string]string // what each secret file held when read
}

func (f *fakeRecoveryDisk) run(name string, args []string, stdin []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if f.failOn != "" && strings.Contains(name+" "+strings.Join(args, " "), f.failOn) {
		return nil, errors.New("boom")
	}
	dev := ""
	if len(args) > 0 {
		dev = args[len(args)-1]
	}
	switch {
	case name == "lsblk":
		var b strings.Builder
		b.WriteString("NAME=\"/dev/vda\" FSTYPE=\"\" LABEL=\"\"\n")
		for d, l := range f.labels {
			fmt.Fprintf(&b, "NAME=%q FSTYPE=\"crypto_LUKS\" LABEL=%q\n", d, l)
		}
		return []byte(b.String()), nil
	case name == "cryptsetup" && args[0] == "luksDump":
		l := f.devs[dev]
		ks := map[string]any{}
		for s := range l.slots {
			ks[s] = map[string]any{"type": "luks2"}
		}
		return json.Marshal(map[string]any{"keyslots": ks, "tokens": l.tokens})
	case name == "cryptsetup" && args[0] == "open":
		l := f.devs[dev]
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "--token-only") {
			if l.tpmOK {
				return nil, nil
			}
			return nil, errors.New("TPM refused")
		}
		want := string(stdin)
		for s, secret := range l.slots {
			if strings.Contains(joined, "--key-slot ") && !strings.Contains(joined, "--key-slot "+s+" ") {
				continue
			}
			if secret != "" && secret == want {
				return nil, nil
			}
		}
		return nil, errors.New("No key available with this passphrase")
	case name == "cryptsetup" && args[0] == "luksAddKey":
		l := f.devs[args[len(args)-2]]
		newFile := args[len(args)-1]
		b, _ := os.ReadFile(newFile)
		f.files[newFile] = string(b)
		// Find the unlock: a key file or the TPM token.
		ok := false
		for i, a := range args {
			if a == "--key-file" {
				old, _ := os.ReadFile(args[i+1])
				for _, s := range l.slots {
					if s != "" && s == string(old) {
						ok = true
					}
				}
			}
			if a == "--token-type" && l.tpmOK {
				ok = true
			}
		}
		if !ok {
			return nil, errors.New("No key available")
		}
		for i := 0; ; i++ {
			s := fmt.Sprint(i)
			if _, used := l.slots[s]; !used {
				l.slots[s] = string(b)
				break
			}
		}
	case name == "cryptsetup" && args[0] == "token" && args[1] == "import":
		l := f.devs[dev]
		var t tok
		if err := json.Unmarshal(stdin, &t); err != nil {
			return nil, err
		}
		for i := 0; ; i++ {
			id := fmt.Sprint(i)
			if _, used := l.tokens[id]; !used {
				l.tokens[id] = t
				break
			}
		}
	case name == "cryptsetup" && args[0] == "token" && args[1] == "remove":
		delete(f.devs[dev].tokens, args[3])
	case name == "cryptsetup" && args[0] == "luksKillSlot":
		l := f.devs[args[len(args)-2]]
		delete(l.slots, args[len(args)-1])
	case name == "systemd-cryptenroll":
		l := f.devs[dev]
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "--unlock-key-file="); ok {
				b, _ := os.ReadFile(v)
				f.files[v] = string(b)
			}
		}
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "--wipe-slot=tpm2") {
			// A wipe: the TPM slots and their tokens go.
			for id, t := range l.tokens {
				if t.Type == tpmTokenType {
					for _, s := range t.Keyslots {
						delete(l.slots, s)
					}
					delete(l.tokens, id)
				}
			}
			return nil, nil
		}
		// An enrollment, skipped like systemd's when a TPM slot is there.
		for _, t := range l.tokens {
			if t.Type == tpmTokenType {
				return nil, nil
			}
		}
		l.slots["9"] = ""
		l.tokens["9"] = tok{Type: tpmTokenType, Keyslots: []string{"9"}}
		l.tpmOK = true
	}
	return nil, nil
}

// trustedBootDisk: OEM and persistent, a TPM slot (1) and a recovery slot
// (0) each, the TPM cleared so it refuses.
func trustedBootDisk() *fakeRecoveryDisk {
	mk := func() *fakeLUKS {
		return &fakeLUKS{
			slots:  map[string]string{"0": testKey, "1": ""},
			tokens: map[string]tok{"0": {tpmTokenType, []string{"1"}}, "1": {recoveryTokenType, []string{"0"}}},
		}
	}
	return &fakeRecoveryDisk{
		devs:   map[string]*fakeLUKS{"/dev/vda2": mk(), "/dev/vda3": mk()},
		labels: map[string]string{"/dev/vda2": "COS_OEM_LUKS", "/dev/vda3": "COS_PERSISTENT_LUKS"},
		files:  map[string]string{},
	}
}

func normalDisk() *fakeRecoveryDisk {
	return &fakeRecoveryDisk{
		devs: map[string]*fakeLUKS{"/dev/vda5": {
			slots:  map[string]string{"0": "correct horse battery", "1": testKey},
			tokens: map[string]tok{"0": {recoveryTokenType, []string{"1"}}},
		}},
		labels: map[string]string{"/dev/vda5": "COS_PERSISTENT_LUKS"},
		files:  map[string]string{},
	}
}

func recoveryEnv(t *testing.T, trusted bool) {
	t.Helper()
	dir := t.TempDir()
	cmdline := filepath.Join(dir, "cmdline")
	line := "console=tty1 root=LABEL=COS_ACTIVE"
	if trusted {
		line = "console=tty1 rd.immucore.uki"
	}
	os.WriteFile(cmdline, []byte(line+"\n"), 0o644)
	oldC, oldR, oldS := procCmdline, recoveryRunDir, tpmSRKPublicKey
	procCmdline, recoveryRunDir, tpmSRKPublicKey = cmdline, filepath.Join(dir, "run"), filepath.Join(dir, "srk")
	t.Cleanup(func() { procCmdline, recoveryRunDir, tpmSRKPublicKey = oldC, oldR, oldS })
}

func TestRecoveryStatus(t *testing.T) {
	recoveryEnv(t, true)
	f := trustedBootDisk()
	st, err := recovery{run: f.run}.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Edition != editionTrustedBoot || len(st.Containers) != 2 {
		t.Fatalf("%+v", st)
	}
	c := st.Containers[0]
	if c.Label != "COS_OEM_LUKS" || c.TPM != 1 || c.Recovery != 1 || c.Passphrase != 0 || c.TPMWorks {
		t.Fatalf("%+v", c)
	}

	recoveryEnv(t, false)
	st, err = recovery{run: normalDisk().run}.Status()
	if err != nil || st.Edition != editionNormal || st.Containers[0].Passphrase != 1 || st.Containers[0].Recovery != 1 {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestResealWithTheRecoveryKey(t *testing.T) {
	recoveryEnv(t, true)
	f := trustedBootDisk()
	r := recovery{run: f.run}
	// Typed without dashes and in upper case: the canonical form is used.
	if err := r.Reseal(strings.ToUpper(strings.ReplaceAll(testKey, "-", ""))); err != nil {
		t.Fatal(err)
	}
	for dev, l := range f.devs {
		h, _ := r.header(dev)
		if tpm, _ := h.slotsOf(tpmTokenType); len(tpm) != 1 || tpm[0] != "9" || !l.tpmOK {
			t.Errorf("%s: TPM slots %v", dev, tpm)
		}
		if rec, _ := h.slotsOf(recoveryTokenType); len(rec) != 1 {
			t.Errorf("%s: the recovery key went away: %v", dev, rec)
		}
	}
	var enrolls, wipes int
	enrolled := map[string]bool{}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "systemd-cryptenroll") && strings.Contains(c, "--wipe-slot=tpm2") {
			wipes++
			if enrolled[c[strings.LastIndex(c, " ")+1:]] {
				t.Errorf("a wipe after the enrollment: %q", c)
			}
			continue
		}
		if strings.HasPrefix(c, "systemd-cryptenroll") {
			enrolled[c[strings.LastIndex(c, " ")+1:]] = true
		}
		if strings.HasPrefix(c, "systemd-cryptenroll") {
			enrolls++
			for _, want := range []string{"--tpm2-device=auto", "--tpm2-public-key=/run/systemd/tpm2-pcr-public-key.pem",
				"--tpm2-public-key-pcrs=11", "--tpm2-pcrs= ", "--tpm2-signature=/run/systemd/tpm2-pcr-signature.json",
				"--unlock-key-file="} {
				if !strings.Contains(c+" ", want) {
					t.Errorf("%q lacks %q", c, want)
				}
			}
		}
		if strings.Contains(c, testKey) || strings.Contains(c, "CBDEFGHI") {
			t.Errorf("a command carries the key: %q", c)
		}
	}
	if enrolls != 2 || wipes != 2 {
		t.Errorf("%d enrollments and %d wipes, want one each on OEM and persistent", enrolls, wipes)
	}
	// A second reseal: the TPM works, nothing to do.
	if err := r.Reseal(testKey); !errors.Is(err, errNothingToReseal) {
		t.Errorf("reseal with a working TPM = %v", err)
	}
	for file, content := range f.files {
		if content != testKey {
			t.Errorf("%s held %q, want the canonical key", file, content)
		}
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("the key file %s is still there", file)
		}
	}
}

func TestResealRefusesAWrongKeyAndTheNormalEdition(t *testing.T) {
	recoveryEnv(t, true)
	f := trustedBootDisk()
	if err := (recovery{run: f.run}).Reseal("not the key"); err == nil {
		t.Fatal("a wrong key resealed")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "systemd-cryptenroll") {
			t.Fatal("enrolled with a wrong key")
		}
	}
	recoveryEnv(t, false)
	if err := (recovery{run: normalDisk().run}).Reseal(testKey); err == nil {
		t.Fatal("resealed a disk with no TPM")
	}
}

func TestNewKeyReplacesTheOldOnlyAfterTheNewWorks(t *testing.T) {
	recoveryEnv(t, false)
	newKey, _ := newRecoveryKey()

	f := normalDisk()
	f.failOn = "--key-slot 2"
	if err := (recovery{run: f.run}).NewKey("correct horse battery", newKey); err == nil {
		t.Fatal("the new key's check failed, yet NewKey succeeded")
	}
	if f.devs["/dev/vda5"].slots["1"] != testKey {
		t.Fatal("the old key was removed before the new one was checked")
	}

	f = normalDisk()
	r := recovery{run: f.run}
	if err := r.NewKey("correct horse battery", newKey); err != nil {
		t.Fatal(err)
	}
	l := f.devs["/dev/vda5"]
	var keys []string
	for _, s := range l.slots {
		keys = append(keys, s)
	}
	if len(l.slots) != 2 || l.slots["0"] != "correct horse battery" || l.slots["2"] != newKey {
		t.Fatalf("slots %v", keys)
	}
	h, _ := r.header("/dev/vda5")
	if rec, _ := h.slotsOf(recoveryTokenType); len(rec) != 1 || rec[0] != "2" {
		t.Fatalf("recovery tokens point at %v, want [2]", rec)
	}
	// The old recovery key authorizes too, typed without dashes.
	newer, _ := newRecoveryKey()
	if err := r.NewKey(strings.ReplaceAll(newKey, "-", ""), newer); err != nil {
		t.Fatalf("authorized by the current recovery key: %v", err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, newKey) || strings.Contains(c, "correct horse") {
			t.Errorf("a command carries a secret: %q", c)
		}
	}
}

func TestNewKeyWithTheTPM(t *testing.T) {
	recoveryEnv(t, true)
	f := trustedBootDisk()
	for _, l := range f.devs {
		l.tpmOK = true
	}
	newKey, _ := newRecoveryKey()
	if err := (recovery{run: f.run}).NewKey("", newKey); err != nil {
		t.Fatal(err)
	}
	for dev, l := range f.devs {
		if l.slots["0"] != "" && l.slots["0"] == testKey {
			t.Errorf("%s still has the old key", dev)
		}
		if l.slots["2"] != newKey {
			t.Errorf("%s: slots %v", dev, l.slots)
		}
	}
	if err := (recovery{run: f.run}).NewKey("", "not-a-key"); err == nil {
		t.Error("a malformed new key was accepted")
	}
}

func TestCanonicalAndGeneratedKeys(t *testing.T) {
	for _, in := range []string{testKey, strings.ToUpper(testKey), strings.ReplaceAll(testKey, "-", " ")} {
		if got := canonicalRecoveryKey(in); got != testKey {
			t.Errorf("canonicalRecoveryKey(%q) = %q", in, got)
		}
	}
	if canonicalRecoveryKey("correct horse battery") != "" {
		t.Error("a passphrase read as a key")
	}
	k, err := generateRecoveryKey(bytes.NewReader(make([]byte, 32)))
	if err != nil || k != "cccccccc-cccccccc-cccccccc-cccccccc-cccccccc-cccccccc-cccccccc-cccccccc" {
		t.Fatalf("%q %v", k, err)
	}
	a, _ := newRecoveryKey()
	b, _ := newRecoveryKey()
	if a == b || !recoveryKeyShape.MatchString(a) {
		t.Fatalf("%q %q", a, b)
	}
}

func TestReadSecretsFromAPipe(t *testing.T) {
	var got []string
	err := withSecrets([]string{"", testKey}, func(fd dbus.UnixFD) error {
		// The service's side gets its own copy, as over the bus.
		dup, err := dupFD(int(fd))
		if err != nil {
			return err
		}
		got, err = readSecrets(dbus.UnixFD(dup))
		return err
	})
	if err != nil || len(got) != 2 || got[0] != "" || got[1] != testKey {
		t.Fatalf("%q %v", got, err)
	}
}

// fakeRecoveryOps records what the service passed on.
type fakeRecoveryOps struct {
	mu      sync.Mutex
	reseal  []string
	newKeys [][2]string
}

func (f *fakeRecoveryOps) Status() (RecoveryStatus, error) {
	return RecoveryStatus{Edition: editionTrustedBoot, Containers: []ContainerStatus{{Label: "COS_OEM_LUKS", Device: "/dev/vda2", TPM: 1, Recovery: 1}}}, nil
}
func (f *fakeRecoveryOps) Reseal(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reseal = append(f.reseal, s)
	return nil
}
func (f *fakeRecoveryOps) NewKey(s, k string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newKeys = append(f.newKeys, [2]string{s, k})
	return nil
}

// Through a real bus: the secrets arrive intact over the fd, and the calls
// ask polkit for the recovery action.
func TestBusRecoveryPassesSecretsByFD(t *testing.T) {
	fo := &fakeRecoveryOps{}
	old := recoveryOps
	recoveryOps = fo
	t.Cleanup(func() { recoveryOps = old })
	daemonEnv(t, "v0.0.5")
	auth := &fakeAuth{allow: true}
	f := startService(t, NewDaemon(&fakeBackend{}), auth)
	conn, err := dbus.Connect(f.addr)
	if err != nil {
		t.Fatal(err)
	}
	c := &client{conn: conn, obj: conn.Object(busName, busPath)}
	defer c.Close()

	if err := c.RecoveryReseal(testKey); err != nil {
		t.Fatal(err)
	}
	newKey, _ := newRecoveryKey()
	if err := c.RecoveryNewKey("correct horse battery", newKey); err != nil {
		t.Fatal(err)
	}
	st, err := c.RecoveryStatus()
	if err != nil || st.Edition != editionTrustedBoot || st.Containers[0].TPM != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	if len(fo.reseal) != 1 || fo.reseal[0] != testKey || len(fo.newKeys) != 1 || fo.newKeys[0] != [2]string{"correct horse battery", newKey} {
		t.Fatalf("reseal %q newKeys %q", fo.reseal, fo.newKeys)
	}
	for _, q := range auth.questions() {
		if !strings.HasSuffix(q, " "+actionRecovery) {
			t.Fatalf("asked %q", q)
		}
	}
	if len(auth.questions()) != 3 {
		t.Fatalf("asked polkit %d times, want 3", len(auth.questions()))
	}

	// Refused: nothing reaches the backend.
	auth.mu.Lock()
	auth.allow = false
	auth.mu.Unlock()
	if err := c.RecoveryReseal(testKey); daemonErrName(err) != "NotAuthorized" {
		t.Fatalf("unauthorized reseal: %v", err)
	}
	if len(fo.reseal) != 1 {
		t.Fatal("an unauthorized reseal reached the backend")
	}
}

// fakeTerm answers lines and secrets in order and records the prompts.
type fakeTerm struct {
	lines, secrets []string
	prompts        []string
}

func (f *fakeTerm) Line(p string) (string, error) {
	f.prompts = append(f.prompts, p)
	if len(f.lines) == 0 {
		return "", errors.New("eof")
	}
	s := f.lines[0]
	f.lines = f.lines[1:]
	return s, nil
}

func (f *fakeTerm) Secret(p string) (string, error) {
	f.prompts = append(f.prompts, p)
	if len(f.secrets) == 0 {
		return "", errors.New("eof")
	}
	s := f.secrets[0]
	f.secrets = f.secrets[1:]
	return s, nil
}

type fakeRecoveryClient struct {
	st      RecoveryStatus
	reseal  []string
	newKeys [][2]string
}

func (f *fakeRecoveryClient) RecoveryStatus() (RecoveryStatus, error) { return f.st, nil }
func (f *fakeRecoveryClient) RecoveryReseal(s string) error {
	f.reseal = append(f.reseal, s)
	return nil
}
func (f *fakeRecoveryClient) RecoveryNewKey(s, k string) error {
	f.newKeys = append(f.newKeys, [2]string{s, k})
	return nil
}
func (f *fakeRecoveryClient) Close() {}

func cliEnv(t *testing.T, fc *fakeRecoveryClient, ft *fakeTerm) {
	t.Helper()
	oldD, oldT, oldK, oldG := dialRecovery, newTerminal, makeRecoveryKey, pickGroup
	dialRecovery = func() (recoveryClient, error) { return fc, nil }
	newTerminal = func(io.Writer) (terminal, error) { return ft, nil }
	makeRecoveryKey = func() (string, error) { return testKey, nil }
	pickGroup = func() int { return 3 }
	t.Cleanup(func() { dialRecovery, newTerminal, makeRecoveryKey, pickGroup = oldD, oldT, oldK, oldG })
}

func TestCLINewKeyShowsOnceAndConfirms(t *testing.T) {
	fc := &fakeRecoveryClient{st: RecoveryStatus{Edition: editionNormal,
		Containers: []ContainerStatus{{Label: "COS_PERSISTENT_LUKS", Device: "/dev/vda5", Passphrase: 1, Recovery: 1}}}}
	ft := &fakeTerm{secrets: []string{"correct horse battery"}, lines: []string{"", "cccccccc", "jklnrtuv"}}
	cliEnv(t, fc, ft)
	var out, errb bytes.Buffer
	if code := cmdRecovery([]string{"new-key"}, &out, &errb); code != exitOK {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	if strings.Count(out.String(), testKey) != 1 {
		t.Errorf("the key is shown %d times, want once", strings.Count(out.String(), testKey))
	}
	if !strings.Contains(out.String(), "7  c b d e f g h i    j k l n r t u v  8") {
		t.Errorf("no grouped key screen:\n%s", out.String())
	}
	if len(fc.newKeys) != 1 || fc.newKeys[0] != [2]string{"correct horse battery", testKey} {
		t.Fatalf("new keys %q", fc.newKeys)
	}
	if !strings.Contains(strings.Join(ft.prompts, "|"), "type group 4 of 8") {
		t.Errorf("prompts %q", ft.prompts)
	}
}

func TestCLINewKeyAbortsWithoutConfirmation(t *testing.T) {
	fc := &fakeRecoveryClient{st: RecoveryStatus{Edition: editionNormal}}
	ft := &fakeTerm{secrets: []string{"correct horse battery"}, lines: []string{"", "x", "y", "z"}}
	cliEnv(t, fc, ft)
	var out, errb bytes.Buffer
	if code := cmdRecovery([]string{"new-key"}, &out, &errb); code == exitOK || len(fc.newKeys) != 0 {
		t.Fatalf("code %d, new keys %q", code, fc.newKeys)
	}
}

func TestCLIStatusAndReseal(t *testing.T) {
	fc := &fakeRecoveryClient{st: RecoveryStatus{Edition: editionTrustedBoot, Containers: []ContainerStatus{
		{Label: "COS_OEM_LUKS", Device: "/dev/vda2", TPM: 1, Recovery: 1},
	}}}
	ft := &fakeTerm{secrets: []string{testKey}}
	cliEnv(t, fc, ft)
	var out, errb bytes.Buffer
	if code := cmdRecovery([]string{"status"}, &out, &errb); code != exitOK {
		t.Fatal(errb.String())
	}
	for _, want := range []string{"Edition: Trusted Boot (TPM)", "TPM 1, recovery key 1", "TPM unlock: FAILS"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if code := cmdRecovery([]string{"reseal"}, &out, &errb); code != exitOK || len(fc.reseal) != 1 || fc.reseal[0] != testKey {
		t.Fatalf("reseal: code %d, %q, %s", code, fc.reseal, errb.String())
	}
	if strings.Contains(out.String(), testKey) {
		t.Error("reseal echoed the key")
	}
}

func dupFD(fd int) (int, error) { return syscall.Dup(fd) }

func TestTrayOffersTheRecoveryKeyOnlyWithAnEncryptedDisk(t *testing.T) {
	old := diskEncrypted
	t.Cleanup(func() { diskEncrypted = old })
	diskEncrypted = func() bool { return false }
	if _, ok := find(BuildMenu(menuStatus(StateIdle), nil, "sway", gpuFull), "recovery"); ok {
		t.Error("a plain disk offers a recovery key")
	}
	diskEncrypted = func() bool { return true }
	if it, ok := find(BuildMenu(menuStatus(StateIdle), nil, "sway", gpuFull), "recovery"); !ok || it.Label != "Recovery key…" || !it.Enabled {
		t.Errorf("recovery = %+v (%v)", it, ok)
	}
}

func TestResealArgsUseTheSRKWhenThere(t *testing.T) {
	recoveryEnv(t, true)
	args := strings.Join(resealArgs(), " ")
	if !strings.Contains(args, "--tpm2-device=auto") || strings.Contains(args, "--tpm2-device-key") {
		t.Errorf("without the SRK file: %q", args)
	}
	os.WriteFile(tpmSRKPublicKey, []byte("srk"), 0o644)
	args = strings.Join(resealArgs(), " ")
	// systemd-cryptenroll refuses both at once ("Multiple operations").
	if strings.Contains(args, "--tpm2-device=auto") || !strings.Contains(args, "--tpm2-device-key="+tpmSRKPublicKey) {
		t.Errorf("with the SRK file: %q", args)
	}
}
