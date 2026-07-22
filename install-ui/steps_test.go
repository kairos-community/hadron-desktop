package main

import "testing"

func TestAdvanceStepMatchesKnownPhases(t *testing.T) {
	cases := []struct {
		line string
		want int
	}{
		{"Partitioning device /dev/vda", 1},
		{"Running stage: before-install", 2},
		{"Creating file system image active.img", 3},
		{"Installing GRUB to /dev/vda", 4},
		{"Copying recovery.img", 5},
		{"Copying passive.img", 6},
		{"Running after-install hook", 7},
		{"Finish Lifecycle hook", 8},
	}
	for _, c := range cases {
		if got := AdvanceStep(c.line, 0); got != c.want {
			t.Errorf("AdvanceStep(%q, 0) = %d, want %d", c.line, got, c.want)
		}
	}
}

func TestAdvanceStepNeverRegresses(t *testing.T) {
	// kairos-agent interleaves output; an early-phase line arriving after a late
	// one must not rewind the progress bar.
	if got := AdvanceStep("Partitioning device /dev/vda", 5); got != 5 {
		t.Errorf("AdvanceStep regressed to %d, want 5", got)
	}
}

func TestAdvanceStepIgnoresUnrelatedLines(t *testing.T) {
	if got := AdvanceStep("some unrelated chatter", 3); got != 3 {
		t.Errorf("AdvanceStep(%q, 3) = %d, want 3", "some unrelated chatter", got)
	}
}

func TestAdvanceStepPrefersHighestMatch(t *testing.T) {
	// A single line containing two markers must land on the later phase.
	line := "Running stage: before-install and Installing GRUB"
	if got := AdvanceStep(line, 0); got != 4 {
		t.Errorf("AdvanceStep(%q, 0) = %d, want 4", line, got)
	}
}

func TestPercentEndpointsAndClamping(t *testing.T) {
	if got := Percent(0); got != 0 {
		t.Errorf("Percent(0) = %d, want 0", got)
	}
	if got := Percent(len(Steps) - 1); got != 100 {
		t.Errorf("Percent(last) = %d, want 100", got)
	}
	if got := Percent(-5); got != 0 {
		t.Errorf("Percent(-5) = %d, want 0", got)
	}
	if got := Percent(9999); got != 100 {
		t.Errorf("Percent(9999) = %d, want 100", got)
	}
}
