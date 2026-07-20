package api

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file publishes the contract's closed value sets as JSON Schema `enum`
// constraints.
//
// The MCP SDK infers each tool's input schema from its Go input type, but Go
// has no enum concept: a named string type like ComputerUseAction reflects as
// a plain string, so the inferred schema said `{"type":"string"}` and the
// eleven legal actions were documented only in a prose description. Clients --
// and the models driving them -- could not discover the legal values; they
// learned them by guessing and reading the rejection. Every value set below is
// already enforced server-side by the Validate methods, so declaring it in the
// schema publishes a constraint that existed all along.
//
// One consequence worth knowing: the SDK validates incoming arguments against
// the schema it serves, so a bad enum value is now rejected at the protocol
// layer (naming the legal values) instead of reaching a handler and coming
// back as an in-band INVALID_ARGUMENT result. The Validate methods stay as
// defense in depth -- they still guard the rpc and root-helper paths, which do
// not go through the SDK's schema validation.
//
// Ordering is significant: these slices are the single source of truth, the
// valid* lookup maps are derived from them, and the order defined here is the
// order clients see.

// ComputerUseActions is the closed, ordered set of computer_use actions.
var ComputerUseActions = []ComputerUseAction{
	ActionCapture,
	ActionAccessibility,
	ActionClick,
	ActionDoubleClick,
	ActionDrag,
	ActionScroll,
	ActionType,
	ActionKey,
	ActionWait,
	ActionListApplications,
	ActionFocusApplication,
}

// ComputerUseScopes is the closed, ordered set of capture/accessibility scopes.
var ComputerUseScopes = []ComputerUseScope{ScopeScreen, ScopeWindow}

// MouseButtons is the closed, ordered set of mouse buttons.
var MouseButtons = []MouseButton{ButtonLeft, ButtonRight, ButtonMiddle}

// ScrollDirections is the closed, ordered set of scroll directions.
var ScrollDirections = []ScrollDirection{DirectionUp, DirectionDown, DirectionLeft, DirectionRight}

// ProcessActions is the closed, ordered set of process actions.
var ProcessActions = []ProcessAction{ProcessStart, ProcessPoll, ProcessWrite, ProcessTerminate}

// FileEncodings is the closed, ordered set of file content encodings.
var FileEncodings = []FileEncoding{EncodingUTF8, EncodingBase64}

// SearchModes is the closed, ordered set of search_files modes.
var SearchModes = []SearchMode{SearchByName, SearchByContent}

// enumRegistry maps each enum-typed Go type to its legal values, so
// InputSchemaFor can annotate any field of that type without the tool
// definitions having to repeat the value list.
var enumRegistry = map[reflect.Type][]any{
	reflect.TypeFor[ComputerUseAction](): toAny(ComputerUseActions),
	reflect.TypeFor[ComputerUseScope]():  toAny(ComputerUseScopes),
	reflect.TypeFor[MouseButton]():       toAny(MouseButtons),
	reflect.TypeFor[ScrollDirection]():   toAny(ScrollDirections),
	reflect.TypeFor[ProcessAction]():     toAny(ProcessActions),
	reflect.TypeFor[FileEncoding]():      toAny(FileEncodings),
	reflect.TypeFor[SearchMode]():        toAny(SearchModes),
}

// toAny widens a slice of enum constants to the []any that JSON Schema's enum
// keyword takes, converting each to its underlying string so the values
// marshal as plain JSON strings.
func toAny[T ~string](values []T) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

// InputSchemaFor infers In's JSON Schema the same way the MCP SDK would, then
// annotates every field whose Go type is a registered enum with that type's
// legal values. Tools pass the result as mcp.Tool.InputSchema, which takes
// precedence over the SDK's own inference.
func InputSchemaFor[In any]() (*jsonschema.Schema, error) {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		return nil, fmt.Errorf("infer schema for %T: %w", *new(In), err)
	}
	annotateEnums(reflect.TypeFor[In](), schema)
	return schema, nil
}

// annotateEnums walks t's fields and stamps enum values onto the matching
// schema properties. It recurses through structs so nested and embedded input
// types are covered too.
func annotateEnums(t reflect.Type, schema *jsonschema.Schema) {
	t = deref(t)
	if t == nil || t.Kind() != reflect.Struct || schema == nil {
		return
	}
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		ft := deref(field.Type)
		if ft == nil {
			continue
		}
		// An embedded struct's fields are inlined into the same JSON object,
		// so its properties live on this schema, not a nested one.
		if field.Anonymous && ft.Kind() == reflect.Struct {
			annotateEnums(ft, schema)
			continue
		}
		if prop := schema.Properties[jsonFieldName(field)]; prop != nil {
			annotateField(ft, prop)
		}
	}
}

// annotateField stamps t's enum values onto prop, descending into a list's
// items first: for a []MouseButton the constraint belongs on the element
// schema, not on the array that holds them.
func annotateField(t reflect.Type, prop *jsonschema.Schema) {
	for t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		if prop.Items == nil {
			return
		}
		prop = prop.Items
		t = deref(t.Elem())
		if t == nil {
			return
		}
	}
	if values, ok := enumRegistry[t]; ok {
		prop.Enum = values
		return
	}
	annotateEnums(t, prop)
}

// deref unwraps pointers to the type pointed at, so an *api.MouseButton is
// annotated like a bare one. Slices are left intact for annotateField, which
// needs to walk into the matching items schema.
func deref(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// jsonFieldName returns the property name the SDK's inference gives a field:
// its json tag name, or the field name when the tag is absent.
func jsonFieldName(field reflect.StructField) string {
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return field.Name
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return field.Name
	}
	return name
}

// validSet turns an ordered enum slice into the lookup map the Validate
// methods use, keeping the slice the single source of truth.
func validSet[T ~string](values []T) map[T]bool {
	set := make(map[T]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}

// ToolFor builds a tool descriptor whose input schema carries the enum
// constraints for In. Supplying InputSchema explicitly takes precedence over
// the SDK's own inference, which cannot see Go's named string types as the
// closed sets they are.
//
// A schema that will not infer is a programming error in this package's types,
// caught the first time a server is built; the SDK panics on a bad schema for
// the same reason.
func ToolFor[In any](name, description string) *mcp.Tool {
	schema, err := InputSchemaFor[In]()
	if err != nil {
		panic(fmt.Errorf("api: input schema for tool %q: %w", name, err))
	}
	return &mcp.Tool{Name: name, Description: description, InputSchema: schema}
}
