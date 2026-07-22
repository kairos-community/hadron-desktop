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
