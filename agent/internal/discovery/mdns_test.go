package discovery

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeServer is a Server that never opens a socket; it just counts how many
// times Shutdown was called, so tests can assert idempotency.
type fakeServer struct {
	shutdowns atomic.Int32
}

func (f *fakeServer) Shutdown() { f.shutdowns.Add(1) }

// registerCall records one invocation of a fake Registrar.
type registerCall struct {
	instance, service, domain string
	port                      int
	text                      []string
	ifaces                    []net.Interface
}

// recordingRegistrar returns a Registrar that records every call it
// receives and returns srv, err. It never opens a real socket.
func recordingRegistrar(srv Server, err error) (Registrar, *int32, func() []registerCall) {
	var mu sync.Mutex
	var calls []registerCall
	var n int32

	reg := func(instance, service, domain string, port int, text []string, ifaces []net.Interface) (Server, error) {
		atomic.AddInt32(&n, 1)
		mu.Lock()
		calls = append(calls, registerCall{instance, service, domain, port, text, ifaces})
		mu.Unlock()
		return srv, err
	}
	snapshot := func() []registerCall {
		mu.Lock()
		defer mu.Unlock()
		out := make([]registerCall, len(calls))
		copy(out, calls)
		return out
	}
	return reg, &n, snapshot
}

// ---------------------------------------------------------------------------
// Enable / disable
// ---------------------------------------------------------------------------

func TestAdvertiser_DisabledOpensNoSocket(t *testing.T) {
	reg, calls, _ := recordingRegistrar(&fakeServer{}, nil)
	a := New(Config{
		Enabled:  false,
		Port:     7443,
		Hostname: "host.example.com",
		Version:  "v1.2.3",
		Register: reg,
	})

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start on disabled advertiser returned error: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("Start on disabled advertiser invoked Register %d times, want 0", got)
	}
	if a.server != nil {
		t.Fatalf("disabled advertiser has a non-nil server after Start")
	}

	// Stop must remain a harmless no-op too.
	a.Stop()
}

func TestAdvertiser_EnabledOpensSocket(t *testing.T) {
	reg, calls, snapshot := recordingRegistrar(&fakeServer{}, nil)
	a := New(Config{
		Enabled:  true,
		Port:     7443,
		Hostname: "host.example.com",
		Version:  "v1.2.3",
		Register: reg,
	})

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("Register invoked %d times, want 1", got)
	}
	got := snapshot()[0]
	if got.service != ServiceType {
		t.Errorf("service = %q, want %q", got.service, ServiceType)
	}
	if got.domain != ServiceDomain {
		t.Errorf("domain = %q, want %q", got.domain, ServiceDomain)
	}
	if got.port != 7443 {
		t.Errorf("port = %d, want 7443", got.port)
	}
}

// ---------------------------------------------------------------------------
// TXT records: exactly the three allowed keys, nothing else.
// ---------------------------------------------------------------------------

func TestAdvertiser_TXTRecordsExact(t *testing.T) {
	reg, _, snapshot := recordingRegistrar(&fakeServer{}, nil)
	a := New(Config{
		Enabled:  true,
		Port:     9999,
		Hostname: "myhost",
		Version:  "v9.9.9",
		Register: reg,
	})

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	want := []string{"path=/mcp", "tls=1", "version=v9.9.9"}
	got := snapshot()[0].text
	if !slices.Equal(got, want) {
		t.Fatalf("TXT records = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Hostname normalization
// ---------------------------------------------------------------------------

func TestNormalizeInstance(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"plain hostname", "myhost", "myhost"},
		{"fqdn strips domain", "myhost.example.com", "myhost"},
		{"fqdn with trailing dot label", "myhost.local", "myhost"},
		{"empty falls back to default", "", defaultInstance},
		{"whitespace only falls back to default", "   ", defaultInstance},
		{"all-dots falls back to default", "....", defaultInstance},
		{"leading/trailing whitespace trimmed", "  myhost  ", "myhost"},
		{"unsafe characters sanitized", "odd host!name#1.local", "odd-host-name-1"},
		{"underscores and hyphens preserved", "my_host-01", "my_host-01"},
		{"leading/trailing separators trimmed after sanitize", "-leading-and-trailing-", "leading-and-trailing"},
		{"case preserved", "MyHost", "MyHost"},
		{"unicode sanitized", "hôte.local", "h-te"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeInstance(tc.input); got != tc.want {
				t.Errorf("normalizeInstance(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestNew_UsesNormalizedHostnameAsInstance(t *testing.T) {
	a := New(Config{Hostname: "some-host.example.com"})
	if got, want := a.Instance(), "some-host"; got != want {
		t.Errorf("Instance() = %q, want %q", got, want)
	}
}

func TestNew_EmptyHostnameFallsBackToOSHostname(t *testing.T) {
	a := New(Config{Hostname: ""})
	if a.Instance() == "" {
		t.Errorf("Instance() is empty, want a non-empty fallback (OS hostname or default)")
	}
}

// ---------------------------------------------------------------------------
// Shutdown: clean, idempotent.
// ---------------------------------------------------------------------------

func TestAdvertiser_StopIsClean(t *testing.T) {
	srv := &fakeServer{}
	reg, _, _ := recordingRegistrar(srv, nil)
	a := New(Config{Enabled: true, Port: 7443, Register: reg})

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	a.Stop()

	if got := srv.shutdowns.Load(); got != 1 {
		t.Fatalf("Shutdown called %d times, want 1", got)
	}
	if a.server != nil {
		t.Fatalf("server field not cleared after Stop")
	}
}

func TestAdvertiser_StopIsIdempotent(t *testing.T) {
	srv := &fakeServer{}
	reg, _, _ := recordingRegistrar(srv, nil)
	a := New(Config{Enabled: true, Port: 7443, Register: reg})

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	a.Stop()
	a.Stop()
	a.Stop()

	if got := srv.shutdowns.Load(); got != 1 {
		t.Fatalf("Shutdown called %d times across repeated Stop calls, want 1", got)
	}
}

func TestAdvertiser_StopWithoutStartIsNoop(t *testing.T) {
	a := New(Config{Enabled: true, Port: 7443})
	a.Stop() // must not panic even though Start was never called
}

// ---------------------------------------------------------------------------
// Duplicate start: idempotent, does not open a second responder.
// ---------------------------------------------------------------------------

func TestAdvertiser_DuplicateStartDoesNotReopen(t *testing.T) {
	reg, calls, _ := recordingRegistrar(&fakeServer{}, nil)
	a := New(Config{Enabled: true, Port: 7443, Register: reg})

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("first Start returned error: %v", err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("second Start returned error: %v", err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("third Start returned error: %v", err)
	}

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("Register invoked %d times across duplicate Start calls, want 1", got)
	}
}

func TestAdvertiser_ConcurrentStartStop(t *testing.T) {
	srv := &fakeServer{}
	reg, calls, _ := recordingRegistrar(srv, nil)
	a := New(Config{Enabled: true, Port: 7443, Register: reg})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.Start(context.Background())
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("Register invoked %d times under concurrent Start, want 1", got)
	}

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.Stop()
		}()
	}
	wg.Wait()

	if got := srv.shutdowns.Load(); got != 1 {
		t.Fatalf("Shutdown called %d times under concurrent Stop, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Start propagates a Registrar error and honors context cancellation.
// ---------------------------------------------------------------------------

func TestAdvertiser_StartPropagatesRegisterError(t *testing.T) {
	wantErr := errors.New("boom")
	reg, _, _ := recordingRegistrar(nil, wantErr)
	a := New(Config{Enabled: true, Port: 7443, Register: reg})

	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("Start returned nil error, want a wrapped Registrar error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Start error = %v, want it to wrap %v", err, wantErr)
	}
	if a.server != nil {
		t.Fatalf("server field set despite a Registrar error")
	}
}

func TestAdvertiser_StartRespectsCancelledContext(t *testing.T) {
	reg, calls, _ := recordingRegistrar(&fakeServer{}, nil)
	a := New(Config{Enabled: true, Port: 7443, Register: reg})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := a.Start(ctx); err == nil {
		t.Fatal("Start with a cancelled context returned nil error")
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("Register invoked %d times despite a cancelled context, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Real registration: attempts a genuine multicast socket and skips
// gracefully when the sandbox forbids it (EACCES, no multicast route, ...).
// These exist purely to exercise RealRegistrar/zeroconf wiring; all the
// enable/disable, TXT-content, normalization, and lifecycle logic above is
// already fully covered hermetically via the fake Registrar.
// ---------------------------------------------------------------------------

func TestRealRegistrar_OpensAndShutsDownRealSocket(t *testing.T) {
	srv, err := RealRegistrar("hadron-agent-test", ServiceType, ServiceDomain, 17443,
		[]string{"path=/mcp", "tls=1", "version=test"}, nil)
	if err != nil {
		t.Skipf("mdns registration unavailable in this environment: %v", err)
	}
	srv.Shutdown()
}

func TestAdvertiser_RealStartStop(t *testing.T) {
	a := New(Config{
		Enabled:  true,
		Port:     17444,
		Hostname: "hadron-agent-test.example.com",
		Version:  "v0.0.0-test",
	})

	if err := a.Start(context.Background()); err != nil {
		t.Skipf("mdns registration unavailable in this environment: %v", err)
	}
	a.Stop()
	a.Stop() // idempotent even against the real zeroconf.Server
}
