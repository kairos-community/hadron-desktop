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
		{"Copying /run/cos/state/cOS/active.img source to /run/cos/recovery/cOS/recovery.img", 5},
		{"Copying /run/cos/state/cOS/active.img source to /run/cos/state/cOS/passive.img", 6},
		{"Running stage: after-install", 7},
		{"Finish Lifecycle hook", 8},
	}
	for _, c := range cases {
		if got := AdvanceStep(c.line, 0); got != c.want {
			t.Errorf("AdvanceStep(%q, 0) = %d, want %d", c.line, got, c.want)
		}
	}
}

// TestAdvanceStepIgnoresIncidentalImageMentions pins the anchoring of steps 5
// and 6. "recovery.img" and "passive.img" are plain filename constants in
// kairos-agent and appear in messages unrelated to the actual copy. With
// unanchored matches, a single such line jumped step 0 -> 6 (75%) immediately
// and, because AdvanceStep never regresses, every genuine earlier phase was
// then ignored forever.
func TestAdvanceStepIgnoresIncidentalImageMentions(t *testing.T) {
	incidental := "Deploying image, will create recovery.img and passive.img later"
	if got := AdvanceStep(incidental, 0); got != 0 {
		t.Fatalf("AdvanceStep(%q, 0) = %d, want 0 (incidental mention must not advance)", incidental, got)
	}

	// The genuine phase lines must still be recognised.
	recovery := "Copying /run/cos/state/cOS/active.img source to /run/cos/recovery/cOS/recovery.img"
	if got := AdvanceStep(recovery, 0); got != 5 {
		t.Errorf("AdvanceStep(recovery copy, 0) = %d, want 5", got)
	}
	passive := "Copying /run/cos/state/cOS/active.img source to /run/cos/state/cOS/passive.img"
	if got := AdvanceStep(passive, 0); got != 6 {
		t.Errorf("AdvanceStep(passive copy, 0) = %d, want 6", got)
	}

	// End-to-end: the incidental line must not eat the real partitioning phase
	// that follows it.
	step := 0
	for _, line := range []string{incidental, "Partitioning device /dev/vda"} {
		step = AdvanceStep(line, step)
	}
	if step != 1 {
		t.Errorf("after incidental line then partitioning, step = %d, want 1", step)
	}
}

// TestAdvanceStepAfterInstallUsesYipStage guards against reintroducing
// "Running after-install hook", a string kairos-agent never emits.
func TestAdvanceStepAfterInstallUsesYipStage(t *testing.T) {
	if got := AdvanceStep("Running stage: after-install", 0); got != 7 {
		t.Errorf("AdvanceStep(yip after-install stage, 0) = %d, want 7", got)
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
