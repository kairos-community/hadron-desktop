# Installer TUI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the desktop installer's POSIX-sh prompts and raw `kairos-agent` log spew with a branded Go TUI: a wizard with back-navigation and a review gate, then a progress screen with a log toggle and a rescue shell.

**Architecture:** A standalone Go module `install-ui/` builds a static `CGO_ENABLED=0` binary to `/usr/bin/hadron-install-ui`. It runs `kairos-agent install` as a subprocess and derives progress by scraping the child's stdout against a substring table, because kairos-agent exposes no progress API. Pure logic (step parsing, cloud-config rendering, disk listing) is separated from bubbletea models so it can be unit-tested without a terminal. The existing `90_desktop_installer.yaml` drop-in is repointed at the new binary; the agent appliance keeps its audited shell installer and gains only the progress screen.

**Tech Stack:** Go 1.25 (`golang:1.25.0-alpine3.22`, matching `Dockerfile.agent`), bubbletea, bubbles, lipgloss, Docker BuildKit.

**Spec:** `docs/superpowers/specs/2026-07-22-boot-and-install-ux-design.md` (Part 3). Parts 1 and 2 are covered by `2026-07-22-boot-splash-and-iso-branding.md` and are independent of this plan.

## Global Constraints

- **VGA-16 colours only.** The installer runs on the kernel VT with `TERM=linux`, a 16-colour console. Use lipgloss colours `"4"` blue, `"8"` dark grey, `"12"` bright blue, `"14"` bright cyan, `"15"` white, and `"9"` red **reserved exclusively for a genuine halt**. Never use hex or 256-colour values.
- **ASCII-only spinners and progress glyphs** (`spinner.Line`, `#`, `.`). Box-drawing is acceptable only in the wordmark, which the `Lat2-Terminus16` console font covers.
- **The binary goes in `/usr/bin`**, never `/usr/local/bin` — `/usr/local` is shadowed by an empty overlay at runtime on Kairos.
- **These environment seams must keep working byte-for-byte.** They are consumed by existing tests and automation:
  `HADRON_INSTALL_NONINTERACTIVE`, `HADRON_USER`, `HADRON_PASS`, `HADRON_DISK`, `HADRON_HOSTNAME`, `HADRON_GITHUB`, `HADRON_INSTALL_CMD`, `HADRON_OEM_DIR`.
- **The generated `/oem/99_hadron-user.yaml` must stay semantically identical** to what `rootfs/usr/local/bin/hadron-install` produces today: `install` block with `device`/`auto`/`reboot`, `hostname`, a `users` entry with the seven groups `[admin, audio, video, render, input, bluetooth, seat, docker]`, optional `ssh_authorized_keys: [github:NAME]`, and a `stages.initramfs` entry writing `/etc/ly/save.ini`.
- **Password hashing shells out to `openssl passwd -6 -stdin`.** Do not vendor a SHA-512-crypt implementation.
- **Do not modify the agent appliance's safety logic.** `rootfs-agent/usr/local/bin/hadron-agent-install` gets exactly one change in this plan: its `INSTALL_CMD` default. The typed-`yes` gate, the `--noninteractive` seed re-verification, and every `HADRON_*` seam stay untouched.
- Go module path: `github.com/kairos-io/hadron-desktop/install-ui`.

---

### Task 1: Go module scaffold, step parser, and log ring

Pure logic with no terminal dependency. This task ends with a tested Go package and no UI.

**Files:**
- Create: `install-ui/go.mod`
- Create: `install-ui/steps.go`
- Create: `install-ui/steps_test.go`
- Create: `install-ui/ring.go`
- Create: `install-ui/ring_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces, in `package main`:
  - `type Step struct { Name string; Match string }`
  - `var Steps []Step` — 9 entries, index 0 is the initial state
  - `func AdvanceStep(line string, current int) int` — never regresses
  - `func Percent(step int) int` — 0..100
  - `type ring struct{...}`, `func newRing(max int) *ring`, `(*ring) add(string)`, `(*ring) lines() []string`, `(*ring) text() string`

- [ ] **Step 1: Initialise the module**

```bash
mkdir -p install-ui
cd install-ui && go mod init github.com/kairos-io/hadron-desktop/install-ui
```

Then pin the Go directive to the toolchain the build stage uses. Edit `install-ui/go.mod` so its `go` line reads:

```
go 1.25
```

- [ ] **Step 2: Write the failing step-parser test**

Create `install-ui/steps_test.go`. The three behaviours worth testing are the ones a naive implementation gets wrong: never regressing, picking the *highest* matching step when a line could match several, and clamping `Percent`.

```go
package main

import "testing"

func TestAdvanceStepMatchesKnownPhases(t *testing.T) {
	cases := []struct {
		line string
		want int
	}{
		{"Partitioning device /dev/vda", 1},
		{"Running stage: before-install", 2},
		{"Creating file system image active.img", 3},
		{"Installing GRUB to /dev/vda", 4},
		{"Copying recovery.img", 5},
		{"Copying passive.img", 6},
		{"Running after-install hook", 7},
		{"Finish Lifecycle hook", 8},
	}
	for _, c := range cases {
		if got := AdvanceStep(c.line, 0); got != c.want {
			t.Errorf("AdvanceStep(%q, 0) = %d, want %d", c.line, got, c.want)
		}
	}
}

func TestAdvanceStepNeverRegresses(t *testing.T) {
	// kairos-agent interleaves output; an early-phase line arriving after a late
	// one must not rewind the progress bar.
	if got := AdvanceStep("Partitioning device /dev/vda", 5); got != 5 {
		t.Errorf("AdvanceStep regressed to %d, want 5", got)
	}
}

func TestAdvanceStepIgnoresUnrelatedLines(t *testing.T) {
	if got := AdvanceStep("some unrelated chatter", 3); got != 3 {
		t.Errorf("AdvanceStep(%q, 3) = %d, want 3", "some unrelated chatter", got)
	}
}

func TestAdvanceStepPrefersHighestMatch(t *testing.T) {
	// A single line containing two markers must land on the later phase.
	line := "Running stage: before-install and Installing GRUB"
	if got := AdvanceStep(line, 0); got != 4 {
		t.Errorf("AdvanceStep(%q, 0) = %d, want 4", line, got)
	}
}

func TestPercentEndpointsAndClamping(t *testing.T) {
	if got := Percent(0); got != 0 {
		t.Errorf("Percent(0) = %d, want 0", got)
	}
	if got := Percent(len(Steps) - 1); got != 100 {
		t.Errorf("Percent(last) = %d, want 100", got)
	}
	if got := Percent(-5); got != 0 {
		t.Errorf("Percent(-5) = %d, want 0", got)
	}
	if got := Percent(9999); got != 100 {
		t.Errorf("Percent(9999) = %d, want 100", got)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
cd install-ui && go test ./...
```

Expected: compile failure — `undefined: AdvanceStep`, `undefined: Percent`, `undefined: Steps`.

- [ ] **Step 4: Implement the step parser**

Create `install-ui/steps.go`:

```go
package main

import "strings"

// Step is one phase of the kairos-agent install, identified by a stable substring
// that appears in the installer's log output. Index 0 is the initial state (no
// match). Mirrors kairos-agent internal/agent/TUIconstants.go.
//
// kairos-agent exposes no progress API, so scraping its stdout is the only way to
// drive a progress bar. These substrings are best-effort and deliberately isolated
// in this file so they are easy to retune after a boot test — a stale substring
// degrades to a stalled progress bar, never to a failed install.
type Step struct {
	Name  string
	Match string
}

var Steps = []Step{
	{"Preparing", ""},
	{"Partitioning disk", "Partitioning device"},
	{"Running before-install", "Running stage: before-install"},
	{"Installing active system", "Creating file system image"},
	{"Configuring bootloader", "Installing GRUB"},
	{"Creating recovery image", "recovery.img"},
	{"Creating passive image", "passive.img"},
	{"Running after-install", "Running after-install hook"},
	{"Complete", "Finish Lifecycle hook"},
}

// AdvanceStep returns the step index after observing line, never regressing below
// current. It picks the highest-indexed step (beyond current) whose Match is in
// line, so a line mentioning two phases lands on the later one.
func AdvanceStep(line string, current int) int {
	for i := len(Steps) - 1; i > current; i-- {
		if Steps[i].Match != "" && strings.Contains(line, Steps[i].Match) {
			return i
		}
	}
	return current
}

// Percent maps a step index to a 0..100 progress value, clamping out-of-range
// input rather than panicking — the caller is a render path.
func Percent(step int) int {
	last := len(Steps) - 1
	if step < 0 {
		step = 0
	}
	if step > last {
		step = last
	}
	return step * 100 / last
}
```

- [ ] **Step 5: Run the test to verify it passes**

```bash
cd install-ui && go test ./... -run 'TestAdvanceStep|TestPercent' -v
```

Expected: five `PASS` lines, `ok`.

- [ ] **Step 6: Write the failing ring test**

Create `install-ui/ring_test.go`:

```go
package main

import "testing"

func TestRingKeepsMostRecentLines(t *testing.T) {
	r := newRing(3)
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		r.add(s)
	}
	got := r.lines()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	want := []string{"c", "d", "e"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("lines()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRingUnderCapacity(t *testing.T) {
	r := newRing(10)
	r.add("only")
	if got := r.text(); got != "only" {
		t.Errorf("text() = %q, want %q", got, "only")
	}
}

func TestRingEmpty(t *testing.T) {
	r := newRing(10)
	if got := r.text(); got != "" {
		t.Errorf("text() = %q, want empty", got)
	}
}
```

- [ ] **Step 7: Run the test to verify it fails**

```bash
cd install-ui && go test ./... -run TestRing
```

Expected: compile failure — `undefined: newRing`.

- [ ] **Step 8: Implement the ring**

Create `install-ui/ring.go`:

```go
package main

import "strings"

// ring is a fixed-capacity FIFO of log lines backing the log viewport. Installs
// can emit tens of thousands of lines; the cap bounds memory on a live system
// whose /var is a tmpfs overlay.
type ring struct {
	buf []string
	max int
}

func newRing(max int) *ring { return &ring{max: max} }

func (r *ring) add(line string) {
	r.buf = append(r.buf, line)
	if len(r.buf) > r.max {
		copy(r.buf, r.buf[len(r.buf)-r.max:])
		r.buf = r.buf[:r.max]
	}
}

func (r *ring) lines() []string { return r.buf }

func (r *ring) text() string { return strings.Join(r.buf, "\n") }
```

- [ ] **Step 9: Run the test to verify it passes**

```bash
cd install-ui && go test ./... -v
```

Expected: all eight tests pass, `ok`.

- [ ] **Step 10: Commit**

```bash
git add install-ui/
git commit -m "feat(install-ui): step parser and log ring

Pure, terminal-free core for the installer TUI: a substring table mirroring
kairos-agent's TUIconstants.go to derive progress from its stdout, and a
capacity-bounded log ring."
```

---

### Task 2: Cloud-config renderer, disk lister, and config detection

The rest of the pure logic. Everything that decides *what gets written to disk* is tested here, before any of it is wired to a terminal.

**Files:**
- Create: `install-ui/config.go`
- Create: `install-ui/config_test.go`
- Create: `install-ui/disks.go`
- Create: `install-ui/disks_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces, in `package main`:
  - `type Choice struct { Hostname, Username, Password, Github, Disk string }`
  - `func RenderCloudConfig(c Choice, hash string) string`
  - `func ConfigPresent(oemDir, outPath string) (bool, error)`
  - `func HashPassword(plain string) (string, error)`
  - `type Disk struct { Path string; SizeGB int }`
  - `func ListDisks(sysBlock string) ([]Disk, error)`

- [ ] **Step 1: Write the failing cloud-config test**

Create `install-ui/config_test.go`. The critical assertions are the ones that would silently produce an unbootable or unloginable system.

```go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderCloudConfigContainsRequiredFields(t *testing.T) {
	c := Choice{Hostname: "box", Username: "ada", Disk: "/dev/vda"}
	out := RenderCloudConfig(c, "$6$abc$def")

	for _, want := range []string{
		"#cloud-config",
		"device: \"/dev/vda\"",
		"auto: true",
		"reboot: true",
		"hostname: \"box\"",
		"- name: \"ada\"",
		"passwd: '$6$abc$def'",
		"groups: [admin, audio, video, render, input, bluetooth, seat, docker]",
		"path: /etc/ly/save.ini",
		"user = ada",
		"session_index = 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered config missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderCloudConfigOmitsSSHWhenNoGithub(t *testing.T) {
	out := RenderCloudConfig(Choice{Hostname: "h", Username: "u", Disk: "/dev/sda"}, "x")
	if strings.Contains(out, "ssh_authorized_keys") {
		t.Errorf("expected no ssh_authorized_keys block:\n%s", out)
	}
}

func TestRenderCloudConfigIncludesSSHWhenGithubSet(t *testing.T) {
	out := RenderCloudConfig(Choice{Hostname: "h", Username: "u", Disk: "/dev/sda", Github: "ada"}, "x")
	if !strings.Contains(out, "ssh_authorized_keys:") || !strings.Contains(out, "- github:ada") {
		t.Errorf("expected github key import:\n%s", out)
	}
}

func TestConfigPresentDetectsUsersOrInstallBlock(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "99_hadron-user.yaml")

	// Empty dir: nothing present.
	if got, err := ConfigPresent(dir, out); err != nil || got {
		t.Fatalf("empty dir: got %v err %v, want false nil", got, err)
	}

	// Our own output file must be ignored, or a rerun would skip the wizard.
	if err := os.WriteFile(out, []byte("users:\n  - name: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ConfigPresent(dir, out); err != nil || got {
		t.Fatalf("own output file: got %v err %v, want false nil", got, err)
	}

	// A foreign config with a users: block counts.
	if err := os.WriteFile(filepath.Join(dir, "10_other.yaml"), []byte("users:\n  - name: y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ConfigPresent(dir, out); err != nil || !got {
		t.Fatalf("foreign users config: got %v err %v, want true nil", got, err)
	}
}

func TestConfigPresentIgnoresUnrelatedKeys(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "10_other.yaml"), []byte("stages:\n  boot: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ConfigPresent(dir, filepath.Join(dir, "99_hadron-user.yaml"))
	if err != nil || got {
		t.Fatalf("got %v err %v, want false nil", got, err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd install-ui && go test ./... -run 'TestRenderCloudConfig|TestConfigPresent'
```

Expected: compile failure — `undefined: Choice`, `undefined: RenderCloudConfig`, `undefined: ConfigPresent`.

- [ ] **Step 3: Implement config rendering and detection**

Create `install-ui/config.go`. The rendered YAML must stay semantically identical to what `rootfs/usr/local/bin/hadron-install` produces today.

```go
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Choice is everything the wizard collects. Password is plaintext and is never
// written anywhere — only its hash reaches disk.
type Choice struct {
	Hostname string
	Username string
	Password string
	Github   string
	Disk     string
}

// desktopGroups are the supplemental groups a desktop user needs: seat/input for
// logind and libinput, audio/video/render for PipeWire and DRM, bluetooth for
// BlueZ, docker for rootful docker, admin for sudo. Kept identical to the shell
// installer this replaces.
const desktopGroups = "[admin, audio, video, render, input, bluetooth, seat, docker]"

// RenderCloudConfig produces the Kairos cloud-config the installer drops in /oem
// for `kairos-agent install` to merge. hash is a crypt(3) SHA-512 string.
//
// The ly save.ini stage preseeds the login screen with this user and selects the
// desktop session, so first boot lands on a filled-in login rather than an empty
// one. session_index 2 matches the session ordering in the image.
func RenderCloudConfig(c Choice, hash string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("# Generated by the Hadron Desktop installer.\n")
	b.WriteString("install:\n")
	fmt.Fprintf(&b, "  device: %q\n", c.Disk)
	b.WriteString("  auto: true\n")
	b.WriteString("  reboot: true\n")
	fmt.Fprintf(&b, "hostname: %q\n", c.Hostname)
	b.WriteString("users:\n")
	fmt.Fprintf(&b, "  - name: %q\n", c.Username)
	fmt.Fprintf(&b, "    passwd: '%s'\n", hash)
	fmt.Fprintf(&b, "    groups: %s\n", desktopGroups)
	if c.Github != "" {
		b.WriteString("    ssh_authorized_keys:\n")
		fmt.Fprintf(&b, "      - github:%s\n", c.Github)
	}
	b.WriteString("stages:\n")
	b.WriteString("  initramfs:\n")
	b.WriteString("    - name: \"Hadron desktop default session\"\n")
	b.WriteString("      files:\n")
	b.WriteString("        - path: /etc/ly/save.ini\n")
	b.WriteString("          permissions: 0644\n")
	b.WriteString("          content: |\n")
	fmt.Fprintf(&b, "            user = %s\n", c.Username)
	b.WriteString("            session_index = 2\n")
	return b.String()
}

// topLevelKey matches a `users:` or `install:` key at the start of a line,
// allowing leading whitespace — the same shape the shell installer grepped for.
var topLevelKey = regexp.MustCompile(`(?m)^[[:space:]]*(users|install):`)

// ConfigPresent reports whether oemDir already holds an install config other than
// our own output file. If so the caller skips the wizard and installs unattended,
// preserving the shell installer's behaviour for seeded/automated installs.
func ConfigPresent(oemDir, outPath string) (bool, error) {
	var found bool
	for _, pat := range []string{"*.yaml", "*.yml"} {
		matches, err := filepath.Glob(filepath.Join(oemDir, pat))
		if err != nil {
			return false, err
		}
		for _, m := range matches {
			same, err := filepath.Abs(m)
			if err != nil {
				continue
			}
			want, err := filepath.Abs(outPath)
			if err == nil && same == want {
				continue // never count our own output
			}
			data, err := os.ReadFile(m)
			if err != nil {
				continue // unreadable file is not a config we can honour
			}
			if topLevelKey.Match(data) {
				found = true
			}
		}
	}
	return found, nil
}

// HashPassword shells out to openssl for a crypt(3) SHA-512 hash.
//
// Go's stdlib has no crypt(3), and openssl is already in the image. Vendoring a
// SHA-512-crypt implementation would put unaudited crypto in the one code path
// where a mistake locks the user out of their own machine.
func HashPassword(plain string) (string, error) {
	cmd := exec.Command("openssl", "passwd", "-6", "-stdin")
	cmd.Stdin = strings.NewReader(plain)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("openssl passwd: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
cd install-ui && go test ./... -run 'TestRenderCloudConfig|TestConfigPresent' -v
```

Expected: five `PASS` lines, `ok`.

- [ ] **Step 5: Write the failing disk-lister test**

Create `install-ui/disks_test.go`. Using a fake `/sys/block` keeps this hermetic.

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeSysBlock builds a /sys/block-shaped tree: one dir per device with a `size`
// file holding the count of 512-byte sectors.
func fakeSysBlock(t *testing.T, devs map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, size := range devs {
		d := filepath.Join(root, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "size"), []byte(size+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestListDisksSkipsPseudoDevices(t *testing.T) {
	// 41943040 sectors * 512 = 20 GiB
	root := fakeSysBlock(t, map[string]string{
		"vda":   "41943040",
		"loop0": "1024",
		"sr0":   "1024",
		"ram0":  "1024",
		"dm-0":  "1024",
		"fd0":   "1024",
	})
	got, err := ListDisks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d disks, want 1: %+v", len(got), got)
	}
	if got[0].Path != "/dev/vda" {
		t.Errorf("Path = %q, want /dev/vda", got[0].Path)
	}
	if got[0].SizeGB != 20 {
		t.Errorf("SizeGB = %d, want 20", got[0].SizeGB)
	}
}

func TestListDisksSkipsZeroSized(t *testing.T) {
	root := fakeSysBlock(t, map[string]string{"vda": "0", "vdb": "41943040"})
	got, err := ListDisks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "/dev/vdb" {
		t.Fatalf("got %+v, want only /dev/vdb", got)
	}
}

func TestListDisksEmpty(t *testing.T) {
	got, err := ListDisks(fakeSysBlock(t, map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
}
```

- [ ] **Step 6: Run the test to verify it fails**

```bash
cd install-ui && go test ./... -run TestListDisks
```

Expected: compile failure — `undefined: ListDisks`.

- [ ] **Step 7: Implement the disk lister**

Create `install-ui/disks.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Disk is a candidate install target.
type Disk struct {
	Path   string
	SizeGB int
}

// pseudoPrefixes are /sys/block entries that are never valid install targets:
// loopback, optical, ramdisks, device-mapper and floppies. Same exclusion list
// as the shell installer this replaces.
var pseudoPrefixes = []string{"loop", "sr", "ram", "dm-", "fd"}

// ListDisks enumerates whole disks under sysBlock (normally /sys/block). The
// `size` file is in 512-byte sectors regardless of the device's logical block
// size, so GB is sectors/2/1024/1024.
//
// sysBlock is a parameter rather than a constant so tests can point it at a
// fixture tree instead of the host's real block devices.
func ListDisks(sysBlock string) ([]Disk, error) {
	entries, err := os.ReadDir(sysBlock)
	if err != nil {
		return nil, err
	}
	var out []Disk
	for _, e := range entries {
		name := e.Name()
		if isPseudo(name) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(sysBlock, name, "size"))
		if err != nil {
			continue
		}
		sectors, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || sectors <= 0 {
			continue
		}
		out = append(out, Disk{
			Path:   "/dev/" + name,
			SizeGB: int(sectors / 2 / 1024 / 1024),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func isPseudo(name string) bool {
	for _, p := range pseudoPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 8: Run the full suite to verify it passes**

```bash
cd install-ui && go test ./... -v
```

Expected: all sixteen tests pass, `ok`.

- [ ] **Step 9: Commit**

```bash
git add install-ui/
git commit -m "feat(install-ui): cloud-config renderer, disk lister, config detection

Everything that decides what reaches disk, unit-tested behind a fake /sys/block
and a temp /oem. Password hashing shells out to openssl rather than vendoring
SHA-512-crypt."
```

---

### Task 3: Progress TUI and `--progress-only` mode

Wrap `kairos-agent install`, render progress from its output. After this task the binary is useful on its own — this is what the agent appliance consumes.

**Files:**
- Create: `install-ui/main.go`
- Create: `install-ui/model.go`
- Create: `install-ui/runner.go`
- Create: `install-ui/logo.go`
- Create: `install-ui/main_test.go`
- Modify: `install-ui/go.mod`, `install-ui/go.sum` (bubbletea deps)

**Interfaces:**
- Consumes: `Steps`, `AdvanceStep`, `Percent`, `newRing` (Task 1); `Choice`, `RenderCloudConfig`, `ConfigPresent`, `HashPassword` (Task 2).
- Produces:
  - `func installCommand() *exec.Cmd` — honours `HADRON_INSTALL_CMD`, defaults to `kairos-agent install`
  - `func runInstall() error` — runs the child under the progress UI, execs a rescue shell if the user presses `s` after a failure
  - `type lineMsg string`, `type doneMsg struct{ err error }`
  - `var logo []string` — the ASCII wordmark
  - `--progress-only` flag handling in `main()`

- [ ] **Step 1: Add the bubbletea dependencies**

```bash
cd install-ui
go get github.com/charmbracelet/bubbletea@v1.3.10
go get github.com/charmbracelet/bubbles@v1.0.0
go get github.com/charmbracelet/lipgloss@v1.1.0
go mod tidy
```

- [ ] **Step 2: Write the failing test for the install-command seam**

Create `install-ui/main_test.go`. `HADRON_INSTALL_CMD` is how every existing test injects a fake installer, so it is the one thing here that must not regress.

```go
package main

import (
	"strings"
	"testing"
)

func TestInstallCommandDefault(t *testing.T) {
	t.Setenv("HADRON_INSTALL_CMD", "")
	cmd := installCommand()
	got := strings.Join(cmd.Args, " ")
	if !strings.Contains(got, "kairos-agent") || !strings.Contains(got, "install") {
		t.Errorf("default command = %q, want kairos-agent install", got)
	}
}

func TestInstallCommandHonoursOverride(t *testing.T) {
	t.Setenv("HADRON_INSTALL_CMD", "/bin/true --fake")
	cmd := installCommand()
	got := strings.Join(cmd.Args, " ")
	if !strings.Contains(got, "/bin/true --fake") {
		t.Errorf("override command = %q, want it to carry /bin/true --fake", got)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
cd install-ui && go test ./... -run TestInstallCommand
```

Expected: compile failure — `undefined: installCommand`.

- [ ] **Step 4: Add the wordmark**

Create `install-ui/logo.go`. Same ANSI-Shadow HADRON as the boot splash, so the handoff from splash to installer reads as one continuous machine.

```go
package main

// logo is the ANSI-Shadow HADRON wordmark, matching splash/main.c so the boot
// splash and the installer read as the same machine. The block and box-drawing
// glyphs need a console font that covers them — see rootfs/etc/vconsole.conf.
var logo = []string{
	"██╗  ██╗ █████╗ ██████╗ ██████╗  ██████╗ ███╗   ██╗",
	"██║  ██║██╔══██╗██╔══██╗██╔══██╗██╔═══██╗████╗  ██║",
	"███████║███████║██║  ██║██████╔╝██║   ██║██╔██╗ ██║",
	"██╔══██║██╔══██║██║  ██║██╔══██╗██║   ██║██║╚██╗██║",
	"██║  ██║██║  ██║██████╔╝██║  ██║╚██████╔╝██║ ╚████║",
	"╚═╝  ╚═╝╚═╝  ╚═╝╚═════╝ ╚═╝  ╚═╝ ╚═════╝ ╚═╝  ╚═══╝",
}
```

- [ ] **Step 5: Add the line streamer**

Create `install-ui/runner.go`:

```go
package main

import (
	"bufio"
	"io"

	tea "github.com/charmbracelet/bubbletea"
)

// streamLines reads r line by line and delivers each as a lineMsg via send.
// Returns at EOF. Install logs can carry very long lines (device lists, kernel
// cmdlines), so the scanner buffer is enlarged well past bufio's 64KiB default —
// otherwise a single long line ends the scan early and the progress bar stalls.
func streamLines(r io.Reader, send func(tea.Msg)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		send(lineMsg(sc.Text()))
	}
}
```

- [ ] **Step 6: Add the progress model**

Create `install-ui/model.go`:

```go
package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type lineMsg string
type doneMsg struct{ err error }

type action int

const (
	actionNone  action = iota
	actionShell        // on exit, main() should exec a rescue shell
)

type model struct {
	spin     spinner.Model
	vp       viewport.Model
	logs     *ring
	step     int
	showLogs bool
	failed   bool
	done     bool
	action   action
	ready    bool
}

func newModel() model {
	s := spinner.New()
	s.Spinner = spinner.Line // ASCII (|/-\) — safe on the VT console font
	return model{spin: s, logs: newRing(2000)}
}

func (m model) Init() tea.Cmd { return m.spin.Tick }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.vp = viewport.New(msg.Width, msg.Height-1)
		m.vp.SetContent(m.logs.text())
		m.ready = true
		return m, nil

	case lineMsg:
		atBottom := m.ready && m.vp.AtBottom()
		m.logs.add(string(msg))
		m.step = AdvanceStep(string(msg), m.step)
		if m.ready {
			m.vp.SetContent(m.logs.text())
			if atBottom {
				m.vp.GotoBottom() // follow the tail unless the user scrolled up
			}
		}
		return m, nil

	case doneMsg:
		if msg.err != nil {
			m.failed = true
			m.showLogs = true // a failure without logs is useless
			if m.ready {
				m.vp.SetContent(m.logs.text())
				m.vp.GotoBottom()
			}
			return m, nil
		}
		m.done = true
		m.step = len(Steps) - 1
		return m, tea.Quit

	case tea.KeyMsg:
		return m.handleKey(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.failed {
		if msg.String() == "s" {
			m.action = actionShell
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg) // allow scrolling the logs
		return m, cmd
	}
	if msg.String() == "l" {
		m.showLogs = !m.showLogs
		return m, nil
	}
	if m.showLogs {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
	// Deliberately no quit key while installing: the disk is being written.
	return m, nil
}

// Tokyo Night styles, matching the boot splash so the handoff reads as one
// continuous machine. 16-color VGA palette only: this runs on the kernel VT
// (TERM=linux), where truecolor and 256-color values round to the wrong hue.
// 12 = bright blue, 14 = bright cyan, 8 = dark grey, 15 = white-hot,
// 9 = red (reserved for a real halt).
var (
	tnLogo = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	tnHot  = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)
	tnDim  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	tnAcc  = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	haltRed = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
)

// progressBar renders [####....] with a bright fill over a dim track.
func progressBar(pct, width int) string {
	if width < 1 {
		width = 1
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	fill := pct * width / 100
	return tnDim.Render("[") + tnAcc.Render(strings.Repeat("#", fill)) +
		tnDim.Render(strings.Repeat(".", width-fill)) + tnDim.Render("]")
}

func (m model) View() string {
	if !m.ready {
		return "\n  " + tnDim.Render("waking the machine...")
	}
	if m.failed {
		return haltRed.Render("  install halted") + "\n" + m.vp.View() +
			"\n  " + tnDim.Render("press 's' for a rescue shell")
	}
	if m.showLogs {
		return m.vp.View() + "\n  " + tnDim.Render("press 'l' to hide logs")
	}

	var b strings.Builder
	b.WriteString("\n")
	for _, l := range logo {
		b.WriteString("        " + tnLogo.Render(l) + "\n")
	}
	b.WriteString("\n")
	if m.done {
		b.WriteString("        " + tnHot.Render("system installed - rebooting...") + "\n")
	} else {
		b.WriteString("        " + tnHot.Render(m.spin.View()) + "  " +
			tnHot.Render(Steps[m.step].Name) + "...\n")
	}
	pct := Percent(m.step)
	b.WriteString("\n        " + progressBar(pct, 20) +
		tnDim.Render(fmt.Sprintf("  %3d%%", pct)) + "\n")
	b.WriteString("\n        " + tnDim.Render("press 'l' to show logs · do not power off") + "\n")
	return b.String()
}
```

- [ ] **Step 7: Add main() with the progress-only path**

Create `install-ui/main.go`. The wizard call is stubbed here and filled in by Task 4 — the stub is a real function returning a real error, not a placeholder.

```go
package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

// installCommand builds the child install process. HADRON_INSTALL_CMD is the
// single seam through which a disk wipe happens; tests inject a fake here to
// assert did/didn't install, so it must keep working exactly as in the shell
// installer this replaces.
func installCommand() *exec.Cmd {
	if c := os.Getenv("HADRON_INSTALL_CMD"); c != "" {
		return exec.Command("/bin/sh", "-c", c)
	}
	return exec.Command("/usr/bin/kairos-agent", "install")
}

// runInstall runs the installer as a subprocess, rendering the branded progress
// UI from its output. kairos-agent has no progress API, so its stdout is the
// only signal available (see steps.go).
func runInstall() error {
	cmd := installCommand()

	// The child must never read the terminal: bubbletea owns it.
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd.Stdin = devnull

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout // merge, so failures show up in the log viewport

	p := tea.NewProgram(newModel(), tea.WithAltScreen())
	go func() {
		if err := cmd.Start(); err != nil {
			p.Send(doneMsg{err: err})
			return
		}
		streamLines(stdout, p.Send)
		p.Send(doneMsg{err: cmd.Wait()})
	}()

	final, err := p.Run()
	if err != nil {
		return err
	}
	if m, ok := final.(model); ok && m.action == actionShell {
		// Replace this process so the rescue shell owns tty1 outright.
		return syscall.Exec("/bin/sh", []string{"sh"}, os.Environ())
	}
	if m, ok := final.(model); ok && m.failed {
		return fmt.Errorf("install failed")
	}
	return nil
}

func main() {
	progressOnly := false
	for _, a := range os.Args[1:] {
		switch a {
		case "--progress-only":
			progressOnly = true
		default:
			fmt.Fprintf(os.Stderr, "hadron-install-ui: unknown argument: %s\n", a)
			os.Exit(2)
		}
	}

	if !progressOnly {
		if err := runWizard(); err != nil {
			fmt.Fprintf(os.Stderr, "hadron-install-ui: %v\n", err)
			os.Exit(1)
		}
	}

	if err := runInstall(); err != nil {
		fmt.Fprintf(os.Stderr, "hadron-install-ui: %v\n", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 8: Add a temporary wizard stub so the package compiles**

Create `install-ui/wizard.go` with just enough to build. Task 4 replaces this file wholesale.

```go
package main

import "errors"

// runWizard is implemented in Task 4. Returning an error here keeps the package
// compiling and makes an accidental release of this stub fail loudly rather than
// silently installing with no user configured.
func runWizard() error {
	return errors.New("wizard not implemented")
}
```

- [ ] **Step 9: Run the test to verify it passes**

```bash
cd install-ui && go build ./... && go test ./... -v
```

Expected: build succeeds, all eighteen tests pass, `ok`.

- [ ] **Step 10: Verify the progress UI against a fake installer**

This is the only way to see the progress screen without wiping a disk. The fake emits the real step markers with pauses.

```bash
cd install-ui && go build -o /tmp/hadron-install-ui . && \
HADRON_INSTALL_CMD='for m in "Partitioning device /dev/vda" "Running stage: before-install" "Creating file system image" "Installing GRUB" "recovery.img" "passive.img" "Running after-install hook" "Finish Lifecycle hook"; do echo "$m"; sleep 1; done' \
/tmp/hadron-install-ui --progress-only
```

Expected: the HADRON wordmark, a spinner, and a progress bar advancing 0% → 100% over ~8s, then exit. Press `l` mid-run to confirm the log viewport toggles.

Then verify the failure path:

```bash
HADRON_INSTALL_CMD='echo "Partitioning device /dev/vda"; echo "something exploded" >&2; exit 1' \
/tmp/hadron-install-ui --progress-only
```

Expected: red `install halted`, the log viewport showing both lines, and `press 's' for a rescue shell`. Press `s` and confirm you land in a shell; `exit` to return.

- [ ] **Step 11: Commit**

```bash
git add install-ui/
git commit -m "feat(install-ui): branded progress screen wrapping kairos-agent

Run the installer as a subprocess and render progress scraped from its stdout:
wordmark, spinner, progress bar, 'l' log toggle, and a rescue shell on failure.
--progress-only skips the wizard, which is how the agent appliance consumes it."
```

---

### Task 4: The wizard

Replace the stub with the real prompt flow, preserving every seam from the shell installer.

**Files:**
- Modify: `install-ui/wizard.go` (replaces the Task 3 stub entirely)
- Create: `install-ui/wizard_test.go`

**Interfaces:**
- Consumes: `Choice`, `RenderCloudConfig`, `ConfigPresent`, `HashPassword`, `ListDisks`, `Disk` (Task 2); the style vars from Task 3.
- Produces: `func runWizard() error` — writes `$HADRON_OEM_DIR/99_hadron-user.yaml` and returns, or returns an error. Also `func choiceFromEnv() (Choice, bool, error)` for the non-interactive path.

- [ ] **Step 1: Write the failing non-interactive test**

Create `install-ui/wizard_test.go`. The non-interactive path is what existing automation uses, so it is the part that must be tested; the interactive bubbletea flow is verified by hand in Step 6.

```go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd install-ui && go test ./... -run 'TestChoiceFromEnv|TestWriteChoice'
```

Expected: compile failure — `undefined: choiceFromEnv`, `undefined: writeChoice`.

- [ ] **Step 3: Replace the wizard stub**

Replace the entire contents of `install-ui/wizard.go`:

```go
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

const defaultHostname = "hadron"

func oemDir() string {
	if d := os.Getenv("HADRON_OEM_DIR"); d != "" {
		return d
	}
	return "/oem"
}

// choiceFromEnv reads the non-interactive configuration. The second return value
// reports whether non-interactive mode is active at all. These variable names are
// a stable contract with existing tests and automation.
func choiceFromEnv() (Choice, bool, error) {
	if os.Getenv("HADRON_INSTALL_NONINTERACTIVE") != "1" {
		return Choice{}, false, nil
	}
	c := Choice{
		Hostname: os.Getenv("HADRON_HOSTNAME"),
		Username: os.Getenv("HADRON_USER"),
		Password: os.Getenv("HADRON_PASS"),
		Github:   os.Getenv("HADRON_GITHUB"),
		Disk:     os.Getenv("HADRON_DISK"),
	}
	if c.Hostname == "" {
		c.Hostname = defaultHostname
	}
	var missing []string
	if c.Username == "" {
		missing = append(missing, "HADRON_USER")
	}
	if c.Password == "" {
		missing = append(missing, "HADRON_PASS")
	}
	if c.Disk == "" {
		missing = append(missing, "HADRON_DISK")
	}
	if len(missing) > 0 {
		return c, true, fmt.Errorf("non-interactive mode requires %s", strings.Join(missing, ", "))
	}
	return c, true, nil
}

// writeChoice hashes the password and writes the cloud-config with owner-only
// permissions — the file carries a password hash.
func writeChoice(path string, c Choice, hash string) error {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(RenderCloudConfig(c, hash)), 0o600)
}

func dirOf(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return "."
}

// --- wizard steps ----------------------------------------------------------

type wizardStep int

const (
	stepHostname wizardStep = iota
	stepUser
	stepPass
	stepGithub
	stepDisk
	stepReview
)

var stepName = map[wizardStep]string{
	stepHostname: "hostname",
	stepUser:     "username",
	stepPass:     "password",
	stepGithub:   "ssh keys",
	stepDisk:     "target disk",
	stepReview:   "review",
}

type wizardModel struct {
	step    wizardStep
	in      textinput.Model
	choice  Choice
	disks   []Disk
	diskIdx int
	err     string
	done    bool
	aborted bool
}

func newWizard(disks []Disk) wizardModel {
	m := wizardModel{step: stepHostname, disks: disks}
	m.in = textinput.New()
	m.enter(stepHostname)
	return m
}

// enter re-seeds the input for step s from already-collected state, so values
// survive back-navigation instead of being silently cleared.
func (m *wizardModel) enter(s wizardStep) {
	m.step = s
	m.err = ""
	m.in.EchoMode = textinput.EchoNormal
	m.in.Prompt = "> "
	switch s {
	case stepHostname:
		m.in.SetValue(orDefault(m.choice.Hostname, defaultHostname))
	case stepUser:
		m.in.SetValue(m.choice.Username)
	case stepPass:
		m.in.EchoMode = textinput.EchoPassword
		m.in.SetValue(m.choice.Password)
	case stepGithub:
		m.in.SetValue(m.choice.Github)
	}
	m.in.Focus()
	m.in.CursorEnd()
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (m wizardModel) Init() tea.Cmd { return textinput.Blink }

func (m wizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.in, cmd = m.in.Update(msg)
		return m, cmd
	}

	switch key.String() {
	case "ctrl+c":
		m.aborted = true
		return m, tea.Quit
	case "esc":
		if m.step > stepHostname {
			m.enter(m.step - 1)
		}
		return m, nil
	}

	if m.step == stepDisk {
		return m.updateDisk(key)
	}
	if m.step == stepReview {
		return m.updateReview(key)
	}
	if key.String() == "enter" {
		return m.commitField()
	}
	var cmd tea.Cmd
	m.in, cmd = m.in.Update(msg)
	return m, cmd
}

func (m wizardModel) commitField() (tea.Model, tea.Cmd) {
	v := strings.TrimSpace(m.in.Value())
	switch m.step {
	case stepHostname:
		m.choice.Hostname = orDefault(v, defaultHostname)
		m.enter(stepUser)
	case stepUser:
		if v == "" {
			m.err = "username is required"
			return m, nil
		}
		m.choice.Username = v
		m.enter(stepPass)
	case stepPass:
		if v == "" {
			m.err = "password is required"
			return m, nil
		}
		m.choice.Password = v
		m.enter(stepGithub)
	case stepGithub:
		m.choice.Github = v // optional
		if len(m.disks) == 0 {
			m.err = "no installable disk found"
			return m, nil
		}
		m.enter(stepDisk)
	}
	return m, nil
}

func (m wizardModel) updateDisk(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "up", "k":
		if m.diskIdx > 0 {
			m.diskIdx--
		}
	case "down", "j":
		if m.diskIdx < len(m.disks)-1 {
			m.diskIdx++
		}
	case "enter":
		m.choice.Disk = m.disks[m.diskIdx].Path
		m.enter(stepReview)
	}
	return m, nil
}

func (m wizardModel) updateReview(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.String() == "enter" {
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m wizardModel) View() string {
	var b strings.Builder
	b.WriteString("\n  " + tnHot.Render("HADRON") +
		tnDim.Render(" // desktop install · "+stepName[m.step]) + "\n\n")

	switch m.step {
	case stepDisk:
		b.WriteString("  " + tnDim.Render("the selected disk will be ERASED") + "\n\n")
		for i, d := range m.disks {
			line := fmt.Sprintf("%s  %d GB", d.Path, d.SizeGB)
			if i == m.diskIdx {
				b.WriteString("  " + tnAcc.Render("> "+line) + "\n")
			} else {
				b.WriteString("    " + tnDim.Render(line) + "\n")
			}
		}
		b.WriteString("\n  " + tnDim.Render("up/down select · enter confirm · esc back") + "\n")

	case stepReview:
		b.WriteString("  " + haltRed.Render("this ERASES "+m.choice.Disk+" and installs hadron-desktop") + "\n\n")
		b.WriteString("    hostname   " + tnHot.Render(m.choice.Hostname) + "\n")
		b.WriteString("    username   " + tnHot.Render(m.choice.Username) + "\n")
		b.WriteString("    ssh keys   " + tnHot.Render(orDefault(githubLabel(m.choice.Github), "none")) + "\n")
		b.WriteString("    disk       " + haltRed.Render(m.choice.Disk) + "\n")
		b.WriteString("\n  " + tnDim.Render("enter to INSTALL · esc back") + "\n")

	default:
		b.WriteString("  " + prompt(m.step) + "\n")
		b.WriteString("  " + m.in.View() + "\n")
		if m.err != "" {
			b.WriteString("\n  " + haltRed.Render(m.err) + "\n")
		}
		hint := "enter continue"
		if m.step > stepHostname {
			hint += " · esc back"
		}
		b.WriteString("\n  " + tnDim.Render(hint) + "\n")
	}
	return b.String()
}

func githubLabel(g string) string {
	if g == "" {
		return ""
	}
	return "github:" + g
}

func prompt(s wizardStep) string {
	switch s {
	case stepHostname:
		return tnDim.Render("hostname for this machine")
	case stepUser:
		return tnDim.Render("username for the desktop account")
	case stepPass:
		return tnDim.Render("password for that account")
	case stepGithub:
		return tnDim.Render("GitHub username to import SSH keys from (blank to skip)")
	}
	return ""
}

// --- entry point -----------------------------------------------------------

// runWizard collects the install configuration and writes it to /oem for
// `kairos-agent install` to merge. It is a no-op when a config is already present,
// preserving the shell installer's unattended behaviour for seeded installs.
func runWizard() error {
	out := oemDir() + "/99_hadron-user.yaml"

	present, err := ConfigPresent(oemDir(), out)
	if err != nil {
		return err
	}
	if present {
		// Someone already supplied a users/install config: install unattended.
		return nil
	}

	c, nonInteractive, err := choiceFromEnv()
	if err != nil {
		return err
	}

	if !nonInteractive {
		disks, err := ListDisks("/sys/block")
		if err != nil {
			return err
		}
		final, err := tea.NewProgram(newWizard(disks), tea.WithAltScreen()).Run()
		if err != nil {
			return err
		}
		wm, ok := final.(wizardModel)
		if !ok || wm.aborted || !wm.done {
			return errors.New("install cancelled")
		}
		c = wm.choice
	}

	hash, err := HashPassword(c.Password)
	if err != nil {
		return err
	}
	return writeChoice(out, c, hash)
}
```

- [ ] **Step 4: Add the textinput dependency**

```bash
cd install-ui && go get github.com/charmbracelet/bubbles/textinput && go mod tidy
```

- [ ] **Step 5: Run the full suite to verify it passes**

```bash
cd install-ui && go build ./... && go test ./... -v
```

Expected: build succeeds, all twenty-two tests pass, `ok`.

- [ ] **Step 6: Verify the wizard by hand**

Run against a temp `/oem` and a fake installer so nothing is written to a real disk:

```bash
cd install-ui && go build -o /tmp/hadron-install-ui . && \
mkdir -p /tmp/fake-oem && rm -f /tmp/fake-oem/*.yaml && \
HADRON_OEM_DIR=/tmp/fake-oem \
HADRON_INSTALL_CMD='echo "Partitioning device /dev/fake"; sleep 1; echo "Finish Lifecycle hook"' \
/tmp/hadron-install-ui
```

Expected: the wizard walks hostname → username → password (masked) → GitHub → disk list → review. Verify:
- `esc` from any step after the first goes back **with the previously entered value still in the field**
- an empty username shows `username is required` rather than advancing
- the review screen shows the disk in red and the summary matches what you typed
- `enter` on review proceeds to the progress screen

Then confirm the output:

```bash
cat /tmp/fake-oem/99_hadron-user.yaml
```

Expected: a `#cloud-config` with your hostname, username, a `$6$`-prefixed hash, the seven groups, and the ly `save.ini` stage. Confirm the password is **not** present in plaintext.

Then confirm the unattended path short-circuits:

```bash
printf 'users:\n  - name: seeded\n' > /tmp/fake-oem/10_seed.yaml && \
rm -f /tmp/fake-oem/99_hadron-user.yaml && \
HADRON_OEM_DIR=/tmp/fake-oem \
HADRON_INSTALL_CMD='echo "Finish Lifecycle hook"' \
/tmp/hadron-install-ui
```

Expected: **no prompts** — straight to the progress screen.

- [ ] **Step 7: Commit**

```bash
git add install-ui/
git commit -m "feat(install-ui): interactive wizard with back-navigation

hostname/user/password/ssh-keys/disk then a review gate that shows the target
disk in red before anything irreversible. Esc re-seeds from collected state so
values survive going back. Preserves the shell installer's env seams and its
skip-when-config-present behaviour."
```

---

### Task 5: Image integration

Build the binary into the image, repoint the installer drop-in, and give the agent appliance the progress screen without touching its safety logic.

**Files:**
- Modify: `Dockerfile` (build stage + install)
- Modify: `rootfs/system/oem/90_desktop_installer.yaml`
- Modify: `rootfs-agent/usr/local/bin/hadron-agent-install` (one line)
- Delete: `rootfs/usr/local/bin/hadron-install`
- Test: `test/boot-ux/check-installer.sh`

**Interfaces:**
- Consumes: the `install-ui/` module from Tasks 1-4.
- Produces: `/usr/bin/hadron-install-ui` in the image. Terminal task.

- [ ] **Step 1: Write the failing test**

Create `test/boot-ux/check-installer.sh`:

```bash
#!/usr/bin/env bash
# Assert the installer TUI is present and wired up in a built image.
#
#   test/boot-ux/check-installer.sh [IMAGE]
set -euo pipefail
IMAGE="${1:-sway-desktop:dev}"
fail=0
check() {
    local desc="$1"; shift
    if docker run --rm "$IMAGE" sh -c "$*" >/dev/null 2>&1; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc"; fail=1
    fi
}

check "hadron-install-ui present in /usr/bin" 'test -x /usr/bin/hadron-install-ui'
check "hadron-install-ui is static (no interpreter needed)" \
    '! ldd /usr/bin/hadron-install-ui 2>/dev/null | grep -q "not found"'
check "rejects unknown arguments" \
    '! /usr/bin/hadron-install-ui --nonsense 2>/dev/null'
check "openssl available for password hashing" 'command -v openssl'
check "old shell installer removed" '! test -e /usr/local/bin/hadron-install'
check "installer drop-in points at hadron-install-ui" \
    'grep -q "/usr/bin/hadron-install-ui" /system/oem/90_desktop_installer.yaml'
check "drop-in keeps the install-mode guard" \
    'grep -q "install-mode" /system/oem/90_desktop_installer.yaml'

exit "$fail"
```

```bash
chmod +x test/boot-ux/check-installer.sh
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
test/boot-ux/check-installer.sh sway-desktop:dev
```

Expected: `FAIL` on the first three checks, on "old shell installer removed", and on "installer drop-in points at hadron-install-ui".

- [ ] **Step 3: Add the Go build stage**

In the `Dockerfile`, add this stage immediately before the `FROM quay.io/kairos/kairos-init:${KAIROS_INIT} AS kairos-init` line. It mirrors the `agent-gobuild` stage in `Dockerfile.agent`.

```dockerfile
# ---------------------------------------------------------------------------
# Installer TUI. A static CGO_ENABLED=0 Go binary, so it runs on musl without
# any runtime linkage. Replaces the POSIX-sh wizard: it collects the same
# answers, writes the same cloud-config, then runs `kairos-agent install` as a
# subprocess and renders a branded progress screen instead of raw log spew.
# ---------------------------------------------------------------------------
FROM golang:1.25.0-alpine3.22 AS install-ui-build
WORKDIR /src
COPY install-ui/go.mod install-ui/go.sum ./
RUN go mod download
COPY install-ui/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hadron-install-ui . \
    && /out/hadron-install-ui --nonsense 2>/dev/null; [ $? -eq 2 ]
```

The trailing check is a smoke test: the binary must run and reject an unknown flag with exit 2. A binary that segfaults on startup fails the build here rather than on tty1 during an install.

- [ ] **Step 4: Install the binary and drop the shell installer**

Find the `RUN` block in the desktop stage that ends with the `hadron-user-setup.service` symlink (currently around line 2731). Immediately after that block, add:

```dockerfile
# Installer TUI. /usr/bin, not /usr/local/bin: /usr/local is shadowed by an empty
# overlay at runtime on Kairos, so a binary there is invisible to the installed
# system. (The shell installer it replaces lived in /usr/local/bin and only
# worked because the installer runs in the live environment.)
COPY --from=install-ui-build /out/hadron-install-ui /usr/bin/hadron-install-ui
```

Then remove the superseded script:

```bash
git rm rootfs/usr/local/bin/hadron-install
```

If the `Dockerfile` has an explicit `COPY` or `chmod` referencing `rootfs/usr/local/bin/hadron-install`, remove that line too:

```bash
grep -n "hadron-install" Dockerfile
```

Expected after the edit: matches only for `hadron-install-ui`. Remove any line still referring to the bare `hadron-install` path.

- [ ] **Step 5: Repoint the installer drop-in**

In `rootfs/system/oem/90_desktop_installer.yaml`, change the `ExecStart` line from:

```yaml
            ExecStart=/usr/local/bin/hadron-install
```

to:

```yaml
            ExecStart=/usr/bin/hadron-install-ui
```

Then update the stage's header comment. Replace the sentence beginning "drops a systemd override so that service runs `/usr/local/bin/hadron-install` instead" with:

```
# drops a systemd override so that service runs /usr/bin/hadron-install-ui
# instead — a Go TUI that collects user/password/disk/hostname, writes a
# cloud-config to /oem, then runs `kairos-agent install` as a subprocess behind a
# branded progress screen. If a cloud-config with a user/install block was already
# provided, it skips the prompts and runs the normal unattended install.
```

- [ ] **Step 6: Give the agent appliance the progress screen**

In `rootfs-agent/usr/local/bin/hadron-agent-install`, change only the `INSTALL_CMD` default. From:

```sh
INSTALL_CMD="${HADRON_INSTALL_CMD:-kairos-agent install}"
```

to:

```sh
INSTALL_CMD="${HADRON_INSTALL_CMD:-hadron-install-ui --progress-only}"
```

And extend the override documentation in the header comment. Replace the `HADRON_INSTALL_CMD` line with:

```sh
#   HADRON_INSTALL_CMD   install command to exec (default
#                        "hadron-install-ui --progress-only", which runs
#                        `kairos-agent install` itself behind a branded progress
#                        screen). This is the single seam through which a wipe
#                        happens; tests inject a fake here to assert did/didn't
#                        install, which still bypasses the UI entirely.
```

**Nothing else in this file changes.** The typed-`yes` gate, the `--noninteractive` seed re-verification via `hadron-agent provision inspect-seed --require-auto-install`, and every other `HADRON_*` seam stay exactly as they are.

- [ ] **Step 7: Verify the agent installer's tests still pass**

This is the check that matters most in this task — the appliance installer is the security-critical path.

```bash
test/agent/install_script_test.sh
```

Expected: the same result as before this change. If the suite has a runner, use it instead:

```bash
test/agent/run.sh 2>&1 | tail -20
```

If any assertion now fails, the `INSTALL_CMD` default change broke a seam — revert Step 6 and report rather than adjusting the test.

- [ ] **Step 8: Rebuild and run the installer test**

```bash
make image
test/boot-ux/check-installer.sh sway-desktop:dev
```

Expected: seven `PASS` lines, exit 0.

- [ ] **Step 9: Add the installer check to the runner**

In `test/boot-ux/run.sh`, add this line immediately after the `check-branding.sh` invocation:

```bash
"$SCRIPT_DIR/check-installer.sh" "$IMAGE" || fail=1
```

Then run the whole suite:

```bash
test/boot-ux/run.sh sway-desktop:dev 'sway · wayland · kairos'
```

Expected: all checks pass, final line `boot-ux: ALL PASS`.

- [ ] **Step 10: Verify a real install in a VM**

The unit tests cover the logic and the image checks cover the wiring; only a real install proves the whole path.

```bash
make iso
tools/vm.sh install
```

Expected, in order: themed GRUB menu → splash → the wizard on tty1 → review screen with the disk in red → progress screen advancing through the real kairos-agent phases → reboot into the installed system → ly login prefilled with the username you chose.

Confirm during the install that `l` toggles the real log output.

- [ ] **Step 11: Update the README**

In `README.md`, find the installer description and update it to reflect the Go TUI. If the README does not currently describe the installer, add to the variant architecture list:

```markdown
- **Installer:** `hadron-install-ui`, a Go TUI that runs on tty1 from the live
  ISO. Collects hostname, user, password, optional GitHub SSH keys and target
  disk with a review gate, then runs `kairos-agent install` behind a progress
  screen (`l` toggles logs, `s` opens a rescue shell if the install fails). An
  existing user/install cloud-config in `/oem` skips the prompts.
```

- [ ] **Step 12: Commit**

```bash
git add Dockerfile rootfs/system/oem/90_desktop_installer.yaml \
        rootfs-agent/usr/local/bin/hadron-agent-install \
        test/boot-ux/check-installer.sh test/boot-ux/run.sh README.md
git add -u rootfs/usr/local/bin/
git commit -m "feat(install): replace the shell wizard with the installer TUI

Build install-ui as a static binary to /usr/bin/hadron-install-ui and repoint
the kairos-installer drop-in at it. The agent appliance keeps its audited shell
installer and gains only the progress screen, via its INSTALL_CMD default."
```

---

## Verification checklist

After all five tasks:

```bash
cd install-ui && go test ./... && cd ..
make image && test/boot-ux/run.sh sway-desktop:dev 'sway · wayland · kairos'
test/agent/run.sh
make iso && tools/vm.sh install
```

The VM install is the real test: wizard → review → progress → reboot → prefilled ly login.

## Notes and risks

- **The step substrings are the fragile part.** They mirror kairos-agent's
  `internal/agent/TUIconstants.go`, which we do not control. If kairos-agent
  changes its log wording the progress bar stalls at a lower percentage — the
  install still succeeds. `steps.go` is deliberately the only place to retune.
  After the first real VM install, compare the observed log against `Steps` and
  correct any substring that never matched.
- **`session_index = 2`** in the generated cloud-config is inherited verbatim
  from the shell installer. It assumes a fixed session ordering in the image. If
  the ly session list changes, this needs to change with it.
- **bubbletea on `TERM=linux`** is proven by AIOS on the same kernel VT, but the
  VGA-16 palette discipline in the Global Constraints is what makes it work. A
  hex colour anywhere in the styles will render as the wrong hue on real hardware
  while looking correct in a terminal emulator during development.
