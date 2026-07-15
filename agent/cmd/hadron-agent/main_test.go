package main

import (
	"bytes"
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
		{"provision unsupported", []string{"provision"}, 1},
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
// provision reserved
// ---------------------------------------------------------------------------

func TestProvisionUnsupportedMessage(t *testing.T) {
	code, _, stderr := runArgs("provision")
	if code != 1 {
		t.Fatalf("provision exit = %d, want 1", code)
	}
	low := strings.ToLower(stderr)
	if !strings.Contains(low, "provision") || !strings.Contains(low, "phase 3") {
		t.Fatalf("provision error should name Phase 3:\n%s", stderr)
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
