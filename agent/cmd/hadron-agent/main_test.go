package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/auth"
	"github.com/mudler/hadron-desktop/agent/internal/buildinfo"
	"github.com/mudler/hadron-desktop/agent/internal/provision"
)

// runArgs drives the dispatch entry point with a fresh set of buffers and
// returns the exit code plus captured stdout/stderr.
func runArgs(args ...string) (code int, stdout, stderr string) {
	var out, errBuf bytes.Buffer
	code = run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// ---------------------------------------------------------------------------
// Exit-code mapping
// ---------------------------------------------------------------------------

func TestExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, 2},
		{"unknown command", []string{"frobnicate"}, 2},
		{"top-level help", []string{"--help"}, 0},
		{"top-level help short", []string{"-h"}, 0},
		{"version", []string{"version"}, 0},
		{"version help", []string{"version", "-h"}, 0},
		{"gateway unknown flag", []string{"gateway", "--nope"}, 2},
		{"gateway help", []string{"gateway", "-h"}, 0},
		{"session unknown flag", []string{"session", "--bogus"}, 2},
		{"display-watchdog help", []string{"display-watchdog", "-h"}, 0},
		{"display-watchdog unknown flag", []string{"display-watchdog", "--nope"}, 2},
		{"control missing action", []string{"control"}, 2},
		{"control unknown action", []string{"control", "wobble"}, 2},
		{"token missing action", []string{"token"}, 2},
		{"token unknown action", []string{"token", "wobble"}, 2},
		{"token rotate missing class", []string{"token", "rotate"}, 2},
		{"token rotate unknown class", []string{"token", "rotate", "wobble"}, 2},
		// Missing required config is a runtime error, not a usage error.
		{"gateway missing digests", []string{"gateway", "--listen", "127.0.0.1:0"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _ := runArgs(tc.args...)
			if code != tc.want {
				t.Fatalf("run(%v) exit = %d, want %d", tc.args, code, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// version output
// ---------------------------------------------------------------------------

func TestVersionOutput(t *testing.T) {
	code, stdout, _ := runArgs("version")
	if code != 0 {
		t.Fatalf("version exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, buildinfo.Version) {
		t.Fatalf("version output missing build version %q:\n%s", buildinfo.Version, stdout)
	}
	// The Cua revision field must be present.
	if !strings.Contains(stdout, buildinfo.CuaRevision) {
		t.Fatalf("version output missing cua revision %q:\n%s", buildinfo.CuaRevision, stdout)
	}
	if !strings.Contains(strings.ToLower(stdout), "cua") {
		t.Fatalf("version output missing a cua revision label:\n%s", stdout)
	}
}

// ---------------------------------------------------------------------------
// provision materializes state into a temp dir (unprivileged)
// ---------------------------------------------------------------------------

func TestProvisionMaterializesIntoTempDirs(t *testing.T) {
	oemDir := t.TempDir()
	stateDir := t.TempDir()
	runtimeDir := t.TempDir()

	// An empty OEM dir resolves to the opt-in enabled default (no digests), so
	// provision generates a fresh identity and a one-shot token.
	code, stdout, stderr := runArgs(
		"provision",
		"--oem-dir", oemDir,
		"--state-dir", stateDir,
		"--runtime-dir", runtimeDir,
	)
	if code != 0 {
		t.Fatalf("provision exit = %d, want 0\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "fingerprint=sha256:") {
		t.Fatalf("provision output missing fingerprint:\n%s", stdout)
	}
	for _, rel := range []string{"gateway/config.json", "gateway/tls.crt", "gateway/tls.key", "session/config.json"} {
		if _, err := os.Stat(filepath.Join(stateDir, rel)); err != nil {
			t.Fatalf("expected %s to be materialized: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "first-run", "token")); err != nil {
		t.Fatalf("expected a one-shot first-run token: %v", err)
	}
}

// A rotation without a controlling terminal must not print the bearer.
func TestTokenRotateWithoutTTYWithholdsSecret(t *testing.T) {
	stateDir := t.TempDir()

	// Provision first so a gateway config exists to rotate.
	if code, _, stderr := runArgs("provision", "--oem-dir", t.TempDir(), "--state-dir", stateDir, "--runtime-dir", t.TempDir()); code != 0 {
		t.Fatalf("provision failed: %s", stderr)
	}

	code, stdout, stderr := runArgs("token", "rotate", "user", "--overlap", "15m", "--state-dir", stateDir)
	if code != 0 {
		t.Fatalf("token rotate exit = %d, want 0\nstderr: %s", code, stderr)
	}
	// The bearers-buffers are not a TTY, so no bearer must appear anywhere.
	if strings.Contains(stdout, "hdn_") || strings.Contains(stderr, "hdn_") {
		t.Fatalf("bearer leaked without a TTY:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
}

// ---------------------------------------------------------------------------
// provision inspect-seed: the zero-touch authorization gate
// ---------------------------------------------------------------------------

// writeOEM writes a single cloud-config into a fresh OEM dir and returns it.
func writeOEM(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "50_seed.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write oem: %v", err)
	}
	return dir
}

// Only a fully-authorized seed (auto=true + /dev device + enabled) may exit 0,
// and ONLY when --require-auto-install is present. --quiet emits nothing.
func TestProvisionInspectSeed(t *testing.T) {
	authorized := "#cloud-config\ninstall:\n  auto: true\n  device: /dev/sda\nhadron_agent:\n  enabled: true\n"

	cases := []struct {
		name    string
		content string
		args    func(oem string) []string
		want    int
	}{
		{"absent", "#cloud-config\n",
			func(o string) []string {
				return []string{"provision", "inspect-seed", "--require-auto-install", "--oem-dir", o}
			}, 1},
		{"unrelated", "#cloud-config\nhostname: box\nusers:\n  - name: alice\n",
			func(o string) []string {
				return []string{"provision", "inspect-seed", "--require-auto-install", "--oem-dir", o}
			}, 1},
		{"auto false", "#cloud-config\ninstall:\n  auto: false\n  device: /dev/sda\nhadron_agent:\n  enabled: true\n",
			func(o string) []string {
				return []string{"provision", "inspect-seed", "--require-auto-install", "--oem-dir", o}
			}, 1},
		{"device-less", "#cloud-config\ninstall:\n  auto: true\nhadron_agent:\n  enabled: true\n",
			func(o string) []string {
				return []string{"provision", "inspect-seed", "--require-auto-install", "--oem-dir", o}
			}, 1},
		{"disabled", "#cloud-config\ninstall:\n  auto: true\n  device: /dev/sda\nhadron_agent:\n  enabled: false\n",
			func(o string) []string {
				return []string{"provision", "inspect-seed", "--require-auto-install", "--oem-dir", o}
			}, 1},
		{"malformed", "#cloud-config\ninstall:\n  auto: true\n  device: [bad\n",
			func(o string) []string {
				return []string{"provision", "inspect-seed", "--require-auto-install", "--oem-dir", o}
			}, 1},
		{"authorized without flag", authorized,
			func(o string) []string { return []string{"provision", "inspect-seed", "--oem-dir", o} }, 1},
		{"authorized", authorized,
			func(o string) []string {
				return []string{"provision", "inspect-seed", "--require-auto-install", "--oem-dir", o}
			}, 0},
		{"authorized quiet", authorized,
			func(o string) []string {
				return []string{"provision", "inspect-seed", "--require-auto-install", "--quiet", "--oem-dir", o}
			}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oem := writeOEM(t, tc.content)
			code, stdout, stderr := runArgs(tc.args(oem)...)
			if code != tc.want {
				t.Fatalf("exit = %d, want %d\nstdout: %q\nstderr: %q", code, tc.want, stdout, stderr)
			}
			quiet := false
			for _, a := range tc.args(oem) {
				if a == "--quiet" {
					quiet = true
				}
			}
			if tc.want == 0 && !quiet && !strings.Contains(stdout, "/dev/sda") {
				t.Fatalf("authorized run must print the device; stdout=%q", stdout)
			}
			if tc.want == 0 && quiet && stdout != "" {
				t.Fatalf("authorized --quiet run must print nothing; stdout=%q", stdout)
			}
		})
	}
}

// --quiet must emit NOTHING on an unauthorized seed so it is a clean systemd
// ExecCondition.
func TestProvisionInspectSeedQuietSilentOnUnauthorized(t *testing.T) {
	oem := writeOEM(t, "#cloud-config\ninstall:\n  auto: false\n")
	code, stdout, stderr := runArgs("provision", "inspect-seed", "--require-auto-install", "--quiet", "--oem-dir", oem)
	if code == 0 {
		t.Fatalf("unauthorized seed must exit non-zero")
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("--quiet must emit nothing; stdout=%q stderr=%q", stdout, stderr)
	}
}

// ---------------------------------------------------------------------------
// gateway --config: the provisioned config.json drives the verifier (rotation)
// ---------------------------------------------------------------------------

// TestGatewayConfigAuthenticatesProvisionedBearer proves the core integration
// gap is closed: a bearer whose digest `provision` persisted into config.json
// authenticates through the exact building blocks runGateway uses to construct
// its Verifier from --config, and that both the current and the unexpired
// previous (rotated-out) bearer are accepted while an unrelated bearer is not.
func TestGatewayConfigAuthenticatesProvisionedBearer(t *testing.T) {
	// A current and a previous user bearer, plus an admin bearer; provision
	// persists their digests (with a bounded overlap) into config.json.
	curBearer, curDigest, err := auth.Generate(auth.ClassUser)
	if err != nil {
		t.Fatalf("generate current: %v", err)
	}
	prevBearer, prevDigest, err := auth.Generate(auth.ClassUser)
	if err != nil {
		t.Fatalf("generate previous: %v", err)
	}
	adminBearer, adminDigest, err := auth.Generate(auth.ClassAdmin)
	if err != nil {
		t.Fatalf("generate admin: %v", err)
	}
	otherBearer, _, err := auth.Generate(auth.ClassUser)
	if err != nil {
		t.Fatalf("generate other: %v", err)
	}

	stateDir := t.TempDir()
	m := provision.NewMaterializer(provision.Options{StateDir: stateDir, RuntimeDir: t.TempDir()})
	cfg := provision.Config{
		Enabled:  true,
		Endpoint: provision.Endpoint{Listen: "127.0.0.1:7443"},
		Auth: provision.Auth{
			UserTokenHash:          string(curDigest),
			UserPreviousTokenHash:  string(prevDigest),
			UserPreviousValidUntil: time.Now().Add(1 * time.Hour),
			AdminTokenHash:         string(adminDigest),
		},
		Limits: provision.Limits{MaxConcurrentCalls: 8, MaxProcesses: 8, MaxRequestBytes: 1 << 20},
	}
	if _, err := m.Materialize(cfg); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// Reconstruct the verifier exactly as runGateway does from --config.
	gw, err := provision.LoadGatewayConfig(filepath.Join(stateDir, "gateway", "config.json"))
	if err != nil {
		t.Fatalf("LoadGatewayConfig: %v", err)
	}
	userRot, err := resolveRotation("", "no-such-env", &gw.User)
	if err != nil {
		t.Fatalf("resolveRotation user: %v", err)
	}
	adminRot, err := resolveRotation("", "no-such-env", &gw.Admin)
	if err != nil {
		t.Fatalf("resolveRotation admin: %v", err)
	}
	verifier, err := auth.NewVerifier(userRot, adminRot)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	if _, err := verifier.Verify(curBearer, auth.ClassUser); err != nil {
		t.Errorf("current user bearer rejected: %v", err)
	}
	if _, err := verifier.Verify(prevBearer, auth.ClassUser); err != nil {
		t.Errorf("unexpired previous user bearer rejected (rotation not honored): %v", err)
	}
	if _, err := verifier.Verify(adminBearer, auth.ClassAdmin); err != nil {
		t.Errorf("provisioned admin bearer rejected: %v", err)
	}
	if _, err := verifier.Verify(otherBearer, auth.ClassUser); err == nil {
		t.Error("an unrelated user bearer was accepted, want rejection")
	}
}

// TestGatewayConfigMissingIsError proves --config pointing at a missing file is
// a clean runtime error (exit 1), not a panic or a silent credential-less start.
func TestGatewayConfigMissingIsError(t *testing.T) {
	code, _, stderr := runArgs("gateway", "--config", filepath.Join(t.TempDir(), "absent.json"))
	if code != 1 {
		t.Fatalf("gateway --config <missing> exit = %d, want 1\nstderr: %s", code, stderr)
	}
}

// TestRootHelperConfigAuthenticatesProvisionedAdmin proves the root helper
// builds its admin verifier from the provisioned root config.json and leaves
// the user class unusable.
func TestRootHelperConfigAuthenticatesProvisionedAdmin(t *testing.T) {
	adminBearer, adminDigest, err := auth.Generate(auth.ClassAdmin)
	if err != nil {
		t.Fatalf("generate admin: %v", err)
	}
	userBearer, _, err := auth.Generate(auth.ClassUser)
	if err != nil {
		t.Fatalf("generate user: %v", err)
	}

	stateDir := t.TempDir()
	m := provision.NewMaterializer(provision.Options{StateDir: stateDir, RuntimeDir: t.TempDir()})
	cfg := provision.Config{
		Enabled:  true,
		Endpoint: provision.Endpoint{Listen: "127.0.0.1:7443"},
		Auth:     provision.Auth{AdminTokenHash: string(adminDigest)},
		Limits:   provision.Limits{MaxConcurrentCalls: 8, MaxProcesses: 8, MaxRequestBytes: 1 << 20},
	}
	if _, err := m.Materialize(cfg); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	rc, err := provision.LoadRootConfig(filepath.Join(stateDir, "root", "config.json"))
	if err != nil {
		t.Fatalf("LoadRootConfig: %v", err)
	}
	adminRot, err := resolveRotation("", "no-such-env", &rc.Admin)
	if err != nil {
		t.Fatalf("resolveRotation: %v", err)
	}
	verifier, err := auth.NewVerifier(auth.RotationConfig{Current: unusableDigest}, adminRot)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if _, err := verifier.Verify(adminBearer, auth.ClassAdmin); err != nil {
		t.Errorf("provisioned admin bearer rejected by root helper: %v", err)
	}
	if _, err := verifier.Verify(userBearer, auth.ClassUser); err == nil {
		t.Error("root helper accepted a user bearer, want the user class unusable")
	}
}

// ---------------------------------------------------------------------------
// unknown command names itself
// ---------------------------------------------------------------------------

func TestUnknownCommandReported(t *testing.T) {
	code, _, stderr := runArgs("frobnicate")
	if code != 2 {
		t.Fatalf("unknown command exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "frobnicate") {
		t.Fatalf("unknown command error should echo the command:\n%s", stderr)
	}
}
