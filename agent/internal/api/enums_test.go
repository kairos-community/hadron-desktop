package api_test

import (
	"encoding/json"
	"testing"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// propertyEnum returns the enum values the published schema for In advertises
// for one property, marshalling through JSON so the test asserts on what a
// client actually receives rather than on the in-memory schema struct.
func propertyEnum[In any](t *testing.T, property string) []string {
	t.Helper()
	schema, err := api.InputSchemaFor[In]()
	if err != nil {
		t.Fatalf("InputSchemaFor: %v", err)
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var doc struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	prop, ok := doc.Properties[property]
	if !ok {
		t.Fatalf("schema has no property %q", property)
	}
	return prop.Enum
}

func assertEnum(t *testing.T, got, want []string, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d values %v, want %d %v", what, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: value %d = %q, want %q (full: %v)", what, i, got[i], want[i], got)
		}
	}
}

// TestPublishedSchemaAdvertisesEveryClosedSet is the guard on the reason this
// exists: the server has always rejected values outside these sets, but until
// the schema declared them a client could only discover them by guessing. Each
// case pins both membership and order.
