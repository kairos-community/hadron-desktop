package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeExt is ExtBackend without kairos-agent: it does to the extension
// directory what kairos-agent does (a .raw file, links in common/ and in the
// merged dir), so the daemon's own listing sees the result.
type fakeExt struct {
	mu    sync.Mutex
	calls []string

	file       string // the file InstallExt creates
	img        ExtImage
	dissectErr error
	installErr error
	enableErr  error
	block      chan struct{} // when set, InstallExt waits for it to close
	started    chan struct{} // when set, receives once per InstallExt
}

func (f *fakeExt) rec(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *fakeExt) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeExt) InstallExt(catalogs []string, ref, version string, progress func(string)) error {
	f.rec(fmt.Sprintf("install %s %q %s", ref, version, strings.Join(catalogs, ",")))
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	progress("downloading " + ref)
	if f.installErr != nil {
		return f.installErr
	}
	if err := os.MkdirAll(extDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(extDir(), f.file), []byte("image"), 0o644)
}

func (f *fakeExt) EnableExt(file string, progress func(string)) error {
	f.rec("enable " + file)
	if f.enableErr != nil {
		return f.enableErr
	}
	for _, d := range []string{filepath.Join(extDir(), "common"), extMergedDir()} {
		os.MkdirAll(d, 0o755)
		os.Symlink(filepath.Join(extDir(), file), filepath.Join(d, file))
	}
	return nil
}

func (f *fakeExt) DisableExt(file string, progress func(string)) error {
	f.rec("disable " + file)
	os.Remove(filepath.Join(extDir(), "common", file))
	os.Remove(filepath.Join(extMergedDir(), file))
	return nil
}

func (f *fakeExt) RemoveExt(file string, progress func(string)) error {
	f.rec("remove " + file)
	os.Remove(filepath.Join(extDir(), "common", file))
	os.Remove(filepath.Join(extMergedDir(), file))
	return os.Remove(filepath.Join(extDir(), file))
}

func (f *fakeExt) Dissect(path string) (ExtImage, error) {
	f.rec("dissect " + filepath.Base(path))
	return f.img, f.dissectErr
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

var goodRel = map[string]string{"ID": "_any", "ARCHITECTURE": "x86-64"}

// extEnv is daemonEnv plus an extension directory, a merged dir, a state
// dir, an extensions.conf and a fake /usr holding libgcrypt with the content
// "base". It returns a daemon on the fake backend.
func extEnv(t *testing.T, fe *fakeExt) *Daemon {
	t.Helper()
	daemonEnv(t, "v0.0.5")
	dir := t.TempDir()
	t.Setenv("HADRON_UPDATE_EXT_DIR", filepath.Join(dir, "ext"))
	t.Setenv("HADRON_UPDATE_EXT_RUN_DIR", filepath.Join(dir, "merged"))
	t.Setenv("HADRON_UPDATE_EXT_STATE", filepath.Join(dir, "state"))
	t.Setenv("HADRON_UPDATE_EXT_CONF", filepath.Join(dir, "extensions.conf"))
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(filepath.Join(root, "usr/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/lib/libgcrypt.so.20"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, oldArch := hostRoot, extGOARCH
	hostRoot, extGOARCH = root, "amd64"
	t.Cleanup(func() { hostRoot, extGOARCH = old, oldArch })
	d := NewDaemon(&fakeBackend{})
	d.ext = fe
	return d
}

// gpgImage is an extension that ships its own libgcrypt with content lib.
func gpgImage(lib string) ExtImage {
	return ExtImage{Release: goodRel, Entries: []mtreeEntry{
		{Path: ".", Type: "dir"},
		{Path: "usr", Type: "dir"},
		{Path: "usr/lib", Type: "dir"},
		{Path: "usr/bin/gpg", Type: "file", Size: 3, SHA256: sha("gpg")},
		{Path: "usr/lib/libgcrypt.so.20", Type: "file", Size: int64(len(lib)), SHA256: sha(lib)},
		{Path: "usr/lib/extension-release.d/extension-release.gpg", Type: "file", Size: 3, SHA256: sha("x")},
	}}
}

func runExtJob(t *testing.T, d *Daemon, start func() error) Status {
	t.Helper()
	if err := start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	d.wg.Wait()
	st := d.Status()
	if st.State == StateExtension || st.ExtResult == "" {
		t.Fatalf("job did not end: %+v", st)
	}
	return st
}

func TestCatalogsConf(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "extensions.conf")
	t.Setenv("HADRON_UPDATE_EXT_CONF", conf)
	if got := Catalogs(); len(got) != 1 || got[0] != defaultCatalog {
		t.Fatalf("no file: %v", got)
	}
	os.WriteFile(conf, []byte("# ours first\ncatalog = https://a.example/x.json\nother=1\ncatalog=ftp://b.example/\ncatalog=https://c.example/a,b\ncatalog=http://d.example/y.json\n"), 0o644)
	if got := strings.Join(Catalogs(), " "); got != "https://a.example/x.json http://d.example/y.json" {
		t.Fatalf("catalogs %q", got)
	}
	os.WriteFile(conf, []byte("catalog=nonsense\n"), 0o644)
	if got := Catalogs(); len(got) != 1 || got[0] != defaultCatalog {
		t.Fatalf("no valid line: %v", got)
	}
}

const testCatalog = `{
  "repo": "kairos-io/hadron-layers",
  "layers": [
    {"name": "tailscale", "title": "Tailscale", "description": "VPN\nmesh", "latest": "1.2",
     "tags": [{"tag": "1.2", "sysext": {"amd64": {"oci": "ghcr.io/x/tailscale@sha256:aa"}, "arm64": {"oci": "y"}}},
              {"tag": "1.1", "sysext": {"amd64": {"oci": "ghcr.io/x/tailscale@sha256:bb"}}},
              {"tag": "1.0", "sysext": {}}]},
    {"name": "git", "description": "only arm", "latest": "2.5",
     "tags": [{"tag": "2.5", "sysext": {"arm64": {"oci": "z"}}}]},
    {"name": "-rf", "latest": "1", "tags": [{"tag": "1", "sysext": {"amd64": {"oci": "q"}}}]},
    {"name": "gpg", "description": "GnuPG", "latest": "2.4",
     "tags": [{"tag": "2.4", "sysext": {"amd64": {"oci": "ghcr.io/x/gpg@sha256:cc"}}}]}
  ]
}`

func TestParseCatalog(t *testing.T) {
	got, err := parseCatalog(strings.NewReader(testCatalog), "https://c/1", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "tailscale" || got[1].Name != "gpg" {
		t.Fatalf("entries %+v", got)
	}
	ts := got[0]
	if ts.Title != "Tailscale" || ts.Description != "VPN mesh" || ts.Latest != "1.2" ||
		strings.Join(ts.Versions, ",") != "1.2,1.1" || ts.Catalog != "https://c/1" {
		t.Fatalf("tailscale %+v", ts)
	}
	if _, err := parseCatalog(strings.NewReader("{"), "u", "amd64"); err == nil {
		t.Fatal("broken JSON parsed")
	}
}

func TestDaemonCatalogMergesAndMarks(t *testing.T) {
	d := extEnv(t, &fakeExt{})
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"layers":[{"name":"gpg","description":"ours","latest":"9","tags":[{"tag":"9","sysext":{"amd64":{"oci":"o"}}}]}]}`)
	}))
	t.Cleanup(first.Close)
	down := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(down.Close)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, testCatalog) }))
	t.Cleanup(second.Close)
	os.WriteFile(os.Getenv("HADRON_UPDATE_EXT_CONF"),
		[]byte("catalog="+first.URL+"\ncatalog="+down.URL+"\ncatalog="+second.URL+"\n"), 0o644)
	os.MkdirAll(extDir(), 0o755)
	os.WriteFile(filepath.Join(extDir(), "tailscale.sysext.raw"), nil, 0o644)

	got, err := d.Catalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, e := range got {
		s = append(s, fmt.Sprintf("%s:%s:%v", e.Name, e.Description, e.Installed))
	}
	// The first catalog wins for gpg; the one that is down is skipped.
	if strings.Join(s, " ") != "gpg:ours:false tailscale:VPN mesh:true" {
		t.Fatalf("catalog %q", s)
	}

	os.WriteFile(os.Getenv("HADRON_UPDATE_EXT_CONF"), []byte("catalog="+down.URL+"\n"), 0o644)
	if _, err := d.Catalog(t.Context()); err == nil {
		t.Fatal("no readable catalog is not an error")
	}
}

func TestParseMtree(t *testing.T) {
	in := `./ type=dir mode=0755 uid=0 gid=0
usr/ type=dir mode=0755 uid=0 gid=0
usr/bin/tailscale type=file mode=0755 uid=0 gid=0 size=12 sha256sum=abc time=1.0
usr/lib/libx.so type=link uid=0 gid=0 link=libx.so.1
usr/share/a\x20b type=file mode=0644 uid=0 gid=0 size=0
usr/share/odd\\name type=file mode=0644 size=1 sha256sum=ff
`
	got, err := parseMtree(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []mtreeEntry{
		{Path: ".", Type: "dir"},
		{Path: "usr", Type: "dir"},
		{Path: "usr/bin/tailscale", Type: "file", Size: 12, SHA256: "abc"},
		{Path: "usr/lib/libx.so", Type: "link", Link: "libx.so.1"},
		{Path: "usr/share/a b", Type: "file"},
		{Path: `usr/share/odd\name`, Type: "file", Size: 1, SHA256: "ff"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestCheckRelease(t *testing.T) {
	for _, c := range []struct {
		rel map[string]string
		ok  bool
	}{
		{goodRel, true},
		{map[string]string{"ID": "hadron", "ARCHITECTURE": "x86-64"}, true},
		{map[string]string{"ID": "_any", "ARCHITECTURE": "arm64"}, false},
		{map[string]string{"ID": "_any"}, false},
		{map[string]string{"ID": "fedora", "ARCHITECTURE": "x86-64"}, false},
		{nil, false},
	} {
		if err := checkRelease(c.rel, "amd64"); (err == nil) != c.ok {
			t.Errorf("%v: %v, want ok=%v", c.rel, err, c.ok)
		}
	}
}

func TestFindConflicts(t *testing.T) {
	root := t.TempDir()
	mk := func(p, content string) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte(content), 0o644)
	}
	mk("usr/lib/libsame.so", "same")
	mk("usr/lib/libdiff.so", "base")
	mk("usr/lib/libsize.so", "longer base")
	mk("usr/bin/notadir", "x")
	mk("usr/share/empty", "")
	os.MkdirAll(filepath.Join(root, "usr/lib/realdir"), 0o755)
	os.Symlink("realdir", filepath.Join(root, "usr/lib/dirlink"))
	os.Symlink("libsame.so", filepath.Join(root, "usr/lib/link-same"))
	os.Symlink("other", filepath.Join(root, "usr/lib/link-diff"))
	mk("etc/passwd", "root")

	entries := []mtreeEntry{
		{Path: "usr", Type: "dir"},
		{Path: "usr/lib/dirlink", Type: "dir"},                                   // a link to a directory: fine
		{Path: "usr/lib/libsame.so", Type: "file", Size: 4, SHA256: sha("same")}, // identical: fine
		{Path: "usr/lib/libdiff.so", Type: "file", Size: 4, SHA256: sha("ours")}, // different: conflict
		{Path: "usr/lib/libsize.so", Type: "file", Size: 4, SHA256: sha("ours")}, // other size: conflict
		{Path: "usr/lib/libnew.so", Type: "file", Size: 3, SHA256: sha("new")},   // new: fine
		{Path: "usr/share/empty", Type: "file"},                                  // both empty: fine
		{Path: "usr/lib/link-same", Type: "link", Link: "libsame.so"},
		{Path: "usr/lib/link-diff", Type: "link", Link: "libdiff.so"},
		{Path: "usr/bin/notadir", Type: "dir"},                              // a file where it has a dir
		{Path: "usr/bin/notadir/inner", Type: "file", Size: 1, SHA256: "x"}, // under that file: not a second report
		{Path: "usr/lib/libsame.so", Type: "link", Link: "x"},               // a file where it has a link
		{Path: "etc/passwd", Type: "file", Size: 1, SHA256: "x"},            // not merged
		{Path: "usr/lib/extension-release.d/extension-release.x", Type: "file", Size: 1, SHA256: "x"},
	}
	got, err := findConflicts(root, entries)
	if err != nil {
		t.Fatal(err)
	}
	want := "/usr/lib/libdiff.so /usr/lib/libsize.so /usr/lib/link-diff /usr/bin/notadir /usr/lib/libsame.so"
	if strings.Join(got, " ") != want {
		t.Fatalf("conflicts %q\nwant      %q", strings.Join(got, " "), want)
	}
}

func TestExtInstallIdenticalFilesAreFine(t *testing.T) {
	fe := &fakeExt{file: "gpg.sysext.raw", img: gpgImage("base")}
	d := extEnv(t, fe)
	st := runExtJob(t, d, func() error { return d.InstallExtension("gpg", "2.4", false) })
	if st.ExtResult != extResultOK || st.State != StateIdle {
		t.Fatalf("status %+v", st)
	}
	want := []string{`install gpg "2.4" ` + defaultCatalog, "dissect gpg.sysext.raw", "enable gpg.sysext.raw"}
	if got := fe.log(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q", got)
	}
	list, _ := d.Extensions()
	if len(list) != 1 || list[0].Name != "gpg" || !list[0].Enabled || !list[0].Merged || list[0].Source != "gpg@2.4" {
		t.Fatalf("installed %+v", list)
	}
	// A second install of the same name is refused before any job.
	if err := d.InstallExtension("gpg", "", false); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("second install: %v", err)
	}
}

func TestExtInstallRefusesAConflictAndRemovesIt(t *testing.T) {
	fe := &fakeExt{file: "gpg.sysext.raw", img: gpgImage("other build")}
	d := extEnv(t, fe)
	st := runExtJob(t, d, func() error { return d.InstallExtension("gpg", "", false) })
	if st.ExtResult != extResultConflict || !strings.Contains(st.ExtDetail, "/usr/lib/libgcrypt.so.20") ||
		strings.Contains(st.ExtDetail, "/usr/bin/gpg") {
		t.Fatalf("status %+v", st)
	}
	if got := fe.log(); got[len(got)-1] != "remove gpg.sysext.raw" {
		t.Fatalf("calls %q: the refused extension must be removed", got)
	}
	if list, _ := d.Extensions(); len(list) != 0 {
		t.Fatalf("left installed: %+v", list)
	}

	// --force installs it anyway.
	fe.calls = nil
	st = runExtJob(t, d, func() error { return d.InstallExtension("gpg", "", true) })
	if st.ExtResult != extResultOK {
		t.Fatalf("forced: %+v", st)
	}
	if got := fe.log(); got[len(got)-1] != "enable gpg.sysext.raw" {
		t.Fatalf("forced calls %q", got)
	}
}

func TestExtInstallWrongArchitectureIsNeverForced(t *testing.T) {
	img := gpgImage("base")
	img.Release = map[string]string{"ID": "_any", "ARCHITECTURE": "arm64"}
	fe := &fakeExt{file: "gpg.raw", img: img}
	d := extEnv(t, fe)
	st := runExtJob(t, d, func() error { return d.InstallExtension("gpg", "", true) })
	if st.ExtResult != extResultFailed || !strings.Contains(st.ExtDetail, "arm64") {
		t.Fatalf("status %+v", st)
	}
	if list, _ := d.Extensions(); len(list) != 0 {
		t.Fatalf("left installed: %+v", list)
	}
}

func TestExtInstallFailures(t *testing.T) {
	fe := &fakeExt{file: "x.raw", img: gpgImage("base"), installErr: errors.New("pull failed")}
	d := extEnv(t, fe)
	st := runExtJob(t, d, func() error { return d.InstallExtension("x", "", false) })
	if st.ExtResult != extResultFailed || !strings.Contains(st.ExtDetail, "pull failed") {
		t.Fatalf("install error: %+v", st)
	}
	fe.installErr, fe.dissectErr = nil, errors.New("bad image")
	st = runExtJob(t, d, func() error { return d.InstallExtension("x", "", false) })
	if st.ExtResult != extResultFailed || !strings.Contains(st.ExtDetail, "bad image") {
		t.Fatalf("dissect error: %+v", st)
	}
	if list, _ := d.Extensions(); len(list) != 0 {
		t.Fatalf("an unreadable image stayed: %+v", list)
	}
}

func TestExtInstallRefusals(t *testing.T) {
	fe := &fakeExt{file: "x.raw", img: gpgImage("base")}
	d := extEnv(t, fe)
	for _, c := range []struct{ ref, version string }{
		{"", ""}, {"-rf", ""}, {"--catalog=x", ""}, {"a b", ""}, {"../x", ""}, {"x\n", ""},
		{"tailscale", "--evil"}, {"oci:ghcr.io/x/y:1", "1.0"}, {strings.Repeat("a", 600), ""},
	} {
		if err := d.InstallExtension(c.ref, c.version, false); err == nil {
			t.Errorf("InstallExtension(%q, %q) accepted", c.ref, c.version)
		}
	}
	// A reference needs advanced mode.
	os.WriteFile(os.Getenv("HADRON_UPDATE_CONF"), []byte("advanced=false\n"), 0o644)
	if err := d.InstallExtension("oci:ghcr.io/x/y:1", "", false); !errors.Is(err, errExtNeedsAdvanced) {
		t.Fatalf("reference without advanced mode: %v", err)
	}
	if err := d.InstallExtension("/tmp/x.raw", "", false); !errors.Is(err, errExtNeedsAdvanced) {
		t.Fatalf("path without advanced mode: %v", err)
	}
	os.WriteFile(os.Getenv("HADRON_UPDATE_CONF"), []byte("advanced=true\n"), 0o644)
	st := runExtJob(t, d, func() error { return d.InstallExtension("oci:ghcr.io/x/y:1", "", false) })
	if st.ExtResult != extResultOK {
		t.Fatalf("reference in advanced mode: %+v", st)
	}
	if len(fe.log()) == 0 || !strings.HasPrefix(fe.log()[0], `install oci:ghcr.io/x/y:1 ""`) {
		t.Fatalf("calls %q", fe.log())
	}

	// No release file: nothing is installed.
	t.Setenv("HADRON_UPDATE_RELEASE_FILE", filepath.Join(t.TempDir(), "missing"))
	u := NewDaemon(&fakeBackend{})
	u.ext = fe
	if err := u.InstallExtension("y", "", false); !errors.Is(err, errUnmanaged) {
		t.Fatalf("unmanaged: %v", err)
	}
}

// TestExtSharesTheJobSlot: an extension job and an upgrade never run
// kairos-agent at the same time, and nothing reboots under either.
func TestExtSharesTheJobSlot(t *testing.T) {
	dg := daemonEnv(t, "v0.0.5")
	fe := &fakeExt{file: "x.raw", img: gpgImage("base"), block: make(chan struct{}), started: make(chan struct{}, 1)}
	d := extEnv(t, fe)
	if err := d.InstallExtension("x", "", false); err != nil {
		t.Fatal(err)
	}
	<-fe.started
	if st := d.Status(); st.State != StateExtension || !d.Busy() {
		t.Fatalf("status during the job %+v", st)
	}
	ref := ghcrRepo + ":sway-v0.0.6"
	if err := d.Apply(t.Context(), ref, dg[ref], false); !errors.Is(err, ErrBusy) {
		t.Fatalf("Apply during an extension job: %v", err)
	}
	if err := d.Check(t.Context()); !errors.Is(err, ErrBusy) {
		t.Fatalf("Check during an extension job: %v", err)
	}
	if err := d.InstallExtension("y", "", false); !errors.Is(err, ErrBusy) {
		t.Fatalf("second extension job: %v", err)
	}
	if err := d.Reboot(); !errors.Is(err, ErrBusy) {
		t.Fatalf("Reboot during an extension job: %v", err)
	}
	if err := d.RebootPrevious(); !errors.Is(err, ErrBusy) {
		t.Fatalf("RebootPrevious during an extension job: %v", err)
	}
	if err := d.Cancel(); !errors.Is(err, errExtRunning) {
		t.Fatalf("Cancel during an extension job: %v", err)
	}
	close(fe.block)
	d.wg.Wait()
	if st := d.Status(); st.State != StateIdle || st.ExtResult != extResultOK {
		t.Fatalf("after the job %+v", st)
	}

	// And the other way round: during an upgrade, an extension job waits.
	b := &fakeBackend{block: make(chan struct{}), started: make(chan struct{}, 1)}
	d2 := NewDaemon(b)
	d2.ext = fe
	if err := d2.Apply(t.Context(), ref, dg[ref], false); err != nil {
		t.Fatal(err)
	}
	<-b.started
	if err := d2.InstallExtension("y", "", false); !errors.Is(err, ErrBusy) {
		t.Fatalf("extension job during an upgrade: %v", err)
	}
	close(b.block)
	d2.wg.Wait()
}

func TestExtEnableDisableRemove(t *testing.T) {
	fe := &fakeExt{file: "gpg.sysext.raw", img: gpgImage("other")}
	d := extEnv(t, fe)
	os.MkdirAll(extDir(), 0o755)
	os.WriteFile(filepath.Join(extDir(), "gpg.sysext.raw"), nil, 0o644)
	recordSource("gpg.sysext.raw", "gpg@2.4")

	st := runExtJob(t, d, func() error { return d.SetExtensionEnabled("gpg", true, false) })
	if st.ExtResult != extResultConflict {
		t.Fatalf("enable over a conflict: %+v", st)
	}
	if list, _ := d.Extensions(); len(list) != 1 || list[0].Enabled {
		t.Fatalf("a refused enable must leave it installed and off: %+v", list)
	}
	st = runExtJob(t, d, func() error { return d.SetExtensionEnabled("gpg", true, true) })
	if st.ExtResult != extResultOK {
		t.Fatalf("forced enable: %+v", st)
	}
	if list, _ := d.Extensions(); !list[0].Enabled || !list[0].Merged {
		t.Fatalf("not enabled: %+v", list)
	}
	st = runExtJob(t, d, func() error { return d.SetExtensionEnabled("gpg", false, false) })
	if list, _ := d.Extensions(); st.ExtResult != extResultOK || list[0].Enabled || list[0].Merged {
		t.Fatalf("disable: %+v %+v", st, list)
	}
	st = runExtJob(t, d, func() error { return d.RemoveExtension("gpg") })
	if list, _ := d.Extensions(); st.ExtResult != extResultOK || len(list) != 0 {
		t.Fatalf("remove: %+v %+v", st, list)
	}
	if _, err := os.Stat(filepath.Join(extStateDir(), "gpg.sysext.raw")); !os.IsNotExist(err) {
		t.Fatal("the source record outlived the extension")
	}
	if err := d.RemoveExtension("gpg"); err == nil {
		t.Fatal("removed an extension that is not installed")
	}
	if err := d.SetExtensionEnabled("../gpg", true, false); err == nil {
		t.Fatal("accepted a path as a name")
	}
}

func TestFindInstalledIsNotAPrefixGuess(t *testing.T) {
	extEnv(t, &fakeExt{})
	os.MkdirAll(extDir(), 0o755)
	for _, f := range []string{"tailscale.raw", "tailscale.sysext.raw", "git.raw"} {
		os.WriteFile(filepath.Join(extDir(), f), nil, 0o644)
	}
	if x, err := findInstalled("git"); err != nil || x.File != "git.raw" {
		t.Fatalf("git: %+v %v", x, err)
	}
	// Both are called tailscale once .raw and .sysext go: ask for the file.
	if x, err := findInstalled("tailscale"); err != nil || x.File != "tailscale.raw" {
		t.Fatalf("tailscale: %+v %v", x, err)
	}
	if x, err := findInstalled("tailscale.sysext.raw"); err != nil || x.File != "tailscale.sysext.raw" {
		t.Fatalf("by file: %+v %v", x, err)
	}
	if _, err := findInstalled("tail"); err == nil {
		t.Fatal("matched a name by its prefix")
	}
}

// TestKairosBackendExtArgv runs the real backend against a fake kairos-agent
// and a fake systemd-dissect.
func TestKairosBackendExtArgv(t *testing.T) {
	agent, argv := fakeTool(t, 0)
	t.Setenv("HADRON_UPDATE_AGENT", agent)
	read := func() string { b, _ := os.ReadFile(argv); return strings.TrimSpace(string(b)) }
	var kb kairosBackend
	var lines []string
	if err := kb.InstallExt([]string{"https://a/x.json", "https://b/y.json"}, "tailscale", ">= 1, < 2", func(l string) { lines = append(lines, l) }); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "sysext\ninstall\n--catalog=https://a/x.json\n--catalog=https://b/y.json\n--version=>= 1, < 2\n--\ntailscale" {
		t.Fatalf("install argv %q", got)
	}
	kb.InstallExt([]string{"https://a/x.json"}, "oci:ghcr.io/x/y:1", "", func(string) {})
	if got := read(); got != "sysext\ninstall\n--catalog=https://a/x.json\n--\noci:ghcr.io/x/y:1" {
		t.Fatalf("install by reference argv %q", got)
	}
	kb.EnableExt("t.raw", func(string) {})
	if got := read(); got != "sysext\nenable\n--common\n--now\n--\nt.raw" {
		t.Fatalf("enable argv %q", got)
	}
	kb.DisableExt("t.raw", func(string) {})
	if got := read(); got != "sysext\ndisable\n--common\n--now\n--\nt.raw" {
		t.Fatalf("disable argv %q", got)
	}
	kb.RemoveExt("t.raw", func(string) {})
	if got := read(); got != "sysext\nremove\n--now\n--\nt.raw" {
		t.Fatalf("remove argv %q", got)
	}
	failing, _ := fakeTool(t, 3)
	t.Setenv("HADRON_UPDATE_AGENT", failing)
	if err := kb.EnableExt("t.raw", func(string) {}); err == nil || !strings.Contains(err.Error(), "kairos-agent sysext enable") {
		t.Fatalf("a failing kairos-agent: %v", err)
	}

	dir := t.TempDir()
	dissect := filepath.Join(dir, "systemd-dissect")
	script := `#!/bin/sh
echo "$@" >> ` + filepath.Join(dir, "log") + `
case "$*" in
  *--json=short*) echo '{"name":"t.raw","sysextRelease":{"ID":"_any","ARCHITECTURE":"x86-64"}}' ;;
  *--mtree*) printf 'usr/ type=dir mode=0755\nusr/bin/t type=file mode=0755 size=2 sha256sum=ab\n' ;;
esac
`
	os.WriteFile(dissect, []byte(script), 0o755)
	t.Setenv("HADRON_UPDATE_DISSECT", dissect)
	img, err := kb.Dissect("/var/lib/kairos/extensions/t.raw")
	if err != nil {
		t.Fatal(err)
	}
	if img.Release["ARCHITECTURE"] != "x86-64" || len(img.Entries) != 2 || img.Entries[1].SHA256 != "ab" {
		t.Fatalf("image %+v", img)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "log"))
	if got := string(b); got != "--no-pager --json=short /var/lib/kairos/extensions/t.raw\n--no-pager --mtree --mtree-hash=yes /var/lib/kairos/extensions/t.raw\n" {
		t.Fatalf("dissect calls %q", got)
	}
}
