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

// TestInstallableDisksTreatsUnreadableAsEmpty: an unreadable /sys/block and an
// empty one are the same fact to the user — nothing to install onto — and the
// wizard already has a clear screen for that. Surfacing the raw syscall error
// instead would abort the installer with "open /sys/block: no such file or
// directory" printed over the branded UI.
func TestInstallableDisksTreatsUnreadableAsEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := ListDisks(missing); err == nil {
		t.Fatal("ListDisks should still report the error to its other callers")
	}
	if got := installableDisks(missing); len(got) != 0 {
		t.Errorf("installableDisks = %+v, want none", got)
	}
	// ...and it must still return real disks when the tree is readable, or the
	// "same as empty" behaviour would be indistinguishable from always-empty.
	root := fakeSysBlock(t, map[string]string{"vda": "41943040"})
	if got := installableDisks(root); len(got) != 1 || got[0].Path != "/dev/vda" {
		t.Errorf("installableDisks = %+v, want /dev/vda", got)
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
