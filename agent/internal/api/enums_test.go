package api_test

import (
	"encoding/json"
	"strings"
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
func TestPublishedSchemaAdvertisesEveryClosedSet(t *testing.T) {
	assertEnum(t, propertyEnum[api.ComputerUseInput](t, "action"), []string{
		"capture", "accessibility", "click", "double_click", "drag", "scroll",
		"type", "key", "wait", "list_applications", "focus_application",
	}, "computer_use.action")

	assertEnum(t, propertyEnum[api.ComputerUseInput](t, "scope"),
		[]string{"screen", "window"}, "computer_use.scope")

	assertEnum(t, propertyEnum[api.ComputerUseInput](t, "button"),
		[]string{"left", "right", "middle"}, "computer_use.button")

	assertEnum(t, propertyEnum[api.ComputerUseInput](t, "direction"),
		[]string{"up", "down", "left", "right"}, "computer_use.direction")

	assertEnum(t, propertyEnum[api.ProcessInput](t, "action"),
		[]string{"start", "poll", "write", "terminate"}, "process.action")

	assertEnum(t, propertyEnum[api.ReadFileInput](t, "encoding"),
		[]string{"utf8", "base64"}, "read_file.encoding")

	assertEnum(t, propertyEnum[api.WriteFileInput](t, "encoding"),
		[]string{"utf8", "base64"}, "write_file.encoding")

	assertEnum(t, propertyEnum[api.SearchFilesInput](t, "mode"),
		[]string{"name", "content"}, "search_files.mode")
}

// TestEveryAdvertisedActionIsAccepted ties the published schema back to the
// server's own validation: a value the schema offers must not be one Validate
// rejects, or clients would be invited to make calls that always fail.
func TestEveryAdvertisedActionIsAccepted(t *testing.T) {
	for _, action := range api.ComputerUseActions {
		in := api.ComputerUseInput{Action: action}
		// Validate legitimately rejects these for missing action-specific
		// fields; the failure this guards against is "unknown action".
		if err := in.Validate(); err != nil && strings.Contains(err.Error(), "invalid action") {
			t.Errorf("advertised computer_use action %q is rejected as unknown: %v", action, err)
		}
	}
	for _, action := range api.ProcessActions {
		in := api.ProcessInput{Action: action}
		if err := in.Validate(); err != nil && strings.Contains(err.Error(), "invalid action") {
			t.Errorf("advertised process action %q is rejected as unknown: %v", action, err)
		}
	}
}
