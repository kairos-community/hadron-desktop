package main

// contract_test.go is the end-to-end MCP contract for hadron-agent. Unlike the
// gateway package's own tests (which stub the brokers with a fake rpc.Handler),
// this test wires the REAL stack together:
//
//   - the real *session.Broker and the real *roothelper.Helper, each behind the
//     real Unix-socket rpc.Server (the root socket with RequireAuth), reached by
//     the real rpc.Client;
//   - the real gateway (gateway.New) with the real auth.Verifier and the real
//     control.Controller;
//   - a real MCP client (mcp.StreamableClientTransport) whose bearer is injected
//     by an http.RoundTripper.
//
// Only the four leaf executors (files, process, Cua, identity) are fakes, so the
// test is hermetic: no cua-driver, no process spawning, no root. It asserts the
// public contract: exactly eight tools; every tool callable; both credential
// classes; genuine root re-verification of the forwarded admin bearer; a pause
// cancels an active call as PAUSED; and health is fully redacted.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mudler/hadron-desktop/agent/internal/api"
	"github.com/mudler/hadron-desktop/agent/internal/auth"
	"github.com/mudler/hadron-desktop/agent/internal/control"
	"github.com/mudler/hadron-desktop/agent/internal/gateway"
	"github.com/mudler/hadron-desktop/agent/internal/roothelper"
	"github.com/mudler/hadron-desktop/agent/internal/rpc"
	"github.com/mudler/hadron-desktop/agent/internal/session"
	"github.com/mudler/hadron-desktop/agent/internal/testutil"
)

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

type testCreds struct {
	userBearer  string
	userDigest  auth.Digest
	adminBearer string
	adminDigest auth.Digest
}

func newCreds(t *testing.T) testCreds {
	t.Helper()
	userBearer, userDigest, err := auth.Generate(auth.ClassUser)
	if err != nil {
		t.Fatalf("generate user bearer: %v", err)
	}
	adminBearer, adminDigest, err := auth.Generate(auth.ClassAdmin)
	if err != nil {
		t.Fatalf("generate admin bearer: %v", err)
	}
	return testCreds{
		userBearer:  userBearer,
		userDigest:  userDigest,
		adminBearer: adminBearer,
		adminDigest: adminDigest,
	}
}

// ---------------------------------------------------------------------------
// Real rpc server over a Unix socket
// ---------------------------------------------------------------------------

// startRPCServer serves handler on a fresh Unix socket and returns an rpc.Client
// dialing it. Server, socket, and client are cleaned up with the test.
func startRPCServer(t *testing.T, handler rpc.Handler, requireAuth bool) *rpc.Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "b.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := rpc.NewServer(handler, rpc.Config{RequireAuth: requireAuth})
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	client := rpc.NewClient(sock)
	t.Cleanup(client.Close)
	return client
}

// ---------------------------------------------------------------------------
// MCP client with a bearer-injecting RoundTripper
// ---------------------------------------------------------------------------

type bearerRoundTripper struct {
	base   http.RoundTripper
	bearer string
}

func (rt bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+rt.bearer)
	}
	return rt.base.RoundTrip(req)
}

func connectMCP(t *testing.T, g *gateway.Gateway, bearer string) *mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(g.Handler())
	t.Cleanup(ts.Close)

	transport := &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerRoundTripper{base: http.DefaultTransport, bearer: bearer}},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "contract-client", Version: "v0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callTool(t *testing.T, s *mcp.ClientSession, tool string, args any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", tool, err)
	}
	return res
}

// resultMeta extracts the ResultMeta (code + identity) from a tool result's
// structured content.
func resultMeta(t *testing.T, res *mcp.CallToolResult) api.ResultMeta {
	t.Helper()
	if res.StructuredContent == nil {
		return api.ResultMeta{}
	}
	data, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var meta api.ResultMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal ResultMeta: %v", err)
	}
	return meta
}

// validArgs returns minimal schema-valid arguments for tool.
func validArgs(tool string) any {
	switch tool {
	case api.ToolComputerUse:
		return api.ComputerUseInput{Action: api.ActionCapture}
	case api.ToolBash:
		return api.BashInput{Command: "true"}
	case api.ToolReadFile:
		return api.ReadFileInput{Path: "/tmp/x"}
	case api.ToolSearchFiles:
		return api.SearchFilesInput{Path: "/tmp", Query: "x"}
	case api.ToolWriteFile:
		return api.WriteFileInput{Path: "/tmp/x", Content: "y"}
	case api.ToolPatch:
		return api.PatchInput{Files: []api.PatchFile{{Path: "/tmp/x", Replacements: []api.PatchReplacement{{Old: "a", New: "b"}}}}}
	default:
		return map[string]any{}
	}
}

// ---------------------------------------------------------------------------
// Real stack builder
// ---------------------------------------------------------------------------

type stack struct {
	g          *gateway.Gateway
	controller *control.Controller
	creds      testCreds
	logBuf     *bytes.Buffer
}

type stackConfig struct {
	// rootAdminDigest overrides the admin digest the root helper verifies
	// against. Defaults to the gateway's admin digest (a matching config); a
	// test sets it to a different digest to prove genuine re-verification.
	rootAdminDigest auth.Digest
	// sessionProc is the fake process executor behind the session broker; a test
	// may pre-configure Hold/Entered to hold a call "active" across a pause.
	sessionProc *testutil.FakeProcess
}

func buildStack(t *testing.T, tweak func(*stackConfig)) *stack {
	t.Helper()
	creds := newCreds(t)

	sc := stackConfig{
		rootAdminDigest: creds.adminDigest,
		sessionProc:     &testutil.FakeProcess{},
	}
	if tweak != nil {
		tweak(&sc)
	}

	// Session broker (unprivileged) behind the non-auth session socket.
	sessionBroker := session.New(session.Config{
		Files:    testutil.FakeFiles{},
		Process:  sc.sessionProc,
		Computer: &testutil.FakeComputer{},
		Identity: testutil.FakeIdentity{ID: api.Identity{User: "tester", UID: 1000}},
	})
	t.Cleanup(func() { _ = sessionBroker.Close() })
	sessionClient := startRPCServer(t, sessionHandler{broker: sessionBroker}, false)

	// Root helper (privileged) behind the auth-gated root socket. Its verifier
	// is INDEPENDENT of the gateway's, exactly as in production.
	rootVerifier, err := buildVerifier(unusableDigest, sc.rootAdminDigest)
	if err != nil {
		t.Fatalf("build root verifier: %v", err)
	}
	rootHelperSvc, err := roothelper.New(roothelper.Config{
		Verifier: rootVerifier,
		Files:    testutil.FakeFiles{},
		Process:  &testutil.FakeProcess{},
	})
	if err != nil {
		t.Fatalf("new root helper: %v", err)
	}
	t.Cleanup(func() { _ = rootHelperSvc.Close() })
	rootClient := startRPCServer(t, rootHandler{helper: rootHelperSvc}, true)

	// Gateway verifier accepts both classes.
	gwVerifier, err := buildVerifier(creds.userDigest, creds.adminDigest)
	if err != nil {
		t.Fatalf("build gateway verifier: %v", err)
	}

	ctrl := control.New(control.Config{
		Version:        "v-test",
		Brokers:        []control.Broker{lifecycleBroker{client: sessionClient}},
		SessionHealthy: func(context.Context) bool { return true },
		CuaHealthy:     func(context.Context) bool { return true },
	})

	var logBuf bytes.Buffer
	g, err := gateway.New(gateway.Config{
		Version:          "v-test",
		ListenAddr:       "127.0.0.1:0",
		InsecureLoopback: true,
		Session:          sessionClient,
		Root:             rootClient,
		Verifier:         gwVerifier,
		Controller:       ctrl,
		Logger:           slog.New(slog.NewJSONHandler(&logBuf, nil)),
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}

	return &stack{g: g, controller: ctrl, creds: creds, logBuf: &logBuf}
}

// ---------------------------------------------------------------------------
// Exactly eight tools
// ---------------------------------------------------------------------------

func TestContractExactlyEightTools(t *testing.T) {
	s := buildStack(t, nil)
	session := connectMCP(t, s.g, s.creds.userBearer)

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
		t.Fatalf("registered tools = %v, want exactly %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Every tool callable under both credential classes
// ---------------------------------------------------------------------------

func TestContractCallEachToolUserClass(t *testing.T) {
	s := buildStack(t, nil)
	session := connectMCP(t, s.g, s.creds.userBearer)

	for _, tool := range api.ToolNames() {
		res := callTool(t, session, tool, validArgs(tool))
		if code := resultMeta(t, res).Code; code != "" {
			t.Fatalf("user %s returned code %q, want success", tool, code)
		}
	}
}

func TestContractCallEachToolAdminClass(t *testing.T) {
	s := buildStack(t, nil)
	session := connectMCP(t, s.g, s.creds.adminBearer)

	for _, tool := range api.ToolNames() {
		res := callTool(t, session, tool, validArgs(tool))
		meta := resultMeta(t, res)
		if meta.Code != "" {
			t.Fatalf("admin %s returned code %q, want success", tool, meta.Code)
		}
		// The six OS tools execute in the root helper under the root identity;
		// computer_use and browser execute in the unprivileged session broker,
		// so an admin bearer never gets a root-owned desktop or browser.
		if tool == api.ToolComputerUse || tool == api.ToolBrowser {
			continue
		}
		if meta.Identity == nil || meta.Identity.User != "root" || meta.Identity.UID != 0 {
			t.Fatalf("admin %s identity = %+v, want root/uid0 (proves it ran in the root helper)", tool, meta.Identity)
		}
	}
}

// ---------------------------------------------------------------------------
// Root re-verification of the forwarded bearer
// ---------------------------------------------------------------------------

// TestContractRootRevalidationStampsRoot proves an admin OS tool reaches the
// root socket, the root helper re-verifies the forwarded admin bearer, and the
// served result carries the fixed root identity.
func TestContractRootRevalidationStampsRoot(t *testing.T) {
	s := buildStack(t, nil)
	session := connectMCP(t, s.g, s.creds.adminBearer)

	res := callTool(t, session, api.ToolReadFile, validArgs(api.ToolReadFile))
	meta := resultMeta(t, res)
	if meta.Code != "" {
		t.Fatalf("admin read_file code = %q, want success", meta.Code)
	}
	if meta.Identity == nil || meta.Identity.User != "root" {
		t.Fatalf("admin read_file identity = %+v, want root", meta.Identity)
	}
}

// TestContractRootRevalidationRejectsMismatch proves the root helper genuinely
// re-verifies, rather than trusting the gateway: when the helper's admin digest
// differs from the gateway's, an admin OS call that the GATEWAY admits is still
// rejected UNAUTHENTICATED by the helper.
func TestContractRootRevalidationRejectsMismatch(t *testing.T) {
	// A separate admin credential whose digest the root helper will NOT accept.
	_, otherAdminDigest, err := auth.Generate(auth.ClassAdmin)
	if err != nil {
		t.Fatalf("generate other admin: %v", err)
	}
	s := buildStack(t, func(sc *stackConfig) {
		sc.rootAdminDigest = otherAdminDigest // root helper trusts a DIFFERENT digest
	})
	session := connectMCP(t, s.g, s.creds.adminBearer)

	res := callTool(t, session, api.ToolReadFile, validArgs(api.ToolReadFile))
	if code := resultMeta(t, res).Code; code != api.CodeUnauthenticated {
		t.Fatalf("mismatched-digest admin read_file code = %q, want UNAUTHENTICATED (root must re-verify)", code)
	}
}

// ---------------------------------------------------------------------------
// Pause cancels an active call
// ---------------------------------------------------------------------------

func TestContractPauseCancelsActiveCall(t *testing.T) {
	entered := make(chan struct{}, 1)
	hold := make(chan struct{})
	s := buildStack(t, func(sc *stackConfig) {
		sc.sessionProc = &testutil.FakeProcess{Entered: entered, Hold: hold}
	})
	session := connectMCP(t, s.g, s.creds.userBearer)

	done := make(chan api.ErrorCode, 1)
	go func() {
		res := callTool(t, session, api.ToolBash, validArgs(api.ToolBash))
		done <- resultMeta(t, res).Code
	}()

	// Wait until the call is active inside the session broker's executor.
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(hold)
		t.Fatal("call never reached the session executor")
	}

	// Pause while the call is blocked: the gateway's generation is cancelled and
	// the in-flight call must resolve as PAUSED.
	if err := s.controller.Pause(context.Background()); err != nil {
		t.Fatalf("pause: %v", err)
	}

	select {
	case code := <-done:
		if code != api.CodePaused {
			t.Fatalf("cancelled call code = %q, want PAUSED", code)
		}
	case <-time.After(3 * time.Second):
		close(hold)
		t.Fatal("in-flight call was not cancelled by pause")
	}
	close(hold)
}

// ---------------------------------------------------------------------------
// Health redaction
// ---------------------------------------------------------------------------

func TestContractHealthzRedacted(t *testing.T) {
	s := buildStack(t, nil)
	ts := httptest.NewServer(s.g.Handler())
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
	for _, bad := range []string{"/run/", "uid", "token", "bearer", "hdn_"} {
		if bytes.Contains(bytes.ToLower(body), []byte(bad)) {
			t.Fatalf("healthz body contains %q: %s", bad, body)
		}
	}
}
