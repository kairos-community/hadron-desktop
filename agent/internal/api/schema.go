// Package api freezes the public MCP contract exposed by the hadron-agent
// service binary: the eight authenticated tool names, their typed
// input/output schemas (inferred by the MCP SDK from the Go types below),
// and the stable error codes every tool result may carry.
//
// This package intentionally contains no tool behavior. Later Phase-2 tasks
// (auth, files, process, the Cua adapter, rpc, the session broker, the root
// helper, and the gateway) implement the handlers that RegisterAll wires up
// with stubs today. Renaming a tool, a JSON field, or an error code here is a
// breaking change to every one of those consumers.
package api

import (
	"context"
	"fmt"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool names. The sorted set of these eight values is the entire public
// surface of hadron-agent; no eighth tool may be added without a deliberate,
// reviewed contract change.
const (
	ToolComputerUse = "computer_use"
	ToolTerminal    = "terminal"
	ToolProcess     = "process"
	ToolReadFile    = "read_file"
	ToolSearchFiles = "search_files"
	ToolWriteFile   = "write_file"
	ToolPatch       = "patch"
	ToolBrowser     = "browser"
)

// ToolNames returns the sorted list of the eight public tool names.
func ToolNames() []string {
	names := []string{
		ToolComputerUse,
		ToolTerminal,
		ToolProcess,
		ToolReadFile,
		ToolSearchFiles,
		ToolWriteFile,
		ToolPatch,
		ToolBrowser,
	}
	sort.Strings(names)
	return names
}

// ErrorCode is a stable, machine-readable failure category. Codes are part
// of the public contract: clients (including the Cua agent loop) branch on
// them, so values must never be renamed or repurposed. The zero value ""
// denotes success and is not one of the stable error codes below.
type ErrorCode string

const (
	// CodeInvalidArgument means the request's arguments failed validation
	// (a bad enum value, a missing required field, or an irrelevant
	// field/action combination).
	CodeInvalidArgument ErrorCode = "INVALID_ARGUMENT"
	// CodeUnauthenticated means the caller did not present a valid
	// credential for the hadron-agent MCP endpoint.
	CodeUnauthenticated ErrorCode = "UNAUTHENTICATED"
	// CodeForbidden means the caller authenticated but is not permitted to
	// perform the requested action.
	CodeForbidden ErrorCode = "FORBIDDEN"
	// CodeSessionUnavailable means the desktop/session backing computer_use
	// (or the process/file session) is not currently reachable.
	CodeSessionUnavailable ErrorCode = "SESSION_UNAVAILABLE"
	// CodeBrowserUnavailable means no browser with a reachable CDP endpoint
	// is running. It is deliberately distinct from CodeSessionUnavailable:
	// the desktop session can be perfectly healthy while no browser is up.
	CodeBrowserUnavailable ErrorCode = "BROWSER_UNAVAILABLE"
	// CodeNotFound means the referenced resource (file, process, window,
	// element) does not exist.
	CodeNotFound ErrorCode = "NOT_FOUND"
	// CodeAlreadyExists means the operation would clobber an existing
	// resource that the caller did not opt into replacing.
	CodeAlreadyExists ErrorCode = "ALREADY_EXISTS"
	// CodeResourceExhausted means a quota, rate limit, or capacity bound was
	// hit (too many concurrent processes, output too large, etc.).
	CodeResourceExhausted ErrorCode = "RESOURCE_EXHAUSTED"
	// CodeDeadlineExceeded means the operation did not complete within its
	// requested or default timeout.
	CodeDeadlineExceeded ErrorCode = "DEADLINE_EXCEEDED"
	// CodeOutputTruncated means the call succeeded but its output was cut
	// short by a size limit; callers should treat the result as partial.
	CodeOutputTruncated ErrorCode = "OUTPUT_TRUNCATED"
	// CodePaused means the session is intentionally paused (for example, a
	// human operator has taken control) and the call was not attempted.
	CodePaused ErrorCode = "PAUSED"
	// CodeInternal means an unexpected server-side failure occurred. The
	// message is safe to show; it must never leak internal detail.
	CodeInternal ErrorCode = "INTERNAL"
)

// Identity describes the OS identity a tool call executed under, when the
// call runs against a concrete session identity (desktop actions, spawned
// processes). It is omitted when a result carries no execution identity
// (for example, a pure validation failure).
type Identity struct {
	// User is the OS username the call executed as.
	User string `json:"user"`
	// UID is the OS numeric user id, when known.
	UID int `json:"uid,omitempty"`
	// PID is the OS process id most closely associated with the call
	// (the spawned process, or the process backing the desktop session).
	PID int `json:"pid,omitempty"`
}

// ResultMeta is the reusable envelope every tool's output type embeds. Code
// is the empty string on success. Message is always safe to surface to an
// end user or model (never a raw internal error or stack trace). Retryable
// tells the caller whether retrying the same call, unmodified, might
// succeed. Identity is populated whenever the call executed under a known
// OS identity.
type ResultMeta struct {
	Code      ErrorCode `json:"code,omitempty"`
	Message   string    `json:"message,omitempty"`
	Retryable bool      `json:"retryable,omitempty"`
	Identity  *Identity `json:"identity,omitempty"`
}

// errorMeta builds a ResultMeta for a failed call.
func errorMeta(code ErrorCode, message string, retryable bool) ResultMeta {
	return ResultMeta{Code: code, Message: message, Retryable: retryable}
}

// notImplementedMeta is used by every stub handler registered by
// RegisterAll: the schema surface is frozen, but behavior is implemented by
// later Phase-2 tasks.
func notImplementedMeta(tool string) ResultMeta {
	return errorMeta(CodeInternal, fmt.Sprintf("%s: not implemented", tool), false)
}

// ---------------------------------------------------------------------------
// computer_use
// ---------------------------------------------------------------------------

// ComputerUseAction selects the operation computer_use performs.
type ComputerUseAction string

const (
	ActionCapture          ComputerUseAction = "capture"
	ActionAccessibility    ComputerUseAction = "accessibility"
	ActionClick            ComputerUseAction = "click"
	ActionDoubleClick      ComputerUseAction = "double_click"
	ActionDrag             ComputerUseAction = "drag"
	ActionScroll           ComputerUseAction = "scroll"
	ActionType             ComputerUseAction = "type"
	ActionKey              ComputerUseAction = "key"
	ActionWait             ComputerUseAction = "wait"
	ActionListApplications ComputerUseAction = "list_applications"
	ActionFocusApplication ComputerUseAction = "focus_application"
)

// validComputerUseActions is the closed set of valid ComputerUseInput.Action
// values, derived from ComputerUseActions so the order clients see and the
// values the server accepts cannot drift apart.
var validComputerUseActions = validSet(ComputerUseActions)

// ComputerUseScope selects what a capture or accessibility query targets.
type ComputerUseScope string

const (
	ScopeScreen ComputerUseScope = "screen"
	ScopeWindow ComputerUseScope = "window"
)

// MouseButton selects which mouse button a click or drag uses.
type MouseButton string

const (
	ButtonLeft   MouseButton = "left"
	ButtonRight  MouseButton = "right"
	ButtonMiddle MouseButton = "middle"
)

// ScrollDirection selects which way a scroll action moves.
type ScrollDirection string

const (
	DirectionUp    ScrollDirection = "up"
	DirectionDown  ScrollDirection = "down"
	DirectionLeft  ScrollDirection = "left"
	DirectionRight ScrollDirection = "right"
)

// ComputerUseInput is the single input type for the computer_use tool. Only
// the fields relevant to Action are meaningful; Validate rejects requests
// that set fields irrelevant to Action, or omit fields Action requires.
//
// Coordinate and index fields are pointers so that an explicit 0 (a valid
// screen coordinate, index, or count) can be distinguished from "not set".
type ComputerUseInput struct {
	Action ComputerUseAction `json:"action" jsonschema:"the operation to perform"`

	// Scope narrows capture/accessibility to the whole screen or a single
	// window. One of "screen", "window".
	Scope ComputerUseScope `json:"scope,omitempty" jsonschema:"capture/accessibility target scope: screen or window"`
	// PID targets a window by owning process id.
	PID *int `json:"pid,omitempty" jsonschema:"OS process id of the target application/window"`
	// WindowID targets a specific window by its native window id.
	WindowID *int64 `json:"window_id,omitempty" jsonschema:"native id of the target window"`
	// ElementIndex targets a specific accessibility-tree element returned by
	// a prior accessibility call.
	ElementIndex *int `json:"element_index,omitempty" jsonschema:"index of an element from a prior accessibility result"`

	// X, Y are absolute screen coordinates for click/double_click/scroll.
	X *int `json:"x,omitempty" jsonschema:"absolute screen x coordinate"`
	Y *int `json:"y,omitempty" jsonschema:"absolute screen y coordinate"`

	// FromX, FromY, ToX, ToY are the drag gesture endpoints.
	FromX *int `json:"from_x,omitempty" jsonschema:"drag start x coordinate"`
	FromY *int `json:"from_y,omitempty" jsonschema:"drag start y coordinate"`
	ToX   *int `json:"to_x,omitempty" jsonschema:"drag end x coordinate"`
	ToY   *int `json:"to_y,omitempty" jsonschema:"drag end y coordinate"`

	// Button selects the mouse button for click/double_click/drag.
	Button MouseButton `json:"button,omitempty" jsonschema:"mouse button: left, right, or middle"`
	// Direction and Amount configure a scroll gesture.
	Direction ScrollDirection `json:"direction,omitempty" jsonschema:"scroll direction: up, down, left, or right"`
	Amount    *int            `json:"amount,omitempty" jsonschema:"scroll amount, in notches"`

	// Text is the literal string typed by the type action.
	Text string `json:"text,omitempty" jsonschema:"literal text to type"`
	// Key is a named key (e.g. Return, Escape) pressed by the key action.
	Key string `json:"key,omitempty" jsonschema:"named key to press"`
	// Modifiers are held down for the duration of a key or click action.
	Modifiers []string `json:"modifiers,omitempty" jsonschema:"modifier keys held during the action, e.g. ctrl, shift"`

	// DurationMs is how long the wait action pauses for, in milliseconds.
	DurationMs *int `json:"duration_ms,omitempty" jsonschema:"wait duration in milliseconds"`

	// Query filters list_applications results or an accessibility search.
	Query string `json:"query,omitempty" jsonschema:"filter query for accessibility search or list_applications"`
	// MaxElements and MaxDepth bound an accessibility tree walk.
	MaxElements *int `json:"max_elements,omitempty" jsonschema:"maximum accessibility elements to return"`
	MaxDepth    *int `json:"max_depth,omitempty" jsonschema:"maximum accessibility tree depth to walk"`
}

// cuField identifies one optional ComputerUseInput field for the purpose of
// per-action allow-listing.
type cuField uint32

const (
	cuScope cuField = 1 << iota
	cuPID
	cuWindowID
	cuElementIndex
	cuX
	cuY
	cuFromX
	cuFromY
	cuToX
	cuToY
	cuButton
	cuDirection
	cuAmount
	cuText
	cuKey
	cuModifiers
	cuDurationMs
	cuQuery
	cuMaxElements
	cuMaxDepth
)

var cuFieldNames = map[cuField]string{
	cuScope:        "scope",
	cuPID:          "pid",
	cuWindowID:     "window_id",
	cuElementIndex: "element_index",
	cuX:            "x",
	cuY:            "y",
	cuFromX:        "from_x",
	cuFromY:        "from_y",
	cuToX:          "to_x",
	cuToY:          "to_y",
	cuButton:       "button",
	cuDirection:    "direction",
	cuAmount:       "amount",
	cuText:         "text",
	cuKey:          "key",
	cuModifiers:    "modifiers",
	cuDurationMs:   "duration_ms",
	cuQuery:        "query",
	cuMaxElements:  "max_elements",
	cuMaxDepth:     "max_depth",
}

// presentFields returns the bitmask of optional fields that are set on in.
func (in ComputerUseInput) presentFields() cuField {
	var f cuField
	if in.Scope != "" {
		f |= cuScope
	}
	if in.PID != nil {
		f |= cuPID
	}
	if in.WindowID != nil {
		f |= cuWindowID
	}
	if in.ElementIndex != nil {
		f |= cuElementIndex
	}
	if in.X != nil {
		f |= cuX
	}
	if in.Y != nil {
		f |= cuY
	}
	if in.FromX != nil {
		f |= cuFromX
	}
	if in.FromY != nil {
		f |= cuFromY
	}
	if in.ToX != nil {
		f |= cuToX
	}
	if in.ToY != nil {
		f |= cuToY
	}
	if in.Button != "" {
		f |= cuButton
	}
	if in.Direction != "" {
		f |= cuDirection
	}
	if in.Amount != nil {
		f |= cuAmount
	}
	if in.Text != "" {
		f |= cuText
	}
	if in.Key != "" {
		f |= cuKey
	}
	if len(in.Modifiers) > 0 {
		f |= cuModifiers
	}
	if in.DurationMs != nil {
		f |= cuDurationMs
	}
	if in.Query != "" {
		f |= cuQuery
	}
	if in.MaxElements != nil {
		f |= cuMaxElements
	}
	if in.MaxDepth != nil {
		f |= cuMaxDepth
	}
	return f
}

// rejectExtraneous returns an error naming the first field set on in that is
// not present in allowed.
func (in ComputerUseInput) rejectExtraneous(allowed cuField) error {
	extra := in.presentFields() &^ allowed
	if extra == 0 {
		return nil
	}
	var names []string
	for bit, name := range cuFieldNames {
		if extra&bit != 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return fmt.Errorf("action %q does not accept field(s): %v", in.Action, names)
}

// targetsWindow reports whether pid or window_id identify a target window.
func (in ComputerUseInput) targetsWindow() bool {
	return in.PID != nil || in.WindowID != nil
}

// Validate checks that ComputerUseInput carries a recognized Action, that
// every field set is relevant to that Action, and that Action's required
// fields (or field combinations) are present.
func (in ComputerUseInput) Validate() error {
	if !validComputerUseActions[in.Action] {
		return fmt.Errorf("unknown action %q", in.Action)
	}

	switch in.Button {
	case "", ButtonLeft, ButtonRight, ButtonMiddle:
	default:
		return fmt.Errorf("invalid button %q", in.Button)
	}
	switch in.Direction {
	case "", DirectionUp, DirectionDown, DirectionLeft, DirectionRight:
	default:
		return fmt.Errorf("invalid direction %q", in.Direction)
	}
	switch in.Scope {
	case "", ScopeScreen, ScopeWindow:
	default:
		return fmt.Errorf("invalid scope %q", in.Scope)
	}

	switch in.Action {
	case ActionCapture:
		if err := in.rejectExtraneous(cuScope | cuPID | cuWindowID); err != nil {
			return err
		}
		if in.Scope == ScopeWindow && !in.targetsWindow() {
			return fmt.Errorf("action %q with scope %q requires pid or window_id", in.Action, in.Scope)
		}

	case ActionAccessibility:
		if err := in.rejectExtraneous(cuScope | cuPID | cuWindowID | cuQuery | cuMaxElements | cuMaxDepth); err != nil {
			return err
		}
		if in.Scope == ScopeWindow && !in.targetsWindow() {
			return fmt.Errorf("action %q with scope %q requires pid or window_id", in.Action, in.Scope)
		}

	case ActionClick, ActionDoubleClick:
		if err := in.rejectExtraneous(cuX | cuY | cuWindowID | cuPID | cuButton | cuElementIndex); err != nil {
			return err
		}
		hasPoint := in.X != nil && in.Y != nil
		if !hasPoint && in.ElementIndex == nil {
			return fmt.Errorf("action %q requires x and y, or element_index", in.Action)
		}
		if (in.X != nil) != (in.Y != nil) {
			return fmt.Errorf("action %q requires both x and y together", in.Action)
		}

	case ActionDrag:
		if err := in.rejectExtraneous(cuFromX | cuFromY | cuToX | cuToY | cuButton | cuWindowID | cuPID); err != nil {
			return err
		}
		if in.FromX == nil || in.FromY == nil || in.ToX == nil || in.ToY == nil {
			return fmt.Errorf("action %q requires from_x, from_y, to_x, and to_y", in.Action)
		}

	case ActionScroll:
		if err := in.rejectExtraneous(cuX | cuY | cuDirection | cuAmount | cuWindowID | cuPID); err != nil {
			return err
		}
		if in.Direction == "" {
			return fmt.Errorf("action %q requires direction", in.Action)
		}
		if in.Amount != nil && *in.Amount <= 0 {
			return fmt.Errorf("action %q requires amount > 0 when set", in.Action)
		}

	case ActionType:
		if err := in.rejectExtraneous(cuText | cuWindowID | cuPID | cuElementIndex); err != nil {
			return err
		}
		if in.Text == "" {
			return fmt.Errorf("action %q requires text", in.Action)
		}

	case ActionKey:
		if err := in.rejectExtraneous(cuKey | cuModifiers | cuWindowID | cuPID); err != nil {
			return err
		}
		if in.Key == "" {
			return fmt.Errorf("action %q requires key", in.Action)
		}

	case ActionWait:
		if err := in.rejectExtraneous(cuDurationMs); err != nil {
			return err
		}
		if in.DurationMs == nil || *in.DurationMs <= 0 {
			return fmt.Errorf("action %q requires duration_ms > 0", in.Action)
		}

	case ActionListApplications:
		if err := in.rejectExtraneous(cuQuery); err != nil {
			return err
		}

	case ActionFocusApplication:
		if err := in.rejectExtraneous(cuPID | cuWindowID); err != nil {
			return err
		}
		if !in.targetsWindow() {
			return fmt.Errorf("action %q requires pid or window_id", in.Action)
		}
	}

	return nil
}

// AccessibilityElement is one node of an accessibility tree returned by the
// accessibility action. The tree is flattened into an ordered list: Index is
// this element's position in that list (what computer_use's element_index
// input field refers back to), and ParentIndex, when set, is the Index of
// its parent. Roots have a nil ParentIndex. A flat, indexed representation
// is used instead of a nested Children slice because the MCP SDK's schema
// inference cannot represent a self-referential (recursive) Go type.
type AccessibilityElement struct {
	Index       int    `json:"index"`
	ParentIndex *int   `json:"parent_index,omitempty"`
	Role        string `json:"role,omitempty"`
	Name        string `json:"name,omitempty"`
	X           int    `json:"x,omitempty"`
	Y           int    `json:"y,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
}

// ApplicationInfo describes one running application/window surfaced by
// list_applications.
type ApplicationInfo struct {
	PID      int    `json:"pid"`
	Name     string `json:"name,omitempty"`
	WindowID int64  `json:"window_id,omitempty"`
}

// ComputerUseOutput is the output type for the computer_use tool. Only the
// fields relevant to the requested action are populated.
type ComputerUseOutput struct {
	ResultMeta

	// ImageBase64 holds a PNG screenshot for the capture action.
	ImageBase64 string `json:"image_base64,omitempty"`
	// Elements holds the accessibility tree for the accessibility action.
	Elements []AccessibilityElement `json:"elements,omitempty"`
	// Applications holds the process list for list_applications.
	Applications []ApplicationInfo `json:"applications,omitempty"`
	// WindowID holds the window that focus_application activated.
	WindowID int64 `json:"window_id,omitempty"`
}

// ---------------------------------------------------------------------------
// terminal
// ---------------------------------------------------------------------------

// TerminalInput runs a single command to completion (subject to timeout and
// output limits).
type TerminalInput struct {
	Command        string   `json:"command" jsonschema:"shell command line to execute"`
	Cwd            string   `json:"cwd,omitempty" jsonschema:"working directory"`
	Env            []string `json:"env,omitempty" jsonschema:"additional KEY=VALUE environment entries"`
	TimeoutMs      *int     `json:"timeout_ms,omitempty" jsonschema:"maximum time to wait, in milliseconds"`
	MaxOutputBytes *int     `json:"max_output_bytes,omitempty" jsonschema:"maximum combined stdout+stderr bytes to return"`
}

// Validate checks TerminalInput's required fields.
func (in TerminalInput) Validate() error {
	if in.Command == "" {
		return fmt.Errorf("command is required")
	}
	return nil
}

// TerminalOutput is the result of a terminal call.
type TerminalOutput struct {
	ResultMeta

	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	ExitCode  int    `json:"exit_code,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ---------------------------------------------------------------------------
// process
// ---------------------------------------------------------------------------

// ProcessAction selects the operation the process tool performs.
type ProcessAction string

const (
	ProcessStart     ProcessAction = "start"
	ProcessPoll      ProcessAction = "poll"
	ProcessWrite     ProcessAction = "write"
	ProcessTerminate ProcessAction = "terminate"
)

var validProcessActions = validSet(ProcessActions)

// ProcessInput manages a long-running background process. ProcessID
// identifies an existing process for poll/write/terminate; it is assigned by
// the server on start and must be echoed back by the caller.
type ProcessInput struct {
	Action    ProcessAction `json:"action" jsonschema:"start, poll, write, or terminate"`
	ProcessID string        `json:"process_id,omitempty" jsonschema:"server-assigned handle of an existing process"`

	// Command, Args, Cwd, Env, and PTY configure a start action.
	Command string   `json:"command,omitempty" jsonschema:"executable to run"`
	Args    []string `json:"args,omitempty" jsonschema:"arguments to the executable"`
	Cwd     string   `json:"cwd,omitempty" jsonschema:"working directory"`
	Env     []string `json:"env,omitempty" jsonschema:"additional KEY=VALUE environment entries"`
	PTY     bool     `json:"pty,omitempty" jsonschema:"allocate a pseudo-terminal for the process"`

	// Input carries the bytes a write action sends to the process's stdin.
	Input string `json:"input,omitempty" jsonschema:"data to write to the process's stdin"`

	// TimeoutMs bounds how long a poll waits for new output.
	TimeoutMs *int `json:"timeout_ms,omitempty" jsonschema:"maximum time to wait, in milliseconds"`
	// Signal names the signal a terminate action sends (default SIGTERM).
	Signal string `json:"signal,omitempty" jsonschema:"signal to send on terminate, e.g. SIGTERM, SIGKILL"`
}

// Validate checks that ProcessInput carries the fields its Action requires.
func (in ProcessInput) Validate() error {
	if !validProcessActions[in.Action] {
		return fmt.Errorf("unknown action %q", in.Action)
	}
	switch in.Action {
	case ProcessStart:
		if in.Command == "" {
			return fmt.Errorf("action %q requires command", in.Action)
		}
		if in.ProcessID != "" {
			return fmt.Errorf("action %q does not accept process_id", in.Action)
		}
	case ProcessPoll, ProcessWrite, ProcessTerminate:
		if in.ProcessID == "" {
			return fmt.Errorf("action %q requires process_id", in.Action)
		}
		if in.Action == ProcessWrite && in.Input == "" {
			return fmt.Errorf("action %q requires input", in.Action)
		}
	}
	return nil
}

// ProcessOutput is the result of a process call.
type ProcessOutput struct {
	ResultMeta

	ProcessID string `json:"process_id,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Running   bool   `json:"running,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ---------------------------------------------------------------------------
// read_file
// ---------------------------------------------------------------------------

// FileEncoding selects how file content bytes are represented in JSON.
type FileEncoding string

const (
	EncodingUTF8   FileEncoding = "utf8"
	EncodingBase64 FileEncoding = "base64"
)

// ReadFileInput reads all or part of a file.
type ReadFileInput struct {
	Path         string       `json:"path" jsonschema:"path of the file to read"`
	Offset       *int64       `json:"offset,omitempty" jsonschema:"byte offset to start reading from"`
	Limit        *int64       `json:"limit,omitempty" jsonschema:"maximum number of bytes to read"`
	Encoding     FileEncoding `json:"encoding,omitempty" jsonschema:"utf8 or base64"`
	MetadataOnly bool         `json:"metadata_only,omitempty" jsonschema:"return only file metadata, no content"`
}

// Validate checks ReadFileInput's required fields and enum values.
func (in ReadFileInput) Validate() error {
	if in.Path == "" {
		return fmt.Errorf("path is required")
	}
	switch in.Encoding {
	case "", EncodingUTF8, EncodingBase64:
	default:
		return fmt.Errorf("invalid encoding %q", in.Encoding)
	}
	return nil
}

// ReadFileOutput is the result of a read_file call.
type ReadFileOutput struct {
	ResultMeta

	Content   string       `json:"content,omitempty"`
	Encoding  FileEncoding `json:"encoding,omitempty"`
	Size      int64        `json:"size,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
}

// ---------------------------------------------------------------------------
// search_files
// ---------------------------------------------------------------------------

// SearchMode selects whether search_files matches file names or file
// contents.
type SearchMode string

const (
	SearchByName    SearchMode = "name"
	SearchByContent SearchMode = "content"
)

// SearchFilesInput searches a directory tree by file name or content.
type SearchFilesInput struct {
	Path         string     `json:"path" jsonschema:"root directory to search"`
	Query        string     `json:"query" jsonschema:"literal string or, if regex is true, a regular expression"`
	Mode         SearchMode `json:"mode,omitempty" jsonschema:"name or content"`
	Regex        bool       `json:"regex,omitempty" jsonschema:"treat query as a regular expression"`
	MaxResults   *int       `json:"max_results,omitempty" jsonschema:"maximum number of matches to return"`
	MaxFileBytes *int64     `json:"max_file_bytes,omitempty" jsonschema:"skip files larger than this many bytes"`
}

// Validate checks SearchFilesInput's required fields and enum values.
func (in SearchFilesInput) Validate() error {
	if in.Path == "" {
		return fmt.Errorf("path is required")
	}
	if in.Query == "" {
		return fmt.Errorf("query is required")
	}
	switch in.Mode {
	case "", SearchByName, SearchByContent:
	default:
		return fmt.Errorf("invalid mode %q", in.Mode)
	}
	return nil
}

// SearchMatch is one search_files hit.
type SearchMatch struct {
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

// SearchFilesOutput is the result of a search_files call.
type SearchFilesOutput struct {
	ResultMeta

	Matches   []SearchMatch `json:"matches,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
}

// ---------------------------------------------------------------------------
// write_file
// ---------------------------------------------------------------------------

// WriteFileInput writes (or overwrites) a file.
type WriteFileInput struct {
	Path          string       `json:"path" jsonschema:"path of the file to write"`
	Content       string       `json:"content" jsonschema:"file content, in the given encoding"`
	Encoding      FileEncoding `json:"encoding,omitempty" jsonschema:"utf8 or base64; defaults to utf8"`
	Mode          string       `json:"mode,omitempty" jsonschema:"POSIX file mode to set, e.g. 0644"`
	CreateParents bool         `json:"create_parents,omitempty" jsonschema:"create missing parent directories"`
}

// Validate checks WriteFileInput's required fields and enum values.
func (in WriteFileInput) Validate() error {
	if in.Path == "" {
		return fmt.Errorf("path is required")
	}
	switch in.Encoding {
	case "", EncodingUTF8, EncodingBase64:
	default:
		return fmt.Errorf("invalid encoding %q", in.Encoding)
	}
	return nil
}

// WriteFileOutput is the result of a write_file call.
type WriteFileOutput struct {
	ResultMeta

	BytesWritten int64 `json:"bytes_written,omitempty"`
	Created      bool  `json:"created,omitempty"`
}

// ---------------------------------------------------------------------------
// patch
// ---------------------------------------------------------------------------

// PatchReplacement is one exact-match text substitution.
type PatchReplacement struct {
	Old string `json:"old" jsonschema:"exact text to find"`
	New string `json:"new" jsonschema:"replacement text"`
	All bool   `json:"all,omitempty" jsonschema:"replace every occurrence instead of requiring exactly one"`
}

// PatchFile is one file's set of replacements.
type PatchFile struct {
	Path         string             `json:"path" jsonschema:"path of the file to patch"`
	Replacements []PatchReplacement `json:"replacements" jsonschema:"exact-match replacements to apply, in order"`
}

// PatchInput applies exact-match text replacements to one or more files.
type PatchInput struct {
	Files []PatchFile `json:"files" jsonschema:"files to patch"`
}

// Validate checks that PatchInput carries at least one file, each with a
// path and at least one replacement whose Old text is non-empty.
func (in PatchInput) Validate() error {
	if len(in.Files) == 0 {
		return fmt.Errorf("files is required and must be non-empty")
	}
	for i, f := range in.Files {
		if f.Path == "" {
			return fmt.Errorf("files[%d].path is required", i)
		}
		if len(f.Replacements) == 0 {
			return fmt.Errorf("files[%d].replacements is required and must be non-empty", i)
		}
		for j, r := range f.Replacements {
			if r.Old == "" {
				return fmt.Errorf("files[%d].replacements[%d].old is required", i, j)
			}
		}
	}
	return nil
}

// PatchOutput is the result of a patch call.
type PatchOutput struct {
	ResultMeta

	FilesChanged        []string `json:"files_changed,omitempty"`
	ReplacementsApplied int      `json:"replacements_applied,omitempty"`
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

// RegisterAll registers all eight public tools on s. Each tool's input schema
// comes from ToolFor, which infers it from the Go types above and then
// publishes their closed value sets as JSON Schema enums; output schemas are
// still inferred by the SDK. Every
// handler here is a stub: it validates its input where this package defines
// a Validate method, and otherwise reports CodeInternal "not implemented".
// Later Phase-2 tasks replace these stubs with real behavior; this function
// exists so the eight-tool contract is registrable and testable today.
func RegisterAll(s *mcp.Server) {
	mcp.AddTool(s, ToolFor[ComputerUseInput](ToolComputerUse, "Drive the desktop: capture, accessibility, click, double_click, drag, scroll, type, key, wait, list_applications, focus_application."), stubComputerUse)

	mcp.AddTool(s, ToolFor[TerminalInput](ToolTerminal, "Run a single shell command to completion."), stubTerminal)

	mcp.AddTool(s, ToolFor[ProcessInput](ToolProcess, "Start, poll, write to, or terminate a long-running background process."), stubProcess)

	mcp.AddTool(s, ToolFor[ReadFileInput](ToolReadFile, "Read all or part of a file."), stubReadFile)

	mcp.AddTool(s, ToolFor[SearchFilesInput](ToolSearchFiles, "Search a directory tree by file name or content."), stubSearchFiles)

	mcp.AddTool(s, ToolFor[WriteFileInput](ToolWriteFile, "Write (or overwrite) a file."), stubWriteFile)

	mcp.AddTool(s, ToolFor[PatchInput](ToolPatch, "Apply exact-match text replacements to one or more files."), stubPatch)

	mcp.AddTool(s, ToolFor[BrowserInput](ToolBrowser, BrowserToolDescription), stubBrowser)
}

func stubComputerUse(_ context.Context, _ *mcp.CallToolRequest, in ComputerUseInput) (*mcp.CallToolResult, ComputerUseOutput, error) {
	if err := in.Validate(); err != nil {
		return nil, ComputerUseOutput{ResultMeta: errorMeta(CodeInvalidArgument, err.Error(), false)}, nil
	}
	return nil, ComputerUseOutput{ResultMeta: notImplementedMeta(ToolComputerUse)}, nil
}

func stubTerminal(_ context.Context, _ *mcp.CallToolRequest, in TerminalInput) (*mcp.CallToolResult, TerminalOutput, error) {
	if err := in.Validate(); err != nil {
		return nil, TerminalOutput{ResultMeta: errorMeta(CodeInvalidArgument, err.Error(), false)}, nil
	}
	return nil, TerminalOutput{ResultMeta: notImplementedMeta(ToolTerminal)}, nil
}

func stubProcess(_ context.Context, _ *mcp.CallToolRequest, in ProcessInput) (*mcp.CallToolResult, ProcessOutput, error) {
	if err := in.Validate(); err != nil {
		return nil, ProcessOutput{ResultMeta: errorMeta(CodeInvalidArgument, err.Error(), false)}, nil
	}
	return nil, ProcessOutput{ResultMeta: notImplementedMeta(ToolProcess)}, nil
}

func stubReadFile(_ context.Context, _ *mcp.CallToolRequest, in ReadFileInput) (*mcp.CallToolResult, ReadFileOutput, error) {
	if err := in.Validate(); err != nil {
		return nil, ReadFileOutput{ResultMeta: errorMeta(CodeInvalidArgument, err.Error(), false)}, nil
	}
	return nil, ReadFileOutput{ResultMeta: notImplementedMeta(ToolReadFile)}, nil
}

func stubSearchFiles(_ context.Context, _ *mcp.CallToolRequest, in SearchFilesInput) (*mcp.CallToolResult, SearchFilesOutput, error) {
	if err := in.Validate(); err != nil {
		return nil, SearchFilesOutput{ResultMeta: errorMeta(CodeInvalidArgument, err.Error(), false)}, nil
	}
	return nil, SearchFilesOutput{ResultMeta: notImplementedMeta(ToolSearchFiles)}, nil
}

func stubWriteFile(_ context.Context, _ *mcp.CallToolRequest, in WriteFileInput) (*mcp.CallToolResult, WriteFileOutput, error) {
	if err := in.Validate(); err != nil {
		return nil, WriteFileOutput{ResultMeta: errorMeta(CodeInvalidArgument, err.Error(), false)}, nil
	}
	return nil, WriteFileOutput{ResultMeta: notImplementedMeta(ToolWriteFile)}, nil
}

func stubPatch(_ context.Context, _ *mcp.CallToolRequest, in PatchInput) (*mcp.CallToolResult, PatchOutput, error) {
	if err := in.Validate(); err != nil {
		return nil, PatchOutput{ResultMeta: errorMeta(CodeInvalidArgument, err.Error(), false)}, nil
	}
	return nil, PatchOutput{ResultMeta: notImplementedMeta(ToolPatch)}, nil
}

func stubBrowser(_ context.Context, _ *mcp.CallToolRequest, in BrowserInput) (*mcp.CallToolResult, BrowserOutput, error) {
	if err := in.Validate(); err != nil {
		return nil, BrowserOutput{ResultMeta: errorMeta(CodeInvalidArgument, err.Error(), false)}, nil
	}
	return nil, BrowserOutput{ResultMeta: notImplementedMeta(ToolBrowser)}, nil
}
