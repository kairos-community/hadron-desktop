package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/api"
	"github.com/mudler/hadron-desktop/agent/internal/control"
)

// fixture bundles a gateway with its two fake brokers and credentials.
type fixture struct {
	g          *Gateway
	session    *fakeBroker
	root       *fakeBroker
	controller *control.Controller
	creds      testCreds
	logBuf     *bytes.Buffer
}

// newFixture builds a gateway wired to real in-process rpc servers backed by
// fake brokers. opts lets a test tweak the config before New.
func newFixture(t *testing.T, opts ...func(*Config, *fixture)) *fixture {
	t.Helper()
	creds := newTestCreds(t)
	session := &fakeBroker{healthy: true}
	root := &fakeBroker{healthy: true}

	sessionClient := startBroker(t, session, false) // session socket: no auth
	rootClient := startBroker(t, root, true)        // root socket: RequireAuth

	ctrl := control.New(control.Config{
		Version:        "v-test",
		SessionHealthy: func(context.Context) bool { return true },
		CuaHealthy:     func(context.Context) bool { return true },
	})

	var logBuf bytes.Buffer
	cfg := Config{
		Version:          "v-test",
		ListenAddr:       "127.0.0.1:0",
		InsecureLoopback: true,
		Session:          sessionClient,
		Root:             rootClient,
		Verifier:         creds.verifier,
		Controller:       ctrl,
		Logger:           slog.New(slog.NewJSONHandler(&logBuf, nil)),
	}
	f := &fixture{session: session, root: root, controller: ctrl, creds: creds, logBuf: &logBuf}
	for _, o := range opts {
		o(&cfg, f)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	f.g = g
	return f
}

// ---------------------------------------------------------------------------
// Tool contract
// ---------------------------------------------------------------------------

func TestExactEightToolsRegistered(t *testing.T) {
	f := newFixture(t)
	session := connectMCP(t, f.g, f.creds.userBearer)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)
	want := api.ToolNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("registered tools = %v, want %v", got, want)
	}
}

// TestListedToolsAdvertiseEnums checks the schema a real client receives from
// tools/list, not just the one api builds: the SDK serves its own inferred
// schema unless a tool supplies InputSchema, and inference cannot see Go's
// named string types as closed sets. Without this the eleven computer_use
// actions reach clients as a bare {"type":"string"} and can only be discovered
// by trial and error.
func TestListedToolsAdvertiseEnums(t *testing.T) {
	f := newFixture(t)
	session := connectMCP(t, f.g, f.creds.userBearer)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	// tool -> property -> first expected value, enough to prove the enum
	// survived the trip through the SDK's schema handling.
	want := map[string]map[string][]string{
		api.ToolComputerUse: {
			"action": {"capture", "accessibility", "click", "double_click", "drag",
				"scroll", "type", "key", "wait", "list_applications", "focus_application"},
			"scope":  {"screen", "window"},
			"button": {"left", "right", "middle"},
		},
		api.ToolProcess:     {"action": {"start", "poll", "write", "terminate"}},
		api.ToolSearchFiles: {"mode": {"name", "content"}},
	}

	for _, tool := range res.Tools {
		expected, ok := want[tool.Name]
		if !ok {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("%s: marshal input schema: %v", tool.Name, err)
		}
		var doc struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: unmarshal input schema: %v", tool.Name, err)
		}
		for property, values := range expected {
			got := doc.Properties[property].Enum
			if strings.Join(got, ",") != strings.Join(values, ",") {
				t.Errorf("%s.%s enum = %v, want %v", tool.Name, property, got, values)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Bearer 401 / 403
// ---------------------------------------------------------------------------

func TestNoBearerRejected401(t *testing.T) {
	f := newFixture(t)
	ts := httptest.NewServer(f.g.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no bearer status = %d, want 401", resp.StatusCode)
	}
}

func TestInvalidBearerRejected401(t *testing.T) {
	f := newFixture(t)
	ts := httptest.NewServer(f.g.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hdn_u_not-a-real-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid bearer status = %d, want 401", resp.StatusCode)
	}
}

func TestInsufficientScopeRejected403(t *testing.T) {
	// Require the admin scope on /mcp; a user bearer carries only the user
	// scope and must be refused with 403 by the SDK bearer middleware.
	f := newFixture(t, func(cfg *Config, _ *fixture) {
		cfg.RequiredScopes = []string{"hadron:admin"}
	})
	ts := httptest.NewServer(f.g.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.creds.userBearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user bearer against admin scope = %d, want 403", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

func TestUserRoutesAllToSession(t *testing.T) {
	f := newFixture(t)
	session := connectMCP(t, f.g, f.creds.userBearer)

	for _, tool := range api.ToolNames() {
		callTool(t, session, tool, validArgs(tool))
	}

	if got := len(f.session.recorded()); got != len(api.ToolNames()) {
		t.Fatalf("session received %d calls, want %d", got, len(api.ToolNames()))
	}
	if got := len(f.root.recorded()); got != 0 {
		t.Fatalf("root received %d calls, want 0 for user bearer", got)
	}
}

func TestAdminRoutesSeatToolsToSessionAndSixToRoot(t *testing.T) {
	f := newFixture(t)
	session := connectMCP(t, f.g, f.creds.adminBearer)

	for _, tool := range api.ToolNames() {
		callTool(t, session, tool, validArgs(tool))
	}

	// computer_use and browser both drive the agent's own desktop seat, so an
	// admin bearer must NOT get them root-owned: they stay on the session
	// broker for every class. The other six OS tools go to the root helper.
	seatTools := map[string]bool{api.ToolComputerUse: true, api.ToolBrowser: true}

	sessionCalls := f.session.recorded()
	if len(sessionCalls) != len(seatTools) {
		t.Fatalf("session calls = %+v, want exactly %v", sessionCalls, seatTools)
	}
	for _, c := range sessionCalls {
		if !seatTools[c.tool] {
			t.Fatalf("unexpected session call %q, want only %v", c.tool, seatTools)
		}
	}

	rootCalls := f.root.recorded()
	if len(rootCalls) != 6 {
		t.Fatalf("root received %d calls, want 6", len(rootCalls))
	}
	// Every root call must carry the forwarded admin bearer verbatim.
	wantHeader := "Bearer " + f.creds.adminBearer
	for _, c := range rootCalls {
		if seatTools[c.tool] {
			t.Fatalf("%s must not reach root: the browser and desktop always run as the unprivileged agent", c.tool)
		}
		if c.authHeader != wantHeader {
			t.Fatalf("root call %s authHeader = %q, want forwarded admin bearer", c.tool, c.authHeader)
		}
	}
}

func TestSessionCallsCarryNoForwardedHeader(t *testing.T) {
	f := newFixture(t)
	session := connectMCP(t, f.g, f.creds.userBearer)
	callTool(t, session, api.ToolTerminal, validArgs(api.ToolTerminal))

	calls := f.session.recorded()
	if len(calls) != 1 {
		t.Fatalf("session calls = %d, want 1", len(calls))
	}
	if calls[0].authHeader != "" {
		t.Fatalf("session call forwarded a header %q, want none", calls[0].authHeader)
	}
}

// ---------------------------------------------------------------------------
// Health / readiness redaction and transitions
// ---------------------------------------------------------------------------

func TestHealthzRedacted(t *testing.T) {
	f := newFixture(t)
	ts := httptest.NewServer(f.g.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal healthz: %v (%s)", err, body)
	}
	allowed := map[string]bool{"version": true, "gateway": true, "session": true, "cua": true, "paused": true}
	for k := range m {
		if !allowed[k] {
			t.Fatalf("healthz leaked field %q: %s", k, body)
		}
	}
	if len(m) != len(allowed) {
		t.Fatalf("healthz field count = %d, want %d: %s", len(m), len(allowed), body)
	}
	// No secrets/paths/users anywhere in the payload.
	for _, bad := range []string{"/run/", "root", "uid", "token", "bearer", "hdn_"} {
		if bytes.Contains(bytes.ToLower(body), []byte(bad)) {
			t.Fatalf("healthz body contains %q: %s", bad, body)
		}
	}
}

func TestReadyzTransitions(t *testing.T) {
	cuaUp := true
	f := newFixture(t, func(cfg *Config, fx *fixture) {
		fx.controller = control.New(control.Config{
			Version:        "v-test",
			SessionHealthy: func(context.Context) bool { return true },
			CuaHealthy:     func(context.Context) bool { return cuaUp },
		})
		cfg.Controller = fx.controller
	})
	ts := httptest.NewServer(f.g.Handler())
	t.Cleanup(ts.Close)

	readyz := func() int {
		resp, err := http.Get(ts.URL + "/readyz")
		if err != nil {
			t.Fatalf("get readyz: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := readyz(); code != http.StatusOK {
		t.Fatalf("ready initially = %d, want 200", code)
	}

	cuaUp = false
	if code := readyz(); code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready (cua down) = %d, want 503", code)
	}
	cuaUp = true

	if err := f.controller.Pause(context.Background()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if code := readyz(); code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready (paused) = %d, want 503", code)
	}

	if err := f.controller.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if code := readyz(); code != http.StatusOK {
		t.Fatalf("ready after resume = %d, want 200", code)
	}
}

// ---------------------------------------------------------------------------
// Origin rejection
// ---------------------------------------------------------------------------

func TestCrossOriginRejected(t *testing.T) {
	f := newFixture(t)
	ts := httptest.NewServer(f.g.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.creds.userBearer)
	// A cross-origin browser request carries an Origin that mismatches Host.
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Pause: all calls PAUSED, brokers told
// ---------------------------------------------------------------------------

func TestPausedRejectsBothClasses(t *testing.T) {
	f := newFixture(t)
	if err := f.controller.Pause(context.Background()); err != nil {
		t.Fatalf("pause: %v", err)
	}

	userSession := connectMCP(t, f.g, f.creds.userBearer)
	res := callTool(t, userSession, api.ToolTerminal, validArgs(api.ToolTerminal))
	if code := structuredCode(t, res); code != string(api.CodePaused) {
		t.Fatalf("user paused code = %q, want PAUSED", code)
	}

	adminSession := connectMCP(t, f.g, f.creds.adminBearer)
	res = callTool(t, adminSession, api.ToolTerminal, validArgs(api.ToolTerminal))
	if code := structuredCode(t, res); code != string(api.CodePaused) {
		t.Fatalf("admin paused code = %q, want PAUSED", code)
	}

	// A paused gateway must never have dispatched to a broker.
	if n := len(f.session.recorded()) + len(f.root.recorded()); n != 0 {
		t.Fatalf("brokers received %d calls while paused, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// Cancellation: pause cancels an in-flight generation
// ---------------------------------------------------------------------------

func TestPauseCancelsInFlightCall(t *testing.T) {
	entered := make(chan struct{}, 1)
	hold := make(chan struct{})
	f := newFixture(t, func(cfg *Config, fx *fixture) {
		fx.session.entered = entered
		fx.session.hold = hold
	})
	session := connectMCP(t, f.g, f.creds.userBearer)

	done := make(chan string, 1)
	go func() {
		res := callTool(t, session, api.ToolTerminal, validArgs(api.ToolTerminal))
		done <- structuredCode(t, res)
	}()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("call never reached broker")
	}

	// Pause while the call is blocked in the broker: it must be cancelled.
	if err := f.controller.Pause(context.Background()); err != nil {
		t.Fatalf("pause: %v", err)
	}

	select {
	case code := <-done:
		if code != string(api.CodePaused) {
			t.Fatalf("cancelled call code = %q, want PAUSED", code)
		}
	case <-time.After(3 * time.Second):
		close(hold)
		t.Fatal("in-flight call not cancelled by pause")
	}
	close(hold)
}

// ---------------------------------------------------------------------------
// Concurrency limit
// ---------------------------------------------------------------------------

func TestConcurrencyLimitShedsExcess(t *testing.T) {
	const limit = 4
	entered := make(chan struct{}, limit)
	hold := make(chan struct{})
	f := newFixture(t, func(cfg *Config, fx *fixture) {
		cfg.MaxConcurrentCalls = limit
		fx.session.entered = entered
		fx.session.hold = hold
	})

	var wg sync.WaitGroup
	for range limit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := connectMCP(t, f.g, f.creds.userBearer)
			callTool(t, s, api.ToolTerminal, validArgs(api.ToolTerminal))
		}()
	}
	// Wait until all `limit` calls are in flight, holding the semaphore.
	for range limit {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(hold)
			t.Fatal("not all calls reached broker")
		}
	}

	// The next call cannot acquire a slot and is shed with RESOURCE_EXHAUSTED.
	extra := connectMCP(t, f.g, f.creds.userBearer)
	res := callTool(t, extra, api.ToolTerminal, validArgs(api.ToolTerminal))
	if code := structuredCode(t, res); code != string(api.CodeResourceExhausted) {
		t.Fatalf("excess call code = %q, want RESOURCE_EXHAUSTED", code)
	}

	close(hold)
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Response-size cap
// ---------------------------------------------------------------------------

func TestResponseSizeCapEnforced(t *testing.T) {
	f := newFixture(t, func(cfg *Config, fx *fixture) {
		cfg.MaxResponseBytes = 4096
		fx.session.payload = strings.Repeat("x", 8192) // exceeds the cap
	})
	session := connectMCP(t, f.g, f.creds.userBearer)
	res := callTool(t, session, api.ToolTerminal, validArgs(api.ToolTerminal))
	if code := structuredCode(t, res); code != string(api.CodeResourceExhausted) {
		t.Fatalf("oversized response code = %q, want RESOURCE_EXHAUSTED", code)
	}
}

// ---------------------------------------------------------------------------
// Audit redaction
// ---------------------------------------------------------------------------

func TestAuditOmitsSecrets(t *testing.T) {
	f := newFixture(t)
	session := connectMCP(t, f.g, f.creds.userBearer)

	const secretCmd = "SUPERSECRETCOMMAND_rm_rf"
	const secretText = "TOPSECRETTYPETEXT"
	const secretPath = "/etc/SECRETFILE_path"

	callTool(t, session, api.ToolTerminal, api.TerminalInput{Command: secretCmd})
	callTool(t, session, api.ToolComputerUse, api.ComputerUseInput{Action: api.ActionType, Text: secretText})
	callTool(t, session, api.ToolReadFile, api.ReadFileInput{Path: secretPath})

	logs := f.logBuf.String()
	for _, secret := range []string{secretCmd, secretText, secretPath, f.creds.userBearer, f.creds.adminBearer, "hdn_u_", "hdn_a_"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("audit log leaked secret %q:\n%s", secret, logs)
		}
	}
	// Sanity: the audit did run and named the tools.
	if !strings.Contains(logs, "terminal") || !strings.Contains(logs, "mcp call") {
		t.Fatalf("audit log missing expected non-secret fields:\n%s", logs)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// validArgs returns minimal schema-valid arguments for tool so the SDK's input
// validation admits the call and it reaches routing.
func validArgs(tool string) any {
	switch tool {
	case api.ToolComputerUse:
		return api.ComputerUseInput{Action: api.ActionCapture}
	case api.ToolTerminal:
		return api.TerminalInput{Command: "true"}
	case api.ToolProcess:
		return api.ProcessInput{Action: api.ProcessStart, Command: "true"}
	case api.ToolReadFile:
		return api.ReadFileInput{Path: "/tmp/x"}
	case api.ToolSearchFiles:
		return api.SearchFilesInput{Path: "/tmp", Query: "x"}
	case api.ToolWriteFile:
		return api.WriteFileInput{Path: "/tmp/x", Content: "y"}
	case api.ToolBrowser:
		return api.BrowserInput{Action: api.BrowserSnapshot}
	case api.ToolPatch:
		return api.PatchInput{Files: []api.PatchFile{{Path: "/tmp/x", Replacements: []api.PatchReplacement{{Old: "a", New: "b"}}}}}
	default:
		return map[string]any{}
	}
}
