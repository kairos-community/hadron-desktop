package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	appauth "github.com/mudler/hadron-desktop/agent/internal/auth"
	"github.com/mudler/hadron-desktop/agent/internal/rpc"
)

// ---------------------------------------------------------------------------
// Bearer + verifier fixtures
// ---------------------------------------------------------------------------

// testCreds is a verifier plus one freshly generated bearer per class.
type testCreds struct {
	verifier    *appauth.Verifier
	userBearer  string
	adminBearer string
}

func newTestCreds(t *testing.T) testCreds {
	t.Helper()
	userBearer, userDigest, err := appauth.Generate(appauth.ClassUser)
	if err != nil {
		t.Fatalf("generate user bearer: %v", err)
	}
	adminBearer, adminDigest, err := appauth.Generate(appauth.ClassAdmin)
	if err != nil {
		t.Fatalf("generate admin bearer: %v", err)
	}
	v, err := appauth.NewVerifier(
		appauth.RotationConfig{Current: userDigest},
		appauth.RotationConfig{Current: adminDigest},
	)
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return testCreds{verifier: v, userBearer: userBearer, adminBearer: adminBearer}
}

// ---------------------------------------------------------------------------
// Fake broker over a real Unix-socket rpc.Server
// ---------------------------------------------------------------------------

type recordedCall struct {
	tool       string
	authHeader string
	requestID  string
}

// fakeBroker is an rpc.Handler that records the calls it received and returns a
// canned success result. Optional gate channels let a test hold calls open to
// exercise concurrency limits.
type fakeBroker struct {
	mu    sync.Mutex
	calls []recordedCall

	// entered, if non-nil, receives one value at the start of every Call.
	entered chan struct{}
	// hold, if non-nil, blocks every Call until it is closed or the call's
	// context is cancelled.
	hold chan struct{}

	// payload, if non-empty, is returned as a "stdout" field in the result's
	// structured content, letting a test inflate the serialized response.
	payload string

	healthy bool
	paused  bool
}

func (b *fakeBroker) Call(ctx context.Context, req rpc.CallRequest, authHeader string) (*rpc.CallResponse, error) {
	if b.entered != nil {
		b.entered <- struct{}{}
	}
	if b.hold != nil {
		select {
		case <-b.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	b.mu.Lock()
	b.calls = append(b.calls, recordedCall{tool: req.Tool, authHeader: authHeader, requestID: req.RequestID})
	b.mu.Unlock()

	raw := json.RawMessage(`{}`)
	if b.payload != "" {
		out, _ := json.Marshal(map[string]string{"stdout": b.payload})
		raw = out
	}
	return &mcp.CallToolResult{
		StructuredContent: raw,
		Content:           []mcp.Content{&mcp.TextContent{Text: string(raw)}},
	}, nil
}

func (b *fakeBroker) Health(context.Context) (rpc.HealthStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return rpc.HealthStatus{OK: b.healthy, Paused: b.paused}, nil
}

func (b *fakeBroker) Pause(context.Context) error  { return nil }
func (b *fakeBroker) Resume(context.Context) error { return nil }

func (b *fakeBroker) recorded() []recordedCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]recordedCall, len(b.calls))
	copy(out, b.calls)
	return out
}

// startBroker serves handler on a fresh Unix socket and returns an rpc.Client
// dialing it. The socket, server, and client are cleaned up with the test.
func startBroker(t *testing.T, handler rpc.Handler, requireAuth bool) *rpc.Client {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "b.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := rpc.NewServer(handler, rpc.Config{RequireAuth: requireAuth})
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
	})
	client := rpc.NewClient(sock)
	t.Cleanup(client.Close)
	return client
}

// ---------------------------------------------------------------------------
// TLS fixtures
// ---------------------------------------------------------------------------

// selfSignedTLS returns a server *tls.Config and a client CertPool trusting it.
func selfSignedTLS(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "hadron-agent-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Config{Certificates: []tls.Certificate{cert}}, pool
}

// ---------------------------------------------------------------------------
// HTTP client that attaches a bearer
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

func bearerClient(bearer string) *http.Client {
	return &http.Client{Transport: bearerRoundTripper{base: http.DefaultTransport, bearer: bearer}}
}

// ---------------------------------------------------------------------------
// MCP client session against a running gateway handler
// ---------------------------------------------------------------------------

// connectMCP starts an httptest server for g.Handler(), connects an MCP client
// presenting bearer, and returns the session. Everything is cleaned up with the
// test.
func connectMCP(t *testing.T, g *Gateway, bearer string) *mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(g.Handler())
	t.Cleanup(ts.Close)

	transport := &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp",
		HTTPClient: bearerClient(bearer),
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// callTool invokes tool with args and returns the structured output decoded
// into a generic map plus the raw result.
func callTool(t *testing.T, session *mcp.ClientSession, tool string, args any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", tool, err)
	}
	return res
}

// structuredCode extracts ResultMeta.Code from a tool result's structured
// content.
func structuredCode(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res.StructuredContent == nil {
		return ""
	}
	data, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var meta struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
	return meta.Code
}
