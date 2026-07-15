// This file turns a parsed, validated hadron_agent Config (see config.go)
// into the persistent machine state the appliance's services read at boot:
// the gateway's TLS identity and service config, the session broker's config,
// and -- only when an admin bearer is provisioned -- the root helper's config.
//
// Two invariants dominate the design:
//
//   - Every persisted file is written atomically (temp file -> fsync -> rename
//     -> fsync parent dir), so a crash mid-write can only ever leave the old
//     file or the new file, never a truncated one.
//   - A plaintext bearer is NEVER written under the state dir (/var). The only
//     plaintext ever emitted is the one-shot first-run user token, written to
//     the runtime dir (/run, a tmpfs) exactly once when a fresh user bearer is
//     generated; the state dir only ever holds digests.
package provision

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/auth"
)

// Default locations for the persistent state dir and the ephemeral runtime
// dir. Both are overridable (Options) so tests can materialize into t.TempDir.
const (
	// DefaultStateDir is the persistent machine-state root. Its subtrees
	// (gateway/, session/, root/) hold digests and TLS material, never a
	// plaintext bearer.
	DefaultStateDir = "/var/lib/hadron-agent"
	// DefaultRuntimeDir is the tmpfs runtime root. The one-shot plaintext
	// first-run token is the only thing this package writes here.
	DefaultRuntimeDir = "/run/hadron-agent"
)

// certValidity is the lifetime of a generated self-signed certificate: 397
// days, the CA/Browser-Forum maximum for a server certificate.
const certValidity = 397 * 24 * time.Hour

// Owner names and file modes for every persisted file. These are the exact
// values the phase-3 brief mandates.
const (
	ownerRoot    = "root"
	ownerAgent   = "agent"
	ownerGateway = "hadron-agent-gateway"
)

// ---------------------------------------------------------------------------
// Options / Materializer
// ---------------------------------------------------------------------------

// CertGenerator produces a fresh self-signed TLS identity for the given
// hostname and additional (non-loopback) IPs, valid as of now. It returns the
// PEM-encoded certificate and private key. generateSelfSignedCert is the
// production implementation; tests may inject an alternative.
type CertGenerator func(now time.Time, hostname string, extraIPs []net.IP) (certPEM, keyPEM []byte, err error)

// Options configures a Materializer. Every field has a production default
// (filled by NewMaterializer), and every external dependency -- the clock,
// hostname, local addresses, owner resolution, and certificate generation --
// is a seam so tests run hermetically and unprivileged.
type Options struct {
	// StateDir is the persistent state root (default DefaultStateDir).
	StateDir string
	// RuntimeDir is the ephemeral runtime root (default DefaultRuntimeDir).
	RuntimeDir string
	// Chown enables applying the intended owner to each file. Production sets
	// it true when running as root; tests leave it false, in which case the
	// intended owner is still recorded on each PersistedFile but never applied.
	Chown bool

	// Now, Hostname, and LocalIPs are the clock and host identity seams.
	Now      func() time.Time
	Hostname func() (string, error)
	LocalIPs func() ([]net.IP, error)

	// LookupIDs resolves a (user, group) name pair to numeric ids. Only called
	// when Chown is true. Defaults to an os/user-based, cgo-free lookup.
	LookupIDs func(user, group string) (uid, gid int, err error)

	// GenerateCert produces a self-signed identity when neither an inline nor a
	// persisted certificate is available. Defaults to generateSelfSignedCert.
	GenerateCert CertGenerator
}

// Materializer writes and rotates the appliance's machine state.
type Materializer struct {
	opts Options
}

// NewMaterializer returns a Materializer with every unset Option filled with
// its production default.
func NewMaterializer(opts Options) *Materializer {
	if opts.StateDir == "" {
		opts.StateDir = DefaultStateDir
	}
	if opts.RuntimeDir == "" {
		opts.RuntimeDir = DefaultRuntimeDir
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Hostname == nil {
		opts.Hostname = os.Hostname
	}
	if opts.LocalIPs == nil {
		opts.LocalIPs = defaultLocalIPs
	}
	if opts.LookupIDs == nil {
		opts.LookupIDs = defaultLookupIDs
	}
	if opts.GenerateCert == nil {
		opts.GenerateCert = generateSelfSignedCert
	}
	return &Materializer{opts: opts}
}

// ---------------------------------------------------------------------------
// Persisted file records and on-disk config shapes
// ---------------------------------------------------------------------------

// PersistedFile records one file Materialize wrote: its absolute path, mode,
// intended owner (recorded even when Chown was skipped), and whether the chown
// was actually applied.
type PersistedFile struct {
	Path    string
	Mode    os.FileMode
	User    string
	Group   string
	Chowned bool
}

// Result summarizes a Materialize call.
type Result struct {
	// CertFingerprint is the "sha256:<hex>" fingerprint of the served
	// certificate's DER, suitable for redacted status display.
	CertFingerprint string
	// TokenWritten reports whether a one-shot plaintext first-run token was
	// written (true only when a fresh user bearer was generated this call).
	TokenWritten bool
	// TokenPath is the runtime path of the plaintext token when TokenWritten.
	TokenPath string
	// AdminEnabled reports whether an admin digest was provisioned (root files
	// written).
	AdminEnabled bool
	// Files lists every persisted file with its intended owner and mode.
	Files []PersistedFile
}

// Rotation is the on-disk representation of one bearer class's rotation
// state, mirroring auth.RotationConfig: a current digest plus an optional
// previous digest accepted until previous_valid_until (RFC3339). It is the
// single shape both the provision writer (Materialize/Rotate) and the service
// reader (LoadGatewayConfig/LoadRootConfig) share, so the on-disk JSON can
// never drift between them. See AuthRotation for converting it into the
// auth.RotationConfig the gateway/root-helper verifier is built from.
type Rotation struct {
	Current            string `json:"current,omitempty"`
	Previous           string `json:"previous,omitempty"`
	PreviousValidUntil string `json:"previous_valid_until,omitempty"`
}

// ServiceLimits is the on-disk concurrency/size bound shared by the gateway and
// session config files.
type ServiceLimits struct {
	MaxConcurrentCalls int `json:"max_concurrent_calls"`
	MaxProcesses       int `json:"max_processes"`
	MaxRequestBytes    int `json:"max_request_bytes"`
}

// GatewayConfig is /var/lib/hadron-agent/gateway/config.json: everything the
// public gateway process needs. It carries only digests, never bearers. The
// gateway service loads it via LoadGatewayConfig at startup.
type GatewayConfig struct {
	Listen           string        `json:"listen"`
	InsecureLoopback bool          `json:"insecure_loopback"`
	MDNS             bool          `json:"mdns"`
	TLSCertPath      string        `json:"tls_cert_path"`
	TLSKeyPath       string        `json:"tls_key_path"`
	CertFingerprint  string        `json:"cert_fingerprint"`
	User             Rotation      `json:"user"`
	Admin            Rotation      `json:"admin"`
	Limits           ServiceLimits `json:"limits"`
}

// SessionConfig is /var/lib/hadron-agent/session/config.json: the unprivileged
// broker's limits. It holds no credentials.
type SessionConfig struct {
	Limits ServiceLimits `json:"limits"`
}

// RootConfig is /var/lib/hadron-agent/root/config.json: the admin digest the
// root helper independently re-verifies. Written only when admin is enabled.
type RootConfig struct {
	Admin Rotation `json:"admin"`
}

// ---------------------------------------------------------------------------
// Materialize
// ---------------------------------------------------------------------------

// Materialize writes the full machine state for cfg and returns a Result. It
// is idempotent: on a re-run over persisted state it reuses the existing user
// digest and TLS identity (never rotating trust) and does not recreate the
// one-shot plaintext token. A fresh user bearer -- hence a plaintext token --
// is generated only when no user digest is provisioned in cfg and none is
// already persisted.
func (m *Materializer) Materialize(cfg Config) (Result, error) {
	stateDir := m.opts.StateDir
	gatewayDir := filepath.Join(stateDir, "gateway")

	existing, err := readGatewayConfigOrNil(filepath.Join(gatewayDir, "config.json"))
	if err != nil {
		return Result{}, fmt.Errorf("provision: %w", err)
	}

	certPEM, keyPEM, err := m.resolveTLS(cfg, gatewayDir)
	if err != nil {
		return Result{}, fmt.Errorf("provision: resolve tls: %w", err)
	}
	fingerprint, err := certFingerprint(certPEM)
	if err != nil {
		return Result{}, fmt.Errorf("provision: %w", err)
	}

	userRot, plaintext, err := resolveUserRotation(cfg, existing)
	if err != nil {
		return Result{}, fmt.Errorf("provision: %w", err)
	}
	adminRot, adminEnabled := resolveAdminRotation(cfg, existing)

	limits := ServiceLimits{
		MaxConcurrentCalls: cfg.Limits.MaxConcurrentCalls,
		MaxProcesses:       cfg.Limits.MaxProcesses,
		MaxRequestBytes:    cfg.Limits.MaxRequestBytes,
	}

	gwCfg := GatewayConfig{
		Listen:           cfg.Endpoint.Listen,
		InsecureLoopback: cfg.Endpoint.InsecureLoopback,
		MDNS:             cfg.Endpoint.MDNS,
		TLSCertPath:      filepath.Join(gatewayDir, "tls.crt"),
		TLSKeyPath:       filepath.Join(gatewayDir, "tls.key"),
		CertFingerprint:  fingerprint,
		User:             userRot,
		Admin:            adminRot,
		Limits:           limits,
	}
	gwJSON, err := marshalJSON(gwCfg)
	if err != nil {
		return Result{}, fmt.Errorf("provision: marshal gateway config: %w", err)
	}
	sessJSON, err := marshalJSON(SessionConfig{Limits: limits})
	if err != nil {
		return Result{}, fmt.Errorf("provision: marshal session config: %w", err)
	}

	// TLS material is written before config.json so the paths config.json
	// references already exist once the gateway reads it.
	specs := []fileSpec{
		{path: filepath.Join(gatewayDir, "tls.crt"), data: certPEM, mode: 0o440, user: ownerRoot, group: ownerGateway},
		{path: filepath.Join(gatewayDir, "tls.key"), data: keyPEM, mode: 0o440, user: ownerRoot, group: ownerGateway},
		{path: filepath.Join(gatewayDir, "config.json"), data: gwJSON, mode: 0o440, user: ownerRoot, group: ownerGateway},
		{path: filepath.Join(stateDir, "session", "config.json"), data: sessJSON, mode: 0o440, user: ownerRoot, group: ownerAgent},
	}
	if adminEnabled {
		rootJSON, err := marshalJSON(RootConfig{Admin: adminRot})
		if err != nil {
			return Result{}, fmt.Errorf("provision: marshal root config: %w", err)
		}
		specs = append(specs,
			fileSpec{path: filepath.Join(stateDir, "root", "config.json"), data: rootJSON, mode: 0o400, user: ownerRoot, group: ownerRoot},
			fileSpec{path: filepath.Join(stateDir, "root", "enabled"), data: []byte("1\n"), mode: 0o400, user: ownerRoot, group: ownerRoot},
		)
	}

	res := Result{CertFingerprint: fingerprint, AdminEnabled: adminEnabled}
	for _, spec := range specs {
		rec, err := m.writeFile(spec)
		if err != nil {
			return Result{}, fmt.Errorf("provision: %w", err)
		}
		res.Files = append(res.Files, rec)
	}

	// The one-shot plaintext token is written LAST and only into the runtime
	// (tmpfs) dir, never under the state dir.
	if plaintext != "" {
		tokenPath := filepath.Join(m.opts.RuntimeDir, "first-run-token")
		rec, err := m.writeFile(fileSpec{
			path: tokenPath, data: []byte(plaintext + "\n"), mode: 0o400, user: ownerAgent, group: ownerAgent,
		})
		if err != nil {
			return Result{}, fmt.Errorf("provision: write first-run token: %w", err)
		}
		res.TokenWritten = true
		res.TokenPath = tokenPath
		res.Files = append(res.Files, rec)
	}

	return res, nil
}

// resolveUserRotation decides the user rotation state to persist and whether a
// fresh plaintext bearer must be emitted:
//   - a CI-provided digest (cfg) is authoritative: use it, emit nothing;
//   - otherwise an already-persisted digest is reused as-is, emitting nothing
//     (installed reuse / idempotent re-run);
//   - otherwise a fresh 256-bit hdn_u_ bearer is generated: its digest is
//     persisted and its plaintext returned for the one-shot token.
func resolveUserRotation(cfg Config, existing *GatewayConfig) (Rotation, string, error) {
	if cfg.Auth.UserTokenHash != "" {
		return rotationFromConfig(cfg.Auth.UserTokenHash, cfg.Auth.UserPreviousTokenHash, cfg.Auth.UserPreviousValidUntil), "", nil
	}
	if existing != nil && existing.User.Current != "" {
		return existing.User, "", nil
	}
	bearer, digest, err := auth.Generate(auth.ClassUser)
	if err != nil {
		return Rotation{}, "", fmt.Errorf("generate user bearer: %w", err)
	}
	return Rotation{Current: string(digest)}, bearer, nil
}

// resolveAdminRotation decides the admin rotation state. Admin is never
// auto-generated: it is enabled only by a CI-provided digest or a
// previously-persisted one.
func resolveAdminRotation(cfg Config, existing *GatewayConfig) (Rotation, bool) {
	if cfg.Auth.AdminTokenHash != "" {
		return rotationFromConfig(cfg.Auth.AdminTokenHash, cfg.Auth.AdminPreviousTokenHash, cfg.Auth.AdminPreviousValidUntil), true
	}
	if existing != nil && existing.Admin.Current != "" {
		return existing.Admin, true
	}
	return Rotation{}, false
}

// rotationFromConfig builds a Rotation from Config's string+time fields.
func rotationFromConfig(current, previous string, validUntil time.Time) Rotation {
	r := Rotation{Current: current, Previous: previous}
	if !validUntil.IsZero() {
		r.PreviousValidUntil = validUntil.UTC().Format(time.RFC3339)
	}
	return r
}

// resolveTLS returns the certificate and key PEM to persist: an inline pair
// from cfg if present, else the already-persisted pair (reused so a later boot
// never rotates trust), else a freshly generated self-signed identity.
func (m *Materializer) resolveTLS(cfg Config, gatewayDir string) (certPEM, keyPEM []byte, err error) {
	if cfg.TLS.CertificatePEM != "" || cfg.TLS.PrivateKeyPEM != "" {
		return []byte(cfg.TLS.CertificatePEM), []byte(cfg.TLS.PrivateKeyPEM), nil
	}
	crt, errCrt := os.ReadFile(filepath.Join(gatewayDir, "tls.crt"))
	key, errKey := os.ReadFile(filepath.Join(gatewayDir, "tls.key"))
	if errCrt == nil && errKey == nil && len(crt) > 0 && len(key) > 0 {
		return crt, key, nil
	}
	hostname, err := m.opts.Hostname()
	if err != nil {
		return nil, nil, fmt.Errorf("hostname: %w", err)
	}
	ips, err := m.opts.LocalIPs()
	if err != nil {
		return nil, nil, fmt.Errorf("local ips: %w", err)
	}
	return m.opts.GenerateCert(m.opts.Now(), hostname, ips)
}

// ---------------------------------------------------------------------------
// Rotate
// ---------------------------------------------------------------------------

// RotateOptions configures a bearer rotation.
type RotateOptions struct {
	// Class selects which bearer to rotate (auth.ClassUser or auth.ClassAdmin).
	Class auth.Class
	// Overlap is how long the outgoing digest stays valid. Must not exceed
	// auth.MaxRotationOverlap (24h); zero means the old bearer is invalidated
	// immediately.
	Overlap time.Duration
	// TTY, when non-nil, is the controlling terminal to print the new bearer
	// to. When nil (no TTY), the bearer is never printed anywhere -- the
	// rotation still happens, but the operator must read the new digest from
	// state instead. This is what keeps a secret out of the journal/stdout.
	TTY interface{ Write([]byte) (int, error) }
	// Reload is invoked after the new digest is persisted, to make the running
	// services adopt it. A non-nil error triggers a full rollback of the config
	// files before Rotate returns.
	Reload func() error
}

// RotateResult summarizes a rotation.
type RotateResult struct {
	// Digest is the digest of the new bearer (safe to log).
	Digest auth.Digest
	// Printed reports whether the new bearer was written to a TTY.
	Printed bool
}

// Rotate generates a fresh bearer for the given class, records the outgoing
// digest as the bounded-overlap previous, persists the new state atomically,
// reloads the services, and only then -- and only to a TTY -- prints the new
// bearer. If the reload fails, every config file it touched is rolled back to
// its prior contents and the new bearer is never printed.
func (m *Materializer) Rotate(opts RotateOptions) (RotateResult, error) {
	if opts.Overlap < 0 {
		return RotateResult{}, errors.New("provision: rotate overlap must not be negative")
	}
	if opts.Overlap > auth.MaxRotationOverlap {
		return RotateResult{}, fmt.Errorf("provision: rotate overlap %s exceeds maximum %s", opts.Overlap, auth.MaxRotationOverlap)
	}
	if opts.Class != auth.ClassUser && opts.Class != auth.ClassAdmin {
		return RotateResult{}, fmt.Errorf("provision: rotate unknown bearer class %q", opts.Class)
	}

	gatewayPath := filepath.Join(m.opts.StateDir, "gateway", "config.json")
	gw, err := readGatewayConfigOrNil(gatewayPath)
	if err != nil {
		return RotateResult{}, fmt.Errorf("provision: rotate: %w", err)
	}
	if gw == nil {
		return RotateResult{}, fmt.Errorf("provision: rotate: no gateway config at %s (run provision first)", gatewayPath)
	}

	bearer, digest, err := auth.Generate(opts.Class)
	if err != nil {
		return RotateResult{}, fmt.Errorf("provision: rotate generate: %w", err)
	}

	validUntil := ""
	if opts.Overlap > 0 {
		validUntil = m.opts.Now().Add(opts.Overlap).UTC().Format(time.RFC3339)
	}

	// Build the new config content and the file writes to perform.
	var specs []fileSpec
	switch opts.Class {
	case auth.ClassUser:
		gw.User = makeRotation(gw.User, string(digest), validUntil, opts.Overlap)
	case auth.ClassAdmin:
		if gw.Admin.Current == "" {
			return RotateResult{}, errors.New("provision: rotate admin: no admin bearer is provisioned")
		}
		gw.Admin = makeRotation(gw.Admin, string(digest), validUntil, opts.Overlap)
	}
	gwJSON, err := marshalJSON(*gw)
	if err != nil {
		return RotateResult{}, fmt.Errorf("provision: rotate marshal gateway: %w", err)
	}
	specs = append(specs, fileSpec{path: gatewayPath, data: gwJSON, mode: 0o440, user: ownerRoot, group: ownerGateway})

	if opts.Class == auth.ClassAdmin {
		rootJSON, err := marshalJSON(RootConfig{Admin: gw.Admin})
		if err != nil {
			return RotateResult{}, fmt.Errorf("provision: rotate marshal root: %w", err)
		}
		specs = append(specs, fileSpec{
			path: filepath.Join(m.opts.StateDir, "root", "config.json"), data: rootJSON, mode: 0o400, user: ownerRoot, group: ownerRoot,
		})
	}

	// Snapshot the current contents so a failed reload can be rolled back.
	snaps := make([]snapshot, len(specs))
	for i, spec := range specs {
		data, statErr := os.ReadFile(spec.path)
		snaps[i] = snapshot{spec: spec, existed: statErr == nil, data: data}
	}

	for _, spec := range specs {
		if _, err := m.writeFile(spec); err != nil {
			// Best-effort rollback of anything already written this call.
			m.rollback(snaps)
			return RotateResult{}, fmt.Errorf("provision: rotate write: %w", err)
		}
	}

	if opts.Reload != nil {
		if err := opts.Reload(); err != nil {
			m.rollback(snaps)
			return RotateResult{}, fmt.Errorf("provision: rotate reload failed, rolled back: %w", err)
		}
	}

	// Only now, after a successful reload, and only to a real TTY, reveal the
	// new bearer.
	printed := false
	if opts.TTY != nil {
		fmt.Fprintf(opts.TTY, "New %s bearer (shown once, store it now):\n%s\n", opts.Class, bearer)
		printed = true
	}
	return RotateResult{Digest: digest, Printed: printed}, nil
}

// makeRotation builds the new rotation state: the new digest becomes current,
// and the old current becomes the bounded-overlap previous (only when there is
// a nonzero overlap and an outgoing digest to keep alive).
func makeRotation(old Rotation, newDigest, validUntil string, overlap time.Duration) Rotation {
	r := Rotation{Current: newDigest}
	if overlap > 0 && old.Current != "" {
		r.Previous = old.Current
		r.PreviousValidUntil = validUntil
	}
	return r
}

// snapshot captures a file's pre-rotation state for rollback.
type snapshot struct {
	spec    fileSpec
	existed bool
	data    []byte
}

// rollback restores every snapshot: rewriting files that existed and removing
// files that did not. It is best-effort; a rollback error cannot itself be
// surfaced meaningfully, so it is intentionally ignored.
func (m *Materializer) rollback(snaps []snapshot) {
	for _, s := range snaps {
		if s.existed {
			restore := s.spec
			restore.data = s.data
			_, _ = m.writeFile(restore)
		} else {
			_ = os.Remove(s.spec.path)
		}
	}
}

// ---------------------------------------------------------------------------
// Atomic file writes
// ---------------------------------------------------------------------------

// fileSpec is one file to persist with its intended owner and mode.
type fileSpec struct {
	path  string
	data  []byte
	mode  os.FileMode
	user  string
	group string
}

// writeFile writes spec atomically: a temp file in the destination directory
// is written, chmod'd, optionally chown'd, fsync'd, then renamed over the
// destination, after which the parent directory is fsync'd so the rename is
// durable. Any failure removes the temp file, so a partial write is never left
// behind. The returned PersistedFile records the intended owner even when
// Chown is disabled.
func (m *Materializer) writeFile(spec fileSpec) (PersistedFile, error) {
	dir := filepath.Dir(spec.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return PersistedFile{}, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".provision-*.tmp")
	if err != nil {
		return PersistedFile{}, fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// remove is set to a no-op once the temp file has been renamed away.
	remove := func() { _ = os.Remove(tmpName) }
	defer func() { remove() }()

	if _, err := tmp.Write(spec.data); err != nil {
		_ = tmp.Close()
		return PersistedFile{}, fmt.Errorf("write %s: %w", spec.path, err)
	}
	if err := tmp.Chmod(spec.mode); err != nil {
		_ = tmp.Close()
		return PersistedFile{}, fmt.Errorf("chmod %s: %w", spec.path, err)
	}

	chowned := false
	if m.opts.Chown {
		uid, gid, err := m.opts.LookupIDs(spec.user, spec.group)
		if err != nil {
			_ = tmp.Close()
			return PersistedFile{}, fmt.Errorf("resolve owner %s:%s for %s: %w", spec.user, spec.group, spec.path, err)
		}
		if err := tmp.Chown(uid, gid); err != nil {
			_ = tmp.Close()
			return PersistedFile{}, fmt.Errorf("chown %s: %w", spec.path, err)
		}
		chowned = true
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return PersistedFile{}, fmt.Errorf("fsync %s: %w", spec.path, err)
	}
	if err := tmp.Close(); err != nil {
		return PersistedFile{}, fmt.Errorf("close %s: %w", spec.path, err)
	}

	if err := os.Rename(tmpName, spec.path); err != nil {
		return PersistedFile{}, fmt.Errorf("rename into %s: %w", spec.path, err)
	}
	remove = func() {} // renamed away; nothing to clean up.

	if err := fsyncDir(dir); err != nil {
		return PersistedFile{}, fmt.Errorf("fsync dir %s: %w", dir, err)
	}

	return PersistedFile{Path: spec.path, Mode: spec.mode, User: spec.user, Group: spec.group, Chowned: chowned}, nil
}

// fsyncDir flushes a directory entry (the rename) to stable storage.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// marshalJSON renders v as indented JSON with a trailing newline.
func marshalJSON(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// readGatewayConfigOrNil reads and parses a gateway config.json. It returns
// (nil, nil) only when the file is genuinely absent (os.IsNotExist: true
// first boot, generating a fresh user bearer is correct). Any other read
// error, or a parse failure on content that does exist, is a corrupt or
// truncated config -- NOT the same as absent -- and is returned as a hard
// error so the caller fails the provision instead of treating it as first
// boot. Silently falling back to nil here would cause resolveUserRotation to
// mint a brand-new user bearer, invalidating the one already issued to the
// user: exactly the "never silently rotate trust" hazard this package
// protects the TLS identity from, extended to the user credential.
func readGatewayConfigOrNil(path string) (*GatewayConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read gateway config %s: %w", path, err)
	}
	var gw GatewayConfig
	if err := json.Unmarshal(data, &gw); err != nil {
		return nil, fmt.Errorf("gateway config %s is corrupt: %w", path, err)
	}
	return &gw, nil
}

// ---------------------------------------------------------------------------
// TLS generation and fingerprinting
// ---------------------------------------------------------------------------

// generateSelfSignedCert creates an ECDSA P-256 self-signed certificate valid
// for 397 days as of now, with SANs localhost, 127.0.0.1, ::1, the hostname,
// <hostname>.local, and every supplied non-loopback IP. It returns the cert
// and key as PEM.
func generateSelfSignedCert(now time.Time, hostname string, extraIPs []net.IP) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	dnsNames := []string{"localhost"}
	if hostname != "" && hostname != "localhost" {
		dnsNames = append(dnsNames, hostname, hostname+".local")
	}

	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	for _, ip := range extraIPs {
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if containsIP(ips, ip) {
			continue
		}
		ips = append(ips, ip)
	}

	cn := hostname
	if cn == "" {
		cn = "hadron-agent"
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now,
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

func containsIP(list []net.IP, ip net.IP) bool {
	for _, x := range list {
		if x.Equal(ip) {
			return true
		}
	}
	return false
}

// certFingerprint returns the "sha256:<hex>" fingerprint of the DER bytes of
// the first CERTIFICATE block in certPEM.
func certFingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", errors.New("no PEM certificate to fingerprint")
	}
	sum := sha256.Sum256(block.Bytes)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ---------------------------------------------------------------------------
// Default host/owner seams (production)
// ---------------------------------------------------------------------------

// defaultLocalIPs returns every non-loopback IP currently assigned to a local
// interface.
func defaultLocalIPs() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() {
			continue
		}
		ips = append(ips, ip)
	}
	return ips, nil
}

// defaultLookupIDs resolves a user and group name to numeric ids using
// os/user, which is cgo-free (parses /etc/passwd and /etc/group) under
// CGO_ENABLED=0.
func defaultLookupIDs(username, group string) (int, int, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return 0, 0, fmt.Errorf("lookup user %q: %w", username, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse uid %q: %w", u.Uid, err)
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return 0, 0, fmt.Errorf("lookup group %q: %w", group, err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse gid %q: %w", g.Gid, err)
	}
	return uid, gid, nil
}
