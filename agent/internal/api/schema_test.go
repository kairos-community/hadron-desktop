package api

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestToolNamesSortedSet is the contract test the brief calls out explicitly:
// hadron-agent exposes exactly these seven tools, no more, no fewer. The
// browser was added by the 2026-07-20 amendment; terminal and process were
// replaced by a single unbounded bash tool.
func TestToolNamesSortedSet(t *testing.T) {
	want := []string{
		"bash",
		"browser",
		"computer_use",
		"patch",
		"read_file",
		"search_files",
		"write_file",
	}

	got := ToolNames()

	if !sort.StringsAreSorted(got) {
		t.Fatalf("ToolNames() is not sorted: %v", got)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolNames() = %v, want %v", got, want)
	}
}

// TestRegisterAllExposesExactlySevenTools proves the names above are not
// just constants sitting in this package: registering them on a real
// *mcp.Server and listing tools over the wire yields the same sorted set.
func TestRegisterAllExposesExactlySevenTools(t *testing.T) {
	ctx := context.Background()

	s := mcp.NewServer(&mcp.Implementation{Name: "hadron-agent-test", Version: "v0.0.0"}, nil)
	RegisterAll(s)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	serverSession, err := s.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)

	want := ToolNames()
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("registered tool names = %v, want %v", names, want)
	}
}

// TestErrorCodesStableSet freezes the eleven stable error codes. Renaming or
// removing one of these is a breaking change to every consumer of this
// package.
func TestErrorCodesStableSet(t *testing.T) {
	got := []string{
		string(CodeInvalidArgument),
		string(CodeUnauthenticated),
		string(CodeForbidden),
		string(CodeSessionUnavailable),
		string(CodeNotFound),
		string(CodeAlreadyExists),
		string(CodeResourceExhausted),
		string(CodeDeadlineExceeded),
		string(CodeOutputTruncated),
		string(CodePaused),
		string(CodeInternal),
	}
	sort.Strings(got)

	want := []string{
		"ALREADY_EXISTS",
		"DEADLINE_EXCEEDED",
		"FORBIDDEN",
		"INTERNAL",
		"INVALID_ARGUMENT",
		"NOT_FOUND",
		"OUTPUT_TRUNCATED",
		"PAUSED",
		"RESOURCE_EXHAUSTED",
		"SESSION_UNAVAILABLE",
		"UNAUTHENTICATED",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("error codes = %v, want %v", got, want)
	}
}

// intPtr and int64Ptr help build ComputerUseInput literals in table tests.
func intPtr(v int) *int       { return &v }
func int64Ptr(v int64) *int64 { return &v }

func TestComputerUseInputValidate(t *testing.T) {
	tests := []struct {
		name    string
		in      ComputerUseInput
		wantErr bool
	}{
		{"unknown action rejected", ComputerUseInput{Action: "frobnicate"}, true},

		{"capture screen ok", ComputerUseInput{Action: ActionCapture}, false},
		{"capture explicit screen scope ok", ComputerUseInput{Action: ActionCapture, Scope: ScopeScreen}, false},
		{"capture window scope without target rejected", ComputerUseInput{Action: ActionCapture, Scope: ScopeWindow}, true},
		{"capture window scope with pid ok", ComputerUseInput{Action: ActionCapture, Scope: ScopeWindow, PID: intPtr(123)}, false},
		{"capture with irrelevant text rejected", ComputerUseInput{Action: ActionCapture, Text: "hi"}, true},

		{"accessibility default ok", ComputerUseInput{Action: ActionAccessibility}, false},
		{"accessibility with query ok", ComputerUseInput{Action: ActionAccessibility, Query: "button"}, false},
		{"accessibility window scope without target rejected", ComputerUseInput{Action: ActionAccessibility, Scope: ScopeWindow}, true},
		{"accessibility with irrelevant duration rejected", ComputerUseInput{Action: ActionAccessibility, DurationMs: intPtr(10)}, true},

		{"click with x,y ok", ComputerUseInput{Action: ActionClick, X: intPtr(1), Y: intPtr(2)}, false},
		{"click with element_index ok", ComputerUseInput{Action: ActionClick, ElementIndex: intPtr(0)}, false},
		{"click with neither target rejected", ComputerUseInput{Action: ActionClick}, true},
		{"click with only x rejected", ComputerUseInput{Action: ActionClick, X: intPtr(1)}, true},
		{"click with bad button rejected", ComputerUseInput{Action: ActionClick, X: intPtr(1), Y: intPtr(2), Button: "wheel"}, true},
		{"click with irrelevant text rejected", ComputerUseInput{Action: ActionClick, X: intPtr(1), Y: intPtr(2), Text: "hi"}, true},

		{"double_click with x,y ok", ComputerUseInput{Action: ActionDoubleClick, X: intPtr(1), Y: intPtr(2)}, false},
		{"double_click with no target rejected", ComputerUseInput{Action: ActionDoubleClick}, true},

		{"drag with all four ok", ComputerUseInput{Action: ActionDrag, FromX: intPtr(0), FromY: intPtr(0), ToX: intPtr(10), ToY: intPtr(10)}, false},
		{"drag missing to_y rejected", ComputerUseInput{Action: ActionDrag, FromX: intPtr(0), FromY: intPtr(0), ToX: intPtr(10)}, true},
		{"drag with irrelevant key rejected", ComputerUseInput{Action: ActionDrag, FromX: intPtr(0), FromY: intPtr(0), ToX: intPtr(10), ToY: intPtr(10), Key: "a"}, true},

		{"scroll with direction ok", ComputerUseInput{Action: ActionScroll, Direction: DirectionDown}, false},
		{"scroll missing direction rejected", ComputerUseInput{Action: ActionScroll, Amount: intPtr(1)}, true},
		{"scroll with bad direction rejected", ComputerUseInput{Action: ActionScroll, Direction: "sideways"}, true},
		{"scroll with non-positive amount rejected", ComputerUseInput{Action: ActionScroll, Direction: DirectionUp, Amount: intPtr(0)}, true},

		{"type with text ok", ComputerUseInput{Action: ActionType, Text: "hello"}, false},
		{"type missing text rejected", ComputerUseInput{Action: ActionType}, true},
		{"type with irrelevant key rejected", ComputerUseInput{Action: ActionType, Text: "hi", Key: "Return"}, true},

		{"key with key ok", ComputerUseInput{Action: ActionKey, Key: "Return"}, false},
		{"key with modifiers ok", ComputerUseInput{Action: ActionKey, Key: "a", Modifiers: []string{"ctrl"}}, false},
		{"key missing key rejected", ComputerUseInput{Action: ActionKey}, true},

		{"wait with duration ok", ComputerUseInput{Action: ActionWait, DurationMs: intPtr(500)}, false},
		{"wait missing duration rejected", ComputerUseInput{Action: ActionWait}, true},
		{"wait with non-positive duration rejected", ComputerUseInput{Action: ActionWait, DurationMs: intPtr(0)}, true},
		{"wait with irrelevant text rejected", ComputerUseInput{Action: ActionWait, DurationMs: intPtr(1), Text: "hi"}, true},

		{"list_applications default ok", ComputerUseInput{Action: ActionListApplications}, false},
		{"list_applications with query ok", ComputerUseInput{Action: ActionListApplications, Query: "chrome"}, false},
		{"list_applications with irrelevant pid rejected", ComputerUseInput{Action: ActionListApplications, PID: intPtr(1)}, true},

		{"focus_application with pid ok", ComputerUseInput{Action: ActionFocusApplication, PID: intPtr(42)}, false},
		{"focus_application with window_id ok", ComputerUseInput{Action: ActionFocusApplication, WindowID: int64Ptr(99)}, false},
		{"focus_application missing target rejected", ComputerUseInput{Action: ActionFocusApplication}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.in.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestTerminalInputValidate(t *testing.T) {
	if err := (BashInput{}).Validate(); err == nil {
		t.Fatal("expected error for missing command")
	}
	if err := (BashInput{Command: "echo hi"}).Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadFileInputValidate(t *testing.T) {
	if err := (ReadFileInput{}).Validate(); err == nil {
		t.Fatal("expected error for missing path")
	}
	if err := (ReadFileInput{Path: "/tmp/x", Encoding: "weird"}).Validate(); err == nil {
		t.Fatal("expected error for invalid encoding")
	}
	if err := (ReadFileInput{Path: "/tmp/x", Encoding: EncodingBase64}).Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSearchFilesInputValidate(t *testing.T) {
	if err := (SearchFilesInput{}).Validate(); err == nil {
		t.Fatal("expected error for missing path/query")
	}
	if err := (SearchFilesInput{Path: "/tmp", Query: "x", Mode: "fuzzy"}).Validate(); err == nil {
		t.Fatal("expected error for invalid mode")
	}
	if err := (SearchFilesInput{Path: "/tmp", Query: "x", Mode: SearchByContent}).Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWriteFileInputValidate(t *testing.T) {
	if err := (WriteFileInput{}).Validate(); err == nil {
		t.Fatal("expected error for missing path")
	}
	if err := (WriteFileInput{Path: "/tmp/x", Content: "hi", Encoding: "weird"}).Validate(); err == nil {
		t.Fatal("expected error for invalid encoding")
	}
	if err := (WriteFileInput{Path: "/tmp/x", Content: "hi"}).Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPatchInputValidate(t *testing.T) {
	if err := (PatchInput{}).Validate(); err == nil {
		t.Fatal("expected error for empty files")
	}
	if err := (PatchInput{Files: []PatchFile{{Path: "/tmp/x"}}}).Validate(); err == nil {
		t.Fatal("expected error for empty replacements")
	}
	if err := (PatchInput{Files: []PatchFile{{Path: "/tmp/x", Replacements: []PatchReplacement{{New: "b"}}}}}).Validate(); err == nil {
		t.Fatal("expected error for missing old text")
	}
	ok := PatchInput{Files: []PatchFile{{Path: "/tmp/x", Replacements: []PatchReplacement{{Old: "a", New: "b"}}}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// schemaFieldSet returns the sorted property names and sorted required names
// for T's inferred JSON schema.
func schemaFieldSet[T any](t *testing.T) (properties []string, required []string) {
	t.Helper()
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		t.Fatalf("jsonschema.For: %v", err)
	}
	for name := range schema.Properties {
		properties = append(properties, name)
	}
	sort.Strings(properties)
	required = append([]string(nil), schema.Required...)
	sort.Strings(required)
	return properties, required
}

func TestBashInputSchemaFrozen(t *testing.T) {
	props, required := schemaFieldSet[BashInput](t)
	wantProps := []string{"command", "cwd", "env"}
	wantRequired := []string{"command"}
	if !reflect.DeepEqual(props, wantProps) {
		t.Fatalf("properties = %v, want %v", props, wantProps)
	}
	if !reflect.DeepEqual(required, wantRequired) {
		t.Fatalf("required = %v, want %v", required, wantRequired)
	}
}

func TestReadFileInputSchemaFrozen(t *testing.T) {
	props, required := schemaFieldSet[ReadFileInput](t)
	wantProps := []string{"encoding", "limit", "metadata_only", "offset", "path"}
	wantRequired := []string{"path"}
	if !reflect.DeepEqual(props, wantProps) {
		t.Fatalf("properties = %v, want %v", props, wantProps)
	}
	if !reflect.DeepEqual(required, wantRequired) {
		t.Fatalf("required = %v, want %v", required, wantRequired)
	}
}

func TestSearchFilesInputSchemaFrozen(t *testing.T) {
	props, required := schemaFieldSet[SearchFilesInput](t)
	wantProps := []string{"max_file_bytes", "max_results", "mode", "path", "query", "regex"}
	wantRequired := []string{"path", "query"}
	if !reflect.DeepEqual(props, wantProps) {
		t.Fatalf("properties = %v, want %v", props, wantProps)
	}
	if !reflect.DeepEqual(required, wantRequired) {
		t.Fatalf("required = %v, want %v", required, wantRequired)
	}
}

func TestWriteFileInputSchemaFrozen(t *testing.T) {
	props, required := schemaFieldSet[WriteFileInput](t)
	wantProps := []string{"content", "create_parents", "encoding", "mode", "path"}
	wantRequired := []string{"content", "path"}
	if !reflect.DeepEqual(props, wantProps) {
		t.Fatalf("properties = %v, want %v", props, wantProps)
	}
	if !reflect.DeepEqual(required, wantRequired) {
		t.Fatalf("required = %v, want %v", required, wantRequired)
	}
}

func TestPatchInputSchemaFrozen(t *testing.T) {
	props, required := schemaFieldSet[PatchInput](t)
	wantProps := []string{"files"}
	wantRequired := []string{"files"}
	if !reflect.DeepEqual(props, wantProps) {
		t.Fatalf("properties = %v, want %v", props, wantProps)
	}
	if !reflect.DeepEqual(required, wantRequired) {
		t.Fatalf("required = %v, want %v", required, wantRequired)
	}

	fileProps, fileRequired := schemaFieldSet[PatchFile](t)
	wantFileProps := []string{"path", "replacements"}
	wantFileRequired := []string{"path", "replacements"}
	if !reflect.DeepEqual(fileProps, wantFileProps) {
		t.Fatalf("PatchFile properties = %v, want %v", fileProps, wantFileProps)
	}
	if !reflect.DeepEqual(fileRequired, wantFileRequired) {
		t.Fatalf("PatchFile required = %v, want %v", fileRequired, wantFileRequired)
	}

	replProps, replRequired := schemaFieldSet[PatchReplacement](t)
	wantReplProps := []string{"all", "new", "old"}
	wantReplRequired := []string{"new", "old"}
	if !reflect.DeepEqual(replProps, wantReplProps) {
		t.Fatalf("PatchReplacement properties = %v, want %v", replProps, wantReplProps)
	}
	if !reflect.DeepEqual(replRequired, wantReplRequired) {
		t.Fatalf("PatchReplacement required = %v, want %v", replRequired, wantReplRequired)
	}
}

func TestComputerUseInputSchemaFrozen(t *testing.T) {
	props, required := schemaFieldSet[ComputerUseInput](t)
	wantProps := []string{
		"action", "amount", "button", "direction", "duration_ms", "element_index",
		"from_x", "from_y", "key", "max_depth", "max_elements", "modifiers",
		"pid", "query", "scope", "text", "to_x", "to_y", "window_id", "x", "y",
	}
	wantRequired := []string{"action"}
	if !reflect.DeepEqual(props, wantProps) {
		t.Fatalf("properties = %v, want %v", props, wantProps)
	}
	if !reflect.DeepEqual(required, wantRequired) {
		t.Fatalf("required = %v, want %v", required, wantRequired)
	}
}

// TestResultMetaShape locks in the four fields every tool result carries.
func TestResultMetaShape(t *testing.T) {
	props, _ := schemaFieldSet[ResultMeta](t)
	want := []string{"code", "identity", "message", "retryable"}
	if !reflect.DeepEqual(props, want) {
		t.Fatalf("ResultMeta properties = %v, want %v", props, want)
	}
}
