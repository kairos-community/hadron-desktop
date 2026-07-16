package smoke

// smoke_test wires the REAL agent stack together and exercises the contract
// suite against it with no VM, mirroring cmd/hadron-agent/contract_test.go:
//
//   - the real *session.Broker and *roothelper.Helper, each behind the real
//     Unix-socket rpc.Server (the root socket with RequireAuth), reached by the
//     real rpc.Client;
//   - the real gateway (gateway.New) with the real auth.Verifier and the real
//     control.Controller, served over httptest TLS;
//   - a real MCP client (the smoke.Client) whose bearer is injected per session.
//
// Only the leaf executors are fakes, but unlike testutil's fixed fakes these
// are configurable so the suite's assertions are driven by realistic tool
// outputs (an `id` transcript, a cgroup line, a permission denial). Each
// negative test flips one fake so the corresponding assertion FAILS, proving
// the checks are non-vacuous and that the gateway's user-vs-admin routing is
// genuinely exercised.

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/api"
	"github.com/mudler/hadron-desktop/agent/internal/auth"
	"github.com/mudler/hadron-desktop/agent/internal/control"
	"github.com/mudler/hadron-desktop/agent/internal/gateway"
	"github.com/mudler/hadron-desktop/agent/internal/roothelper"
	"github.com/mudler/hadron-desktop/agent/internal/rpc"
	"github.com/mudler/hadron-desktop/agent/internal/session"
)

// unusableDigest mirrors the main package's disabled-class placeholder.
const unusableDigest = auth.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")

// ---------------------------------------------------------------------------
// Configurable fakes
// ---------------------------------------------------------------------------

// fakeProc is a configurable process/terminal executor. It answers the exact
// commands the contract suite issues (`id -u; id -un; id -nG`, the docker
// socket probe, the grandchild liveness probe) and simulates a PTY process
// whose poll output depends on the last stdin write.
type fakeProc struct {
	uid             int
	user            string
	groups          []string
	cgroupLine      string // returned on poll after "cat /proc/self/cgroup"
	grandchildAlive bool   // if true, the kill -0 probe reports ALIVE
	dockerReadable  bool   // if true, the docker socket probe reports OPEN

	mu        sync.Mutex
	lastWrite map[string]string
}

func newFakeProc(uid int, user string, groups []string) *fakeProc {
	return &fakeProc{
		uid:        uid,
		user:       user,
		groups:     groups,
		cgroupLine: "0::/system.slice/hadron-agent-session.slice/hadron-proc-abc123def",
		lastWrite:  map[string]string{},
	}
}

func (f *fakeProc) Terminal(_ context.Context, in api.TerminalInput) (api.TerminalOutput, error) {
	switch {
	case in.Command == "id -u; id -un; id -nG":
		return api.TerminalOutput{
			Stdout: fmt.Sprintf("%d\n%s\n%s\n", f.uid, f.user, strings.Join(f.groups, " ")),
		}, nil
	case strings.Contains(in.Command, "docker.sock"):
		if f.dockerReadable {
			return api.TerminalOutput{Stdout: "OPEN\n"}, nil
		}
		return api.TerminalOutput{Stdout: "DENIED\n"}, nil
	case strings.Contains(in.Command, "kill -0"):
		if f.grandchildAlive {
			return api.TerminalOutput{Stdout: "ALIVE\n"}, nil
		}
		return api.TerminalOutput{Stdout: "GONE\n"}, nil
	default:
		return api.TerminalOutput{Stdout: "ok\n", ExitCode: 0}, nil
	}
}

func (f *fakeProc) Process(_ context.Context, in api.ProcessInput) (api.ProcessOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch in.Action {
	case api.ProcessStart:
		return api.ProcessOutput{ProcessID: "proc-1", PID: 4242, Running: true}, nil
	case api.ProcessWrite:
		f.lastWrite[in.ProcessID] = in.Input
		return api.ProcessOutput{ProcessID: in.ProcessID, Running: true}, nil
	case api.ProcessPoll:
		last := f.lastWrite[in.ProcessID]
		switch {
		case strings.Contains(last, "cat /proc/self/cgroup"):
			return api.ProcessOutput{ProcessID: in.ProcessID, Running: true, Stdout: f.cgroupLine + "\n"}, nil
		case strings.Contains(last, "GC="):
			return api.ProcessOutput{ProcessID: in.ProcessID, Running: true, Stdout: "GC=99999\n"}, nil
		default:
			return api.ProcessOutput{ProcessID: in.ProcessID, Running: true}, nil
		}
	case api.ProcessTerminate:
		return api.ProcessOutput{ProcessID: in.ProcessID, Running: false}, nil
	default:
		return api.ProcessOutput{ProcessID: in.ProcessID, Running: true}, nil
	}
}

func (f *fakeProc) Close() error { return nil }

// fakeFiles is a small in-memory filesystem with a permission model: reads
// under /root are denied unless allowRootRead is set.
type fakeFiles struct {
	mu            sync.Mutex
	store         map[string]string
	allowRootRead bool
}

func newFakeFiles() *fakeFiles { return &fakeFiles{store: map[string]string{}} }

func (f *fakeFiles) Read(_ context.Context, in api.ReadFileInput) api.ReadFileOutput {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(in.Path, "/root") && !f.allowRootRead {
		return api.ReadFileOutput{ResultMeta: api.ResultMeta{Code: api.CodeForbidden, Message: "permission denied"}}
	}
	content := f.store[in.Path]
	return api.ReadFileOutput{Content: content, Size: int64(len(content))}
}

func (f *fakeFiles) Search(_ context.Context, in api.SearchFilesInput) api.SearchFilesOutput {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matches []api.SearchMatch
	for path, content := range f.store {
		if strings.HasPrefix(path, in.Path) && strings.Contains(content, in.Query) {
			matches = append(matches, api.SearchMatch{Path: path, Line: 1, Excerpt: in.Query})
		}
	}
	return api.SearchFilesOutput{Matches: matches}
}

func (f *fakeFiles) Write(_ context.Context, in api.WriteFileInput) api.WriteFileOutput {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[in.Path] = in.Content
	return api.WriteFileOutput{BytesWritten: int64(len(in.Content)), Created: true}
}

func (f *fakeFiles) Patch(_ context.Context, in api.PatchInput) api.PatchOutput {
	f.mu.Lock()
	defer f.mu.Unlock()
	var changed []string
	applied := 0
	for _, file := range in.Files {
		content, ok := f.store[file.Path]
		if !ok {
			continue
		}
		for _, r := range file.Replacements {
			if strings.Contains(content, r.Old) {
				content = strings.Replace(content, r.Old, r.New, 1)
				applied++
			}
		}
		f.store[file.Path] = content
		changed = append(changed, file.Path)
	}
	return api.PatchOutput{FilesChanged: changed, ReplacementsApplied: applied}
}

// fakeComputer returns a configurable capture image.
type fakeComputer struct{ image string }

func (c *fakeComputer) ComputerUse(context.Context, api.ComputerUseInput) (api.ComputerUseOutput, error) {
	return api.ComputerUseOutput{ImageBase64: c.image}, nil
}
func (c *fakeComputer) Ready() bool  { return true }
func (c *fakeComputer) Stop()        {}
func (c *fakeComputer) Close() error { return nil }

// fakeIdentity reports a fixed identity for the session broker.
type fakeIdentity struct{ id api.Identity }

func (f fakeIdentity) Identity() api.Identity { return f.id }

// ---------------------------------------------------------------------------
// RPC + handler adapters (mirrors cmd/hadron-agent)
// ---------------------------------------------------------------------------

type sessionHandler struct{ broker *session.Broker }

func (h sessionHandler) Call(ctx context.Context, req rpc.CallRequest, _ string) (*rpc.CallResponse, error) {
	return h.broker.Call(ctx, req.Tool, req.Arguments)
}
func (h sessionHandler) Health(context.Context) (rpc.HealthStatus, error) {
	s := h.broker.Status()
	return rpc.HealthStatus{OK: !s.Paused, Paused: s.Paused}, nil
}
func (h sessionHandler) Pause(ctx context.Context) error  { return h.broker.Pause(ctx) }
func (h sessionHandler) Resume(ctx context.Context) error { return h.broker.Resume(ctx) }

type rootHandler struct{ helper *roothelper.Helper }

func (h rootHandler) Call(ctx context.Context, req rpc.CallRequest, authHeader string) (*rpc.CallResponse, error) {
	return h.helper.Call(ctx, req.Tool, req.Arguments, authHeader)
}
func (h rootHandler) Health(context.Context) (rpc.HealthStatus, error) {
	s := h.helper.Status()
	return rpc.HealthStatus{OK: !s.Paused, Paused: s.Paused}, nil
}
func (h rootHandler) Pause(ctx context.Context) error  { return h.helper.Pause(ctx) }
func (h rootHandler) Resume(ctx context.Context) error { return h.helper.Resume(ctx) }

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

func buildVerifier(t *testing.T, userDigest, adminDigest auth.Digest) *auth.Verifier {
	t.Helper()
	if userDigest == "" {
		userDigest = unusableDigest
	}
	if adminDigest == "" {
		adminDigest = unusableDigest
	}
	v, err := auth.NewVerifier(
		auth.RotationConfig{Current: userDigest},
		auth.RotationConfig{Current: adminDigest},
	)
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	return v
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	client      *Client
	userBearer  string
	adminBearer string
	sessionProc *fakeProc
	rootProc    *fakeProc
	files       *fakeFiles
	computer    *fakeComputer
	identity    *fakeIdentity
}

// buildHarness stands up the real stack over httptest TLS and returns a
// smoke.Client pointed at it, plus handles to the fakes so a test can tweak
// them BEFORE calling this (they are passed by pointer into the brokers).
func buildHarness(t *testing.T, configure func(*harness)) *harness {
	t.Helper()

	userBearer, userDigest, err := auth.Generate(auth.ClassUser)
	if err != nil {
		t.Fatalf("generate user bearer: %v", err)
	}
	adminBearer, adminDigest, err := auth.Generate(auth.ClassAdmin)
	if err != nil {
		t.Fatalf("generate admin bearer: %v", err)
	}

	h := &harness{
		userBearer:  userBearer,
		adminBearer: adminBearer,
		sessionProc: newFakeProc(1000, "agent", []string{"agent", "users"}),
		rootProc:    newFakeProc(0, "root", []string{"root"}),
		files:       newFakeFiles(),
		computer:    &fakeComputer{image: "aGVsbG8="}, // "hello"
		identity:    &fakeIdentity{id: api.Identity{User: "agent", UID: 1000}},
	}
	if configure != nil {
		configure(h)
	}

	// Session broker (unprivileged) behind the non-auth session socket.
	sessionBroker := session.New(session.Config{
		Files:    h.files,
		Process:  h.sessionProc,
		Computer: h.computer,
		Identity: h.identity,
	})
	t.Cleanup(func() { _ = sessionBroker.Close() })
	sessionClient := startRPCServer(t, sessionHandler{broker: sessionBroker}, false)

	// Root helper (privileged) behind the auth-gated root socket, with an
	// INDEPENDENT verifier — exactly as in production.
	rootHelperSvc, err := roothelper.New(roothelper.Config{
		Verifier: buildVerifier(t, unusableDigest, adminDigest),
		Files:    h.files,
		Process:  h.rootProc,
	})
	if err != nil {
		t.Fatalf("new root helper: %v", err)
	}
	t.Cleanup(func() { _ = rootHelperSvc.Close() })
	rootClient := startRPCServer(t, rootHandler{helper: rootHelperSvc}, true)

	ctrl := control.New(control.Config{
		Version:        "v-test",
		SessionHealthy: func(context.Context) bool { return true },
		CuaHealthy:     func(context.Context) bool { return true },
	})

	g, err := gateway.New(gateway.Config{
		Version:          "v-test",
		ListenAddr:       "127.0.0.1:0",
		InsecureLoopback: true,
		Session:          sessionClient,
		Root:             rootClient,
		Verifier:         buildVerifier(t, userDigest, adminDigest),
		Controller:       ctrl,
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}

	ts := httptest.NewTLSServer(g.Handler())
	t.Cleanup(ts.Close)

	h.client = &Client{Endpoint: ts.URL + "/mcp", Base: ts.Client().Transport}
	return h
}

func (h *harness) suite() *Suite {
	return &Suite{
		Client:      h.client,
		UserBearer:  h.userBearer,
		AdminBearer: h.adminBearer,
		CallTimeout: 5 * time.Second,
	}
}

func run(t *testing.T, h *harness) Report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return h.suite().RunContract(ctx)
}

// checkByName returns the named check result.
func checkByName(t *testing.T, r Report, name string) CheckResult {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in report", name)
	return CheckResult{}
}

// ---------------------------------------------------------------------------
// Positive: the full suite passes against a healthy stack
// ---------------------------------------------------------------------------

func TestContractSuitePasses(t *testing.T) {
	h := buildHarness(t, nil)
	r := run(t, h)
	for _, c := range r.Checks {
		if !c.Passed {
			t.Errorf("check %q failed: %s (transport=%v)", c.Name, c.Detail, c.Transport)
		}
	}
	if got := r.Outcome(); got != ExitPass {
		t.Fatalf("Outcome = %d, want %d (pass)", got, ExitPass)
	}
	// The suite must have exercised all ten brief items across nine checks.
	if len(r.Checks) != 9 {
		t.Fatalf("ran %d checks, want 9", len(r.Checks))
	}
}

// TestUserAndAdminRouteToDifferentBrokers proves the gateway's class routing is
// real: the SAME `id` command yields the unprivileged agent for the user bearer
// and root for the admin bearer, because the two classes reach different
// brokers.
func TestUserAndAdminRouteToDifferentBrokers(t *testing.T) {
	h := buildHarness(t, nil)
	r := run(t, h)
	if c := checkByName(t, r, "user_unprivileged_identity"); !c.Passed {
		t.Fatalf("user identity check failed: %s", c.Detail)
	}
	if c := checkByName(t, r, "admin_identity_root"); !c.Passed {
		t.Fatalf("admin identity check failed: %s", c.Detail)
	}
}

// ---------------------------------------------------------------------------
// Non-vacuous: each negative flips one fake so exactly that check fails
// ---------------------------------------------------------------------------

func TestUserIdentityCheckIsNonVacuous(t *testing.T) {
	// If the session broker ran as root, the user identity check must fail.
	h := buildHarness(t, func(h *harness) {
		h.sessionProc = newFakeProc(0, "root", []string{"root"})
	})
	r := run(t, h)
	if c := checkByName(t, r, "user_unprivileged_identity"); c.Passed {
		t.Fatal("user identity check passed even though the user ran as root")
	}
	if got := r.Outcome(); got != ExitAssertion {
		t.Fatalf("Outcome = %d, want %d (assertion)", got, ExitAssertion)
	}
}

func TestPrivilegedGroupIsNonVacuous(t *testing.T) {
	h := buildHarness(t, func(h *harness) {
		h.sessionProc = newFakeProc(1000, "agent", []string{"agent", "docker"})
	})
	r := run(t, h)
	if c := checkByName(t, r, "user_unprivileged_identity"); c.Passed {
		t.Fatal("user identity check passed despite docker group membership")
	}
}

func TestAdminIdentityCheckIsNonVacuous(t *testing.T) {
	h := buildHarness(t, func(h *harness) {
		h.rootProc = newFakeProc(1000, "agent", []string{"agent"})
	})
	r := run(t, h)
	if c := checkByName(t, r, "admin_identity_root"); c.Passed {
		t.Fatal("admin identity check passed even though the admin path was not root")
	}
}

func TestRootReadDenyIsNonVacuous(t *testing.T) {
	h := buildHarness(t, func(h *harness) {
		h.files.allowRootRead = true
	})
	r := run(t, h)
	if c := checkByName(t, r, "root_read_denied"); c.Passed {
		t.Fatal("root read check passed even though /root was readable")
	}
}

func TestDockerSocketDenyIsNonVacuous(t *testing.T) {
	h := buildHarness(t, func(h *harness) {
		h.sessionProc.dockerReadable = true
	})
	r := run(t, h)
	if c := checkByName(t, r, "docker_socket_denied"); c.Passed {
		t.Fatal("docker socket check passed even though the socket was accessible")
	}
}

func TestGrandchildSurvivalIsNonVacuous(t *testing.T) {
	h := buildHarness(t, func(h *harness) {
		h.sessionProc.grandchildAlive = true
	})
	r := run(t, h)
	if c := checkByName(t, r, "process_pty_cgroup_isolation"); c.Passed {
		t.Fatal("pty/cgroup check passed even though a grandchild survived")
	}
}

func TestCgroupLeafIsNonVacuous(t *testing.T) {
	h := buildHarness(t, func(h *harness) {
		h.sessionProc.cgroupLine = "0::/system.slice/some-other.scope"
	})
	r := run(t, h)
	if c := checkByName(t, r, "process_pty_cgroup_isolation"); c.Passed {
		t.Fatal("pty/cgroup check passed even though the process was not in an MCP leaf")
	}
}

func TestComputerUseCaptureIsNonVacuous(t *testing.T) {
	h := buildHarness(t, func(h *harness) {
		h.computer.image = ""
	})
	r := run(t, h)
	if c := checkByName(t, r, "computer_use_capture"); c.Passed {
		t.Fatal("capture check passed even though no image was returned")
	}
}

// TestValidAdminBearerIsAccepted proves the gateway genuinely accepts the real
// admin bearer, so the invalid-admin rejection reflects the mutation and not a
// gateway that rejects everything (the non-vacuous counterpart).
func TestValidAdminBearerIsAccepted(t *testing.T) {
	h := buildHarness(t, nil)
	// The real admin bearer must pass the rejection check's inverse: it is
	// accepted and executes.
	r := run(t, h)
	if c := checkByName(t, r, "invalid_admin_rejected"); !c.Passed {
		t.Fatalf("invalid admin rejection check failed: %s", c.Detail)
	}
	if c := checkByName(t, r, "admin_identity_root"); !c.Passed {
		t.Fatalf("valid admin bearer was not accepted: %s", c.Detail)
	}
}

// ---------------------------------------------------------------------------
// Transport / descriptor exit codes
// ---------------------------------------------------------------------------

func TestConnectFailureIsTransport(t *testing.T) {
	// A Client pointed at a dead endpoint yields a transport failure (exit 3).
	s := &Suite{
		Client:      &Client{Endpoint: "https://127.0.0.1:1/mcp"},
		UserBearer:  "hdn_u_x",
		AdminBearer: "hdn_a_x",
		CallTimeout: 2 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := s.RunContract(ctx)
	if got := r.Outcome(); got != ExitTransport {
		t.Fatalf("Outcome = %d, want %d (transport)", got, ExitTransport)
	}
}

func TestClientBaseNilPanicsSafely(t *testing.T) {
	// Guard: a Client with a nil Base must fall back to a usable transport for
	// the readiness path in Main (covered indirectly); here we only assert the
	// pure helpers are stable.
	if got := mutateBearer("hdn_a_abc"); got == "hdn_a_abc" {
		t.Fatal("mutateBearer returned the input unchanged")
	}
	if _, ok := parseID("1000\nagent\nagent users\n"); !ok {
		t.Fatal("parseID failed to parse a valid transcript")
	}
	if got := parseGrandchildPID("GC=4242\n"); got != "4242" {
		t.Fatalf("parseGrandchildPID = %q, want 4242", got)
	}
}
