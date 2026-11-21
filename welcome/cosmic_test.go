package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// testdata/cosmic-layouts holds the two layouts of cosmic-initial-setup,
// tag epoch-1.11.0, res/layouts/ (the image's /usr/share/cosmic-layouts).
const layoutFixtures = "testdata/cosmic-layouts"

func fixtureLayout(t *testing.T, id string) Layout {
	t.Helper()
	ls, err := Layouts(layoutFixtures)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		if l.ID == id {
			return l
		}
	}
	t.Fatalf("no layout %q in %v", id, ls)
	return Layout{}
}

func TestLayoutsReadsBoth(t *testing.T) {
	t.Setenv("LANG", "en_US.UTF-8")
	ls, err := Layouts(layoutFixtures)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 2 || ls[0].ID != "top-panel-and-bottom-dock" || ls[1].ID != "bottom-panel" {
		t.Fatalf("layouts %+v, want top-panel-and-bottom-dock (id 0) then bottom-panel (id 1)", ls)
	}
	if ls[0].Name != "Top Panel & Bottom Dock" || ls[1].Name != "Bottom Panel" {
		t.Errorf("names %q, %q", ls[0].Name, ls[1].Name)
	}
	if filepath.Base(ls[1].Icon) != "icon.png" {
		t.Errorf("icon %q", ls[1].Icon)
	}
	t.Setenv("LANG", "pt_BR.UTF-8")
	ls, _ = Layouts(layoutFixtures)
	if ls[1].Name != "Painel inferior" {
		t.Errorf("pt name %q", ls[1].Name)
	}
}

func TestLayoutsSkipsOnesWithoutIcon(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "x", "layout.kdl"), []byte("id 0\nname en=\"X\"\n"), 0o644)
	if ls, _ := Layouts(dir); len(ls) != 0 {
		t.Fatalf("a layout with no icon.png was listed: %+v", ls)
	}
}

func TestApplyLayoutReplacesDirs(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, ".config", "cosmic")
	stale := filepath.Join(cfg, "com.system76.CosmicPanel.Panel", "v1")
	os.MkdirAll(stale, 0o755)
	os.WriteFile(filepath.Join(stale, "anchor"), []byte("Top"), 0o644)
	os.WriteFile(filepath.Join(stale, "stale-key"), []byte("x"), 0o644)

	r := &fakeRunner{}
	if err := ApplyLayout(r, home, fixtureLayout(t, "bottom-panel")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(stale, "anchor")); got != "Bottom" {
		t.Errorf("anchor = %q, want Bottom", got)
	}
	if _, err := os.Stat(filepath.Join(stale, "stale-key")); err == nil {
		t.Error("the old directory was merged, not replaced")
	}
	if got := readFile(t, filepath.Join(cfg, "com.system76.CosmicPanel", "v1", "entries")); got != "[\n    \"Panel\",\n]" {
		t.Errorf("entries = %q", got)
	}
	if c := r.called(); len(c) != 1 || c[0] != "pkill -x cosmic-panel" {
		t.Fatalf("commands %q, want only pkill -x cosmic-panel", c)
	}
}

// The dock layout installs the Dock directory and lists both panels.
func TestApplyLayoutDockLayoutReplacesStaleDock(t *testing.T) {
	home := t.TempDir()
	dock := filepath.Join(home, ".config", "cosmic", "com.system76.CosmicPanel.Dock", "v1")
	os.MkdirAll(dock, 0o755)
	os.WriteFile(filepath.Join(dock, "anchor"), []byte("Left"), 0o644)
	if err := ApplyLayout(&fakeRunner{}, home, fixtureLayout(t, "top-panel-and-bottom-dock")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dock, "anchor")); got != "Bottom" {
		t.Errorf("dock anchor = %q", got)
	}
}

func TestApplyLayoutKeepsNoMetadata(t *testing.T) {
	src := t.TempDir()
	d := filepath.Join(src, "l", "com.example.Cfg", "v1")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "k"), []byte("v"), 0o444)
	home := t.TempDir()
	if err := ApplyLayout(&fakeRunner{}, home, Layout{ID: "l", Dir: filepath.Join(src, "l")}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(home, ".config", "cosmic", "com.example.Cfg", "v1", "k"))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("copy: %v, %v", fi, err)
	}
}

// No panel running: pkill exits 1, which is not a failure. A pkill that
// cannot run at all is.
func TestApplyLayoutPkillExit1IsFine(t *testing.T) {
	l := fixtureLayout(t, "bottom-panel")
	if err := ApplyLayout(&fakeRunner{fail: []string{"pkill"}}, t.TempDir(), l); err != nil {
		t.Fatalf("pkill exit 1: %v", err)
	}
	if err := ApplyLayout(&missingRunner{}, t.TempDir(), l); err == nil {
		t.Fatal("a missing pkill must be reported")
	}
}

type missingRunner struct{ fakeRunner }

func (*missingRunner) Run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return nil, &exec.Error{Name: "pkill", Err: exec.ErrNotFound}
}

func TestCurrentLayout(t *testing.T) {
	home := t.TempDir()
	ls, _ := Layouts(layoutFixtures)
	if got := CurrentLayout(home, ls); got != "" {
		t.Fatalf("empty home: %q", got)
	}
	ApplyLayout(&fakeRunner{}, home, fixtureLayout(t, "bottom-panel"))
	if got := CurrentLayout(home, ls); got != "bottom-panel" {
		t.Fatalf("after bottom-panel: %q", got)
	}
	os.WriteFile(filepath.Join(home, ".config/cosmic/com.system76.CosmicPanel.Panel/v1/anchor"), []byte("Top"), 0o644)
	if got := CurrentLayout(home, ls); got != "" {
		t.Fatalf("edited: %q", got)
	}
}

// isotopeLayout builds a layout with the shipped isotope id and the panel
// directories Task 4 ships, in a temp dir.
func isotopeLayout(t *testing.T) Layout {
	t.Helper()
	d := filepath.Join(t.TempDir(), SystemLayoutID)
	for _, n := range []string{"com.system76.CosmicPanel", "com.system76.CosmicPanel.Panel", "com.system76.CosmicPanel.Dock"} {
		os.MkdirAll(filepath.Join(d, n, "v1"), 0o755)
		os.WriteFile(filepath.Join(d, n, "v1", "k"), []byte("v"), 0o644)
	}
	return Layout{ID: SystemLayoutID, Name: "Isotope", Dir: d}
}

func TestApplySystemLayoutRemovesUserPanelDirs(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, ".config", "cosmic")
	panel := filepath.Join(cfg, "com.system76.CosmicPanel.Panel")
	os.MkdirAll(filepath.Join(panel, "v1"), 0o755)
	os.WriteFile(filepath.Join(panel, "v1", "size"), []byte("XS"), 0o644)
	tk := filepath.Join(cfg, "com.system76.CosmicTk", "v1")
	os.MkdirAll(tk, 0o755)
	os.WriteFile(filepath.Join(tk, "k"), []byte("v"), 0o644)

	r := &fakeRunner{}
	if err := ApplyLayout(r, home, isotopeLayout(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(panel); err == nil {
		t.Error("the user's panel dir survived the system layout")
	}
	if _, err := os.Stat(filepath.Join(tk, "k")); err != nil {
		t.Errorf("an unrelated cosmic dir was touched: %v", err)
	}
	if c := r.called(); len(c) != 1 || c[0] != "pkill -x cosmic-panel" {
		t.Fatalf("commands %q, want pkill -x cosmic-panel", c)
	}
}

func TestCurrentLayoutIsSystemWhenUserHasNoPanelDirs(t *testing.T) {
	ls, _ := Layouts(layoutFixtures)
	ls = append([]Layout{isotopeLayout(t)}, ls...)
	home := t.TempDir()
	if got := CurrentLayout(home, ls); got != SystemLayoutID {
		t.Fatalf("empty home: %q", got)
	}
	// Non-panel config does not count as a panel edit.
	os.MkdirAll(filepath.Join(home, ".config/cosmic/com.system76.CosmicTk/v1"), 0o755)
	if got := CurrentLayout(home, ls); got != SystemLayoutID {
		t.Fatalf("unrelated dir: %q", got)
	}
}

func TestCurrentLayoutNotSystemWhenUserHasPanelDirs(t *testing.T) {
	ls, _ := Layouts(layoutFixtures)
	ls = append([]Layout{isotopeLayout(t)}, ls...)
	home := t.TempDir()
	ApplyLayout(&fakeRunner{}, home, fixtureLayout(t, "bottom-panel"))
	if got := CurrentLayout(home, ls); got != "bottom-panel" {
		t.Fatalf("bottom-panel files: %q", got)
	}
	os.WriteFile(filepath.Join(home, ".config/cosmic/com.system76.CosmicPanel.Panel/v1/anchor"), []byte("Top"), 0o644)
	if got := CurrentLayout(home, ls); got != "" {
		t.Fatalf("other panel edit: %q", got)
	}
}

func TestApplyOtherLayoutStillCopies(t *testing.T) {
	home := t.TempDir()
	if err := ApplyLayout(&fakeRunner{}, home, fixtureLayout(t, "bottom-panel")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(home, ".config/cosmic/com.system76.CosmicPanel.Panel/v1/anchor")); got != "Bottom" {
		t.Errorf("anchor = %q", got)
	}
	if fixtureLayout(t, "bottom-panel").IsSystem() {
		t.Error("bottom-panel reported as the system layout")
	}
}
