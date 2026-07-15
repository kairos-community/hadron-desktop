package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mudler/hadron-desktop/agent/internal/buildinfo"
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
	if _, err := os.Stat(filepath.Join(runtimeDir, "first-run-token")); err != nil {
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
