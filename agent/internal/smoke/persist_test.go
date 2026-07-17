package smoke

import "testing"

func TestParsePasswdLocked(t *testing.T) {
	cases := []struct {
		name       string
		stdout     string
		wantLocked bool
		wantParsed bool
	}{
		{"locked L", "agent L 2024-01-01 0 99999 7 -1", true, true},
		{"locked LK", "agent LK 01/01/2024", true, true},
		{"usable P", "agent P 2024-01-01 0 99999 7 -1", false, true},
		{"usable PS", "agent PS 01/01/2024", false, true},
		{"no password NP", "agent NP", false, true},
		{"unparseable single field", "agent", false, false},
		{"empty", "", false, false},
		{"garbage status", "agent ??? whatever", false, false},
		{"skips noise line then reads locked", "some warning text\nagent L 2024", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			locked, parsed := parsePasswdLocked(tc.stdout)
			if locked != tc.wantLocked || parsed != tc.wantParsed {
				t.Fatalf("parsePasswdLocked(%q) = (locked=%v, parsed=%v), want (locked=%v, parsed=%v)",
					tc.stdout, locked, parsed, tc.wantLocked, tc.wantParsed)
			}
		})
	}
}
