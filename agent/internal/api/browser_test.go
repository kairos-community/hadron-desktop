package api_test

import (
	"strings"
	"testing"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

func intPtr(v int) *int { return &v }

// TestBrowserInputValidate covers the per-action field matrix: an action must
// carry what it needs and must not carry fields belonging to another action.
// The second half matters as much as the first -- silently ignoring a field a
// caller set is how a click on the wrong thing gets reported as a success.
func TestBrowserInputValidate(t *testing.T) {
	cases := []struct {
		name    string
		in      api.BrowserInput
		wantErr string // substring; empty means the input must validate
	}{
		{
			name: "navigate with url",
			in:   api.BrowserInput{Action: api.BrowserNavigate, URL: "https://kairos.io"},
		},
		{
			name:    "navigate without url",
			in:      api.BrowserInput{Action: api.BrowserNavigate},
			wantErr: "requires url",
		},
		{
			name:    "navigate rejects a ref",
			in:      api.BrowserInput{Action: api.BrowserNavigate, URL: "https://kairos.io", Ref: "e1"},
			wantErr: "does not accept field(s): [ref]",
		},
		{
			name: "snapshot needs nothing",
			in:   api.BrowserInput{Action: api.BrowserSnapshot},
		},
		{
			name: "snapshot bounds elements",
			in:   api.BrowserInput{Action: api.BrowserSnapshot, MaxElements: intPtr(50)},
		},
		{
			name:    "snapshot rejects text",
			in:      api.BrowserInput{Action: api.BrowserSnapshot, Text: "hello"},
			wantErr: "does not accept field(s): [text]",
		},
		{
			name: "click with ref",
			in:   api.BrowserInput{Action: api.BrowserClick, Ref: "e7"},
		},
		{
			name:    "click without ref",
			in:      api.BrowserInput{Action: api.BrowserClick},
			wantErr: "requires ref",
		},
		{
			name: "type with ref and text",
			in:   api.BrowserInput{Action: api.BrowserType, Ref: "e2", Text: "kairos"},
		},
		{
			name: "type may submit",
			in:   api.BrowserInput{Action: api.BrowserType, Ref: "e2", Text: "kairos", Submit: true},
		},
		{
			name:    "type without text",
			in:      api.BrowserInput{Action: api.BrowserType, Ref: "e2"},
			wantErr: "requires text",
		},
		{
			name: "press with key",
			in:   api.BrowserInput{Action: api.BrowserPress, Key: "Enter"},
		},
		{
			name:    "press without key",
			in:      api.BrowserInput{Action: api.BrowserPress},
			wantErr: "requires key",
		},
		{
			name: "scroll with direction",
			in:   api.BrowserInput{Action: api.BrowserScroll, Direction: api.DirectionDown},
		},
		{
			name:    "scroll without direction",
			in:      api.BrowserInput{Action: api.BrowserScroll},
			wantErr: "requires direction",
		},
		{
			name:    "scroll rejects an unknown direction",
			in:      api.BrowserInput{Action: api.BrowserScroll, Direction: "sideways"},
			wantErr: `invalid direction "sideways"`,
		},
		{
			name: "back needs nothing",
			in:   api.BrowserInput{Action: api.BrowserBack},
		},
		{
			name: "text without a ref reads the document",
			in:   api.BrowserInput{Action: api.BrowserText},
		},
		{
			name: "text with a ref reads the element",
			in:   api.BrowserInput{Action: api.BrowserText, Ref: "e3"},
		},
		{
			name: "window_id is accepted by every action",
			in:   api.BrowserInput{Action: api.BrowserBack, WindowID: intPtr(4194304)},
		},
		{
			name:    "unknown action",
			in:      api.BrowserInput{Action: "teleport"},
			wantErr: `unknown action "teleport"`,
		},
		{
			name:    "empty action",
			in:      api.BrowserInput{},
			wantErr: `unknown action ""`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Validate() = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestBrowserSubmitFalseIsNotAField guards the one field whose "was it set?"
// answer is not obvious: Submit is a bool, so an explicit false is
// indistinguishable from unset. It must not count as present, or every action
// that does not allow it would reject a caller who sent submit:false.
func TestBrowserSubmitFalseIsNotAField(t *testing.T) {
	in := api.BrowserInput{Action: api.BrowserBack, Submit: false}
	if err := in.Validate(); err != nil {
		t.Fatalf("submit:false on back = %v, want nil", err)
	}
	if err := (api.BrowserInput{Action: api.BrowserBack, Submit: true}).Validate(); err == nil {
		t.Fatal("submit:true on back = nil, want a rejection")
	}
}
