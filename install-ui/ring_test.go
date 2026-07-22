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

func TestRingEmpty(t *testing.T) {
	r := newRing(10)
	if got := r.text(); got != "" {
		t.Errorf("text() = %q, want empty", got)
	}
}
