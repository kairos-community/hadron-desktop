package main

import (
	"os"
	"os/exec"
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

// The generated config must hook stages.initramfs, not stages.boot: save.ini has
// to exist before ly starts, and the boot stage runs too late. This is a hard
// constraint of the installer, so pin the literal nesting rather than just the
// presence of the key.
func TestRenderCloudConfigUsesInitramfsStage(t *testing.T) {
	out := RenderCloudConfig(Choice{Hostname: "h", Username: "u", Disk: "/dev/sda"}, "x")
	if !strings.Contains(out, "stages:\n  initramfs:\n") {
		t.Errorf("expected stages.initramfs hook:\n%s", out)
	}
	// Anchored to the stage indent so `reboot: true` in the install block does
	// not satisfy it.
	if strings.Contains(out, "\n  boot:") {
		t.Errorf("expected no boot stage, save.ini would land after ly starts:\n%s", out)
	}
}

// save.ini is read by ly running as root but must stay world-readable so the
// greeter can be inspected/debugged; 0644 is what the shell installer writes and
// the rendered config must remain byte-identical to it.
func TestRenderCloudConfigSaveIniPermissions(t *testing.T) {
	out := RenderCloudConfig(Choice{Hostname: "h", Username: "u", Disk: "/dev/sda"}, "x")
	if !strings.Contains(out, "          permissions: 0644\n") {
		t.Errorf("expected save.ini permissions 0644:\n%s", out)
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

// ConfigPresent deliberately diverges from the shell's literal `[ "$f" = "$OUT" ]`
// by comparing resolved absolute paths. Without that, mixing relative and
// absolute spellings makes the glob emit a path string that never equals
// outPath, so our own previous output counts as a foreign config and the wizard
// is skipped — an unattended install using whatever the last run wrote.
//
// Note the spellings below are specifically relative-vs-absolute mismatches:
// filepath.Join already cleans trailing slashes and "." components, so those
// alone do not distinguish Abs from a naive compare.
func TestConfigPresentIgnoresOwnOutputAcrossPathSpellings(t *testing.T) {
	const body = "users:\n  - name: x\n"
	const outName = "99_hadron-user.yaml"

	t.Run("relative oemDir, absolute outPath", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, outName)
		if err := os.WriteFile(out, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		// Glob yields the bare relative name; only Abs resolution matches out.
		got, err := ConfigPresent(".", out)
		if err != nil || got {
			t.Fatalf("got %v err %v, want false nil", got, err)
		}
	})

	t.Run("absolute oemDir, relative outPath", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, outName), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		// Glob yields "<dir>/99_hadron-user.yaml" against a bare relative name.
		got, err := ConfigPresent(dir, outName)
		if err != nil || got {
			t.Fatalf("got %v err %v, want false nil", got, err)
		}
	})

	t.Run("a foreign config is still found under a mixed spelling", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, outName)
		if err := os.WriteFile(out, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "10_other.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		got, err := ConfigPresent(".", out)
		if err != nil || !got {
			t.Fatalf("got %v err %v, want true nil", got, err)
		}
	})
}

func TestHashPasswordProducesSHA512Crypt(t *testing.T) {
	requireOpenSSL(t)

	const plain = "correct horse battery staple"
	hash, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	// $6$<salt>$<hash> — SplitN keeps an empty leading field before the first $.
	parts := strings.Split(hash, "$")
	if len(parts) != 4 {
		t.Fatalf("hash %q: got %d $-separated fields, want 4", hash, len(parts))
	}
	if parts[0] != "" {
		t.Errorf("hash %q: want leading $, got prefix %q", hash, parts[0])
	}
	if parts[1] != "6" {
		t.Errorf("hash %q: crypt id %q, want 6 (SHA-512); a different id means openssl ignored -6", hash, parts[1])
	}
	if parts[2] == "" || parts[3] == "" {
		t.Fatalf("hash %q: empty salt or digest", hash)
	}

	// Re-derive with the extracted salt. A truncated or otherwise mangled read
	// cannot reproduce this; only a genuine SHA-512-crypt result can.
	redone := opensslPasswd(t, plain, "-salt", parts[2])
	if redone != hash {
		t.Errorf("re-derivation with salt %q:\n got %q\nwant %q", parts[2], redone, hash)
	}
}

// openssl terminates its output with a newline. It is interpolated into a
// single-quoted YAML scalar (passwd: '<hash>'), where a surviving newline breaks
// the document or the hash — either way, nobody can log in.
func TestHashPasswordStripsTrailingNewline(t *testing.T) {
	requireOpenSSL(t)

	hash, err := HashPassword("pw")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.ContainsAny(hash, "\r\n") {
		t.Errorf("hash %q contains a line break", hash)
	}
	if hash != strings.TrimSpace(hash) {
		t.Errorf("hash %q has surrounding whitespace", hash)
	}
	// And the newline must actually have been there to strip: prove the rendered
	// scalar is a single line.
	out := RenderCloudConfig(Choice{Hostname: "h", Username: "u", Disk: "/dev/sda"}, hash)
	if !strings.Contains(out, "    passwd: '"+hash+"'\n") {
		t.Errorf("hash did not land on one line in the config:\n%s", out)
	}
}

// TrimSpace must only touch openssl's output, never the password. Padding a
// password with spaces is legal and users do it; silently trimming them would
// hash something other than what the user typed.
func TestHashPasswordPreservesSpacesInPassword(t *testing.T) {
	requireOpenSSL(t)

	const padded = "  spaced out  "
	hash, err := HashPassword(padded)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	salt := strings.Split(hash, "$")[2]

	if got := opensslPasswd(t, padded, "-salt", salt); got != hash {
		t.Errorf("padded password not hashed verbatim:\n got %q\nwant %q", got, hash)
	}
	if got := opensslPasswd(t, strings.TrimSpace(padded), "-salt", salt); got == hash {
		t.Errorf("hash of %q equals hash of its trimmed form — spaces were dropped", padded)
	}
}

func TestHashPasswordSurfacesOpensslStderr(t *testing.T) {
	// A stub openssl that fails the way an old build does: a diagnostic on
	// stderr and a non-zero exit. The plain exit status alone is useless.
	dir := t.TempDir()
	stub := filepath.Join(dir, "openssl")
	script := "#!/bin/sh\necho 'passwd: Unknown option: -6' >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	_, err := HashPassword("pw")
	if err == nil {
		t.Fatal("expected an error from the failing stub")
	}
	if !strings.Contains(err.Error(), "Unknown option: -6") {
		t.Errorf("error %q does not carry openssl's own diagnostic", err)
	}
}

func requireOpenSSL(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not on PATH")
	}
}

// opensslPasswd calls openssl directly, independently of HashPassword, so the
// tests above compare the implementation against the reference rather than
// against itself.
func opensslPasswd(t *testing.T, plain string, extra ...string) string {
	t.Helper()
	args := append([]string{"passwd", "-6"}, extra...)
	cmd := exec.Command("openssl", append(args, "-stdin")...)
	cmd.Stdin = strings.NewReader(plain)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("openssl %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}
