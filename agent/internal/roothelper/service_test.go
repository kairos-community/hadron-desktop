package roothelper

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mudler/hadron-desktop/agent/internal/api"
	"github.com/mudler/hadron-desktop/agent/internal/auth"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeFiles is a hermetic FileService that records whether any executor method
// was ever invoked. Read optionally blocks on gate so a test can hold a call in
// flight and observe a pause cancel it.
type fakeFiles struct {
	mu    sync.Mutex
	calls int

	gate    chan struct{} // if non-nil, Read blocks on it (or ctx)
	entered chan struct{} // if non-nil, one send per Read start
}

func (f *fakeFiles) mark() {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
}

func (f *fakeFiles) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeFiles) Read(ctx context.Context, _ api.ReadFileInput) api.ReadFileOutput {
	f.mark()
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return api.ReadFileOutput{ResultMeta: api.ResultMeta{Code: api.CodeInternal, Message: ctx.Err().Error()}}
		}
	}
	return api.ReadFileOutput{Content: "ok", Size: 2}
}

func (f *fakeFiles) Search(_ context.Context, _ api.SearchFilesInput) api.SearchFilesOutput {
	f.mark()
	return api.SearchFilesOutput{}
}

func (f *fakeFiles) Write(_ context.Context, _ api.WriteFileInput) api.WriteFileOutput {
	f.mark()
	return api.WriteFileOutput{BytesWritten: 1, Created: true}
}

func (f *fakeFiles) Patch(_ context.Context, _ api.PatchInput) api.PatchOutput {
	f.mark()
	return api.PatchOutput{ReplacementsApplied: 1}
}

// fakeProcess is a hermetic ProcessManager. It records whether an executor
// method was invoked, and tracks started/terminated process ids. Terminal
// optionally blocks on a gate so a test can pause it mid-flight.
type fakeProcess struct {
	mu         sync.Mutex
	calls      int
	nextID     int
	terminated []string
	closed     bool

	terminalGate    chan struct{} // if non-nil, Terminal blocks on it (or ctx)
	terminalStarted chan struct{} // if non-nil, one send per Terminal start
}

func (p *fakeProcess) mark() {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
}

func (p *fakeProcess) called() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *fakeProcess) Terminal(ctx context.Context, _ api.TerminalInput) (api.TerminalOutput, error) {
	p.mark()
	if p.terminalStarted != nil {
		p.terminalStarted <- struct{}{}
	}
	if p.terminalGate == nil {
		return api.TerminalOutput{Stdout: "ok"}, nil
	}
	select {
	case <-ctx.Done():
		return api.TerminalOutput{ResultMeta: api.ResultMeta{Code: api.CodeDeadlineExceeded, Message: ctx.Err().Error()}}, nil
	case <-p.terminalGate:
		return api.TerminalOutput{Stdout: "ok"}, nil
	}
}

func (p *fakeProcess) Process(_ context.Context, in api.ProcessInput) (api.ProcessOutput, error) {
	p.mark()
	p.mu.Lock()
	defer p.mu.Unlock()
	switch in.Action {
	case api.ProcessStart:
		p.nextID++
		id := fmt.Sprintf("proc-%d", p.nextID)
		return api.ProcessOutput{ProcessID: id, Running: true}, nil
	case api.ProcessTerminate:
		p.terminated = append(p.terminated, in.ProcessID)
		return api.ProcessOutput{ProcessID: in.ProcessID, Running: false}, nil
	default:
		return api.ProcessOutput{ProcessID: in.ProcessID, Running: true}, nil
	}
}

func (p *fakeProcess) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

func (p *fakeProcess) terminatedIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.terminated))
	copy(out, p.terminated)
	return out
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// adminCreds generates a fresh admin bearer plus a Verifier that accepts it.
func adminCreds(t *testing.T) (bearer string, v *auth.Verifier) {
	t.Helper()
	bearer, digest, err := auth.Generate(auth.ClassAdmin)
	if err != nil {
		t.Fatalf("generate admin bearer: %v", err)
	}
	// The Verifier also needs a (never-presented) user config; generate one.
	_, userDigest, err := auth.Generate(auth.ClassUser)
	if err != nil {
		t.Fatalf("generate user digest: %v", err)
	}
	v, err = auth.NewVerifier(
		auth.RotationConfig{Current: userDigest},
		auth.RotationConfig{Current: digest},
	)
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return bearer, v
}

// mismatchedVerifier returns a Verifier whose admin digest matches a DIFFERENT
// admin bearer than the one a test presents, so a well-formed admin bearer that
// is not the configured one is rejected as invalid.
func mismatchedVerifier(t *testing.T) *auth.Verifier {
	t.Helper()
	_, v := adminCreds(t)
	return v
}

func newHelper(t *testing.T, v *auth.Verifier, files FileService, proc ProcessManager) *Helper {
	t.Helper()
	h, err := New(Config{Verifier: v, Files: files, Process: proc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func bearerHeader(bearer string) string { return "Bearer " + bearer }

// ---------------------------------------------------------------------------
// security invariants: no path reaches an executor without a valid admin bearer
// ---------------------------------------------------------------------------

// TestUserTokenRejected proves a user-class bearer never reaches an executor.
func TestUserTokenRejected(t *testing.T) {
	userBearer, _, err := auth.Generate(auth.ClassUser)
	if err != nil {
		t.Fatalf("generate user bearer: %v", err)
	}
	_, v := adminCreds(t)
	files := &fakeFiles{}
	proc := &fakeProcess{}
	h := newHelper(t, v, files, proc)
	defer h.Close()

	meta := callMeta(t, h, api.ToolReadFile, api.ReadFileInput{Path: "/x"}, bearerHeader(userBearer))
	if meta.Code != api.CodeForbidden && meta.Code != api.CodeUnauthenticated {
		t.Fatalf("user token: got code %q, want FORBIDDEN or UNAUTHENTICATED", meta.Code)
	}
	if files.called() != 0 {
		t.Fatalf("user token reached the file executor (%d calls)", files.called())
	}
}

// TestInvalidAdminTokenRejected proves a well-formed admin bearer that is not
// the configured one never reaches an executor.
func TestInvalidAdminTokenRejected(t *testing.T) {
	// A real admin bearer, but the helper is built with a verifier that
	// accepts a DIFFERENT admin digest.
	wrongBearer, _, err := auth.Generate(auth.ClassAdmin)
	if err != nil {
		t.Fatalf("generate admin bearer: %v", err)
	}
	v := mismatchedVerifier(t)
	files := &fakeFiles{}
	proc := &fakeProcess{}
	h := newHelper(t, v, files, proc)
	defer h.Close()

	meta := callMeta(t, h, api.ToolReadFile, api.ReadFileInput{Path: "/x"}, bearerHeader(wrongBearer))
	if meta.Code != api.CodeUnauthenticated {
		t.Fatalf("invalid admin token: got code %q, want UNAUTHENTICATED", meta.Code)
	}
	if files.called() != 0 {
		t.Fatalf("invalid admin token reached the file executor (%d calls)", files.called())
	}
}

// TestMissingTokenRejected proves an empty Authorization header never reaches an
// executor.
func TestMissingTokenRejected(t *testing.T) {
	_, v := adminCreds(t)
	files := &fakeFiles{}
	proc := &fakeProcess{}
	h := newHelper(t, v, files, proc)
	defer h.Close()

	meta := callMeta(t, h, api.ToolReadFile, api.ReadFileInput{Path: "/x"}, "")
	if meta.Code != api.CodeUnauthenticated {
		t.Fatalf("missing token: got code %q, want UNAUTHENTICATED", meta.Code)
	}
	if files.called() != 0 {
		t.Fatalf("missing token reached the file executor (%d calls)", files.called())
	}
}

// TestForgedScopeStillRequiresBearer proves that a gateway that tries to assert
// admin some other way (there is no scope/boolean in the call surface, so it can
// only stuff a bogus value into the Authorization header) still cannot reach an
// executor without the raw admin bearer.
func TestForgedScopeStillRequiresBearer(t *testing.T) {
	_, v := adminCreds(t)
	forged := []string{
		"Bearer hadron:admin",           // the scope string, not a bearer
		"Bearer admin",                  // a bare claim
		"Bearer true",                   // a boolean-ish claim
		"admin",                         // no scheme, a bare claim
		"Bearer hdn_a_not-a-real-token", // right prefix, malformed payload
	}
	for _, header := range forged {
		files := &fakeFiles{}
		proc := &fakeProcess{}
		h := newHelper(t, v, files, proc)

		meta := callMeta(t, h, api.ToolTerminal, api.TerminalInput{Command: "id"}, header)
		if meta.Code != api.CodeUnauthenticated && meta.Code != api.CodeForbidden {
			t.Fatalf("forged header %q: got code %q, want UNAUTHENTICATED/FORBIDDEN", header, meta.Code)
		}
		if proc.called() != 0 {
			t.Fatalf("forged header %q reached the process executor (%d calls)", header, proc.called())
		}
		h.Close()
	}
}

// TestComputerUseRejectedBeforeAuth proves computer_use is rejected with
// FORBIDDEN before any authentication happens: it is refused even with NO token
// (which would otherwise yield UNAUTHENTICATED) and even with a VALID admin
// bearer, and never reaches an executor.
func TestComputerUseRejectedBeforeAuth(t *testing.T) {
	adminBearer, v := adminCreds(t)

	for _, header := range []string{"", bearerHeader(adminBearer)} {
		files := &fakeFiles{}
		proc := &fakeProcess{}
		h := newHelper(t, v, files, proc)

		meta := callMeta(t, h, api.ToolComputerUse, api.ComputerUseInput{Action: api.ActionCapture}, header)
		if meta.Code != api.CodeForbidden {
			t.Fatalf("computer_use (header=%q): got code %q, want FORBIDDEN", header, meta.Code)
		}
		if files.called() != 0 || proc.called() != 0 {
			t.Fatalf("computer_use reached an executor (files=%d proc=%d)", files.called(), proc.called())
		}
		h.Close()
	}
}

// TestUnknownToolRejected proves an unknown tool name is rejected before
// dispatch and never reaches an executor, even with a valid admin bearer.
func TestUnknownToolRejected(t *testing.T) {
	adminBearer, v := adminCreds(t)
	files := &fakeFiles{}
	proc := &fakeProcess{}
	h := newHelper(t, v, files, proc)
	defer h.Close()

	res, err := h.Call(context.Background(), "no_such_tool", nil, bearerHeader(adminBearer))
	if err != nil {
		t.Fatalf("Call unknown tool returned Go error: %v", err)
	}
	meta := metaOf(t, res)
	if meta.Code != api.CodeInvalidArgument {
		t.Fatalf("unknown tool: got code %q, want INVALID_ARGUMENT", meta.Code)
	}
	if files.called() != 0 || proc.called() != 0 {
		t.Fatalf("unknown tool reached an executor (files=%d proc=%d)", files.called(), proc.called())
	}
}

// TestEveryOSToolRequiresAdminBearer sweeps all six OS tools with an invalid
// admin token and asserts none reaches an executor.
func TestEveryOSToolRequiresAdminBearer(t *testing.T) {
	wrongBearer, _, err := auth.Generate(auth.ClassAdmin)
	if err != nil {
		t.Fatalf("generate admin bearer: %v", err)
	}
	v := mismatchedVerifier(t)

	cases := []struct {
		tool string
		args any
	}{
		{api.ToolTerminal, api.TerminalInput{Command: "id"}},
		{api.ToolProcess, api.ProcessInput{Action: api.ProcessStart, Command: "sleep"}},
		{api.ToolReadFile, api.ReadFileInput{Path: "/x"}},
		{api.ToolSearchFiles, api.SearchFilesInput{Path: "/x", Query: "q"}},
		{api.ToolWriteFile, api.WriteFileInput{Path: "/x", Content: "c"}},
		{api.ToolPatch, api.PatchInput{Files: []api.PatchFile{{Path: "/x"}}}},
	}
	for _, tc := range cases {
		files := &fakeFiles{}
		proc := &fakeProcess{}
		h := newHelper(t, v, files, proc)

		meta := callMeta(t, h, tc.tool, tc.args, bearerHeader(wrongBearer))
		if meta.Code != api.CodeUnauthenticated {
			t.Fatalf("%s with invalid admin token: got code %q, want UNAUTHENTICATED", tc.tool, meta.Code)
		}
		if files.called() != 0 || proc.called() != 0 {
			t.Fatalf("%s reached an executor without a valid admin bearer (files=%d proc=%d)", tc.tool, files.called(), proc.called())
		}
		h.Close()
	}
}

// TestValidAdminReachesExecutor proves a valid admin bearer with an OS tool
// reaches the executor and stamps the exact root identity.
func TestValidAdminReachesExecutor(t *testing.T) {
	adminBearer, v := adminCreds(t)
	files := &fakeFiles{}
	proc := &fakeProcess{}
	h := newHelper(t, v, files, proc)
	defer h.Close()

	// terminal -> process executor
	tr := call[api.TerminalOutput](t, h, api.ToolTerminal, api.TerminalInput{Command: "id"}, bearerHeader(adminBearer))
	if tr.Code != "" {
		t.Fatalf("terminal with valid admin: got code %q, want success", tr.Code)
	}
	if proc.called() == 0 {
		t.Fatalf("terminal never reached the process executor")
	}
	if tr.Identity == nil {
		t.Fatalf("terminal success carried no identity")
	}
	if got := identityDescriptor(*tr.Identity); got != "uid=0,user=root,credential=admin" {
		t.Fatalf("terminal identity = %q, want uid=0,user=root,credential=admin", got)
	}

	// read_file -> file executor
	rf := call[api.ReadFileOutput](t, h, api.ToolReadFile, api.ReadFileInput{Path: "/x"}, bearerHeader(adminBearer))
	if rf.Code != "" {
		t.Fatalf("read_file with valid admin: got code %q, want success", rf.Code)
	}
	if files.called() == 0 {
		t.Fatalf("read_file never reached the file executor")
	}
	if rf.Identity == nil || identityDescriptor(*rf.Identity) != "uid=0,user=root,credential=admin" {
		t.Fatalf("read_file identity = %+v, want uid=0,user=root,credential=admin", rf.Identity)
	}
}

// TestPauseCancelsRootCommandAndProcessGroup proves a pause cancels an active
// root command (reported PAUSED) and terminates a tracked process group.
func TestPauseCancelsRootCommandAndProcessGroup(t *testing.T) {
	adminBearer, v := adminCreds(t)
	proc := &fakeProcess{terminalGate: make(chan struct{}), terminalStarted: make(chan struct{}, 1)}
	h := newHelper(t, v, &fakeFiles{}, proc)
	defer h.Close()

	ctx := context.Background()
	header := bearerHeader(adminBearer)

	// Start a long-running tracked process the helper will remember.
	ps := call[api.ProcessOutput](t, h, api.ToolProcess, api.ProcessInput{Action: api.ProcessStart, Command: "sleep"}, header)
	if ps.ProcessID == "" {
		t.Fatalf("process start returned no id")
	}

	// Launch a terminal command that blocks until cancelled.
	termOut := make(chan api.TerminalOutput, 1)
	go func() {
		termOut <- call[api.TerminalOutput](t, h, api.ToolTerminal, api.TerminalInput{Command: "sleep 100"}, header)
	}()
	select {
	case <-proc.terminalStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal never started")
	}

	// Pause: cancels the active command, terminates the tracked group.
	if err := h.Pause(ctx); err != nil {
		t.Fatalf("pause: %v", err)
	}
	select {
	case out := <-termOut:
		if out.Code != api.CodePaused {
			t.Fatalf("cancelled root command: got code %q, want PAUSED", out.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("root command was not cancelled by pause")
	}
	if got := proc.terminatedIDs(); len(got) != 1 || got[0] != ps.ProcessID {
		t.Fatalf("pause terminated %v, want [%s]", got, ps.ProcessID)
	}

	// New calls during pause are rejected with PAUSED.
	rf := call[api.ReadFileOutput](t, h, api.ToolReadFile, api.ReadFileInput{Path: "/x"}, header)
	if rf.Code != api.CodePaused {
		t.Fatalf("read during pause: got %q, want PAUSED", rf.Code)
	}

	// Resume restores service.
	if err := h.Resume(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	rf = call[api.ReadFileOutput](t, h, api.ToolReadFile, api.ReadFileInput{Path: "/x"}, header)
	if rf.Code != "" {
		t.Fatalf("read after resume: got %q, want success", rf.Code)
	}
}

// TestNewRejectsMissingDependencies asserts the constructor refuses to build a
// helper without a verifier, file service, or process manager.
func TestNewRejectsMissingDependencies(t *testing.T) {
	_, v := adminCreds(t)
	if _, err := New(Config{Files: &fakeFiles{}, Process: &fakeProcess{}}); err == nil {
		t.Fatalf("New without verifier: want error")
	}
	if _, err := New(Config{Verifier: v, Process: &fakeProcess{}}); err == nil {
		t.Fatalf("New without files: want error")
	}
	if _, err := New(Config{Verifier: v, Files: &fakeFiles{}}); err == nil {
		t.Fatalf("New without process: want error")
	}
}

// ---------------------------------------------------------------------------
// test plumbing
// ---------------------------------------------------------------------------

func callMeta(t *testing.T, h *Helper, tool string, args any, header string) api.ResultMeta {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := h.Call(context.Background(), tool, raw, header)
	if err != nil {
		t.Fatalf("Call %s: %v", tool, err)
	}
	return metaOf(t, res)
}

func call[T any](t *testing.T, h *Helper, tool string, args any, header string) T {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := h.Call(context.Background(), tool, raw, header)
	if err != nil {
		t.Fatalf("Call %s: %v", tool, err)
	}
	var out T
	decodeStructured(t, res, &out)
	return out
}

func metaOf(t *testing.T, res *mcp.CallToolResult) api.ResultMeta {
	t.Helper()
	var meta api.ResultMeta
	decodeStructured(t, res, &meta)
	return meta
}

func decodeStructured(t *testing.T, res *mcp.CallToolResult, out any) {
	t.Helper()
	if res == nil {
		t.Fatal("nil call result")
	}
	raw, ok := res.StructuredContent.(json.RawMessage)
	if !ok {
		t.Fatalf("structured content is %T, want json.RawMessage", res.StructuredContent)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
}
