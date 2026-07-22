package main

import "strings"

// Step is one phase of the kairos-agent install, identified by a stable substring
// that appears in the installer's log output. Index 0 is the initial state (no
// match). Mirrors kairos-agent internal/agent/TUIconstants.go.
//
// kairos-agent exposes no progress API, so scraping its stdout is the only way to
// drive a progress bar. These substrings are best-effort and deliberately isolated
// in this file so they are easy to retune after a boot test — a stale substring
// degrades to a stalled progress bar, never to a failed install.
type Step struct {
	Name  string
	Match string
}

var Steps = []Step{
	{"Preparing", ""},
	{"Partitioning disk", "Partitioning device"},
	{"Running before-install", "Running stage: before-install"},
	{"Installing active system", "Creating file system image"},
	{"Configuring bootloader", "Installing GRUB"},
	{"Creating recovery image", "recovery.img"},
	{"Creating passive image", "passive.img"},
	{"Running after-install", "Running after-install hook"},
	{"Complete", "Finish Lifecycle hook"},
}

// AdvanceStep returns the step index after observing line, never regressing below
// current. It picks the highest-indexed step (beyond current) whose Match is in
// line, so a line mentioning two phases lands on the later one.
func AdvanceStep(line string, current int) int {
	for i := len(Steps) - 1; i > current; i-- {
		if Steps[i].Match != "" && strings.Contains(line, Steps[i].Match) {
			return i
		}
	}
	return current
}

// Percent maps a step index to a 0..100 progress value, clamping out-of-range
// input rather than panicking — the caller is a render path.
func Percent(step int) int {
	last := len(Steps) - 1
	if step < 0 {
		step = 0
	}
	if step > last {
		step = last
	}
	return step * 100 / last
}
