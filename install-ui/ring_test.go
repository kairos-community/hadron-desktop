package main

import "testing"

func TestRingKeepsMostRecentLines(t *testing.T) {
	r := newRing(3)
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		r.add(s)
	}
	got := r.lines()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	want := []string{"c", "d", "e"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("lines()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRingUnderCapacity(t *testing.T) {
	r := newRing(10)
	r.add("only")
	if got := r.text(); got != "only" {
		t.Errorf("text() = %q, want %q", got, "only")
	}
}

// TestRingTextJoinsWithNewlines pins the separator. text() feeds the log
// viewport, so joining with anything but "\n" renders the whole install log on
// a single line. Needs >= 2 lines to be observable at all.
func TestRingTextJoinsWithNewlines(t *testing.T) {
	r := newRing(10)
	for _, s := range []string{"first", "second", "third"} {
		r.add(s)
	}
	if got, want := r.text(), "first\nsecond\nthird"; got != want {
		t.Errorf("text() = %q, want %q", got, want)
	}
}

func TestRingEmpty(t *testing.T) {
	r := newRing(10)
	if got := r.text(); got != "" {
		t.Errorf("text() = %q, want empty", got)
	}
}
