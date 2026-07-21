package watchdog

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The ONLY restart command the watchdog may issue is
// `systemctl restart ly@tty1.service` -- never X, i3, Xvfb, or a nested session.
func TestSystemRestarter_LyOnly(t *testing.T) {
	var gotName string
	var gotArgs []string
	r := &SystemRestarter{
		run: func(_ context.Context, name string, args ...string) error {
			gotName = name
			gotArgs = args
			return nil
		},
	}
	if err := r.RestartLy(context.Background()); err != nil {
		t.Fatalf("RestartLy: %v", err)
	}
	if gotName != "systemctl" {
		t.Fatalf("restart binary: got %q, want systemctl", gotName)
	}
	want := []string{"restart", "ly@tty1.service"}
	if len(gotArgs) != len(want) {
		t.Fatalf("restart args: got %v, want %v", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("restart args[%d]: got %q, want %q", i, gotArgs[i], want[i])
		}
	}
	// Guard against ever launching a display directly.
	for _, a := range append([]string{gotName}, gotArgs...) {
		for _, forbidden := range []string{"Xvfb", "/usr/bin/X", "i3", "startx", "xinit"} {
			if a == forbidden {
				t.Fatalf("watchdog issued forbidden display launch token %q", a)
			}
		}
	}
}

// SystemDetector.Present requires BOTH a graphical logind session AND an
// agent-owned i3 process. Each half missing => not present.
func TestSystemDetector_Present(t *testing.T) {
	const uid = 1000

	// Fake loginctl: a graphical (x11) session owned by uid 1000.
	graphicalLoginctl := func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list-sessions" {
			return []byte("2 1000 agent seat0 tty1\n"), nil
		}
		// show-session <id> ...
		return []byte("Name=agent\nUser=1000\nType=x11\n"), nil
	}
	nonGraphicalLoginctl := func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list-sessions" {
			return []byte("c1 1000 agent seat0\n"), nil
		}
		return []byte("Name=agent\nUser=1000\nType=tty\n"), nil
	}

	procWithI3 := makeProcRoot(t, map[int]string{4321: "i3", 4322: "bash"}, uid)
	procNoI3 := makeProcRoot(t, map[int]string{4322: "bash"}, uid)
	procI3WrongUser := makeProcRoot(t, map[int]string{4321: "i3"}, 0)

	tests := []struct {
		name     string
		loginctl func(context.Context, ...string) ([]byte, error)
		proc     string
		want     bool
	}{
		{"both present", graphicalLoginctl, procWithI3, true},
		{"no graphical session", nonGraphicalLoginctl, procWithI3, false},
		{"no i3 process", graphicalLoginctl, procNoI3, false},
		{"i3 owned by another user", graphicalLoginctl, procI3WrongUser, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &SystemDetector{
				User:        "agent",
				uid:         uid, // pre-resolved to avoid touching /etc/passwd
				runLoginctl: tc.loginctl,
				procRoot:    tc.proc,
			}
			got, err := d.Present(context.Background())
			if err != nil {
				t.Fatalf("Present: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Present: got %v, want %v", got, tc.want)
			}
		})
	}
}

// makeProcRoot builds a fake /proc with the given pid->comm entries, all owned
// by ownerUID, and returns its path.
func makeProcRoot(t *testing.T, procs map[int]string, ownerUID int) string {
	t.Helper()
	root := t.TempDir()
	for pid, comm := range procs {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		status := "Name:\t" + comm + "\nUid:\t" + strconv.Itoa(ownerUID) + "\t" +
			strconv.Itoa(ownerUID) + "\t" + strconv.Itoa(ownerUID) + "\t" + strconv.Itoa(ownerUID) + "\n"
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A non-pid entry to prove the scanner ignores it.
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}
