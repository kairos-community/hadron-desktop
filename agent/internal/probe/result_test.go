package probe

import (
	"encoding/json"
	"testing"
)

// fullyPassing returns a Result with every capability field set true.
func fullyPassing() Result {
	return Result{
		Environment:           true,
		XTest:                 true,
		WindowDiscovery:       true,
		EWMHActivation:        true,
		DesktopCapture:        true,
		WindowCapture:         true,
		GTKAccessibility:      true,
		GTKClick:              true,
		GTKDoubleClick:        true,
		GTKDrag:               true,
		GTKScroll:             true,
		GTKType:               true,
		GTKNamedKey:           true,
		ChromiumAccessibility: true,
		ChromiumClick:         true,
		ChromiumType:          true,
	}
}

func TestAllPassedTrueWhenEveryFieldSet(t *testing.T) {
	if !fullyPassing().AllPassed() {
		t.Fatal("AllPassed() = false, want true when every capability field is set")
	}
}

func TestAllPassedFalseWhenAnyFieldUnset(t *testing.T) {
	// Marshal the fully-passing result to a generic map so the test enumerates
	// exactly the JSON fields the collector and reporter observe, and flips each
	// one in turn. This guards against AllPassed() forgetting to consult a field.
	encoded, err := json.Marshal(fullyPassing())
	if err != nil {
		t.Fatalf("marshalling fully-passing result: %v", err)
	}
	var fields map[string]bool
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshalling fully-passing result: %v", err)
	}
	if len(fields) != 16 {
		t.Fatalf("Result has %d boolean fields, want 16", len(fields))
	}

	for name := range fields {
		flipped := fullyPassing()
		reencoded, err := json.Marshal(flipped)
		if err != nil {
			t.Fatalf("marshalling result: %v", err)
		}
		var asMap map[string]bool
		if err := json.Unmarshal(reencoded, &asMap); err != nil {
			t.Fatalf("unmarshalling result: %v", err)
		}
		asMap[name] = false
		roundTrip, err := json.Marshal(asMap)
		if err != nil {
			t.Fatalf("re-marshalling toggled map: %v", err)
		}
		if err := json.Unmarshal(roundTrip, &flipped); err != nil {
			t.Fatalf("unmarshalling toggled result: %v", err)
		}
		if flipped.AllPassed() {
			t.Errorf("AllPassed() = true with %q unset, want false", name)
		}
	}
}
