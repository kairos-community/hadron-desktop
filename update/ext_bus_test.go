package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

// TestBusExtensionsRefuseUnauthorized: every extension change asks polkit
// for the extensions action, and a no reaches neither kairos-agent nor the
// argument checks. The read-only methods ask nothing.
func TestBusExtensionsRefuseUnauthorized(t *testing.T) {
	fe := &fakeExt{file: "gpg.raw", img: gpgImage("base")}
	d := extEnv(t, fe)
	os.MkdirAll(extDir(), 0o755)
	os.WriteFile(filepath.Join(extDir(), "gpg.raw"), nil, 0o644)
	auth := &fakeAuth{allow: false}
	f := startService(t, d, auth)

	calls := []*dbus.Call{
		f.call("InstallExtension", "tailscale", "", false),
		f.call("SetExtensionEnabled", "gpg", true, false),
		f.call("SetExtensionEnabled", "gpg", false, false),
		f.call("RemoveExtension", "gpg"),
		// Bad arguments are not even looked at.
		f.call("InstallExtension", "-rf", "", true),
	}
	for i, c := range calls {
		if got := errName(c.Err); got != busInterface+".Error.NotAuthorized" {
			t.Fatalf("call %d: %s (%v), want NotAuthorized", i, got, c.Err)
		}
	}
	me := f.client.Names()[0]
	for _, q := range auth.questions() {
		if q != me+" "+actionExtensions {
			t.Fatalf("asked %q, want the extensions action", q)
		}
	}
	if n := len(auth.questions()); n != len(calls) {
		t.Fatalf("asked %d times, want %d", n, len(calls))
	}
	if len(fe.log()) != 0 {
		t.Fatalf("the backend was called: %q", fe.log())
	}

	var list []map[string]dbus.Variant
	if err := f.call("Extensions").Store(&list); err != nil {
		t.Fatalf("Extensions: %v", err)
	}
	if len(list) != 1 || list[0]["name"].Value() != "gpg" || list[0]["enabled"].Value() != false {
		t.Fatalf("Extensions %v", list)
	}
	if n := len(auth.questions()); n != len(calls) {
		t.Fatal("Extensions asked polkit")
	}
}

// TestBusExtensionsRefuseAFlatpakCaller: a sandboxed app is refused before
// polkit is asked, as for Apply.
func TestBusExtensionsRefuseAFlatpakCaller(t *testing.T) {
	fe := &fakeExt{file: "gpg.raw", img: gpgImage("base")}
	d := extEnv(t, fe)
	fakeProc(t, uint32(os.Getpid()), true)
	auth := &fakeAuth{allow: true}
	f := startService(t, d, auth)
	for _, c := range []*dbus.Call{
		f.call("InstallExtension", "gpg", "", false),
		f.call("SetExtensionEnabled", "gpg", true, false),
		f.call("RemoveExtension", "gpg"),
	} {
		if got := errName(c.Err); got != busInterface+".Error.NotAuthorized" {
			t.Fatalf("from a sandbox: %s", got)
		}
	}
	if len(auth.questions()) != 0 || len(fe.log()) != 0 {
		t.Fatalf("polkit asked %d times, backend calls %q", len(auth.questions()), fe.log())
	}
}

// TestBusExtensionInstall runs an install through the bus: the job's states
// arrive as StateChanged, its lines as Progress, and its outcome in Status.
func TestBusExtensionInstall(t *testing.T) {
	fe := &fakeExt{file: "gpg.sysext.raw", img: gpgImage("other")}
	d := extEnv(t, fe)
	cat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, testCatalog) }))
	t.Cleanup(cat.Close)
	os.WriteFile(os.Getenv("HADRON_UPDATE_EXT_CONF"), []byte("catalog="+cat.URL+"\n"), 0o644)
	f := startService(t, d, &fakeAuth{allow: true})

	var entries []map[string]dbus.Variant
	if err := f.call("Catalog").Store(&entries); err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(entries) != 2 || entries[1]["name"].Value() != "gpg" || entries[1]["installed"].Value() != false {
		t.Fatalf("Catalog %v", entries)
	}
	if v, _ := entries[0]["versions"].Value().([]string); strings.Join(v, ",") != "1.2,1.1" {
		t.Fatalf("versions %v", entries[0]["versions"])
	}

	sigs := make(chan *dbus.Signal, 64)
	f.client.Signal(sigs)
	if err := f.client.AddMatchSignal(dbus.WithMatchInterface(busInterface)); err != nil {
		t.Fatal(err)
	}
	if err := f.call("InstallExtension", "gpg", "", false).Err; err != nil {
		t.Fatalf("InstallExtension: %v", err)
	}
	var states, lines []string
	for len(states) < 2 {
		ev, ok := eventFromSignal(<-sigs)
		if !ok {
			continue
		}
		if ev.Status != nil {
			states = append(states, string(ev.Status.State)+":"+ev.Status.ExtResult)
		} else {
			lines = append(lines, ev.Line)
		}
	}
	if strings.Join(states, " ") != "extension: idle:conflict" {
		t.Fatalf("states %q", states)
	}
	if len(lines) == 0 || lines[0] != "downloading gpg" {
		t.Fatalf("progress %q", lines)
	}
	st := f.status(t)
	if st["ext_result"].Value() != extResultConflict || !strings.Contains(st["ext_detail"].Value().(string), "libgcrypt") {
		t.Fatalf("status %v", st)
	}

	// A bad name is refused as an error, after authorization.
	if got := errName(f.call("SetExtensionEnabled", "../x", true, false).Err); got != busInterface+".Error.Failed" {
		t.Fatalf("bad name: %s", got)
	}
	// Advanced mode off refuses a URL by name Refused.
	os.WriteFile(os.Getenv("HADRON_UPDATE_CONF"), []byte("advanced=false\n"), 0o644)
	if got := errName(f.call("InstallExtension", "oci:x/y:1", "", false).Err); got != busInterface+".Error.Refused" {
		t.Fatalf("URL without advanced mode: %s", got)
	}

	// The client decodes the same.
	c := &client{conn: f.client, obj: f.obj}
	got, err := c.Catalog()
	if err != nil || len(got) != 2 || got[0].Name != "tailscale" || got[0].Description != "VPN mesh" {
		t.Fatalf("client Catalog %+v %v", got, err)
	}
	if _, err := c.ExtensionFiles("gpg"); err == nil {
		t.Fatal("files of an extension that is not installed")
	}
}
