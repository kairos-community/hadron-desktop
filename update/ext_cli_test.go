package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeExtClient is extClient without a bus. A started job feeds script to
// the watcher, as the service would.
type fakeExtClient struct {
	status    Status
	installed []InstalledExt
	catalog   []CatalogEntry
	files     []string
	startErr  error
	script    []Event

	calls  []string
	events chan Event
}

func (f *fakeExtClient) rec(s string)            { f.calls = append(f.calls, s) }
func (f *fakeExtClient) Status() (Status, error) { return f.status, nil }
func (f *fakeExtClient) Watch() (<-chan Event, func(), error) {
	f.events = make(chan Event, 16)
	return f.events, func() {}, nil
}
func (f *fakeExtClient) Extensions() ([]InstalledExt, error) { return f.installed, nil }
func (f *fakeExtClient) Catalog() ([]CatalogEntry, error)    { f.rec("Catalog"); return f.catalog, nil }
func (f *fakeExtClient) ExtensionFiles(name string) ([]string, error) {
	f.rec("Files " + name)
	return f.files, nil
}
func (f *fakeExtClient) start(call string) error {
	f.rec(call)
	if f.startErr != nil {
		return f.startErr
	}
	for _, e := range f.script {
		f.events <- e
	}
	close(f.events)
	return nil
}
func (f *fakeExtClient) InstallExtension(ref, version string, force bool) error {
	return f.start(fmt.Sprintf("Install %s %q %v", ref, version, force))
}
func (f *fakeExtClient) SetExtensionEnabled(name string, on, force bool) error {
	return f.start(fmt.Sprintf("Enable %s %v %v", name, on, force))
}
func (f *fakeExtClient) RemoveExtension(name string) error { return f.start("Remove " + name) }
func (f *fakeExtClient) Close()                            {}

func (f *fakeExtClient) startedAny() bool {
	for _, c := range f.calls {
		if c != "Catalog" && !strings.HasPrefix(c, "Files") {
			return true
		}
	}
	return false
}

func extDone(result, detail string) []Event {
	return []Event{
		{Status: &Status{State: StateIdle}}, // a stale state before the job: ignored
		{Status: &Status{State: StateExtension}},
		{Line: "downloading"},
		{Status: &Status{State: StateIdle, ExtResult: result, ExtDetail: detail}},
	}
}

func newExtClient() *fakeExtClient {
	return &fakeExtClient{
		status: Status{Advanced: true},
		catalog: []CatalogEntry{
			{Name: "tailscale", Title: "Tailscale", Description: "VPN", Latest: "1.2", Versions: []string{"1.2", "1.1"}, Catalog: "https://c/1"},
			{Name: "gpg", Description: "GnuPG", Latest: "2.4", Versions: []string{"2.4"}, Catalog: "https://c/1", Installed: true, Enabled: true},
		},
		installed: []InstalledExt{{Name: "gpg", File: "gpg.sysext.raw", Enabled: true, Merged: true, Source: "gpg@2.4"}},
		script:    extDone(extResultOK, "done."),
	}
}

func useExt(t *testing.T, f *fakeExtClient) {
	t.Helper()
	setup(t, "v0.1.0", "v0.1.0")
	old, oldWait := dialExt, busyWait
	dialExt = func() (extClient, error) { return f, nil }
	busyWait = 0
	t.Cleanup(func() { dialExt, busyWait = old, oldWait })
}

func TestExtCLIList(t *testing.T) {
	f := newExtClient()
	useExt(t, f)
	code, out, _ := runCLI("", "ext")
	if code != exitOK || !strings.Contains(out, "gpg   on") || !strings.Contains(out, "gpg@2.4") {
		t.Fatalf("list: %d %q", code, out)
	}
	f.installed = nil
	if _, out, _ := runCLI("", "ext", "list"); !strings.Contains(out, "No system extensions are installed") {
		t.Fatalf("empty list %q", out)
	}
	if got := extState(InstalledExt{Enabled: true}); got != "on from the next boot" {
		t.Fatal(got)
	}
	if got := extState(InstalledExt{Merged: true}); got != "off from the next boot" {
		t.Fatal(got)
	}
}

func TestExtCLIBrowse(t *testing.T) {
	f := newExtClient()
	useExt(t, f)
	_, out, _ := runCLI("", "ext", "browse")
	if !strings.Contains(out, "  tailscale    1.2        VPN") || !strings.Contains(out, "✓ gpg") {
		t.Fatalf("browse %q", out)
	}
	if _, out, _ := runCLI("", "ext", "browse", "VPN"); strings.Contains(out, "gpg") || !strings.Contains(out, "tailscale") {
		t.Fatalf("browse VPN %q", out)
	}
	if _, out, _ := runCLI("", "ext", "browse", "nothing"); !strings.Contains(out, `No extension in the catalogs matches "nothing"`) {
		t.Fatalf("browse nothing %q", out)
	}
	// The lines hadron-menu shows: a mark, the name, the description, and
	// the URL item in advanced mode only.
	_, out, _ = runCLI("", "ext", "browse", "--menu")
	if out != "  tailscale — VPN\n✓ gpg — GnuPG\n"+menuURLItem+"\n" {
		t.Fatalf("menu %q", out)
	}
	f.status.Advanced = false
	if _, out, _ := runCLI("", "ext", "browse", "--menu"); strings.Contains(out, menuURLItem) {
		t.Fatalf("menu without advanced mode %q", out)
	}
}

func TestExtCLIInfo(t *testing.T) {
	f := newExtClient()
	f.files = []string{"/usr/bin/gpg", "/usr/lib/libgcrypt.so.20"}
	useExt(t, f)
	code, out, _ := runCLI("", "ext", "info", "gpg")
	if code != exitOK || !strings.Contains(out, "Versions:    2.4") || !strings.Contains(out, "  /usr/lib/libgcrypt.so.20") ||
		!strings.Contains(out, "Installed:   gpg.sysext.raw (on)") {
		t.Fatalf("info gpg: %d %q", code, out)
	}
	_, out, _ = runCLI("", "ext", "info", "tailscale")
	if !strings.Contains(out, "Installed:   no") || strings.Contains(out, "/usr/") {
		t.Fatalf("info tailscale %q", out)
	}
	if code, _, errOut := runCLI("", "ext", "info", "nope"); code != exitError || !strings.Contains(errOut, "no catalog offers") {
		t.Fatalf("info nope: %d %q", code, errOut)
	}
}

func TestExtCLIInstallFromTheCatalog(t *testing.T) {
	f := newExtClient()
	useExt(t, f)
	code, out, _ := runCLI("\n", "ext", "install", "tailscale")
	if code != exitOK || !strings.Contains(out, "Description: VPN") || !strings.Contains(out, "Install it? [Y/n]") ||
		!strings.Contains(out, "downloading") || !strings.HasSuffix(out, "done.\n") {
		t.Fatalf("install: %d %q", code, out)
	}
	if got := f.calls[len(f.calls)-1]; got != `Install tailscale "" false` {
		t.Fatalf("call %q", got)
	}

	f = newExtClient()
	useExt(t, f)
	if code, out, _ := runCLI("n\n", "ext", "install", "tailscale"); code != exitOK || !strings.Contains(out, "Cancelled.") || f.startedAny() {
		t.Fatalf("cancelled: %d %q %q", code, out, f.calls)
	}
	runCLI("", "ext", "install", "tailscale@1.1", "--yes")
	if got := f.calls[len(f.calls)-1]; got != `Install tailscale "1.1" false` {
		t.Fatalf("versioned call %q", got)
	}
	if code, _, errOut := runCLI("", "ext", "install", "nope", "--yes"); code != exitError || !strings.Contains(errOut, "no catalog offers") {
		t.Fatalf("unknown name: %d %q", code, errOut)
	}
	if code, _, errOut := runCLI("", "ext", "install", "gpg", "--yes"); code != exitError || !strings.Contains(errOut, "already installed") {
		t.Fatalf("installed name: %d %q", code, errOut)
	}
}

func TestExtCLIConflictSuggestsForce(t *testing.T) {
	f := newExtClient()
	f.script = extDone(extResultConflict, "it replaces files ...: /usr/lib/libgcrypt.so.20")
	useExt(t, f)
	code, _, errOut := runCLI("", "ext", "install", "tailscale", "--yes")
	if code != exitError || !strings.Contains(errOut, "refused: it replaces") || !strings.Contains(errOut, "`hadron-ext install tailscale --force`") {
		t.Fatalf("conflict: %d %q", code, errOut)
	}
	f.script = extDone(extResultOK, "ok")
	code, out, _ := runCLI("", "ext", "install", "tailscale", "--yes", "--force")
	if code != exitOK || !strings.Contains(out, "Warning: --force") || f.calls[len(f.calls)-1] != `Install tailscale "" true` {
		t.Fatalf("forced: %d %q %q", code, out, f.calls)
	}
	f.script = extDone(extResultConflict, "it replaces files")
	if code, _, errOut := runCLI("", "ext", "enable", "gpg"); code != exitError || !strings.Contains(errOut, "`hadron-ext enable gpg --force`") {
		t.Fatalf("enable conflict: %d %q", code, errOut)
	}
	f.script = extDone(extResultFailed, "pull failed")
	if code, _, errOut := runCLI("", "ext", "install", "tailscale", "--yes"); code != exitError || !strings.Contains(errOut, "failed: pull failed") {
		t.Fatalf("failure: %d %q", code, errOut)
	}
}

func TestExtCLIInstallByURL(t *testing.T) {
	f := newExtClient()
	f.status.Advanced = false
	useExt(t, f)
	if code, _, errOut := runCLI("y\n", "ext", "install", "oci:ghcr.io/x/y:1"); code != exitUsage || !strings.Contains(errOut, "advanced mode is off") || f.startedAny() {
		t.Fatalf("without advanced mode: %d %q", code, errOut)
	}
	f.status.Advanced = true
	// --yes does not answer the question about an unchecked image.
	code, out, _ := runCLI("", "ext", "install", "oci:ghcr.io/x/y@sha256:abc", "--yes")
	if code != exitOK || !strings.Contains(out, "nobody checked it") || !strings.Contains(out, "[y/N]") || f.startedAny() {
		t.Fatalf("unanswered: %d %q %q", code, out, f.calls)
	}
	runCLI("y\n", "ext", "install", "oci:ghcr.io/x/y@sha256:abc")
	if got := f.calls[len(f.calls)-1]; got != `Install oci:ghcr.io/x/y@sha256:abc "" false` {
		t.Fatalf("call %q: an OCI digest is not a version", got)
	}
	// "-" asks for the URL first, as the menu's "Install from a URL…" does.
	runCLI("https://example.com/t.raw\ny\n", "ext", "install", "-")
	if got := f.calls[len(f.calls)-1]; got != `Install https://example.com/t.raw "" false` {
		t.Fatalf("call %q", got)
	}
}

func TestExtCLIEnableDisableRemove(t *testing.T) {
	f := newExtClient()
	useExt(t, f)
	runCLI("", "ext", "enable", "gpg", "--force")
	runCLI("", "ext", "disable", "gpg")
	if got := strings.Join(f.calls, "|"); got != "Enable gpg true true|Enable gpg false false" {
		t.Fatalf("calls %q", got)
	}
	f.calls = nil
	if code, out, _ := runCLI("\n", "ext", "remove", "gpg"); code != exitOK || !strings.Contains(out, "Cancelled.") || len(f.calls) != 0 {
		t.Fatalf("remove defaults to no: %d %q", code, out)
	}
	if code, _, _ := runCLI("y\n", "ext", "remove", "gpg"); code != exitOK || strings.Join(f.calls, "|") != "Remove gpg" {
		t.Fatalf("remove: %d %q", code, f.calls)
	}
	f.startErr = &daemonError{Name: "NotAuthorized", Msg: "not authorized"}
	if code, _, errOut := runCLI("", "ext", "disable", "gpg"); code != exitError || !strings.Contains(errOut, "Only members of the admin group can change system extensions.") {
		t.Fatalf("unauthorized: %d %q", code, errOut)
	}
}

func TestExtCLIUsage(t *testing.T) {
	f := newExtClient()
	useExt(t, f)
	for _, args := range [][]string{
		{"ext", "frobnicate"}, {"ext", "install"}, {"ext", "remove", "gpg", "--force"},
		{"ext", "list", "--menu"}, {"ext", "enable", "../x"}, {"ext", "install", "-rf"},
		{"ext", "info", "a", "b"},
	} {
		if code, _, _ := runCLI("", args...); code != exitUsage {
			t.Errorf("%q: exit %d, want %d", args, code, exitUsage)
		}
	}
	if f.startedAny() {
		t.Fatalf("a usage error reached the service: %q", f.calls)
	}
	if code, out, _ := runCLI("", "ext", "help"); code != exitOK || !strings.Contains(out, "hadron-ext [command]") {
		t.Fatalf("help: %d %q", code, out)
	}
}

func TestExtCLIOnAgentAndWithoutTheService(t *testing.T) {
	setupAgent(t, "v0.1.0", "v0.1.0")
	dialed := false
	old := dialExt
	dialExt = func() (extClient, error) { dialed = true; return nil, errors.New("no") }
	t.Cleanup(func() { dialExt = old })
	if code, _, errOut := runCLI("", "ext"); code != exitUsage || !strings.Contains(errOut, "agent flavor") || dialed {
		t.Fatalf("agent: %d %q dialed=%v", code, errOut, dialed)
	}
	setup(t, "v0.1.0", "v0.1.0")
	if code, _, errOut := runCLI("", "ext", "browse"); code != exitError || !strings.Contains(errOut, "not available (hadron-updated)") {
		t.Fatalf("no service: %d %q", code, errOut)
	}
}
