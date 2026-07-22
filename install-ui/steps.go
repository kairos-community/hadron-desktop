package main

import "strings"

// Step is one phase of the kairos-agent install, identified by a stable substring
// that appears in the installer's log output. Index 0 is the initial state (no
// match). Derived from kairos-installer internal/tui/TUIconstants.go.
//
// WARNING: do NOT blindly resync this table with upstream. Step 7 diverges on
// purpose: upstream's AgentAfterInstallLog = "Running after-install hook" is a
// bug — kairos-agent emits that string nowhere, so matching on it would leave
// the progress bar stuck. We match yip's "Running stage: after-install" instead.
// A future maintainer diffing against upstream will find exactly this one
// mismatch; it is intentional and must stay.
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
	// Anchored on the full "Copying %s source to %s" line emitted by
	// kairos-agent pkg/elemental/elemental.go. The bare "recovery.img" /
	// "passive.img" filenames are also plain constants in
	// kairos-agent/pkg/constants/constants.go and so appear in unrelated
	// messages; since AdvanceStep never regresses, one incidental early mention
	// would permanently skip steps 1-4.
	{"Creating recovery image", "Copying /run/cos/state/cOS/active.img source to /run/cos/recovery/cOS/recovery.img"},
	{"Creating passive image", "Copying /run/cos/state/cOS/active.img source to /run/cos/state/cOS/passive.img"},
	// The after-install *stage* is emitted by yip's executor as
	// "Running stage: %s" — kairos-agent itself never logs an
	// "after-install hook" line.
	{"Running after-install", "Running stage: after-install"},
	{"Complete", "Finish Lifecycle hook"},
}

// NOTE: the parser alone cannot reliably reach the final step, so the caller
// must also set the last step index on a successful process exit — do not
// remove that exit-code handling believing this table covers completion.
//
// Two reasons:
//   - "Finish Lifecycle hook" is logged at *Debug* level only
//     (kairos-agent internal/agent/hooks/lifecycle.go), so it is absent at the
//     default log level.
//   - It sits *after* the reboot/poweroff branches in that same hook, so on a
//     rebooting install the process is torn down before ever reaching it.

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
