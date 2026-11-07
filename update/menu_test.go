package main

import (
	"strings"
	"testing"
)

// find returns the item with id anywhere in the tree.
func find(items []MenuItem, id string) (MenuItem, bool) {
	for _, it := range items {
		if it.ID == id {
			return it, true
		}
		if c, ok := find(it.Children, id); ok {
			return c, true
		}
	}
	return MenuItem{}, false
}

// ids lists every ID in the tree, depth first.
func ids(items []MenuItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.ID)
		out = append(out, ids(it.Children)...)
	}
	return out
}

var menuReleases = []Latest{{Tag: "v0.0.6"}, {Tag: "v0.0.5"}, {Tag: "v0.0.4"}}

func menuStatus(s State) Status {
	return Status{State: s, Installed: "v0.0.5", Latest: "v0.0.6", Advanced: true}
}

func TestBuildMenuUpdateAvailable(t *testing.T) {
	m := BuildMenu(menuStatus(StateUpdateAvailable), menuReleases, "sway", gpuFull)
	it, ok := find(m, "install-latest")
	if !ok || it.Label != "Install v0.0.6" || !it.Enabled {
		t.Fatalf("install-latest = %+v (%v)", it, ok)
	}
	if m[0].ID != "status" || m[0].Enabled {
		t.Fatalf("the first item must be the disabled status line: %+v", m[0])
	}
	for _, tag := range []string{"v0.0.6", "v0.0.4"} {
		if r, ok := find(m, "releases/"+tag); !ok || !r.Enabled || r.Label != tag {
			t.Errorf("releases/%s = %+v (%v)", tag, r, ok)
		}
	}
	if _, ok := find(m, "releases/v0.0.5"); ok {
		t.Error("the installed release is offered again")
	}
}

func TestBuildMenuPinned(t *testing.T) {
	st := menuStatus(StateUpToDate)
	st.Pin = "ghcr.io/kairos-community/hadron-desktop:sway-v0.0.5"
	m := BuildMenu(st, menuReleases, "sway", gpuFull)
	it, ok := find(m, "install-latest")
	if !ok || it.Label != "Install latest (v0.0.6)" || !it.Enabled {
		t.Fatalf("install-latest = %+v (%v)", it, ok)
	}
}

func TestBuildMenuUpToDateOffersNoInstall(t *testing.T) {
	st := menuStatus(StateUpToDate)
	st.Installed = "v0.0.6"
	if _, ok := find(BuildMenu(st, menuReleases, "sway", gpuFull), "install-latest"); ok {
		t.Fatal("install-latest on an up-to-date, unpinned system")
	}
}

func TestBuildMenuApplyingDisablesInstalls(t *testing.T) {
	m := BuildMenu(menuStatus(StateApplying), menuReleases, "sway", gpuFull)
	for _, id := range ids(m) {
		if id == "install-latest" || strings.HasPrefix(id, "releases/") || strings.HasPrefix(id, "switch/") || id == "advanced" {
			if it, _ := find(m, id); it.Enabled {
				t.Errorf("%s is enabled while applying", id)
			}
		}
	}
	if _, ok := find(m, "install-latest"); !ok {
		t.Error("install-latest should stay visible, disabled")
	}
	if _, ok := find(m, "cancel"); ok {
		t.Error("cancel offered while applying: kairos-agent is writing the passive slot")
	}
}

func TestBuildMenuInspectingOffersCancel(t *testing.T) {
	m := BuildMenu(menuStatus(StateInspecting), menuReleases, "sway", gpuFull)
	if it, ok := find(m, "cancel"); !ok || !it.Enabled {
		t.Fatalf("cancel = %+v (%v)", it, ok)
	}
	if it, _ := find(m, "install-latest"); it.Enabled {
		t.Error("install-latest enabled while inspecting")
	}
}

func TestBuildMenuRebootPending(t *testing.T) {
	m := BuildMenu(menuStatus(StateRebootPending), menuReleases, "sway", gpuFull)
	if it, ok := find(m, "restart"); !ok || !it.Enabled {
		t.Fatalf("restart = %+v (%v)", it, ok)
	}
	for _, id := range []string{"install-latest", "releases/v0.0.6", "switch/i3", "advanced"} {
		if it, ok := find(m, id); !ok || it.Enabled {
			t.Errorf("%s = %+v (%v), want present and disabled", id, it, ok)
		}
	}
	if _, ok := find(m, "cancel"); ok {
		t.Error("cancel while reboot-pending")
	}
}

func TestBuildMenuSwitchListsTheOtherDesktops(t *testing.T) {
	for _, flavor := range switchable {
		m := BuildMenu(menuStatus(StateUpToDate), menuReleases, flavor, gpuFull)
		var got []string
		for _, id := range ids(m) {
			if f, ok := strings.CutPrefix(id, "switch/"); ok {
				got = append(got, f)
			}
		}
		if len(got) != 3 {
			t.Errorf("%s: switch targets %v, want the three others", flavor, got)
		}
		for _, f := range got {
			if f == flavor || f == flavorAgent {
				t.Errorf("%s: offers switch/%s", flavor, f)
			}
		}
	}
}

func TestBuildMenuAdvancedFollowsStatus(t *testing.T) {
	st := menuStatus(StateUpToDate)
	if _, ok := find(BuildMenu(st, menuReleases, "sway", gpuFull), "advanced"); !ok {
		t.Error("advanced missing in advanced mode")
	}
	st.Advanced = false
	if _, ok := find(BuildMenu(st, menuReleases, "sway", gpuFull), "advanced"); ok {
		t.Error("advanced shown with advanced=false")
	}
}

// The service refuses every install on an unmanaged system, so the menu
// offers none, advanced mode or not.
func TestBuildMenuUnmanaged(t *testing.T) {
	m := BuildMenu(Status{State: StateUnmanaged, Advanced: true}, menuReleases, "", "")
	if got := ids(m); strings.Join(got, ",") != "status" {
		t.Fatalf("ids = %v, want status only", got)
	}
	if m[0].Label != "Unmanaged image" {
		t.Fatalf("status label %q", m[0].Label)
	}
}

func TestBuildMenuOffersOnlyReleaseTags(t *testing.T) {
	rel := []Latest{{Tag: "v0.0.6"}, {Tag: "nightly"}, {Tag: ""}, {Tag: "v0.0.7-rc1"}}
	var got []string
	for _, id := range ids(BuildMenu(menuStatus(StateUpToDate), rel, "sway", gpuFull)) {
		if tag, ok := strings.CutPrefix(id, "releases/"); ok {
			got = append(got, tag)
		}
	}
	if strings.Join(got, ",") != "v0.0.6,v0.0.7-rc1" {
		t.Fatalf("release items %v", got)
	}
}

func TestBuildMenuStatusLines(t *testing.T) {
	for _, tc := range []struct {
		st   Status
		want string
	}{
		{Status{State: StateIdle, Installed: "v0.0.5"}, "Hadron Desktop v0.0.5"},
		{Status{State: StateChecking, Installed: "v0.0.5"}, "Checking for updates"},
		{Status{State: StateUpToDate, Installed: "v0.0.5"}, "Hadron Desktop v0.0.5 is up to date"},
		{Status{State: StateUpToDate, Installed: "v0.0.5", Pin: "x"}, "Hadron Desktop v0.0.5 (pinned)"},
		{Status{State: StateUpdateAvailable, Installed: "v0.0.5", Latest: "v0.0.6"}, "Hadron Desktop v0.0.6 is available"},
		{Status{State: StateApplying}, "Installing the update"},
		{Status{State: StateRebootPending}, "Update installed; restart to use it"},
		{Status{State: StateRebootPending, Pending: pendingInstalled}, "Update installed; restart to use it"},
		{Status{State: StateRebootPending, Pending: pendingFailed}, "Update failed; restart before trying again"},
		{Status{State: StateRebootPending, Pending: pendingStarted}, "Update interrupted; restart before trying again"},
		{Status{State: StateError, Detail: "github down"}, "Error: github down"},
	} {
		if got := BuildMenu(tc.st, nil, "sway", gpuFull)[0].Label; !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: status %q, want prefix %q", tc.st.State, got, tc.want)
		}
	}
}

// TestBuildMenuRollback: "Restart into the previous system" is offered when
// nothing runs and nothing waits for a reboot. While reboot-pending the
// passive slot holds the system the user runs now, so "previous" would mean
// the wrong one.
func TestBuildMenuRollback(t *testing.T) {
	for _, tc := range []struct {
		state State
		want  bool
	}{
		{StateIdle, true},
		{StateUpToDate, true},
		{StateUpdateAvailable, true},
		{StateError, true},
		{StateChecking, false},
		{StateInspecting, false},
		{StateApplying, false},
		{StateRebootPending, false},
		{StateUnmanaged, false},
	} {
		it, ok := find(BuildMenu(menuStatus(tc.state), menuReleases, "sway", gpuFull), "rollback")
		if ok != tc.want {
			t.Errorf("%s: rollback present %v, want %v", tc.state, ok, tc.want)
			continue
		}
		if ok && (it.Label != "Restart into the previous system" || !it.Enabled) {
			t.Errorf("%s: rollback = %+v", tc.state, it)
		}
	}
}

// The menu offers the other image line, never the running one.
func TestBuildMenuOffersTheOtherLine(t *testing.T) {
	for line, want := range map[string]string{gpuFull: "gpu/vm", gpuVM: "gpu/full"} {
		m := BuildMenu(menuStatus(StateUpToDate), menuReleases, "sway", line)
		var got []string
		for _, id := range ids(m) {
			if strings.HasPrefix(id, "gpu/") {
				got = append(got, id)
			}
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s: line items %v, want [%s]", line, got, want)
		}
		if it, _ := find(m, want); !it.Enabled {
			t.Errorf("%s: %s disabled while up to date", line, want)
		}
	}
	m := BuildMenu(menuStatus(StateApplying), menuReleases, "sway", gpuFull)
	if it, ok := find(m, "gpu/vm"); !ok || it.Enabled {
		t.Errorf("gpu/vm = %+v (%v) during a job, want present and disabled", it, ok)
	}
}

func TestBuildMenuOffersExtensions(t *testing.T) {
	for _, s := range []State{StateIdle, StateUpToDate, StateApplying, StateExtension, StateRebootPending} {
		m := BuildMenu(menuStatus(s), menuReleases, "sway", gpuFull)
		if it, ok := find(m, "extensions"); !ok || !it.Enabled || it.Label != "Extensions…" {
			t.Errorf("%s: extensions = %+v (%v)", s, it, ok)
		}
	}
	if _, ok := find(BuildMenu(menuStatus(StateUnmanaged), nil, "sway", gpuFull), "extensions"); ok {
		t.Error("an unmanaged system offers extensions; the service refuses them there")
	}
	m := BuildMenu(menuStatus(StateExtension), menuReleases, "sway", gpuFull)
	if m[0].Label != "Changing system extensions…" {
		t.Errorf("status line %q", m[0].Label)
	}
	if it, _ := find(m, "install-latest"); it.Enabled {
		t.Error("an install is offered during an extension job")
	}
}
