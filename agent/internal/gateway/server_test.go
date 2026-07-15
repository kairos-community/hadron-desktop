package gateway

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/control"
)

// ---------------------------------------------------------------------------
// Listen-address validation and --insecure-loopback
// ---------------------------------------------------------------------------

func TestValidateListen(t *testing.T) {
	cases := []struct {
		addr             string
		insecureLoopback bool
		wantErr          bool
	}{
		{"0.0.0.0:7443", false, false},
		{"127.0.0.1:7443", true, false},
		{"localhost:7443", true, false},
		{"[::1]:7443", true, false},
		{"0.0.0.0:7443", true, true}, // insecure-loopback on wildcard: rejected
		{":7443", true, true},        // insecure-loopback on empty host: rejected
		{"192.168.1.5:7443", true, true},
		{"not-an-addr", false, true},
	}
	for _, tc := range cases {
		err := validateListen(tc.addr, tc.insecureLoopback)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateListen(%q, %v) err=%v, wantErr=%v", tc.addr, tc.insecureLoopback, err, tc.wantErr)
		}
	}
}

func TestNewRequiresTLSUnlessInsecureLoopback(t *testing.T) {
	creds := newTestCreds(t)
	base := func() Config {
		return Config{
			ListenAddr: "0.0.0.0:7443",
			Session:    startBroker(t, &fakeBroker{healthy: true}, false),
			Verifier:   creds.verifier,
			Controller: newTestController(),
		}
	}

	// No TLS, no insecure-loopback: rejected.
	if _, err := New(base()); err == nil {
		t.Fatal("expected error when TLS absent and not insecure-loopback")
	}

	// Insecure-loopback on a non-loopback address: rejected.
	cfg := base()
	cfg.InsecureLoopback = true
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error for insecure-loopback on non-loopback address")
	}

	// Insecure-loopback on loopback: accepted.
	cfg = base()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.InsecureLoopback = true
	if _, err := New(cfg); err != nil {
		t.Fatalf("insecure-loopback on loopback rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TLS 1.3 minimum
// ---------------------------------------------------------------------------

func TestServeEnforcesTLS13(t *testing.T) {
	creds := newTestCreds(t)
	serverTLS, pool := selfSignedTLS(t)
	g, err := New(Config{
		Version:    "v-test",
		ListenAddr: "127.0.0.1:0",
		TLSConfig:  serverTLS,
		Session:    startBroker(t, &fakeBroker{healthy: true}, false),
		Verifier:   creds.verifier,
		Controller: newTestController(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = g.Serve(l) }()
	t.Cleanup(func() { _ = l.Close() })
	addr := l.Addr().String()

	waitDialable(t, addr)

	// A TLS 1.3 client succeeds against /healthz.
	ok := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}}}
	resp, err := ok.Get("https://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("TLS1.3 client failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz over TLS = %d, want 200", resp.StatusCode)
	}

	// A client capped at TLS 1.2 must be refused (server MinVersion is 1.3).
	bad := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MaxVersion: tls.VersionTLS12}}}
	if _, err := bad.Get("https://" + addr + "/healthz"); err == nil {
		t.Fatal("TLS1.2 client was not refused by a TLS1.3-minimum server")
	}
}

// ---------------------------------------------------------------------------
// Loopback-only development HTTP
// ---------------------------------------------------------------------------

func TestInsecureLoopbackServesPlainHTTP(t *testing.T) {
	creds := newTestCreds(t)
	g, err := New(Config{
		Version:          "v-test",
		ListenAddr:       "127.0.0.1:0",
		InsecureLoopback: true,
		Session:          startBroker(t, &fakeBroker{healthy: true}, false),
		Verifier:         creds.verifier,
		Controller:       newTestController(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = g.Serve(l) }()
	t.Cleanup(func() { _ = l.Close() })
	addr := l.Addr().String()
	waitDialable(t, addr)

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("plain HTTP get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("plain HTTP healthz = %d, want 200", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Body limit (2 MiB)
// ---------------------------------------------------------------------------

func TestBodyLimitRejectsOversized(t *testing.T) {
	f := newFixture(t, func(cfg *Config, _ *fixture) {
		cfg.MaxBodyBytes = 1 << 20 // 1 MiB for a faster test
	})
	l, addr := serveInsecure(t, f.g)
	_ = l

	big := strings.Repeat("A", (1<<20)+4096)
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.creds.userBearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("oversized body accepted with 200")
	}
}

// ---------------------------------------------------------------------------
// Connection limit (limitListener)
// ---------------------------------------------------------------------------

func TestLimitListenerBoundsConnections(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer base.Close()
	const max = 2
	ll := newLimitListener(base, max)

	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := ll.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()

	addr := base.Addr().String()
	// Open `max` connections; the limiter accepts (and returns a server-side
	// conn for) each. Hold onto the server conns: a slot is freed only when a
	// server-side conn is Closed, not when the client disconnects.
	var clientConns, serverConns []net.Conn
	for range max {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		clientConns = append(clientConns, c)
		select {
		case sc := <-accepted:
			serverConns = append(serverConns, sc)
		case <-time.After(time.Second):
			t.Fatal("expected connection to be accepted")
		}
	}

	// A further connection must NOT be accepted until a slot frees.
	extra, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial extra: %v", err)
	}
	clientConns = append(clientConns, extra)
	select {
	case <-accepted:
		t.Fatal("connection accepted beyond the limit")
	case <-time.After(300 * time.Millisecond):
		// expected: blocked at the limit.
	}

	// Close one server-side conn; that frees a slot and the extra is accepted.
	serverConns[0].Close()
	select {
	case sc := <-accepted:
		sc.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("connection not accepted after a slot freed")
	}
	for _, c := range serverConns[1:] {
		c.Close()
	}
	for _, c := range clientConns {
		c.Close()
	}
}

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

func newTestController() *control.Controller {
	return control.New(control.Config{
		Version:        "v-test",
		SessionHealthy: func(context.Context) bool { return true },
		CuaHealthy:     func(context.Context) bool { return true },
	})
}

func waitDialable(t *testing.T, addr string) {
	t.Helper()
	for range 100 {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s never became dialable", addr)
}

func serveInsecure(t *testing.T, g *Gateway) (net.Listener, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = g.Serve(l) }()
	t.Cleanup(func() { _ = l.Close() })
	addr := l.Addr().String()
	waitDialable(t, addr)
	return l, addr
}
