package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	wpDay   = "/usr/share/backgrounds/isotope/isotope-desktop-day.png"
	wpNight = "/usr/share/backgrounds/isotope/isotope-desktop-night.png"
)

const shippedBackground = "../rootfs-cosmic/usr/share/cosmic/com.system76.CosmicBackground/v1/all"

// fixtureBackground is a copy of the shipped file in testdata/, because the
// image build's test stage has only update/ in its context.
func fixtureBackground(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/CosmicBackground-all")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The copy must not drift from the shipped file; skipped when the repo
// outside update/ is not there.
func TestBackgroundFixtureMatchesShipped(t *testing.T) {
	shipped, err := os.ReadFile(shippedBackground)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("repo outside update/ not available")
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(shipped) != fixtureBackground(t) {
		t.Error("update/testdata/CosmicBackground-all differs from the shipped file; copy it again")
	}
}

func writeCfg(t *testing.T, root, name, key, val string) {
	t.Helper()
	p := filepath.Join(root, name, "v1", key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(val), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWallpaperForSwitchesIsotopePair(t *testing.T) {
	for _, c := range []struct {
		cur  string
		dark bool
		want string
		ok   bool
	}{
		{wpNight, false, wpDay, true},
		{wpDay, true, wpNight, true},
		{wpNight, true, wpNight, false},
		{wpDay, false, wpDay, false},
	} {
		got, ok := wallpaperFor(c.cur, c.dark)
		if got != c.want || ok != c.ok {
			t.Errorf("wallpaperFor(%q, %v) = %q, %v; want %q, %v", c.cur, c.dark, got, ok, c.want, c.ok)
		}
	}
}

func TestWallpaperForLeavesOwnPicture(t *testing.T) {
	for _, dark := range []bool{true, false} {
		if got, ok := wallpaperFor("/home/u/cat.jpg", dark); ok || got != "/home/u/cat.jpg" {
			t.Errorf("dark=%v: got %q, %v", dark, got, ok)
		}
	}
}

// cosmic-bg records the link's target; the target files count as ours too,
// and the answer is written in the style in use.
func TestWallpaperForFollowsLinkTargets(t *testing.T) {
	d := t.TempDir()
	old := wallpaperDir
	wallpaperDir = d
	defer func() { wallpaperDir = old }()
	for _, f := range []string{"n-real.png", "d-real.png"} {
		os.WriteFile(filepath.Join(d, f), nil, 0o644)
	}
	os.Symlink("n-real.png", filepath.Join(d, "isotope-desktop-night.png"))
	os.Symlink("d-real.png", filepath.Join(d, "isotope-desktop-day.png"))
	real, _ := filepath.EvalSymlinks(d)
	got, ok := wallpaperFor(filepath.Join(real, "n-real.png"), false)
	if !ok || got != filepath.Join(real, "d-real.png") {
		t.Errorf("target style: %q, %v", got, ok)
	}
	got, ok = wallpaperFor(filepath.Join(d, "isotope-desktop-night.png"), false)
	if !ok || got != filepath.Join(d, "isotope-desktop-day.png") {
		t.Errorf("link style: %q, %v", got, ok)
	}
	if _, ok := wallpaperFor(filepath.Join(real, "n-real.png"), true); ok {
		t.Error("night target in dark mode must stay")
	}
}

func TestReadIsDarkFallsBackToSystem(t *testing.T) {
	home, sys := t.TempDir(), t.TempDir()
	writeCfg(t, sys, "com.system76.CosmicTheme.Mode", "is_dark", "true\n")
	if !readIsDark(home, sys) {
		t.Error("system true, no user file: want dark")
	}
	writeCfg(t, filepath.Join(home, ".config", "cosmic"), "com.system76.CosmicTheme.Mode", "is_dark", "false")
	if readIsDark(home, sys) {
		t.Error("user false: want light")
	}
}

func TestSetWallpaperKeepsOtherFields(t *testing.T) {
	in := fixtureBackground(t)
	home, sys := t.TempDir(), t.TempDir()
	writeCfg(t, sys, "com.system76.CosmicBackground", "all", in)
	ron, path, err := currentWallpaper(home, sys)
	if err != nil || path != wpNight {
		t.Fatalf("current = %q, %v", path, err)
	}
	if err := setWallpaper(home, ron, wpDay); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(home, ".config/cosmic/com.system76.CosmicBackground/v1/all"))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(in, wpNight, wpDay, 1); string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

func TestFollowCatchesUpAtStart(t *testing.T) {
	home, sys := t.TempDir(), t.TempDir()
	in := fixtureBackground(t)
	writeCfg(t, sys, "com.system76.CosmicBackground", "all", in)
	writeCfg(t, filepath.Join(home, ".config", "cosmic"), "com.system76.CosmicTheme.Mode", "is_dark", "false")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // one pass, then stop
	if err := runWallpaperFollow(ctx, home, sys); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(home, ".config/cosmic/com.system76.CosmicBackground/v1/all"))
	if err != nil || !strings.Contains(string(out), wpDay) {
		t.Fatalf("user file: %q, %v", out, err)
	}
}

func TestFollowSeesLaterChange(t *testing.T) {
	home, sys := t.TempDir(), t.TempDir()
	in := fixtureBackground(t)
	writeCfg(t, sys, "com.system76.CosmicBackground", "all", in)
	writeCfg(t, sys, "com.system76.CosmicTheme.Mode", "is_dark", "true")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runWallpaperFollow(ctx, home, sys) }()
	time.Sleep(300 * time.Millisecond)
	writeCfg(t, filepath.Join(home, ".config", "cosmic"), "com.system76.CosmicTheme.Mode", "is_dark", "false")
	p := filepath.Join(home, ".config/cosmic/com.system76.CosmicBackground/v1/all")
	for i := 0; i < 40; i++ {
		if b, _ := os.ReadFile(p); strings.Contains(string(b), wpDay) {
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	t.Fatal("wallpaper did not follow the mode change")
}

func TestFollowSurvivesRemovedModeDir(t *testing.T) {
	home, sys := t.TempDir(), t.TempDir()
	in := fixtureBackground(t)
	writeCfg(t, sys, "com.system76.CosmicBackground", "all", in)
	writeCfg(t, sys, "com.system76.CosmicTheme.Mode", "is_dark", "true")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runWallpaperFollow(ctx, home, sys) }()
	time.Sleep(300 * time.Millisecond)
	// The user removes the theme config, as when resetting the look.
	cosmic := filepath.Join(home, ".config", "cosmic")
	// The helper re-creates the directory as soon as it goes, so removing it
	// can race with that; a second rm sweeps what came back.
	modeDir := filepath.Join(cosmic, "com.system76.CosmicTheme.Mode")
	if err := os.RemoveAll(modeDir); err != nil {
		time.Sleep(200 * time.Millisecond)
		_ = os.RemoveAll(modeDir)
	}
	time.Sleep(500 * time.Millisecond)
	writeCfg(t, cosmic, "com.system76.CosmicTheme.Mode", "is_dark", "false")
	p := filepath.Join(cosmic, "com.system76.CosmicBackground/v1/all")
	for i := 0; i < 50; i++ {
		if b, _ := os.ReadFile(p); strings.Contains(string(b), wpDay) {
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	runErr := <-done
	ls, _ := exec.Command("find", home).CombinedOutput()
	t.Fatalf("no follow; run error %v; tree:\n%s", runErr, ls)
}
