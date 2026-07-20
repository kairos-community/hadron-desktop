package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mudler/hadron-desktop/agent/internal/api"
	"github.com/mudler/hadron-desktop/agent/internal/cua"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeFiles is a hermetic FileService. Read optionally blocks on gate so a test
// can hold OS calls in flight and observe the broker's concurrency bound. It
// tracks the maximum number of Reads ever concurrently in flight and signals
// entered each time a Read begins.
type fakeFiles struct {
	mu        sync.Mutex
	active    int
	maxActive int

	gate    chan struct{} // if non-nil, Read blocks on it (or ctx)
	entered chan struct{} // if non-nil, one send per Read start
}

func (f *fakeFiles) enter() {
	f.mu.Lock()
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()
}

func (f *fakeFiles) leave() {
	f.mu.Lock()
	f.active--
	f.mu.Unlock()
}

func (f *fakeFiles) maxSeen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func (f *fakeFiles) Read(ctx context.Context, _ api.ReadFileInput) api.ReadFileOutput {
	f.enter()
	defer f.leave()
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
	return api.SearchFilesOutput{}
}

func (f *fakeFiles) Write(_ context.Context, _ api.WriteFileInput) api.WriteFileOutput {
	return api.WriteFileOutput{BytesWritten: 1, Created: true}
}

func (f *fakeFiles) Patch(_ context.Context, _ api.PatchInput) api.PatchOutput {
	return api.PatchOutput{ReplacementsApplied: 1}
}

// fakeProcess is a hermetic ProcessManager. Terminal optionally blocks on gate
// so a test can pause an in-flight call. Process tracks started/terminated ids.
type fakeProcess struct {
	mu         sync.Mutex
	nextID     int
	terminated []string
	closed     bool

	terminalGate    chan struct{} // if non-nil, Terminal blocks on it (or ctx)
	terminalStarted chan struct{} // if non-nil, one send per Terminal start
}

func (p *fakeProcess) Terminal(ctx context.Context, _ api.TerminalInput) (api.TerminalOutput, error) {
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

// fakeComputer is a hermetic Computer whose readiness a test flips directly.
type fakeComputer struct {
	mu     sync.Mutex
	ready  bool
	stops  int
	closes int
	uses   int
}

func (c *fakeComputer) ComputerUse(_ context.Context, _ api.ComputerUseInput) (api.ComputerUseOutput, error) {
	c.mu.Lock()
	c.uses++
	ready := c.ready
	c.mu.Unlock()
	if !ready {
		return api.ComputerUseOutput{ResultMeta: api.ResultMeta{Code: api.CodeSessionUnavailable, Message: "cua down", Retryable: true}}, nil
	}
	return api.ComputerUseOutput{ImageBase64: "img"}, nil
}

func (c *fakeComputer) Browser(_ context.Context, _ api.BrowserInput) (api.BrowserOutput, error) {
	c.mu.Lock()
	c.uses++
	ready := c.ready
	c.mu.Unlock()
	if !ready {
		return api.BrowserOutput{ResultMeta: api.ResultMeta{Code: api.CodeSessionUnavailable, Message: "cua down", Retryable: true}}, nil
	}
	return api.BrowserOutput{URL: "https://example.test/", Title: "fake page"}, nil
}

func (c *fakeComputer) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

func (c *fakeComputer) setReady(v bool) {
	c.mu.Lock()
	c.ready = v
	c.mu.Unlock()
}

func (c *fakeComputer) Stop() {
	c.mu.Lock()
	c.stops++
	c.mu.Unlock()
}

func (c *fakeComputer) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	return nil
}

func (c *fakeComputer) stopCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stops
}

// fakeIdentity returns a fixed identity.
type fakeIdentity struct{ id api.Identity }

func (f fakeIdentity) Identity() api.Identity { return f.id }

// seatComputer adapts a real *cua.Adapter to the Computer interface so a test
// can exercise the adapter's single seat through the broker.
type seatComputer struct{ a *cua.Adapter }

func (s seatComputer) ComputerUse(ctx context.Context, in api.ComputerUseInput) (api.ComputerUseOutput, error) {
	return s.a.ComputerUse(ctx, in)
}
func (s seatComputer) Browser(ctx context.Context, in api.BrowserInput) (api.BrowserOutput, error) {
	return s.a.Browser(ctx, in)
}
func (s seatComputer) Ready() bool  { return true }
func (s seatComputer) Stop()        {}
func (s seatComputer) Close() error { return nil }

// fakeCaller implements cua.Caller and tracks maximum concurrent Call depth.
type fakeCaller struct {
	mu        sync.Mutex
	active    int
	maxActive int
	delay     time.Duration
}

func (f *fakeCaller) Call(_ context.Context, _ string, _ any) (*mcp.CallToolResult, error) {
	f.mu.Lock()
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()

	time.Sleep(f.delay)

	f.mu.Lock()
	f.active--
	f.mu.Unlock()
	return &mcp.CallToolResult{}, nil
}

func (f *fakeCaller) maxSeen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

// newBroker builds a broker with the given deps and a fixed test identity.
func newTestBroker(t *testing.T, files FileService, proc ProcessManager, comp Computer) *Broker {
	t.Helper()
	return New(Config{
		Files:    files,
		Process:  proc,
		Computer: comp,
		Identity: fakeIdentity{id: api.Identity{User: "tester", UID: 4242}},
	})
}

func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestDegradationShellStaysLive verifies computer_use degrades to
// SESSION_UNAVAILABLE while Cua is down, that shell/file calls keep working
// concurrently, and that computer_use recovers once Cua becomes ready.
func TestDegradationShellStaysLive(t *testing.T) {
	files := &fakeFiles{}
	proc := &fakeProcess{}
	comp := &fakeComputer{ready: false}
	b := newTestBroker(t, files, proc, comp)
	defer b.Close()

	ctx := context.Background()

	// computer_use is down.
	cu, _ := b.ComputerUse(ctx, api.ComputerUseInput{Action: api.ActionCapture})
	if cu.Code != api.CodeSessionUnavailable {
		t.Fatalf("computer_use while down: got code %q, want SESSION_UNAVAILABLE", cu.Code)
	}

	// shell/file calls remain live concurrently while Cua is down.
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, _ := b.ReadFile(ctx, api.ReadFileInput{Path: "/x"}); r.Code != "" {
				errs <- fmt.Sprintf("read_file code %q", r.Code)
			}
			if tr, _ := b.Terminal(ctx, api.TerminalInput{Command: "echo hi"}); tr.Code != "" {
				errs <- fmt.Sprintf("terminal code %q", tr.Code)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("shell/file call failed while Cua down: %s", e)
	}

	// Cua recovers.
	comp.setReady(true)
	cu, _ = b.ComputerUse(ctx, api.ComputerUseInput{Action: api.ActionCapture})
	if cu.Code != "" {
		t.Fatalf("computer_use after recovery: got code %q, want success", cu.Code)
	}
	if cu.ImageBase64 == "" {
		t.Fatalf("computer_use after recovery: empty image")
	}
}

// TestOSConcurrencyBoundedToEight asserts at most 8 OS calls run at once and a
// 9th queues behind them.
func TestOSConcurrencyBoundedToEight(t *testing.T) {
	files := &fakeFiles{gate: make(chan struct{}), entered: make(chan struct{}, 9)}
	b := newTestBroker(t, files, &fakeProcess{}, &fakeComputer{})
	defer b.Close()

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 9; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.ReadFile(ctx, api.ReadFileInput{Path: "/x"})
		}()
	}

	// Exactly 8 Reads should enter; the 9th must wait on the semaphore.
	for i := 0; i < 8; i++ {
		select {
		case <-files.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 8 reads entered", i)
		}
	}
	select {
	case <-files.entered:
		t.Fatal("a 9th read entered while 8 were still in flight; semaphore not bounded to 8")
	case <-time.After(150 * time.Millisecond):
	}

	// Release the gate; the 9th proceeds, all finish, max concurrency is 8.
	close(files.gate)
	wg.Wait()
	if got := files.maxSeen(); got != 8 {
		t.Fatalf("max concurrent OS calls = %d, want 8", got)
	}
}

// TestComputerUseSingleSeat drives the real cua.Adapter through the broker and
// asserts concurrent computer_use calls are serialized to one at a time by the
// adapter's seat (and that the broker does not add or double-count it).
func TestComputerUseSingleSeat(t *testing.T) {
	caller := &fakeCaller{delay: 2 * time.Millisecond}
	adapter := cua.NewAdapter(caller)
	b := newTestBroker(t, &fakeFiles{}, &fakeProcess{}, seatComputer{a: adapter})
	defer b.Close()

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.ComputerUse(ctx, api.ComputerUseInput{Action: api.ActionClick, X: ptr(1), Y: ptr(2)})
		}()
	}
	wg.Wait()
	if got := caller.maxSeen(); got != 1 {
		t.Fatalf("max concurrent Cua calls = %d, want 1 (single seat)", got)
	}
}

// TestComputerUseBypassesOSSemaphore verifies computer_use is not gated by the
// shell/file semaphore: with the OS semaphore fully occupied by a blocked read,
// a computer_use call still proceeds.
func TestComputerUseBypassesOSSemaphore(t *testing.T) {
	files := &fakeFiles{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	b := New(Config{
		Files:         files,
		Process:       &fakeProcess{},
		Computer:      &fakeComputer{ready: true},
		Identity:      fakeIdentity{},
		OSConcurrency: 1,
	})
	defer b.Close()

	ctx := context.Background()
	go b.ReadFile(ctx, api.ReadFileInput{Path: "/x"}) // occupies the single OS slot
	select {
	case <-files.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking read never entered")
	}

	done := make(chan api.ComputerUseOutput, 1)
	go func() {
		out, _ := b.ComputerUse(ctx, api.ComputerUseInput{Action: api.ActionCapture})
		done <- out
	}()
	select {
	case out := <-done:
		if out.Code != "" {
			t.Fatalf("computer_use blocked-by-OS test: got code %q, want success", out.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("computer_use blocked behind a full OS semaphore; it must bypass it")
	}
	close(files.gate)
}

// TestPauseCancelsActiveRejectsNewThenResume covers the full pause lifecycle:
// an in-flight terminal is cancelled and reported PAUSED, a tracked process
// group is terminated, new calls during pause are rejected with PAUSED, and
// resume restores service and restarts Cua.
func TestPauseCancelsActiveRejectsNewThenResume(t *testing.T) {
	proc := &fakeProcess{terminalGate: make(chan struct{}), terminalStarted: make(chan struct{}, 1)}
	comp := &fakeComputer{ready: true}
	b := newTestBroker(t, &fakeFiles{}, proc, comp)
	defer b.Close()

	ctx := context.Background()

	// Start a long-running tracked process the broker will remember.
	ps, _ := b.Process(ctx, api.ProcessInput{Action: api.ProcessStart, Command: "sleep"})
	if ps.ProcessID == "" {
		t.Fatalf("process start returned no id")
	}

	// Launch a terminal call that blocks until cancelled.
	termOut := make(chan api.TerminalOutput, 1)
	go func() {
		out, _ := b.Terminal(ctx, api.TerminalInput{Command: "sleep 100"})
		termOut <- out
	}()
	select {
	case <-proc.terminalStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal never started")
	}

	// Pause: cancels the active terminal, terminates the tracked group.
	if err := b.Pause(ctx); err != nil {
		t.Fatalf("pause: %v", err)
	}

	select {
	case out := <-termOut:
		if out.Code != api.CodePaused {
			t.Fatalf("cancelled terminal: got code %q, want PAUSED", out.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal was not cancelled by pause")
	}

	if got := proc.terminatedIDs(); len(got) != 1 || got[0] != ps.ProcessID {
		t.Fatalf("pause terminated %v, want [%s]", got, ps.ProcessID)
	}

	// New calls during pause are rejected.
	if r, _ := b.ReadFile(ctx, api.ReadFileInput{Path: "/x"}); r.Code != api.CodePaused {
		t.Fatalf("read during pause: got %q, want PAUSED", r.Code)
	}
	if cu, _ := b.ComputerUse(ctx, api.ComputerUseInput{Action: api.ActionCapture}); cu.Code != api.CodePaused {
		t.Fatalf("computer_use during pause: got %q, want PAUSED", cu.Code)
	}

	// Resume restores service and restarts Cua.
	stopsBefore := comp.stopCount()
	if err := b.Resume(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if comp.stopCount() != stopsBefore+1 {
		t.Fatalf("resume did not restart Cua (stop calls %d -> %d)", stopsBefore, comp.stopCount())
	}
	if r, _ := b.ReadFile(ctx, api.ReadFileInput{Path: "/x"}); r.Code != "" {
		t.Fatalf("read after resume: got %q, want success", r.Code)
	}
	if cu, _ := b.ComputerUse(ctx, api.ComputerUseInput{Action: api.ActionCapture}); cu.Code != "" {
		t.Fatalf("computer_use after resume: got %q, want success", cu.Code)
	}
}

// TestPauseResumeIdempotent asserts repeated pause/resume calls are no-ops.
func TestPauseResumeIdempotent(t *testing.T) {
	proc := &fakeProcess{}
	b := newTestBroker(t, &fakeFiles{}, proc, &fakeComputer{ready: true})
	defer b.Close()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := b.Pause(ctx); err != nil {
			t.Fatalf("pause %d: %v", i, err)
		}
	}
	if r, _ := b.ReadFile(ctx, api.ReadFileInput{Path: "/x"}); r.Code != api.CodePaused {
		t.Fatalf("read while paused: got %q, want PAUSED", r.Code)
	}
	for i := 0; i < 3; i++ {
		if err := b.Resume(ctx); err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
	}
	if r, _ := b.ReadFile(ctx, api.ReadFileInput{Path: "/x"}); r.Code != "" {
		t.Fatalf("read after resume: got %q, want success", r.Code)
	}
}

// TestSuccessResultsCarryIdentity asserts served results carry the injected
// execution identity, and failures do not.
func TestSuccessResultsCarryIdentity(t *testing.T) {
	b := newTestBroker(t, &fakeFiles{}, &fakeProcess{}, &fakeComputer{ready: true})
	defer b.Close()
	ctx := context.Background()

	tr, _ := b.Terminal(ctx, api.TerminalInput{Command: "echo hi"})
	if tr.Identity == nil {
		t.Fatalf("terminal success carried no identity")
	}
	if tr.Identity.User != "tester" || tr.Identity.UID != 4242 {
		t.Fatalf("terminal identity = %+v, want {tester 4242}", *tr.Identity)
	}

	rf, _ := b.ReadFile(ctx, api.ReadFileInput{Path: "/x"})
	if rf.Identity == nil || rf.Identity.User != "tester" {
		t.Fatalf("read_file success carried wrong/no identity: %+v", rf.Identity)
	}

	cu, _ := b.ComputerUse(ctx, api.ComputerUseInput{Action: api.ActionCapture})
	if cu.Identity == nil || cu.Identity.UID != 4242 {
		t.Fatalf("computer_use success carried wrong/no identity: %+v", cu.Identity)
	}

	// A degraded computer_use carries no identity.
	down := newTestBroker(t, &fakeFiles{}, &fakeProcess{}, &fakeComputer{ready: false})
	defer down.Close()
	cd, _ := down.ComputerUse(ctx, api.ComputerUseInput{Action: api.ActionCapture})
	if cd.Identity != nil {
		t.Fatalf("failed computer_use carried an identity: %+v", cd.Identity)
	}
}

// TestCallDispatch exercises the raw-JSON Call surface for a known and an
// unknown tool.
func TestCallDispatch(t *testing.T) {
	b := newTestBroker(t, &fakeFiles{}, &fakeProcess{}, &fakeComputer{ready: true})
	defer b.Close()
	ctx := context.Background()

	args, _ := json.Marshal(api.ReadFileInput{Path: "/x"})
	res, err := b.Call(ctx, api.ToolReadFile, args)
	if err != nil {
		t.Fatalf("Call read_file: %v", err)
	}
	var out api.ReadFileOutput
	decodeStructured(t, res, &out)
	if out.Code != "" {
		t.Fatalf("Call read_file result code = %q, want success", out.Code)
	}
	if out.Identity == nil || out.Identity.User != "tester" {
		t.Fatalf("Call read_file lost identity: %+v", out.Identity)
	}

	// Unknown tool -> INVALID_ARGUMENT (in-band, not a Go error).
	res, err = b.Call(ctx, "no_such_tool", nil)
	if err != nil {
		t.Fatalf("Call unknown tool returned Go error: %v", err)
	}
	var meta api.ResultMeta
	decodeStructured(t, res, &meta)
	if meta.Code != api.CodeInvalidArgument {
		t.Fatalf("unknown tool code = %q, want INVALID_ARGUMENT", meta.Code)
	}
}

// TestCuaComputerEnvGating verifies the production Cua backend refuses to start
// (and never spawns) when a required session variable is absent.
func TestCuaComputerEnvGating(t *testing.T) {
	env := map[string]string{
		"DISPLAY":                  ":0",
		"XAUTHORITY":               "/home/u/.Xauthority",
		"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus",
	}
	for _, missing := range requiredCuaEnv {
		local := map[string]string{}
		for k, v := range env {
			local[k] = v
		}
		delete(local, missing)

		c := NewCuaComputer(CuaConfig{
			Binary: "definitely-not-a-real-binary-should-never-run",
			Getenv: func(k string) string { return local[k] },
		})
		out, err := c.ComputerUse(context.Background(), api.ComputerUseInput{Action: api.ActionCapture})
		if err != nil {
			t.Fatalf("missing %s: unexpected Go error: %v", missing, err)
		}
		if out.Code != api.CodeSessionUnavailable {
			t.Fatalf("missing %s: got code %q, want SESSION_UNAVAILABLE", missing, out.Code)
		}
		if c.Ready() {
			t.Fatalf("missing %s: backend reported ready", missing)
		}
		_ = c.Close()
	}
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
