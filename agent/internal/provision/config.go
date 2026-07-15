// Package provision parses and validates the hadron_agent provisioning
// section merged from Kairos OEM /oem/*.yaml cloud-config files.
//
// hadron-agent ships as a separate, opt-in appliance ISO: choosing that ISO
// is itself the opt-in, so an ABSENT hadron_agent section resolves to an
// enabled configuration with the documented defaults, and only an explicit
// "hadron_agent.enabled: false" suppresses the runtime. Ordinary Kairos
// images carry no hadron_agent key and no provisioning service at all, so
// this package's default can never affect them.
//
// This package only parses, merges, defaults, and validates the schema. It
// does not read TLS material from disk, generate machine state, or start
// anything; later packages consume the Config it returns.
package provision

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mudler/hadron-desktop/agent/internal/auth"
)

// Defaults for the fields of a hadron_agent section left unset across every
// merged /oem/*.yaml file.
const (
	// DefaultListen is the endpoint.listen default: all interfaces, the
	// fixed MCP port also used as internal/gateway's DefaultListenAddr.
	DefaultListen = "0.0.0.0:7443"
	// DefaultMaxConcurrentCalls is the limits.max_concurrent_calls default.
	DefaultMaxConcurrentCalls = 16
	// DefaultMaxProcesses is the limits.max_processes default.
	DefaultMaxProcesses = 16
	// DefaultMaxRequestBytes is the limits.max_request_bytes default (2 MiB).
	DefaultMaxRequestBytes = 2 << 20
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// Config is the fully merged, defaulted, and validated hadron_agent
// provisioning configuration.
type Config struct {
	// Enabled reports whether the appliance's agent runtime should start at
	// all. It defaults to true: choosing the agent ISO is the opt-in, so an
	// absent hadron_agent section (or one that never sets "enabled") still
	// resolves to Enabled == true with every other field at its documented
	// default.
	//
	// When Enabled is false -- which only happens when some /oem/*.yaml file
	// explicitly sets "hadron_agent.enabled: false" -- callers MUST treat
	// this Config as fully suppressed: skip starting the gateway, the
	// session and root brokers, the root-helper, autologin, and the
	// first-run onboarding panel. The rest of this Config's fields are still
	// parsed and defaulted normally when Enabled is false, but callers must
	// not act on them; Enabled is the single field that gates everything
	// else.
	Enabled bool

	Endpoint Endpoint
	Auth     Auth
	TLS      TLS
	Limits   Limits
}

// Endpoint configures where and how the gateway listens.
type Endpoint struct {
	// Listen is the host:port the gateway binds. Defaults to DefaultListen.
	Listen string
	// MDNS enables mDNS/DNS-SD advertisement of the endpoint on the local
	// network. Defaults to true.
	MDNS bool
	// InsecureLoopback permits serving plain HTTP instead of HTTPS. Valid
	// ONLY when Listen resolves to the loopback interface; Validate rejects
	// any other combination. Defaults to false.
	InsecureLoopback bool
}

// Auth configures the bearer digests the gateway accepts, mirroring
// internal/auth's Digest format ("sha256:" + 64 lowercase hex) and
// RotationConfig's bounded-overlap rotation model for each of the two
// bearer classes (user, admin).
type Auth struct {
	// UserTokenHash is the digest of the currently active user bearer.
	// Empty means the user class is not yet configured.
	UserTokenHash string
	// UserPreviousTokenHash, if non-empty, is the digest of a user bearer
	// being rotated out; it remains acceptable until UserPreviousValidUntil.
	UserPreviousTokenHash string
	// UserPreviousValidUntil is the instant UserPreviousTokenHash stops
	// being accepted. Zero when UserPreviousTokenHash is empty.
	UserPreviousValidUntil time.Time

	// AdminTokenHash, AdminPreviousTokenHash, and AdminPreviousValidUntil
	// are the admin-class equivalents of the three fields above.
	AdminTokenHash          string
	AdminPreviousTokenHash  string
	AdminPreviousValidUntil time.Time
}

// TLS carries inline PEM-encoded TLS material. Both fields must be present
// together or both absent; Validate rejects exactly one being set. Inline
// TLS is intended only for permission-restricted seed configuration -- this
// package does not read or write certificate files.
type TLS struct {
	CertificatePEM string
	PrivateKeyPEM  string
}

// Limits bounds the gateway's concurrency and request size.
type Limits struct {
	// MaxConcurrentCalls bounds how many tool calls execute at once.
	// Defaults to DefaultMaxConcurrentCalls.
	MaxConcurrentCalls int
	// MaxProcesses bounds how many processes the agent may run at once.
	// Defaults to DefaultMaxProcesses.
	MaxProcesses int
	// MaxRequestBytes bounds the size of any single MCP request body.
	// Defaults to DefaultMaxRequestBytes.
	MaxRequestBytes int
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

// Load reads every *.yaml file directly inside oemDir in sorted filename
// order, merges their hadron_agent sections (later files win for scalars;
// see the package doc and mergeSecret for the stricter rule that applies to
// auth digest fields), applies defaults to anything no file set, and
// validates the fully resolved result.
//
// oemDir need not exist and may contain no matching files at all -- both
// cases resolve to the opt-in default (Config.Enabled == true with every
// other field at its documented default), matching the "absent
// hadron_agent section" case described in the package doc.
func Load(oemDir string) (Config, error) {
	paths, err := findYAMLFiles(oemDir)
	if err != nil {
		return Config{}, fmt.Errorf("provision: %w", err)
	}

	acc := &accumulator{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("provision: read %s: %w", path, err)
		}
		ra, present, err := decodeHadronAgent(data)
		if err != nil {
			return Config{}, fmt.Errorf("provision: %s: %w", filepath.Base(path), err)
		}
		if !present {
			continue
		}
		if err := acc.merge(ra); err != nil {
			return Config{}, fmt.Errorf("provision: %s: %w", filepath.Base(path), err)
		}
	}

	cfg, err := acc.resolve()
	if err != nil {
		return Config{}, fmt.Errorf("provision: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("provision: %w", err)
	}
	return cfg, nil
}

// findYAMLFiles returns every "*.yaml" file directly inside dir, sorted by
// filename. A missing directory is not an error: it yields no files, which
// Load resolves to the opt-in default.
func findYAMLFiles(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("glob %s: %w", dir, err)
	}
	sort.Strings(matches)
	return matches, nil
}

// decodeHadronAgent loosely decodes data (a complete Kairos cloud-config
// document) to isolate its top-level "hadron_agent" node -- every other
// Kairos key (users, stages, ...) is ignored simply by not appearing in the
// decode target, so it can never trip strict field checking. If a
// hadron_agent node is present, decodeHadronAgent then re-encodes just that
// node and decodes it AGAIN, this time with KnownFields(true), so unknown
// fields INSIDE hadron_agent are rejected while unrelated top-level Kairos
// keys never were part of that second, strict decode at all.
func decodeHadronAgent(data []byte) (rawAgent, bool, error) {
	var doc struct {
		HadronAgent yaml.Node `yaml:"hadron_agent"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return rawAgent{}, false, fmt.Errorf("parse yaml: %w", err)
	}
	if doc.HadronAgent.Kind == 0 {
		// The document has no hadron_agent key at all.
		return rawAgent{}, false, nil
	}

	sub, err := yaml.Marshal(&doc.HadronAgent)
	if err != nil {
		return rawAgent{}, false, fmt.Errorf("re-encode hadron_agent: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(sub))
	dec.KnownFields(true)
	var ra rawAgent
	if err := dec.Decode(&ra); err != nil {
		return rawAgent{}, false, fmt.Errorf("hadron_agent: %w", err)
	}
	return ra, true, nil
}

// ---------------------------------------------------------------------------
// Raw (per-file) decode shape
// ---------------------------------------------------------------------------

// rawAgent is the strict per-file decode target for a single file's
// hadron_agent node. Every leaf is a pointer so the merge step (accumulator.
// merge) can tell "this file did not mention the field" (nil) apart from
// "this file set the field to its zero value" (non-nil pointer to zero
// value), which matters for the last-defined-wins merge rule.
type rawAgent struct {
	Enabled  *bool       `yaml:"enabled"`
	Endpoint rawEndpoint `yaml:"endpoint"`
	Auth     rawAuth     `yaml:"auth"`
	TLS      rawTLS      `yaml:"tls"`
	Limits   rawLimits   `yaml:"limits"`
}

type rawEndpoint struct {
	Listen           *string `yaml:"listen"`
	MDNS             *bool   `yaml:"mdns"`
	InsecureLoopback *bool   `yaml:"insecure_loopback"`
}

type rawAuth struct {
	UserTokenHash           *string `yaml:"user_token_hash"`
	UserPreviousTokenHash   *string `yaml:"user_previous_token_hash"`
	UserPreviousValidUntil  *string `yaml:"user_previous_valid_until"`
	AdminTokenHash          *string `yaml:"admin_token_hash"`
	AdminPreviousTokenHash  *string `yaml:"admin_previous_token_hash"`
	AdminPreviousValidUntil *string `yaml:"admin_previous_valid_until"`
}

type rawTLS struct {
	CertificatePEM *string `yaml:"certificate_pem"`
	PrivateKeyPEM  *string `yaml:"private_key_pem"`
}

type rawLimits struct {
	MaxConcurrentCalls *int `yaml:"max_concurrent_calls"`
	MaxProcesses       *int `yaml:"max_processes"`
	MaxRequestBytes    *int `yaml:"max_request_bytes"`
}

// ---------------------------------------------------------------------------
// Merge accumulator
// ---------------------------------------------------------------------------

// accumulator holds the merged-so-far value of every hadron_agent leaf
// across all files processed. A nil pointer means no file processed so far
// set that field; resolve() applies the documented default to any field
// still nil once every file has been merged.
type accumulator struct {
	enabled *bool

	listen           *string
	mdns             *bool
	insecureLoopback *bool

	userTokenHash          *string
	userPreviousTokenHash  *string
	userPreviousValidUntil *string

	adminTokenHash          *string
	adminPreviousTokenHash  *string
	adminPreviousValidUntil *string

	certificatePEM *string
	privateKeyPEM  *string

	maxConcurrentCalls *int
	maxProcesses       *int
	maxRequestBytes    *int
}

// merge folds one file's decoded rawAgent into the accumulator.
//
// Ordinary scalars follow "last-defined wins": if this file set the field
// (its pointer is non-nil), it unconditionally replaces whatever an earlier
// file set, even to a zero value. The four auth digest fields
// (user_token_hash, user_previous_token_hash, admin_token_hash,
// admin_previous_token_hash) instead go through mergeSecret, which enforces
// the stricter "reject duplicate CONFLICTING secrets" rule described on
// mergeSecret.
func (a *accumulator) merge(ra rawAgent) error {
	if ra.Enabled != nil {
		a.enabled = ra.Enabled
	}

	if ra.Endpoint.Listen != nil {
		a.listen = ra.Endpoint.Listen
	}
	if ra.Endpoint.MDNS != nil {
		a.mdns = ra.Endpoint.MDNS
	}
	if ra.Endpoint.InsecureLoopback != nil {
		a.insecureLoopback = ra.Endpoint.InsecureLoopback
	}

	if err := mergeSecret(&a.userTokenHash, ra.Auth.UserTokenHash, "auth.user_token_hash"); err != nil {
		return err
	}
	if err := mergeSecret(&a.userPreviousTokenHash, ra.Auth.UserPreviousTokenHash, "auth.user_previous_token_hash"); err != nil {
		return err
	}
	if ra.Auth.UserPreviousValidUntil != nil {
		a.userPreviousValidUntil = ra.Auth.UserPreviousValidUntil
	}
	if err := mergeSecret(&a.adminTokenHash, ra.Auth.AdminTokenHash, "auth.admin_token_hash"); err != nil {
		return err
	}
	if err := mergeSecret(&a.adminPreviousTokenHash, ra.Auth.AdminPreviousTokenHash, "auth.admin_previous_token_hash"); err != nil {
		return err
	}
	if ra.Auth.AdminPreviousValidUntil != nil {
		a.adminPreviousValidUntil = ra.Auth.AdminPreviousValidUntil
	}

	if ra.TLS.CertificatePEM != nil {
		a.certificatePEM = ra.TLS.CertificatePEM
	}
	if ra.TLS.PrivateKeyPEM != nil {
		a.privateKeyPEM = ra.TLS.PrivateKeyPEM
	}

	if ra.Limits.MaxConcurrentCalls != nil {
		a.maxConcurrentCalls = ra.Limits.MaxConcurrentCalls
	}
	if ra.Limits.MaxProcesses != nil {
		a.maxProcesses = ra.Limits.MaxProcesses
	}
	if ra.Limits.MaxRequestBytes != nil {
		a.maxRequestBytes = ra.Limits.MaxRequestBytes
	}

	return nil
}

// mergeSecret applies the merge rule for a single auth digest field:
//
//   - A file that does not mention the field (file == nil) changes nothing.
//   - A file that sets the field to "" is treated as "no opinion" -- it
//     never clears an already-recorded non-empty secret. This is
//     deliberately NOT the same as the general "last-defined wins, even to
//     zero value" rule applied to ordinary scalars: a secret is dangerous to
//     accidentally wipe just because a later /oem/*.yaml file happens to
//     ship the field at its empty default.
//   - A file that sets the field to a non-empty value that has not been
//     recorded yet becomes the recorded value.
//   - A file that sets the field to a non-empty value that DIFFERS from an
//     already-recorded non-empty value is a "duplicate conflicting secret"
//     and is rejected, per the phase3-task-1 brief.
//   - A file that repeats the exact same non-empty value already recorded is
//     accepted as a no-op (idempotent, not a conflict).
func mergeSecret(acc **string, file *string, field string) error {
	if file == nil || *file == "" {
		return nil
	}
	if *acc != nil && **acc != "" && **acc != *file {
		return fmt.Errorf("%s: conflicting values across /oem/*.yaml files (secret must be set to the same value everywhere it is defined)", field)
	}
	*acc = file
	return nil
}

// resolve turns the accumulated (possibly partial) merge state into a fully
// defaulted Config. It does not validate the result; Load calls Validate
// separately so that Config.Validate remains usable standalone.
func (a *accumulator) resolve() (Config, error) {
	cfg := Config{
		Enabled: derefBool(a.enabled, true),
		Endpoint: Endpoint{
			Listen:           derefString(a.listen, DefaultListen),
			MDNS:             derefBool(a.mdns, true),
			InsecureLoopback: derefBool(a.insecureLoopback, false),
		},
		Auth: Auth{
			UserTokenHash:          derefString(a.userTokenHash, ""),
			UserPreviousTokenHash:  derefString(a.userPreviousTokenHash, ""),
			AdminTokenHash:         derefString(a.adminTokenHash, ""),
			AdminPreviousTokenHash: derefString(a.adminPreviousTokenHash, ""),
		},
		TLS: TLS{
			CertificatePEM: derefString(a.certificatePEM, ""),
			PrivateKeyPEM:  derefString(a.privateKeyPEM, ""),
		},
		Limits: Limits{
			MaxConcurrentCalls: derefInt(a.maxConcurrentCalls, DefaultMaxConcurrentCalls),
			MaxProcesses:       derefInt(a.maxProcesses, DefaultMaxProcesses),
			MaxRequestBytes:    derefInt(a.maxRequestBytes, DefaultMaxRequestBytes),
		},
	}

	var err error
	cfg.Auth.UserPreviousValidUntil, err = parseOptionalTime(derefString(a.userPreviousValidUntil, ""))
	if err != nil {
		return Config{}, fmt.Errorf("auth.user_previous_valid_until: %w", err)
	}
	cfg.Auth.AdminPreviousValidUntil, err = parseOptionalTime(derefString(a.adminPreviousValidUntil, ""))
	if err != nil {
		return Config{}, fmt.Errorf("auth.admin_previous_valid_until: %w", err)
	}
	return cfg, nil
}

func derefBool(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func derefString(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func derefInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// parseOptionalTime parses s as RFC 3339 unless it is empty, in which case
// it returns the zero time.Time.
func parseOptionalTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid RFC3339 timestamp %q: %w", s, err)
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// Validate
// ---------------------------------------------------------------------------

// Validate checks c for internal consistency: a non-loopback listener with
// InsecureLoopback set, a digest with a prefix other than "sha256:" on any
// non-empty *TokenHash field, a rotation overlap exceeding
// auth.MaxRotationOverlap (24h), a zero listen port, and exactly one of
// TLS.CertificatePEM/TLS.PrivateKeyPEM being set are all rejected.
//
// Digest format is validated via auth.Digest.Validate and the rotation
// overlap bound is auth.MaxRotationOverlap, both reused directly from
// internal/auth rather than reimplemented, so this package's rules can never
// silently drift from the Verifier's.
//
// Validate is safe to call on a Config built directly (not via Load), and
// applies regardless of c.Enabled: a disabled config still gets its fields
// defaulted and checked for consistency, so a later flip to enabled=true
// cannot surface a schema error that validation should have caught earlier.
func (c Config) Validate() error {
	return c.validateAt(time.Now())
}

// validateAt is Validate with an explicit reference instant, used by this
// package's own tests to exercise the rotation-overlap boundary
// deterministically without depending on wall-clock timing.
func (c Config) validateAt(now time.Time) error {
	if err := c.Endpoint.validate(); err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	if err := validateAuthPair(c.Auth.UserTokenHash, c.Auth.UserPreviousTokenHash, c.Auth.UserPreviousValidUntil, now); err != nil {
		return fmt.Errorf("auth: user_%w", err)
	}
	if err := validateAuthPair(c.Auth.AdminTokenHash, c.Auth.AdminPreviousTokenHash, c.Auth.AdminPreviousValidUntil, now); err != nil {
		return fmt.Errorf("auth: admin_%w", err)
	}
	if (c.TLS.CertificatePEM == "") != (c.TLS.PrivateKeyPEM == "") {
		return errors.New("tls: certificate_pem and private_key_pem must both be set or both be empty")
	}
	return nil
}

// validate checks e for internal consistency, mirroring
// internal/gateway.validateListen/isLoopbackHost exactly (those helpers are
// unexported in that package, so the small amount of logic is duplicated
// here rather than reused across packages).
func (e Endpoint) validate() error {
	host, portStr, err := net.SplitHostPort(e.Listen)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", e.Listen, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("invalid listen port in %q: %w", e.Listen, err)
	}
	if port == 0 {
		return fmt.Errorf("listen port must not be 0, got %q", e.Listen)
	}
	if e.InsecureLoopback && !isLoopbackHost(host) {
		return fmt.Errorf("insecure_loopback requires a loopback listen address, got %q", e.Listen)
	}
	return nil
}

// isLoopbackHost reports whether host names the loopback interface. An
// empty host (a wildcard bind such as ":7443") is NOT loopback.
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

// validateAuthPair validates one bearer class's (current, previous,
// previousValidUntil) triple, mirroring auth.RotationConfig.validate as
// closely as this schema's looser requirements allow: unlike
// auth.RotationConfig, current may legitimately be empty here (the class is
// simply not configured yet), so -- unlike auth.RotationConfig.validate,
// which unconditionally requires and validates Current -- this only
// validates current's digest format when it is non-empty. Every other rule
// (previous requires previousValidUntil and vice versa, previous must
// differ from current, and the overlap bound) exactly mirrors
// auth.RotationConfig.validate, reusing auth.Digest.Validate and
// auth.MaxRotationOverlap directly.
func validateAuthPair(current, previous string, previousValidUntil time.Time, now time.Time) error {
	if current != "" {
		if err := auth.Digest(current).Validate(); err != nil {
			return fmt.Errorf("token_hash: %w", err)
		}
	}
	if previous == "" {
		if !previousValidUntil.IsZero() {
			return errors.New("previous_valid_until set without previous_token_hash")
		}
		return nil
	}
	if err := auth.Digest(previous).Validate(); err != nil {
		return fmt.Errorf("previous_token_hash: %w", err)
	}
	if previous == current {
		return errors.New("previous_token_hash must differ from token_hash")
	}
	if previousValidUntil.IsZero() {
		return errors.New("previous_token_hash set without previous_valid_until")
	}
	if overlap := previousValidUntil.Sub(now); overlap > auth.MaxRotationOverlap {
		return fmt.Errorf("rotation overlap %s exceeds maximum %s", overlap, auth.MaxRotationOverlap)
	}
	return nil
}
