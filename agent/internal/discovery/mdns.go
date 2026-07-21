// Package discovery provides OPTIONAL mDNS/DNS-SD advertisement of
// hadron-agent's MCP endpoint on the local network, so a LAN client can find
// the gateway without being told its address up front.
//
// This package advertises exactly three non-sensitive facts under the
// service "_hadron-agent._tcp" in the "local." domain: the fixed request
// path ("path=/mcp"), that the endpoint requires TLS ("tls=1"), and the
// running binary's build version ("version=..."). It NEVER advertises a
// token ID, a certificate fingerprint, an IP address (zeroconf resolves
// those itself from the interfaces it is given), any path other than
// "/mcp", or the runtime's paused/admin state -- advertising more than
// those three TXT keys would leak operational state to anyone on the LAN
// who can listen for multicast, which is a materially different exposure
// than the gateway's own auth-gated /mcp.
//
// Advertisement is entirely opt-in and lifecycle-controlled by the caller:
// constructing or Start-ing an Advertiser with Enabled == false opens no
// multicast socket at all. A caller that does enable it MUST call Start
// only once the gateway's listener is already live (advertising before the
// port accepts connections would point LAN clients at a socket that is not
// there yet), and MUST call Stop only when the gateway itself is shutting
// down -- not on an ordinary emergency pause, which leaves the listener up
// and should leave the advertisement up too.
//
// This package intentionally does not import internal/provision or
// internal/buildinfo: Config takes the enabled flag, port, and version as
// plain values so callers wire those in from wherever they already have
// them, with no import-cycle risk either way.
//
// The multicast socket lifecycle sits entirely behind Registrar, a narrow
// function seam that RealRegistrar backs with zeroconf.Register. Tests
// inject a fake Registrar so the TXT-content, enable/disable,
// hostname-normalization, and Start/Stop lifecycle logic run hermetically,
// with no real multicast socket, in any CI sandbox.
package discovery

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/grandcat/zeroconf"
)

const (
	// ServiceType is the DNS-SD service type hadron-agent advertises.
	ServiceType = "_hadron-agent._tcp"
	// ServiceDomain is the mDNS domain the service is advertised in.
	ServiceDomain = "local."

	// defaultInstance is the service instance name used when the caller
	// supplies no hostname, or the supplied hostname normalizes to nothing.
	defaultInstance = "hadron-agent"
)

// Server is the minimal lifecycle of a live mDNS registration: shutting
// down the multicast responder and releasing its socket. *zeroconf.Server
// satisfies it.
type Server interface {
	Shutdown()
}

// Registrar opens a multicast mDNS responder for one service instance and
// returns a Server to shut it down later. It is the ONLY place this package
// ever touches a real socket, which makes it the seam tests replace with a
// fake that never opens one.
type Registrar func(instance, service, domain string, port int, text []string, ifaces []net.Interface) (Server, error)

// RealRegistrar is the production Registrar, backed by zeroconf.Register.
func RealRegistrar(instance, service, domain string, port int, text []string, ifaces []net.Interface) (Server, error) {
	return zeroconf.Register(instance, service, domain, port, text, ifaces)
}

// Config constructs an Advertiser.
type Config struct {
	// Enabled gates everything: false means Start is a no-op that opens no
	// multicast socket, matching a disabled endpoint.mdns provisioning
	// setting.
	Enabled bool
	// Port is the CONFIGURED gateway listen port to advertise. It must be
	// the same port the gateway is actually bound to.
	Port int
	// Hostname seeds the service instance name; it is normalized (domain
	// stripped, sanitized) before use. Empty falls back to the OS
	// hostname, and a hostname that normalizes to nothing falls back to
	// "hadron-agent".
	Hostname string
	// Version is the build version advertised under the TXT key "version".
	// Callers pass buildinfo.Version.
	Version string
	// Register opens the multicast responder. Defaults to RealRegistrar;
	// tests inject a fake to stay hermetic.
	Register Registrar
	// Interfaces restricts advertisement to specific network interfaces.
	// Nil (the default) advertises on every interface zeroconf discovers.
	Interfaces []net.Interface
}

// Advertiser is the optional mDNS advertisement of hadron-agent's MCP
// endpoint. Construct it with New and drive its lifecycle with Start and
// Stop; it is safe for concurrent use.
type Advertiser struct {
	enabled  bool
	port     int
	instance string
	version  string
	register Registrar
	ifaces   []net.Interface

	mu     sync.Mutex
	server Server
}

// New constructs an Advertiser from cfg. It never opens a socket itself;
// that only happens on Start, and only when cfg.Enabled is true.
func New(cfg Config) *Advertiser {
	register := cfg.Register
	if register == nil {
		register = RealRegistrar
	}

	hostname := cfg.Hostname
	if hostname == "" {
		if h, err := os.Hostname(); err == nil {
			hostname = h
		}
	}

	return &Advertiser{
		enabled:  cfg.Enabled,
		port:     cfg.Port,
		instance: normalizeInstance(hostname),
		version:  cfg.Version,
		register: register,
		ifaces:   cfg.Interfaces,
	}
}

// Instance reports the normalized service instance name Start advertises
// under. Exported for tests and diagnostics.
func (a *Advertiser) Instance() string { return a.instance }

// Start opens the multicast responder and advertises the configured
// endpoint. It is a no-op -- no socket is opened -- when the Advertiser was
// constructed with Enabled == false. Calling Start again while already
// started is also a no-op: it does not open a second responder and does
// not error. Callers MUST call Start only after the gateway's listener is
// already accepting connections.
func (a *Advertiser) Start(ctx context.Context) error {
	if !a.enabled {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		return nil
	}

	srv, err := a.register(a.instance, ServiceType, ServiceDomain, a.port, a.txtRecords(), a.ifaces)
	if err != nil {
		return fmt.Errorf("discovery: register mdns service: %w", err)
	}
	a.server = srv
	return nil
}

// Stop shuts down the multicast responder, if one is running. It is
// idempotent and safe to call even when Start was never called or the
// Advertiser is disabled. Callers MUST call Stop only when the gateway
// itself is stopping, not for an ordinary emergency pause.
func (a *Advertiser) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server == nil {
		return
	}
	a.server.Shutdown()
	a.server = nil
}

// txtRecords returns EXACTLY the three TXT key=value pairs this package
// ever advertises. Nothing else may be added here: no token ID, no
// certificate fingerprint, no IP address, no path other than "/mcp", no
// admin/paused state.
func (a *Advertiser) txtRecords() []string {
	return []string{
		"path=/mcp",
		"tls=1",
		"version=" + a.version,
	}
}

// normalizeInstance turns an arbitrary hostname (possibly an FQDN, possibly
// containing characters unsafe for a service instance name) into a sane
// mDNS service instance name: the domain is stripped (only the leftmost
// label is kept), and every character outside [A-Za-z0-9-_] is replaced
// with '-'. An empty, all-domain, or all-unsafe input falls back to
// "hadron-agent".
func normalizeInstance(hostname string) string {
	h := strings.TrimSpace(hostname)
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	h = strings.TrimSpace(h)
	if h == "" {
		return defaultInstance
	}

	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return defaultInstance
	}
	return out
}
