package main

import "os"

// MenuItem is one entry of the tray menu, independent of any tray library, so
// the menu can be tested as data. A disabled item is shown greyed out.
type MenuItem struct {
	ID, Label string
	Enabled   bool
	Children  []MenuItem
}

// flavorLabels are the desktop names the switch submenu shows.
var flavorLabels = map[string]string{"sway": "Sway", "i3": "i3", "cosmic": "COSMIC", "niri": "niri"}

// installsAllowed reports whether st lets the user start an install: never
// while a job runs, and never while a reboot is pending (a second install
// would overwrite the known-good passive slot).
func installsAllowed(s State) bool {
	switch s {
	case StateIdle, StateUpToDate, StateUpdateAvailable, StateError:
		return true
	}
	return false
}

// statusLine is the first, disabled menu item: what the system is doing now.
func statusLine(st Status) string {
	switch st.State {
	case StateUnmanaged:
		return "Unmanaged image"
	case StateChecking:
		return "Checking for updates…"
	case StateUpToDate:
		if st.Pin != "" {
			return "Hadron Desktop " + st.Installed + " (pinned)"
		}
		return "Hadron Desktop " + st.Installed + " is up to date"
	case StateUpdateAvailable:
		return "Hadron Desktop " + st.Latest + " is available"
	case StateInspecting:
		return "Checking the image…"
	case StateApplying:
		return "Installing the update…"
	case StateExtension:
		return "Changing system extensions…"
	case StateRebootPending:
		// After a failed or interrupted install the slots may already be
		// swapped: no retry before a restart.
		switch st.Pending {
		case pendingFailed:
			return "Update failed; restart before trying again"
		case pendingStarted:
			return "Update interrupted; restart before trying again"
		}
		return "Update installed; restart to use it"
	case StateError:
		return "Error: " + shorten(st.Detail, 80)
	}
	return "Hadron Desktop " + st.Installed
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// BuildMenu is the tray menu for a status. It is pure: the tray rebuilds it on
// every state change, and every rule about what may be clicked when lives
// here. releases are the published tags; flavor is the running desktop and
// line its image line (gpuFull or gpuVM).
func BuildMenu(st Status, releases []Latest, flavor, line string) []MenuItem {
	menu := []MenuItem{{ID: "status", Label: statusLine(st)}}
	if st.State == StateUnmanaged {
		// Nothing here was installed from a release, and the service
		// refuses every install on such a system: offer nothing.
		return menu
	}
	can := installsAllowed(st.State)

	// A pinned system reports up-to-date, but "latest" is still worth a click:
	// it is how a user leaves the pin.
	if st.Latest != "" && (st.Pin != "" || Newer(st.Latest, st.Installed)) {
		label := "Install " + st.Latest
		if st.Pin != "" {
			label = "Install latest (" + st.Latest + ")"
		}
		menu = append(menu, MenuItem{ID: "install-latest", Label: label, Enabled: can})
	}

	var rel []MenuItem
	for _, r := range releases {
		// Only release tags: anything else would fail `upgrade --version`.
		if _, ok := parseSemver(r.Tag); !ok || r.Tag == st.Installed {
			continue
		}
		rel = append(rel, MenuItem{ID: "releases/" + r.Tag, Label: r.Tag, Enabled: can})
	}
	if len(rel) > 0 {
		menu = append(menu, MenuItem{ID: "releases", Label: "Other releases", Enabled: can, Children: rel})
	}

	var sw []MenuItem
	for _, f := range switchable {
		if f == flavor {
			continue
		}
		sw = append(sw, MenuItem{ID: "switch/" + f, Label: flavorLabels[f], Enabled: can})
	}
	menu = append(menu, MenuItem{ID: "switch", Label: "Switch desktop", Enabled: can, Children: sw})
	// The other image line, the way the switch above offers the other
	// desktops: a terminal shows what changes and asks.
	if line == gpuVM {
		menu = append(menu, MenuItem{ID: "gpu/" + gpuFull, Label: "Switch to the hardware image", Enabled: can})
	} else {
		menu = append(menu, MenuItem{ID: "gpu/" + gpuVM, Label: "Switch to the virtual machine image", Enabled: can})
	}

	if st.Advanced {
		menu = append(menu, MenuItem{ID: "advanced", Label: "Install an image…", Enabled: can})
	}
	// Cancel only before kairos-agent starts writing: an interrupted write can
	// leave the passive slot unbootable.
	if st.State == StateInspecting {
		menu = append(menu, MenuItem{ID: "cancel", Label: "Cancel the update", Enabled: true})
	}
	if st.State == StateRebootPending {
		menu = append(menu, MenuItem{ID: "restart", Label: "Restart now", Enabled: true})
	}
	// The Hadron menu's Extensions list. Always clickable: it only reads,
	// and the service refuses a change while a job runs.
	menu = append(menu, MenuItem{ID: "extensions", Label: "Extensions…", Enabled: true})
	// The disk recovery key, on a system whose disk is encrypted.
	if diskEncrypted() {
		menu = append(menu, MenuItem{ID: "recovery", Label: "Recovery key…", Enabled: true})
	}
	// Hidden, not greyed, when it cannot be used: while a job runs, and while
	// an install waits for its reboot, when the passive slot holds the system
	// the user runs now and "previous" would mean the wrong one.
	if can {
		menu = append(menu, MenuItem{ID: "rollback", Label: "Restart into the previous system", Enabled: true})
	}
	return menu
}

// encryptionMarkerFile is written by the installer for an encrypted disk
// (mode=passphrase or mode=tpm-recovery-key). A Trusted Boot disk is always
// encrypted, with or without the marker.
var encryptionMarkerFile = "/etc/hadron-desktop/encryption"

// diskEncrypted reports whether the recovery key entry makes sense here.
// A variable so the tests can choose.
var diskEncrypted = func() bool {
	if _, err := os.Stat(encryptionMarkerFile); err == nil {
		return true
	}
	return edition() == editionTrustedBoot
}
