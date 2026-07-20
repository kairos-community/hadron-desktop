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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
	dockerAbsent    bool   // if true (and dockerReadable is false), the probe reports ABSENT instead of DENIED

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
		switch {
		case f.dockerReadable:
			return api.TerminalOutput{Stdout: "OPEN\n"}, nil
		case f.dockerAbsent:
			return api.TerminalOutput{Stdout: "ABSENT\n"}, nil
		default:
			return api.TerminalOutput{Stdout: "DENIED\n"}, nil
		}
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

// fakeFiles is a small in-memory filesystem with a permission model: reads of
// root-only paths (/root/... and the checkRootReadDenied probe target
// rootOnlyProbeFile) are FORBIDDEN unless allowRootRead is set, or — to drive
// the I1 non-vacuity proof — report NOT_FOUND instead when rootReadNotFound
// is set (simulating a probe target that doesn't exist, which must NOT count
// as a clean deny).
type fakeFiles struct {
	mu               sync.Mutex
	store            map[string]string
	allowRootRead    bool
	rootReadNotFound bool
}

func newFakeFiles() *fakeFiles { return &fakeFiles{store: map[string]string{}} }

func isRootOnlyPath(p string) bool {
	return p == rootOnlyProbeFile || strings.HasPrefix(p, "/root")
}

func (f *fakeFiles) Read(_ context.Context, in api.ReadFileInput) api.ReadFileOutput {
	f.mu.Lock()
	defer f.mu.Unlock()
	if isRootOnlyPath(in.Path) && !f.allowRootRead {
		if f.rootReadNotFound {
			return api.ReadFileOutput{ResultMeta: api.ResultMeta{Code: api.CodeNotFound, Message: "not found"}}
		}
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
func (c *fakeComputer) Browser(context.Context, api.BrowserInput) (api.BrowserOutput, error) {
	return api.BrowserOutput{URL: "https://example.test/", Title: "fake page"}, nil
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
	// The suite must have exercised all ten brief items across ten checks
	// (the tenth, browser_reachable, came with the 2026-07-20 amendment).
	if len(r.Checks) != 10 {
		t.Fatalf("ran %d checks, want 10", len(r.Checks))
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
	// Regressed case: the boundary broke and the root-only probe file is
	// readable. The check must fail, not GREEN.
	h := buildHarness(t, func(h *harness) {
		h.files.allowRootRead = true
	})
	r := run(t, h)
	if c := checkByName(t, r, "root_read_denied"); c.Passed {
		t.Fatalf("root read check passed even though %s was readable", rootOnlyProbeFile)
	}
}

// TestRootReadDenyNotFoundDoesNotPass is the I1 non-vacuity proof: if the
// boundary regressed (the deny logic is gone) but the probe target happens to
// be absent, the OLD check (which passed on ANY non-empty rd.Code) would have
// GREENed while compromised. The new check must fail on NOT_FOUND exactly as
// it fails on success, because NOT_FOUND doesn't prove the boundary at all.
func TestRootReadDenyNotFoundDoesNotPass(t *testing.T) {
	h := buildHarness(t, func(h *harness) {
		h.files.rootReadNotFound = true
	})
	r := run(t, h)
	c := checkByName(t, r, "root_read_denied")
	if c.Passed {
		t.Fatal("root read check passed on NOT_FOUND, which does not prove the deny boundary")
	}
	if c.Transport {
		t.Fatal("root read check should be an assertion failure, not a transport failure, on NOT_FOUND")
	}
}

func TestDockerSocketDenyIsNonVacuous(t *testing.T) {
	// Regressed case: the socket is present AND accessible. The check must
	// fail, not GREEN.
	h := buildHarness(t, func(h *harness) {
		h.sessionProc.dockerReadable = true
	})
	r := run(t, h)
	if c := checkByName(t, r, "docker_socket_denied"); c.Passed {
		t.Fatal("docker socket check passed even though the socket was accessible")
	}
}

// TestDockerSocketAbsentIsFlaggedNotConflatedWithDenied is the I2 non-vacuity
// proof: an ABSENT socket must not silently pass with the SAME detail as a
// proven DENIED. The security property does hold (nothing to access), so the
// run does not fail, but the recorded Detail must make the drift visible and
// distinguishable from a real, present-and-denied outcome.
func TestDockerSocketAbsentIsFlaggedNotConflatedWithDenied(t *testing.T) {
	// Baseline: the expected, strong pass — socket present, access denied.
	deniedHarness := buildHarness(t, nil)
	deniedReport := run(t, deniedHarness)
	deniedCheck := checkByName(t, deniedReport, "docker_socket_denied")
	if !deniedCheck.Passed {
		t.Fatalf("baseline docker socket check failed: %s", deniedCheck.Detail)
	}

	// Drift: the socket is absent.
	absentHarness := buildHarness(t, func(h *harness) {
		h.sessionProc.dockerAbsent = true
	})
	absentReport := run(t, absentHarness)
	absentCheck := checkByName(t, absentReport, "docker_socket_denied")

	if !absentCheck.Passed {
		t.Fatalf("docker socket check failed on ABSENT, but the security property holds and must not fail the run: %s", absentCheck.Detail)
	}
	if absentReport.Outcome() != ExitPass {
		t.Fatalf("Outcome = %d, want %d (pass) when the socket is merely absent", absentReport.Outcome(), ExitPass)
	}
	if absentCheck.Detail == deniedCheck.Detail {
		t.Fatal("ABSENT and DENIED produced the identical Detail; the drift is not distinguishable")
	}
	if !strings.Contains(strings.ToUpper(absentCheck.Detail), "ABSENT") {
		t.Fatalf("ABSENT outcome Detail does not flag the drift: %q", absentCheck.Detail)
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

// TestClientHelpersStable exercises the pure, stateless helper functions
// (mutateBearer, parseID, parseGrandchildPID) directly. It does not exercise
// a nil Client.Base or any real request; the nil-Base fallback used by the
// readiness path in Main is covered indirectly by TestConnectFailureIsTransport
// and the Main descriptor tests.
func TestClientHelpersStable(t *testing.T) {
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

// ---------------------------------------------------------------------------
// M1: NewTLSClient is hermetically proven to pin, not merely to "use TLS"
// ---------------------------------------------------------------------------

// writePEMCert PEM-encodes cert and writes it to a new file under t.TempDir,
// returning the path.
func writePEMCert(t *testing.T, name string, cert *x509.Certificate) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	block := &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// generateUnrelatedSelfSignedCert builds a throwaway self-signed certificate
// that has nothing to do with the httptest server under test. httptest's TLS
// servers in a given process all present the SAME fixed certificate/key pair
// (see net/http/httptest's internal testcert), so a second httptest server
// would NOT exercise rejection — this generates a genuinely different CA.
func generateUnrelatedSelfSignedCert(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated-ca.invalid"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}

// TestNewTLSClientPinsToTheGivenCAOnly is the M1 hermetic proof: NewTLSClient
// never falls back to the system trust store or InsecureSkipVerify. A client
// pinned to the SERVER's own certificate validates; a client pinned to an
// unrelated certificate rejects the SAME server outright.
func TestNewTLSClientPinsToTheGivenCAOnly(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	t.Run("pinned to the correct CA validates", func(t *testing.T) {
		caFile := writePEMCert(t, "correct-ca.pem", ts.Certificate())
		client, err := NewTLSClient(ts.URL, caFile)
		if err != nil {
			t.Fatalf("NewTLSClient: %v", err)
		}
		hc := &http.Client{Transport: client.Base}
		resp, err := hc.Get(ts.URL)
		if err != nil {
			t.Fatalf("GET with the pinned correct CA failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
	})

	t.Run("pinned to a different CA rejects", func(t *testing.T) {
		wrongCert := generateUnrelatedSelfSignedCert(t)
		caFile := writePEMCert(t, "wrong-ca.pem", wrongCert)
		client, err := NewTLSClient(ts.URL, caFile)
		if err != nil {
			t.Fatalf("NewTLSClient: %v", err)
		}
		hc := &http.Client{Transport: client.Base}
		_, err = hc.Get(ts.URL)
		if err == nil {
			t.Fatal("GET with a wrong pinned CA unexpectedly succeeded")
		}
		var unknownAuth x509.UnknownAuthorityError
		var certInvalid x509.CertificateInvalidError
		if !errors.As(err, &unknownAuth) && !errors.As(err, &certInvalid) {
			t.Fatalf("expected an x509 trust error, got: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// M2: Main's descriptor/argument (exit 2) branches
// ---------------------------------------------------------------------------

// writeDescriptorFile marshals a Descriptor to a temp file and returns its
// path.
func writeDescriptorFile(t *testing.T, d Descriptor) string {
	t.Helper()
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	path := filepath.Join(t.TempDir(), "descriptor.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write descriptor: %v", err)
	}
	return path
}

// TestMainExitDescriptorPaths drives Main over every exit-2 (descriptor /
// argument error) branch. Exit 3 (transport) is covered by
// TestConnectFailureIsTransport and exit 0/1 by the contract-suite tests
// above; this table does not duplicate those.
func TestMainExitDescriptorPaths(t *testing.T) {
	dir := t.TempDir()

	artifactDir := filepath.Join(dir, "artifacts")
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: generateUnrelatedSelfSignedCert(t).Raw,
	}), 0o600); err != nil {
		t.Fatalf("write ca file: %v", err)
	}
	bearerFile := filepath.Join(dir, "user.token")
	if err := os.WriteFile(bearerFile, []byte("hdn_u_dummy\n"), 0o600); err != nil {
		t.Fatalf("write bearer file: %v", err)
	}
	adminBearerFile := filepath.Join(dir, "admin.token")
	if err := os.WriteFile(adminBearerFile, []byte("hdn_a_dummy\n"), 0o600); err != nil {
		t.Fatalf("write admin bearer file: %v", err)
	}

	validDescriptor := Descriptor{
		MCPURL:            "https://127.0.0.1:1/mcp",
		BearerTokenFile:   bearerFile,
		CACertificateFile: caFile,
		ArtifactDirectory: artifactDir,
	}
	validDescriptorPath := writeDescriptorFile(t, validDescriptor)

	missingCADescriptor := validDescriptor
	missingCADescriptor.CACertificateFile = filepath.Join(dir, "does-not-exist-ca.pem")
	missingCADescriptorPath := writeDescriptorFile(t, missingCADescriptor)

	missingBearerDescriptor := validDescriptor
	missingBearerDescriptor.BearerTokenFile = filepath.Join(dir, "does-not-exist-bearer")
	missingBearerDescriptorPath := writeDescriptorFile(t, missingBearerDescriptor)

	invalidJSONPath := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalidJSONPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write invalid descriptor: %v", err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{
			name: "unknown mode",
			args: []string{"--mode=bogus", "--descriptor=" + validDescriptorPath, "--admin-bearer-file=" + adminBearerFile},
		},
		{
			name: "missing descriptor flag",
			args: []string{"--admin-bearer-file=" + adminBearerFile},
		},
		{
			name: "missing admin-bearer-file flag",
			args: []string{"--descriptor=" + validDescriptorPath},
		},
		{
			name: "descriptor file does not exist",
			args: []string{"--descriptor=" + filepath.Join(dir, "nope.json"), "--admin-bearer-file=" + adminBearerFile},
		},
		{
			name: "descriptor file has invalid JSON",
			args: []string{"--descriptor=" + invalidJSONPath, "--admin-bearer-file=" + adminBearerFile},
		},
		{
			name: "descriptor's bearer_token_file is missing",
			args: []string{"--descriptor=" + missingBearerDescriptorPath, "--admin-bearer-file=" + adminBearerFile},
		},
		{
			name: "admin-bearer-file does not exist",
			args: []string{"--descriptor=" + validDescriptorPath, "--admin-bearer-file=" + filepath.Join(dir, "nope-admin.token")},
		},
		{
			name: "descriptor's ca_certificate_file is missing",
			args: []string{"--descriptor=" + missingCADescriptorPath, "--admin-bearer-file=" + adminBearerFile},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Main(tc.args, io.Discard, io.Discard)
			if got != ExitDescriptor {
				t.Fatalf("Main(%v) = %d, want %d (descriptor/argument error)", tc.args, got, ExitDescriptor)
			}
		})
	}
}

// TestExecModeRunsThroughThePublicTerminal covers the driver the recovery gate
// depends on: it must reach the appliance through the public terminal tool with
// the requested credential class, and hand back the command's own exit status
// so a shell caller can branch on it.
func TestExecModeRunsThroughThePublicTerminal(t *testing.T) {
	h := buildHarness(t, nil)
	suite := h.suite()

	report, res := suite.RunExec(context.Background(), "echo hello", false, 10*time.Second)
	if got := report.Outcome(); got != ExitPass {
		t.Fatalf("Outcome = %d, want %d: %+v", got, ExitPass, report.Checks)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}
	// The harness's broker is a stub that echoes a canned reply, so this
	// asserts the plumbing carries stdout back, not what a real shell would
	// print -- that is the live recovery gate's job.
	if res.Stdout == "" {
		t.Fatal("stdout was empty; exec must return the terminal tool's output")
	}
	if len(report.Checks) != 1 || report.Checks[0].Name != "exec_user" {
		t.Fatalf("checks = %+v, want a single exec_user check", report.Checks)
	}
}

// TestExecModeAdminUsesTheAdminBearer: the recovery gate kills root-owned
// services, so exec must actually route through the admin class rather than
// quietly running everything as the unprivileged user.
func TestExecModeAdminUsesTheAdminBearer(t *testing.T) {
	h := buildHarness(t, nil)
	suite := h.suite()

	report, _ := suite.RunExec(context.Background(), "id -un", true, 10*time.Second)
	if got := report.Outcome(); got != ExitPass {
		t.Fatalf("Outcome = %d, want %d: %+v", got, ExitPass, report.Checks)
	}
	if len(report.Checks) != 1 || report.Checks[0].Name != "exec_admin" {
		t.Fatalf("checks = %+v, want a single exec_admin check", report.Checks)
	}
}
