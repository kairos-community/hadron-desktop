package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// cosmic-bg 1.11 does not switch pictures with the theme, so on COSMIC a
// small session helper (isotope wallpaper-follow) sets the Background config
// to the Isotope day or night picture when the dark mode changes. It touches
// the wallpaper only while it is one of the two Isotope pictures.

const (
	cosmicSystemDir = "/usr/share/cosmic"
	themeModeName   = "com.system76.CosmicTheme.Mode"
	backgroundName  = "com.system76.CosmicBackground"
)

// wallpaperDir holds isotope-desktop-day.png and isotope-desktop-night.png,
// usually links to the release's picture. A variable so tests can move it.
var wallpaperDir = "/usr/share/backgrounds/isotope"

var wallpaperSource = regexp.MustCompile(`source:\s*Path\("([^"]*)"\)`)

// wallpaperSet returns the day and night pictures in the three spellings that
// can be in the config: the link path and the resolved target, each of which
// counts as an Isotope wallpaper. Index 0 is the link style, 1 the target.
func wallpaperSet() (day, night [2]string) {
	for i, p := range []*[2]string{&day, &night} {
		name := [2]string{"isotope-desktop-day.png", "isotope-desktop-night.png"}[i]
		link := filepath.Join(wallpaperDir, name)
		p[0], p[1] = link, link
		if t, err := filepath.EvalSymlinks(link); err == nil {
			p[1] = t
		}
	}
	return
}

// wallpaperFor is the rule: for the current wallpaper path and the mode, the
// path to use and whether it differs. Anything but an Isotope picture is left
// alone. The answer is in the style of the input: cosmic-bg records the link's
// target, so a target path gets the other target, a link path the other link.
// Both stay valid files; matching the style keeps the config as cosmic-bg
// wrote it.
func wallpaperFor(current string, dark bool) (string, bool) {
	day, night := wallpaperSet()
	for style := 0; style < 2; style++ {
		switch current {
		case night[style]:
			if !dark {
				return day[style], true
			}
			return current, false
		case day[style]:
			if dark {
				return night[style], true
			}
			return current, false
		}
	}
	return current, false
}

// cosmicConfig reads a COSMIC config key: the user's file, else the system's.
func cosmicConfig(home, sysDir, name, key string) (string, error) {
	b, err := os.ReadFile(filepath.Join(home, ".config", "cosmic", name, "v1", key))
	if errors.Is(err, os.ErrNotExist) {
		b, err = os.ReadFile(filepath.Join(sysDir, name, "v1", key))
	}
	return string(b), err
}

// readIsDark is the effective dark mode; COSMIC's default, and Isotope's, is dark.
func readIsDark(home, sysDir string) bool {
	v, err := cosmicConfig(home, sysDir, themeModeName, "is_dark")
	return err != nil || strings.TrimSpace(v) != "false"
}

// currentWallpaper returns the effective "all" Background RON and its picture.
func currentWallpaper(home, sysDir string) (ron string, path string, err error) {
	ron, err = cosmicConfig(home, sysDir, backgroundName, "all")
	if err != nil {
		return "", "", err
	}
	m := wallpaperSource.FindStringSubmatch(ron)
	if m == nil {
		return ron, "", errors.New("the background is not a picture")
	}
	return ron, m[1], nil
}

// setWallpaper writes ron with only the picture replaced to the user's config,
// by temp file and rename as cosmic-config does.
func setWallpaper(home, ron, newPath string) error {
	if !wallpaperSource.MatchString(ron) {
		return errors.New("no picture source in the background config")
	}
	out := wallpaperSource.ReplaceAllLiteralString(ron, `source: Path("`+newPath+`")`)
	dir := filepath.Join(home, ".config", "cosmic", backgroundName, "v1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".all-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.WriteString(tmp, out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "all"))
}

// wallpaperPass applies the rule once.
func wallpaperPass(home, sysDir string) error {
	ron, cur, err := currentWallpaper(home, sysDir)
	if err != nil {
		return err
	}
	if next, change := wallpaperFor(cur, readIsDark(home, sysDir)); change {
		return setWallpaper(home, ron, next)
	}
	return nil
}

// runWallpaperFollow applies the rule at start, then after every change in the
// Mode config directory, until ctx ends. The directory is watched, not the
// file: cosmic-config replaces it by rename, and it may not exist yet.
func runWallpaperFollow(ctx context.Context, home, sysDir string) error {
	pass := func() {
		if err := wallpaperPass(home, sysDir); err != nil {
			fmt.Fprintf(os.Stderr, "isotope: wallpaper-follow: %v\n", err)
		}
	}
	pass()
	if ctx.Err() != nil {
		return nil
	}
	dir := filepath.Join(home, ".config", "cosmic", themeModeName, "v1")
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	// watch (re-)creates the directory and adds the watch. It runs again when
	// the kernel drops the watch, because the directory was removed or moved
	// (for example by a user resetting their COSMIC theme config).
	watch := func() error {
		var err error
		// The directory can vanish again between the mkdir and the watch.
		for try := 0; try < 5; try++ {
			if err = os.MkdirAll(dir, 0o755); err == nil {
				_, err = unix.InotifyAddWatch(fd, dir,
					unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO|unix.IN_CREATE|unix.IN_DELETE|unix.IN_MOVED_FROM|
						unix.IN_DELETE_SELF|unix.IN_MOVE_SELF)
			}
			if err == nil || !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		return err
	}
	if err := watch(); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	for ctx.Err() == nil {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 500)
		if err != nil && err != unix.EINTR {
			return err
		}
		if n <= 0 {
			continue
		}
		got, err := unix.Read(fd, buf)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			return err
		}
		if inotifyLostWatch(buf[:got]) {
			if err := watch(); err != nil {
				return err
			}
		}
		time.Sleep(50 * time.Millisecond) // let a rename burst settle
		pass()
	}
	return nil
}

// inotifyLostWatch reports whether the events say the watched directory is
// gone or moved, so the watch is dead or points at the wrong place.
func inotifyLostWatch(buf []byte) bool {
	for len(buf) >= unix.SizeofInotifyEvent {
		ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[0]))
		if ev.Mask&(unix.IN_IGNORED|unix.IN_DELETE_SELF|unix.IN_MOVE_SELF) != 0 {
			return true
		}
		size := unix.SizeofInotifyEvent + int(ev.Len)
		if size > len(buf) {
			break
		}
		buf = buf[size:]
	}
	return false
}

func cmdWallpaperFollow(stderr io.Writer) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "isotope: wallpaper-follow: %v\n", err)
		return exitError
	}
	if err := runWallpaperFollow(context.Background(), home, cosmicSystemDir); err != nil {
		fmt.Fprintf(stderr, "isotope: wallpaper-follow: %v\n", err)
		return exitError
	}
	return exitOK
}
