package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CosmicLayoutsDir is where the image ships the panel layouts of
// cosmic-initial-setup (src/page/layout.rs:68).
const CosmicLayoutsDir = "/usr/share/cosmic-layouts"

// Layout is one panel layout: a directory with a layout.kdl (id and
// names), an icon.png and one directory per cosmic-config id to install
// under ~/.config/cosmic.
type Layout struct {
	ID   string // the directory's name, e.g. "bottom-panel"
	Name string // the layout.kdl name for the user's language
	Dir  string
	Icon string // path of icon.png

	order int // the id of layout.kdl; layouts are listed by it
}

// SystemLayoutID is the layout that is the image's own panel defaults
// (/usr/share/cosmic): applying it removes the user's overrides instead of
// copying files.
const SystemLayoutID = "isotope"

// IsSystem reports whether l is the Isotope layout, the system default.
func (l Layout) IsSystem() bool { return l.ID == SystemLayoutID }

var kdlName = regexp.MustCompile(`(?:(\w+)=)?"([^"]*)"`)

// parseLayoutKDL reads the two nodes layout.rs reads (layout.rs:101-120):
// `id <int>` and `name <locale>="..."...`; a name with no locale is "en".
func parseLayoutKDL(src string) (id int, names map[string]string) {
	id, names = -1, map[string]string{}
	for _, line := range strings.Split(src, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "id":
			if n, err := strconv.Atoi(f[1]); err == nil {
				id = n
			}
		case "name":
			for _, m := range kdlName.FindAllStringSubmatch(strings.TrimPrefix(strings.TrimSpace(line), "name"), -1) {
				loc := m[1]
				if loc == "" {
					loc = "en"
				}
				names[loc] = m[2]
			}
		}
	}
	return id, names
}

// layoutName picks the name for lang ("de_DE.UTF-8"), as layout_button does
// (layout.rs:190-200): the full locale, then its language, then English.
func layoutName(names map[string]string, lang string) string {
	loc, _, _ := strings.Cut(lang, ".")
	if n, ok := names[loc]; ok {
		return n
	}
	short, _, _ := strings.Cut(loc, "_")
	if n, ok := names[short]; ok {
		return n
	}
	return names["en"]
}

// Layouts reads the layouts in dir, sorted by the id of their layout.kdl.
// A directory with no readable layout.kdl, no icon.png or no English name is
// left out, as upstream does (layout.rs:79-127).
func Layouts(dir string) ([]Layout, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	lang := os.Getenv("LANG")
	var out []Layout
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		d := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(filepath.Join(d, "layout.kdl"))
		if err != nil {
			continue
		}
		icon := filepath.Join(d, "icon.png")
		if _, err := os.Stat(icon); err != nil {
			continue
		}
		id, names := parseLayoutKDL(string(b))
		name := layoutName(names, lang)
		if name == "" {
			continue
		}
		out = append(out, Layout{ID: e.Name(), Name: name, Dir: d, Icon: icon, order: id})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].order < out[j].order })
	return out, nil
}

// configDirs are the layout's cosmic-config directories, e.g.
// com.system76.CosmicPanel.Panel (layout.rs:259-281).
func (l Layout) configDirs() ([]string, error) {
	ents, err := os.ReadDir(l.Dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// copyDir copies the files of src into dst, contents only, so no mode or
// time of the image's files is kept (layout.rs:218-251).
func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(s, d); err != nil {
				return err
			}
			continue
		}
		if !e.Type().IsRegular() {
			// Like upstream's is_file(), a link to a file counts.
			if fi, err := os.Stat(s); err != nil || !fi.Mode().IsRegular() {
				continue
			}
		}
		b, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ApplyLayout installs l: each of its config directories replaces the same
// one under ~/.config/cosmic, then the panel restarts. cosmic-panel reads
// the list of panels (CosmicPanel/v1/entries) only when it starts, and the
// image has no killall, so pkill -x ends it and cosmic-session starts it
// again (cosmic-session src/main.rs:139-141, restarts without limit). A
// pkill that finds nothing exits 1: that is not a failure, there was no
// panel to restart.
func ApplyLayout(r Runner, home string, l Layout) error {
	dirs, err := l.configDirs()
	if err != nil {
		return fmt.Errorf("read layout %s: %w", l.ID, err)
	}
	if len(dirs) == 0 {
		return fmt.Errorf("layout %s has no settings", l.ID)
	}
	for _, name := range dirs {
		dst := filepath.Join(home, ".config", "cosmic", name)
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if l.IsSystem() {
			continue
		}
		if err := copyDir(filepath.Join(l.Dir, name), dst); err != nil {
			return fmt.Errorf("copy %s: %w", name, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := r.Run(ctx, "pkill", "-x", "cosmic-panel"); err != nil && !errors.As(err, new(*exec.ExitError)) {
		return fmt.Errorf("restart the panel: %w", err)
	}
	return nil
}

// CurrentLayout is the id of the layout whose every file is what
// ~/.config/cosmic holds now, "" when none is (edits in COSMIC Settings).
// The system layout matches when the user has none of its directories.
func CurrentLayout(home string, ls []Layout) string {
	for _, l := range ls {
		if l.installedIn(home) {
			return l.ID
		}
	}
	return ""
}

func (l Layout) installedIn(home string) bool {
	dirs, err := l.configDirs()
	if err != nil || len(dirs) == 0 {
		return false
	}
	if l.IsSystem() {
		for _, name := range dirs {
			if _, err := os.Stat(filepath.Join(home, ".config", "cosmic", name)); err == nil {
				return false
			}
		}
		return true
	}
	for _, name := range dirs {
		same := true
		src := filepath.Join(l.Dir, name)
		_ = filepath.WalkDir(src, func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(l.Dir, p)
			a, err1 := os.ReadFile(p)
			b, err2 := os.ReadFile(filepath.Join(home, ".config", "cosmic", rel))
			if err1 != nil || err2 != nil || !bytes.Equal(a, b) {
				same = false
			}
			return nil
		})
		if !same {
			return false
		}
	}
	return true
}
