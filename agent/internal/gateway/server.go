// Package gateway implements hadron-agent's public front door: a stateless
// HTTPS MCP endpoint that authenticates every /mcp request, routes each tool
// call to the correct broker over the internal Unix-socket RPC, and exposes
// fully redacted health and readiness endpoints.
//
// The gateway itself never executes a tool, opens a path, starts Cua, runs as
// root, or touches Docker. It only:
//
//   - verifies the presented bearer (via package auth) and admits both the
//     user and admin classes to /mcp;
//   - routes by class and tool -- an ordinary user bearer sends ALL seven OS
//     tools to the unprivileged session broker; an admin bearer sends
//     computer_use to the session broker too, but the other six OS tools to
//     the privileged root helper, forwarding the raw Authorization header so
//     the root helper re-verifies it independently;
//   - enforces the emergency pause and per-generation cancellation owned by
//     package control;
//   - bounds request bodies, serialized responses, concurrent calls, and open
//     connections;
//   - audits every call with a secret-free structured log line.
//
// Authentication is applied to /mcp ONLY; /healthz and /readyz are open and
// return nothing but a version string and booleans.
package gateway

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mudler/hadron-desktop/agent/internal/api"
	appauth "github.com/mudler/hadron-desktop/agent/internal/auth"
	"github.com/mudler/hadron-desktop/agent/internal/control"
	"github.com/mudler/hadron-desktop/agent/internal/rpc"
)

// Defaults for the gateway's operational bounds and behavior.
const (
	// DefaultListenAddr is the address the gateway listens on unless
	// overridden. It binds all interfaces on the fixed MCP port.
	DefaultListenAddr = "0.0.0.0:7443"

	// DefaultMaxBodyBytes bounds the size of any /mcp request body.
	DefaultMaxBodyBytes = 2 << 20 // 2 MiB
	// DefaultMaxResponseBytes bounds the serialized size of any tool result
	// the gateway will return; a larger result is refused as exhausted.
	DefaultMaxResponseBytes = 16 << 20 // 16 MiB
	// DefaultMaxConcurrentCalls bounds how many tool calls execute at once.
	DefaultMaxConcurrentCalls = 16
	// DefaultMaxConnections bounds how many HTTP connections may be open.
	DefaultMaxConnections = 64

	// DefaultRequestTimeout is the per-request maximum for /mcp.
	DefaultRequestTimeout = 60 * time.Second
	// DefaultTokenTTL is how far in the future the gateway stamps a verified
	// token's expiration. package auth leaves TokenInfo.Expiration zero, which
	// the SDK's bearer middleware treats as unauthenticated; the gateway sets
	// a short non-zero expiration so a freshly verified token is accepted.
	DefaultTokenTTL = 5 * time.Minute
)

// brokerCaller is the call subset of a broker the gateway routes to. *rpc.Client
// satisfies it; tests may substitute a fake dialing an in-process rpc.Server.
type brokerCaller interface {
	Call(ctx context.Context, req rpc.CallRequest, authHeader string) (*rpc.CallResponse, error)
}

// Config constructs a Gateway.
type Config struct {
	// Version is the build version reported by /healthz and used as the MCP
	// server implementation version.
	Version string

	// ListenAddr is the host:port the gateway binds. Defaults to
	// DefaultListenAddr.
	ListenAddr string

	// TLSConfig, when set, is used to serve HTTPS. The gateway forces its
	// MinVersion to TLS 1.3. It is REQUIRED unless InsecureLoopback is set and
	// ListenAddr is a loopback address.
	TLSConfig *tls.Config
	// InsecureLoopback permits serving plain HTTP, and is accepted ONLY when
	// ListenAddr parses to a loopback address. It exists for local
	// development; production always uses TLS.
	InsecureLoopback bool

	// Session and Root are the RPC clients for the two brokers. Session is
	// required; Root is required whenever an admin bearer may be presented.
	Session brokerCaller
	Root    brokerCaller

	// Verifier verifies presented bearers. Required.
	Verifier *appauth.Verifier

	// Controller owns the pause state and per-call generation. Required.
	Controller *control.Controller

	// CertFingerprint, MDNS, and StatusFile drive the redacted status.json the
	// gateway publishes for the appliance's status bar and first-run panel.
	// CertFingerprint and MDNS are the static, provisioned config values
	// republished verbatim (redacted); both are absent (empty / false) when the
	// gateway runs without a --config. StatusFile is the path the redacted
	// status is written to; an empty StatusFile DISABLES the writer entirely, so
	// tests and the contract suite need no writable status directory.
	CertFingerprint string
	MDNS            bool
	StatusFile      string

	// RequiredScopes, when non-empty, are enforced by the bearer middleware:
	// a valid token lacking any of them is refused with 403. Left empty, any
	// valid bearer of either class is admitted to /mcp.
	RequiredScopes []string

	// Logger receives the per-call audit line. If nil, a discarding logger is
	// used.
	Logger *slog.Logger

	// The following bound the gateway; each defaults when zero.
	MaxBodyBytes       int64
	MaxResponseBytes   int
	MaxConcurrentCalls int
	MaxConnections     int
	RequestTimeout     time.Duration
	TokenTTL           time.Duration

	// now, if set, overrides the clock used to stamp token expirations. Tests
	// only.
	now func() time.Time
}

// Gateway is the public MCP front door. Construct it with New and serve it with
// Serve or ListenAndServe.
type Gateway struct {
	version    string
	listenAddr string
	tlsConfig  *tls.Config
	insecure   bool

	session brokerCaller
	root    brokerCaller

	verifier   *appauth.Verifier
	controller *control.Controller
	scopes     []string
	logger     *slog.Logger

	certFingerprint string
	mdns            bool
	statusFile      string

	maxBodyBytes     int64
	maxResponseBytes int
	maxConnections   int
	requestTimeout   time.Duration
	tokenTTL         time.Duration
	now              func() time.Time

	// computerUse counts the remote computer_use tool calls CURRENTLY executing
	// (only computer_use, never another tool nor a merely-open MCP connection).
	// ComputerUseActive reports whether the count is above zero.
	computerUse atomic.Int64

	sem     chan struct{}
	handler http.Handler
}

// New validates cfg and constructs a Gateway. It returns an error when the
// listen address is unparseable, when InsecureLoopback is requested for a
// non-loopback address, or when TLS is required but absent.
func New(cfg Config) (*Gateway, error) {
	if cfg.Verifier == nil {
		return nil, errors.New("gateway: Verifier is required")
	}
	if cfg.Controller == nil {
		return nil, errors.New("gateway: Controller is required")
	}
	if cfg.Session == nil {
		return nil, errors.New("gateway: Session broker is required")
	}

	listenAddr := cfg.ListenAddr
	if listenAddr == "" {
		listenAddr = DefaultListenAddr
	}
	if err := validateListen(listenAddr, cfg.InsecureLoopback); err != nil {
		return nil, err
	}
	if cfg.TLSConfig == nil && !cfg.InsecureLoopback {
		return nil, errors.New("gateway: TLSConfig is required unless InsecureLoopback is set on a loopback address")
	}

	var tlsConfig *tls.Config
	if cfg.TLSConfig != nil {
		tlsConfig = cfg.TLSConfig.Clone()
		tlsConfig.MinVersion = tls.VersionTLS13
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	g := &Gateway{
		version:          cfg.Version,
		listenAddr:       listenAddr,
		tlsConfig:        tlsConfig,
		insecure:         cfg.InsecureLoopback,
		session:          cfg.Session,
		root:             cfg.Root,
		verifier:         cfg.Verifier,
		controller:       cfg.Controller,
		scopes:           cfg.RequiredScopes,
		logger:           logger,
		certFingerprint:  cfg.CertFingerprint,
		mdns:             cfg.MDNS,
		statusFile:       cfg.StatusFile,
		maxBodyBytes:     orInt64(cfg.MaxBodyBytes, DefaultMaxBodyBytes),
		maxResponseBytes: orInt(cfg.MaxResponseBytes, DefaultMaxResponseBytes),
		maxConnections:   orInt(cfg.MaxConnections, DefaultMaxConnections),
		requestTimeout:   orDuration(cfg.RequestTimeout, DefaultRequestTimeout),
		tokenTTL:         orDuration(cfg.TokenTTL, DefaultTokenTTL),
		now:              cfg.now,
	}
	if g.now == nil {
		g.now = time.Now
	}
	g.sem = make(chan struct{}, orInt(cfg.MaxConcurrentCalls, DefaultMaxConcurrentCalls))
	g.handler = g.buildHandler()
	return g, nil
}

// ListenAddr reports the resolved address the gateway will bind.
func (g *Gateway) ListenAddr() string { return g.listenAddr }

// Handler returns the gateway's root http.Handler. It is exported for testing
// with httptest; Serve/ListenAndServe use it internally.
func (g *Gateway) Handler() http.Handler { return g.handler }

// ---------------------------------------------------------------------------
// Handler assembly
// ---------------------------------------------------------------------------

func (g *Gateway) buildHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "hadron-agent", Version: g.version}, nil)
	g.registerTools(server)

	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)

	// Inner-to-outer: streamable <- request timeout <- body limit <- bearer
	// auth <- cross-origin protection. Auth and the other guards apply to
	// /mcp only.
	var mcpChain http.Handler = streamable
	mcpChain = g.withRequestTimeout(mcpChain)
	mcpChain = g.withBodyLimit(mcpChain)
	mcpChain = auth.RequireBearerToken(g.verifyBearer, &auth.RequireBearerTokenOptions{
		Scopes: g.scopes,
	})(mcpChain)
	mcpChain = http.NewCrossOriginProtection().Handler(mcpChain)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpChain)
	mux.HandleFunc("/healthz", g.handleHealthz)
	mux.HandleFunc("/readyz", g.handleReadyz)
	return mux
}

func (g *Gateway) withRequestTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), g.requestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (g *Gateway) withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, g.maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// verifyBearer adapts package auth's class-scoped Verifier to the SDK's
// TokenVerifier signature. It admits BOTH bearer classes: it tries user then
// admin and returns the first success. Because package auth leaves
// TokenInfo.Expiration zero -- which the SDK middleware treats as
// unauthenticated -- it stamps a short non-zero expiration on success. A
// bearer that matches no class fails with a wrapped ErrInvalidToken so the
// middleware returns 401.
func (g *Gateway) verifyBearer(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	for _, class := range []appauth.Class{appauth.ClassUser, appauth.ClassAdmin} {
		ti, err := g.verifier.Verify(token, class)
		if err == nil {
			ti.Expiration = g.now().Add(g.tokenTTL)
			return ti, nil
		}
	}
	return nil, fmt.Errorf("%w: bearer rejected", auth.ErrInvalidToken)
}

// ---------------------------------------------------------------------------
// Health / readiness (fully redacted)
// ---------------------------------------------------------------------------

func (g *Gateway) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// control.Status marshals to exactly {version, gateway, session, cua,
	// paused} -- no paths, users, tokens, process data, or arguments.
	writeJSON(w, http.StatusOK, g.controller.Status(r.Context()))
}

func (g *Gateway) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if g.controller.Status(r.Context()).Ready() {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

// ---------------------------------------------------------------------------
// Tool registration and routing
// ---------------------------------------------------------------------------

// registerTools installs the seven public tools on server, each backed by a
// routing handler. The input/output types are package api's, so the JSON
// schema the SDK infers is byte-for-byte the frozen contract.
func (g *Gateway) registerTools(server *mcp.Server) {
	addRoute[api.ComputerUseInput, api.ComputerUseOutput](server, g, api.ToolComputerUse,
		"Drive the desktop: capture, accessibility, click, double_click, drag, scroll, type, key, wait, list_applications, focus_application.",
		func(o *api.ComputerUseOutput) *api.ResultMeta { return &o.ResultMeta })
	addRoute[api.TerminalInput, api.TerminalOutput](server, g, api.ToolTerminal,
		"Run a single shell command to completion.",
		func(o *api.TerminalOutput) *api.ResultMeta { return &o.ResultMeta })
	addRoute[api.ProcessInput, api.ProcessOutput](server, g, api.ToolProcess,
		"Start, poll, write to, or terminate a long-running background process.",
		func(o *api.ProcessOutput) *api.ResultMeta { return &o.ResultMeta })
	addRoute[api.ReadFileInput, api.ReadFileOutput](server, g, api.ToolReadFile,
		"Read all or part of a file.",
		func(o *api.ReadFileOutput) *api.ResultMeta { return &o.ResultMeta })
	addRoute[api.SearchFilesInput, api.SearchFilesOutput](server, g, api.ToolSearchFiles,
		"Search a directory tree by file name or content.",
		func(o *api.SearchFilesOutput) *api.ResultMeta { return &o.ResultMeta })
	addRoute[api.WriteFileInput, api.WriteFileOutput](server, g, api.ToolWriteFile,
		"Write (or overwrite) a file.",
		func(o *api.WriteFileOutput) *api.ResultMeta { return &o.ResultMeta })
	addRoute[api.PatchInput, api.PatchOutput](server, g, api.ToolPatch,
		"Apply exact-match text replacements to one or more files.",
		func(o *api.PatchOutput) *api.ResultMeta { return &o.ResultMeta })
}

// addRoute registers one tool whose handler routes to the appropriate broker.
// In is used only to build the published input schema (api.ToolFor infers it
// and annotates the contract's closed value sets as JSON Schema enums); the
// raw client arguments are forwarded verbatim. Out carries the tool's typed result and its embedded ResultMeta,
// which meta exposes so the gateway can stamp a synthetic code (paused,
// exhausted, unavailable, ...) without knowing the concrete type.
func addRoute[In, Out any](server *mcp.Server, g *Gateway, tool, desc string, meta func(*Out) *api.ResultMeta) {
	mcp.AddTool(server, api.ToolFor[In](tool, desc),
		func(ctx context.Context, req *mcp.CallToolRequest, _ In) (*mcp.CallToolResult, Out, error) {
			return route(ctx, g, req, tool, meta)
		})
}

// route is the shared per-call pipeline: read credential, enforce pause, bound
// concurrency, choose the broker by class+tool, forward the call (with the raw
// Authorization header for the root helper), cap the response, and audit.
func route[Out any](ctx context.Context, g *Gateway, req *mcp.CallToolRequest, tool string, meta func(*Out) *api.ResultMeta) (*mcp.CallToolResult, Out, error) {
	var out Out
	start := g.now()
	rid := newRequestID()

	cred, tokenID, ok := credentialOf(req)
	if !ok {
		*meta(&out) = api.ResultMeta{Code: api.CodeUnauthenticated, Message: "missing or unusable credential"}
		g.audit(ctx, rid, tokenID, "", tool, g.now().Sub(start), api.CodeUnauthenticated, false, false)
		return nil, out, nil
	}
	class := cred.Class()

	// Emergency pause gate. A paused controller rejects BOTH classes.
	genCtx, release, admitted := g.controller.Enter(ctx)
	if !admitted {
		*meta(&out) = api.ResultMeta{Code: api.CodePaused, Message: "agent is paused"}
		g.audit(ctx, rid, tokenID, class, tool, g.now().Sub(start), api.CodePaused, false, false)
		return nil, out, nil
	}
	defer release()

	// Bounded concurrency: shed load beyond the limit rather than queue.
	if !g.acquire() {
		*meta(&out) = api.ResultMeta{Code: api.CodeResourceExhausted, Message: "too many concurrent calls", Retryable: true}
		g.audit(ctx, rid, tokenID, class, tool, g.now().Sub(start), api.CodeResourceExhausted, false, false)
		return nil, out, nil
	}
	defer g.releaseSem()

	client, authHeader, err := g.pick(class, tool, req)
	if err != nil {
		*meta(&out) = api.ResultMeta{Code: api.CodeInternal, Message: "routing unavailable"}
		g.audit(ctx, rid, tokenID, class, tool, g.now().Sub(start), api.CodeInternal, false, false)
		return nil, out, nil
	}

	// Track in-flight computer_use so the redacted status.json can report
	// computer_use_active. Only computer_use is counted -- never another tool,
	// and never a merely-open MCP connection. The count is incremented around
	// the dispatch and decremented when it returns (success, error, or cancel).
	if tool == api.ToolComputerUse {
		g.computerUse.Add(1)
		defer g.computerUse.Add(-1)
	}

	resp, callErr := client.Call(genCtx, rpc.CallRequest{
		RequestID: rid,
		Tool:      tool,
		Arguments: req.Params.Arguments,
	}, authHeader)
	if callErr != nil {
		code, timedOut := mapCallError(genCtx, callErr)
		*meta(&out) = api.ResultMeta{Code: code, Message: safeMessage(code), Retryable: code == api.CodeSessionUnavailable}
		g.audit(ctx, rid, tokenID, class, tool, g.now().Sub(start), code, timedOut, false)
		return nil, out, nil
	}

	// Cap the serialized response.
	if serializedSize(resp) > g.maxResponseBytes {
		*meta(&out) = api.ResultMeta{Code: api.CodeResourceExhausted, Message: "tool response exceeds the maximum size", Retryable: false}
		g.audit(ctx, rid, tokenID, class, tool, g.now().Sub(start), api.CodeResourceExhausted, false, true)
		return nil, out, nil
	}

	decodeInto(resp, &out)
	code := meta(&out).Code
	g.audit(ctx, rid, tokenID, class, tool, g.now().Sub(start), code, false, false)
	return resp, out, nil
}

// pick selects the broker for (class, tool) and returns the raw Authorization
// header to forward. An admin bearer routes the six privileged OS tools to the
// root helper, forwarding the header verbatim so the helper re-verifies it;
// computer_use and every user-class call go to the session broker with no
// forwarded header.
func (g *Gateway) pick(class appauth.Class, tool string, req *mcp.CallToolRequest) (brokerCaller, string, error) {
	if class == appauth.ClassAdmin && tool != api.ToolComputerUse {
		if g.root == nil {
			return nil, "", errors.New("gateway: no root broker configured")
		}
		return g.root, rawAuthHeader(req), nil
	}
	return g.session, "", nil
}

func (g *Gateway) acquire() bool {
	select {
	case g.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *Gateway) releaseSem() { <-g.sem }

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// audit writes one structured, secret-free log line per call. It records the
// request ID, the token ID (a non-secret digest prefix), the credential class,
// the tool, the duration, the result code, and the timeout/truncation flags.
// It never logs tool arguments, response bodies, file paths, or the bearer.
func (g *Gateway) audit(ctx context.Context, rid, tokenID string, class appauth.Class, tool string, dur time.Duration, code api.ErrorCode, timedOut, truncated bool) {
	result := string(code)
	if result == "" {
		result = "OK"
	}
	g.logger.LogAttrs(ctx, slog.LevelInfo, "mcp call",
		slog.String("request_id", rid),
		slog.String("token_id", tokenID),
		slog.String("class", string(class)),
		slog.String("tool", tool),
		slog.Duration("duration", dur),
		slog.String("code", result),
		slog.Bool("timed_out", timedOut),
		slog.Bool("truncated", truncated),
	)
}

// ---------------------------------------------------------------------------
// Serving
// ---------------------------------------------------------------------------

// Serve serves on l until the server is shut down. It wraps l with a
// connection-count limiter and, unless running in insecure-loopback mode,
// TLS 1.3+.
func (g *Gateway) Serve(l net.Listener) error {
	limited := newLimitListener(l, g.maxConnections)
	srv := &http.Server{Handler: g.handler}
	if g.tlsConfig != nil {
		return srv.Serve(tls.NewListener(limited, g.tlsConfig))
	}
	return srv.Serve(limited)
}

// ListenAndServe binds the configured listen address and serves until shut
// down.
func (g *Gateway) ListenAndServe() error {
	l, err := net.Listen("tcp", g.listenAddr)
	if err != nil {
		return err
	}
	return g.Serve(l)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// credentialOf extracts the authenticated credential and non-secret token ID
// the bearer middleware attached to the request. The token ID is the SDK
// TokenInfo.UserID (a digest prefix), never the bearer itself.
func credentialOf(req *mcp.CallToolRequest) (appauth.Credential, string, bool) {
	if req == nil || req.Extra == nil || req.Extra.TokenInfo == nil {
		return appauth.Credential{}, "", false
	}
	ti := req.Extra.TokenInfo
	raw, ok := ti.Extra["credential"]
	if !ok {
		return appauth.Credential{}, ti.UserID, false
	}
	cred, ok := raw.(appauth.Credential)
	if !ok {
		return appauth.Credential{}, ti.UserID, false
	}
	return cred, ti.UserID, true
}

// rawAuthHeader returns the exact Authorization header the client presented, so
// it can be forwarded to the root helper for independent re-verification.
func rawAuthHeader(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return ""
	}
	return req.Extra.Header.Get("Authorization")
}

// mapCallError classifies a broker call failure into a stable code and whether
// the failure was a timeout. A cancelled generation (emergency pause) surfaces
// as PAUSED; a deadline surfaces as DEADLINE_EXCEEDED; an authorization
// rejection from the root helper surfaces faithfully; anything else is treated
// as a transient session-unavailable condition.
func mapCallError(ctx context.Context, err error) (api.ErrorCode, bool) {
	if cerr := ctx.Err(); cerr != nil {
		if errors.Is(cerr, context.DeadlineExceeded) {
			return api.CodeDeadlineExceeded, true
		}
		return api.CodePaused, false
	}
	var se *rpc.StatusError
	if errors.As(err, &se) {
		switch se.StatusCode {
		case http.StatusUnauthorized:
			return api.CodeUnauthenticated, false
		case http.StatusForbidden:
			return api.CodeForbidden, false
		case http.StatusRequestEntityTooLarge:
			return api.CodeResourceExhausted, false
		default:
			return api.CodeInternal, false
		}
	}
	return api.CodeSessionUnavailable, false
}

// safeMessage returns a fixed, user-safe message for a synthetic code. It never
// echoes an internal error.
func safeMessage(code api.ErrorCode) string {
	switch code {
	case api.CodePaused:
		return "agent is paused"
	case api.CodeDeadlineExceeded:
		return "tool call timed out"
	case api.CodeSessionUnavailable:
		return "the backing session is temporarily unavailable"
	case api.CodeUnauthenticated:
		return "not authenticated for this operation"
	case api.CodeForbidden:
		return "not permitted to perform this operation"
	case api.CodeResourceExhausted:
		return "a resource limit was exceeded"
	default:
		return "the call could not be completed"
	}
}

// decodeInto re-decodes a broker's structured result into the typed Out so the
// SDK can re-validate and re-serialize it against the frozen output schema.
func decodeInto(resp *mcp.CallToolResult, out any) {
	if resp == nil || resp.StructuredContent == nil {
		return
	}
	data, err := json.Marshal(resp.StructuredContent)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, out)
}

// serializedSize reports the byte length of resp serialized as JSON, used to
// enforce the response-size cap.
func serializedSize(resp *mcp.CallToolResult) int {
	data, err := json.Marshal(resp)
	if err != nil {
		return 0
	}
	return len(data)
}

// newRequestID returns a short random hex identifier for one call.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orInt64(v, def int64) int64 {
	if v <= 0 {
		return def
	}
	return v
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

// ---------------------------------------------------------------------------
// Listen-address validation and connection limiting
// ---------------------------------------------------------------------------

// validateListen rejects an unparseable address and, when insecureLoopback is
// set, any address that does not resolve to loopback. --insecure-loopback is
// permitted ONLY for a loopback bind.
func validateListen(addr string, insecureLoopback bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("gateway: invalid listen address %q: %w", addr, err)
	}
	if insecureLoopback && !isLoopbackHost(host) {
		return fmt.Errorf("gateway: --insecure-loopback requires a loopback listen address, got %q", addr)
	}
	return nil
}

// isLoopbackHost reports whether host names the loopback interface. An empty
// host (a wildcard bind such as ":7443") is NOT loopback.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// limitListener wraps a net.Listener and bounds the number of simultaneously
// open connections. Accept blocks once the limit is reached and unblocks as
// connections close.
type limitListener struct {
	net.Listener
	sem chan struct{}
}

func newLimitListener(l net.Listener, max int) net.Listener {
	if max <= 0 {
		max = DefaultMaxConnections
	}
	return &limitListener{Listener: l, sem: make(chan struct{}, max)}
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.sem <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: l.releaseOnce()}, nil
}

func (l *limitListener) releaseOnce() func() {
	var once sync.Once
	return func() { once.Do(func() { <-l.sem }) }
}

type limitConn struct {
	net.Conn
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}
