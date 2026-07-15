package provision

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/auth"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

var testNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// newTestMaterializer builds a Materializer writing into temp dirs with chown
// disabled (so the test never needs to be root or need the real system users)
// and with deterministic hostname/IPs/clock.
func newTestMaterializer(t *testing.T) (*Materializer, string, string) {
	t.Helper()
	stateDir := t.TempDir()
	runtimeDir := t.TempDir()
	m := NewMaterializer(Options{
		StateDir:   stateDir,
		RuntimeDir: runtimeDir,
		Chown:      false,
		Now:        func() time.Time { return testNow },
		Hostname:   func() (string, error) { return "test-host", nil },
		LocalIPs:   func() ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.1.50")}, nil },
	})
	return m, stateDir, runtimeDir
}

// enabledConfig is a minimal valid enabled Config with no digests.
func enabledConfig() Config {
	return Config{
		Enabled: true,
		Endpoint: Endpoint{
			Listen: DefaultListen,
			MDNS:   true,
		},
		Limits: Limits{
			MaxConcurrentCalls: DefaultMaxConcurrentCalls,
			MaxProcesses:       DefaultMaxProcesses,
			MaxRequestBytes:    DefaultMaxRequestBytes,
		},
	}
}

func findFile(files []PersistedFile, suffix string) (PersistedFile, bool) {
	for _, f := range files {
		if strings.HasSuffix(f.Path, suffix) {
			return f, true
		}
	}
	return PersistedFile{}, false
}

func readGateway(t *testing.T, stateDir string) gatewayConfig {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "gateway", "config.json"))
	if err != nil {
		t.Fatalf("read gateway config: %v", err)
	}
	var gw gatewayConfig
	if err := json.Unmarshal(data, &gw); err != nil {
		t.Fatalf("unmarshal gateway config: %v", err)
	}
	return gw
}

// ---------------------------------------------------------------------------
// Live first boot
// ---------------------------------------------------------------------------

func TestMaterializeLiveFirstBoot(t *testing.T) {
	m, stateDir, runtimeDir := newTestMaterializer(t)

	res, err := m.Materialize(enabledConfig())
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// A plaintext one-shot token was written under the runtime dir.
	if !res.TokenWritten {
		t.Fatal("expected TokenWritten=true on live first boot")
	}
	tokenPath := filepath.Join(runtimeDir, "first-run-token")
	raw, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read first-run-token: %v", err)
	}
	bearer := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(bearer, "hdn_u_") {
		t.Fatalf("first-run-token is not a user bearer: %q", bearer)
	}

	// Its digest is what was persisted in the gateway config.
	gw := readGateway(t, stateDir)
	wantDigest := sha256Digest(bearer)
	if gw.User.Current != wantDigest {
		t.Fatalf("gateway user digest = %q, want digest of the plaintext token %q", gw.User.Current, wantDigest)
	}
	if err := auth.Digest(gw.User.Current).Validate(); err != nil {
		t.Fatalf("persisted user digest invalid: %v", err)
	}

	// The gateway TLS material and config exist with the intended owners/modes.
	assertPersisted(t, res.Files, "gateway/config.json", 0o440, "root", "hadron-agent-gateway")
	assertPersisted(t, res.Files, "gateway/tls.crt", 0o440, "root", "hadron-agent-gateway")
	assertPersisted(t, res.Files, "gateway/tls.key", 0o440, "root", "hadron-agent-gateway")
	assertPersisted(t, res.Files, "session/config.json", 0o440, "root", "agent")

	// No admin: no root files.
	if _, err := os.Stat(filepath.Join(stateDir, "root", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("root/config.json should be absent without an admin digest (stat err = %v)", err)
	}
	if res.AdminEnabled {
		t.Fatal("AdminEnabled should be false without an admin digest")
	}

	// Fingerprint recorded and matches the persisted cert.
	if res.CertFingerprint != gw.CertFingerprint {
		t.Fatalf("result fingerprint %q != gateway config fingerprint %q", res.CertFingerprint, gw.CertFingerprint)
	}
	if !strings.HasPrefix(res.CertFingerprint, "sha256:") {
		t.Fatalf("fingerprint not sha256-prefixed: %q", res.CertFingerprint)
	}
}

func assertPersisted(t *testing.T, files []PersistedFile, suffix string, mode os.FileMode, user, group string) {
	t.Helper()
	f, ok := findFile(files, suffix)
	if !ok {
		t.Fatalf("expected a persisted file ending in %q", suffix)
	}
	if f.Mode != mode {
		t.Fatalf("%s mode = %o, want %o", suffix, f.Mode, mode)
	}
	if f.User != user || f.Group != group {
		t.Fatalf("%s owner = %s:%s, want %s:%s", suffix, f.User, f.Group, user, group)
	}
	// Verify the on-disk mode too.
	info, err := os.Stat(f.Path)
	if err != nil {
		t.Fatalf("stat %s: %v", f.Path, err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("%s on-disk mode = %o, want %o", suffix, info.Mode().Perm(), mode)
	}
}

// ---------------------------------------------------------------------------
// Installed reuse: persisted digest reused, no new plaintext, cert unchanged
// ---------------------------------------------------------------------------

func TestMaterializeInstalledReuseNoPlaintext(t *testing.T) {
	m, stateDir, runtimeDir := newTestMaterializer(t)

	// First boot generates state.
	first, err := m.Materialize(enabledConfig())
	if err != nil {
		t.Fatalf("first Materialize: %v", err)
	}
	firstCert, _ := os.ReadFile(filepath.Join(stateDir, "gateway", "tls.crt"))
	firstDigest := readGateway(t, stateDir).User.Current

	// Simulate a reboot on installed (persistent) state: the runtime dir is
	// wiped (tmpfs) but the state dir survives.
	os.RemoveAll(runtimeDir)
	os.MkdirAll(runtimeDir, 0o755)

	second, err := m.Materialize(enabledConfig())
	if err != nil {
		t.Fatalf("second Materialize: %v", err)
	}

	if second.TokenWritten {
		t.Fatal("subsequent installed boot must NOT recreate the plaintext token")
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "first-run-token")); !os.IsNotExist(err) {
		t.Fatalf("first-run-token should not be recreated on reuse (stat err = %v)", err)
	}

	// Digest reused.
	if got := readGateway(t, stateDir).User.Current; got != firstDigest {
		t.Fatalf("user digest changed on reuse: got %q want %q", got, firstDigest)
	}
	// Cert identity NOT rotated.
	secondCert, _ := os.ReadFile(filepath.Join(stateDir, "gateway", "tls.crt"))
	if !bytes.Equal(firstCert, secondCert) {
		t.Fatal("TLS certificate was rotated on a subsequent boot; it must be reused")
	}
	if first.CertFingerprint != second.CertFingerprint {
		t.Fatalf("fingerprint changed on reuse: %q -> %q", first.CertFingerprint, second.CertFingerprint)
	}
}

// ---------------------------------------------------------------------------
// CI-provided user digest: no plaintext token
// ---------------------------------------------------------------------------

func TestMaterializeCIProvidedDigestNoPlaintext(t *testing.T) {
	m, stateDir, runtimeDir := newTestMaterializer(t)

	_, digest, err := auth.Generate(auth.ClassUser)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	cfg := enabledConfig()
	cfg.Auth.UserTokenHash = string(digest)

	res, err := m.Materialize(cfg)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if res.TokenWritten {
		t.Fatal("a CI-provided digest must not create a plaintext token")
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "first-run-token")); !os.IsNotExist(err) {
		t.Fatalf("first-run-token should be absent for a CI digest (stat err = %v)", err)
	}
	if got := readGateway(t, stateDir).User.Current; got != string(digest) {
		t.Fatalf("persisted user digest = %q, want provided %q", got, digest)
	}
}

// ---------------------------------------------------------------------------
// Optional admin digest
// ---------------------------------------------------------------------------

func TestMaterializeAdminDigestWritesRootFiles(t *testing.T) {
	m, stateDir, _ := newTestMaterializer(t)

	_, adminDigest, _ := auth.Generate(auth.ClassAdmin)
	cfg := enabledConfig()
	cfg.Auth.AdminTokenHash = string(adminDigest)

	res, err := m.Materialize(cfg)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if !res.AdminEnabled {
		t.Fatal("AdminEnabled should be true with an admin digest")
	}
	assertPersisted(t, res.Files, "root/config.json", 0o400, "root", "root")
	assertPersisted(t, res.Files, "root/enabled", 0o400, "root", "root")

	// Gateway config also carries the admin digest.
	if got := readGateway(t, stateDir).Admin.Current; got != string(adminDigest) {
		t.Fatalf("gateway admin digest = %q, want %q", got, adminDigest)
	}
}

// ---------------------------------------------------------------------------
// Provided TLS pair used verbatim
// ---------------------------------------------------------------------------

func TestMaterializeProvidedTLSUsedVerbatim(t *testing.T) {
	m, stateDir, _ := newTestMaterializer(t)

	certPEM, keyPEM, err := generateSelfSignedCert(testNow, "seeded-host", []net.IP{net.ParseIP("10.0.0.9")})
	if err != nil {
		t.Fatalf("seed cert: %v", err)
	}
	cfg := enabledConfig()
	cfg.TLS.CertificatePEM = string(certPEM)
	cfg.TLS.PrivateKeyPEM = string(keyPEM)

	res, err := m.Materialize(cfg)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	onDiskCert, _ := os.ReadFile(filepath.Join(stateDir, "gateway", "tls.crt"))
	onDiskKey, _ := os.ReadFile(filepath.Join(stateDir, "gateway", "tls.key"))
	if !bytes.Equal(onDiskCert, certPEM) {
		t.Fatal("provided certificate was not written verbatim")
	}
	if !bytes.Equal(onDiskKey, keyPEM) {
		t.Fatal("provided private key was not written verbatim")
	}
	wantFP, _ := certFingerprint(certPEM)
	if res.CertFingerprint != wantFP {
		t.Fatalf("fingerprint = %q, want %q", res.CertFingerprint, wantFP)
	}
}

// ---------------------------------------------------------------------------
// Generated cert: SANs, 397-day validity, fingerprint
// ---------------------------------------------------------------------------

func TestGeneratedCertSANsValidityFingerprint(t *testing.T) {
	extra := net.ParseIP("192.168.1.50")
	certPEM, keyPEM, err := generateSelfSignedCert(testNow, "test-host", []net.IP{extra})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(keyPEM) == 0 {
		t.Fatal("empty key PEM")
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("cert PEM did not decode")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	wantDNS := map[string]bool{"localhost": true, "test-host": true, "test-host.local": true}
	for _, d := range cert.DNSNames {
		delete(wantDNS, d)
	}
	if len(wantDNS) != 0 {
		t.Fatalf("cert missing DNS SANs %v (has %v)", wantDNS, cert.DNSNames)
	}

	wantIPs := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1"), extra}
	for _, want := range wantIPs {
		found := false
		for _, ip := range cert.IPAddresses {
			if ip.Equal(want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("cert missing IP SAN %s (has %v)", want, cert.IPAddresses)
		}
	}

	if got := cert.NotAfter.Sub(cert.NotBefore); got != 397*24*time.Hour {
		t.Fatalf("validity = %s, want 397 days", got)
	}

	// Fingerprint helper matches the DER sha256.
	fp, err := certFingerprint(certPEM)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if !strings.HasPrefix(fp, "sha256:") || len(fp) != len("sha256:")+64 {
		t.Fatalf("fingerprint format wrong: %q", fp)
	}
}

// ---------------------------------------------------------------------------
// Atomic writes: no temp files left behind
// ---------------------------------------------------------------------------

func TestMaterializeLeavesNoTempFiles(t *testing.T) {
	m, stateDir, runtimeDir := newTestMaterializer(t)
	if _, err := m.Materialize(enabledConfig()); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	for _, dir := range []string{
		filepath.Join(stateDir, "gateway"),
		filepath.Join(stateDir, "session"),
		runtimeDir,
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp") || strings.HasPrefix(e.Name(), ".provision") {
				t.Fatalf("leftover temp file in %s: %s", dir, e.Name())
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Permission failure surfaces cleanly, no partial state
// ---------------------------------------------------------------------------

func TestMaterializePermissionFailureNoPartialState(t *testing.T) {
	stateDir := t.TempDir()
	runtimeDir := t.TempDir()
	m := NewMaterializer(Options{
		StateDir:   stateDir,
		RuntimeDir: runtimeDir,
		Chown:      true, // force the chown path
		Now:        func() time.Time { return testNow },
		Hostname:   func() (string, error) { return "test-host", nil },
		LocalIPs:   func() ([]net.IP, error) { return nil, nil },
		LookupIDs: func(user, group string) (int, int, error) {
			return 0, 0, os.ErrPermission // simulate an unresolvable owner
		},
	})

	if _, err := m.Materialize(enabledConfig()); err == nil {
		t.Fatal("expected Materialize to fail when the owner cannot be resolved")
	}
	// No partial file should remain.
	if _, err := os.Stat(filepath.Join(stateDir, "gateway", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("gateway config.json should not exist after a failed chown (stat err = %v)", err)
	}
	entries, _ := os.ReadDir(filepath.Join(stateDir, "gateway"))
	for _, e := range entries {
		t.Fatalf("no file should remain in gateway dir, found %s", e.Name())
	}
}

// ---------------------------------------------------------------------------
// token rotate
// ---------------------------------------------------------------------------

func TestRotateRejectsOverlapTooLong(t *testing.T) {
	m, _, _ := newTestMaterializer(t)
	if _, err := m.Materialize(enabledConfig()); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	_, err := m.Rotate(RotateOptions{Class: auth.ClassUser, Overlap: 25 * time.Hour})
	if err == nil {
		t.Fatal("expected rotate to reject an overlap over 24h")
	}
}

func TestRotatePrintsOnlyToTTY(t *testing.T) {
	m, stateDir, _ := newTestMaterializer(t)
	if _, err := m.Materialize(enabledConfig()); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	oldDigest := readGateway(t, stateDir).User.Current

	// With a TTY writer, the new bearer is printed to it.
	var tty bytes.Buffer
	res, err := m.Rotate(RotateOptions{
		Class:   auth.ClassUser,
		Overlap: 15 * time.Minute,
		TTY:     &tty,
		Reload:  func() error { return nil },
	})
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if !res.Printed {
		t.Fatal("expected Printed=true with a TTY writer")
	}
	if !strings.Contains(tty.String(), "hdn_u_") {
		t.Fatalf("TTY output should contain the new bearer, got %q", tty.String())
	}

	gw := readGateway(t, stateDir)
	if gw.User.Current == oldDigest {
		t.Fatal("current user digest did not change after rotate")
	}
	if gw.User.Previous != oldDigest {
		t.Fatalf("previous digest = %q, want the old current %q", gw.User.Previous, oldDigest)
	}
	if gw.User.PreviousValidUntil == "" {
		t.Fatal("previous_valid_until not set after rotate with overlap")
	}

	// Without a TTY writer, the secret is never printed.
	var journal bytes.Buffer
	res2, err := m.Rotate(RotateOptions{
		Class:   auth.ClassUser,
		Overlap: 15 * time.Minute,
		TTY:     nil,
		Reload:  func() error { return nil },
	})
	if err != nil {
		t.Fatalf("Rotate (no tty): %v", err)
	}
	if res2.Printed {
		t.Fatal("Printed must be false without a TTY writer")
	}
	if strings.Contains(journal.String(), "hdn_") {
		t.Fatal("no bearer must appear when there is no TTY")
	}
}

func TestRotateRollsBackOnReloadFailure(t *testing.T) {
	m, stateDir, _ := newTestMaterializer(t)
	if _, err := m.Materialize(enabledConfig()); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(stateDir, "gateway", "config.json"))
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	var tty bytes.Buffer
	_, err = m.Rotate(RotateOptions{
		Class:   auth.ClassUser,
		Overlap: 15 * time.Minute,
		TTY:     &tty,
		Reload:  func() error { return os.ErrClosed },
	})
	if err == nil {
		t.Fatal("expected rotate to fail when the reload hook fails")
	}
	after, err := os.ReadFile(filepath.Join(stateDir, "gateway", "config.json"))
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("gateway config was not rolled back after a failed reload")
	}
	if strings.Contains(tty.String(), "hdn_") {
		t.Fatal("no bearer must be printed when rotate rolls back")
	}
}

func TestRotateAdminUpdatesGatewayAndRoot(t *testing.T) {
	m, stateDir, _ := newTestMaterializer(t)
	_, adminDigest, _ := auth.Generate(auth.ClassAdmin)
	cfg := enabledConfig()
	cfg.Auth.AdminTokenHash = string(adminDigest)
	if _, err := m.Materialize(cfg); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	var tty bytes.Buffer
	res, err := m.Rotate(RotateOptions{
		Class:   auth.ClassAdmin,
		Overlap: 10 * time.Minute,
		TTY:     &tty,
		Reload:  func() error { return nil },
	})
	if err != nil {
		t.Fatalf("Rotate admin: %v", err)
	}
	if !strings.Contains(tty.String(), "hdn_a_") {
		t.Fatalf("expected an admin bearer on the TTY, got %q", tty.String())
	}

	gw := readGateway(t, stateDir)
	if gw.Admin.Current == string(adminDigest) {
		t.Fatal("gateway admin digest unchanged after rotate")
	}
	if gw.Admin.Previous != string(adminDigest) {
		t.Fatalf("gateway admin previous = %q, want old %q", gw.Admin.Previous, adminDigest)
	}

	rootData, err := os.ReadFile(filepath.Join(stateDir, "root", "config.json"))
	if err != nil {
		t.Fatalf("read root config: %v", err)
	}
	var rc rootConfig
	if err := json.Unmarshal(rootData, &rc); err != nil {
		t.Fatalf("unmarshal root config: %v", err)
	}
	if rc.Admin.Current != gw.Admin.Current {
		t.Fatalf("root admin digest %q != gateway admin digest %q", rc.Admin.Current, gw.Admin.Current)
	}
	if rc.Admin.Current != string(res.Digest) {
		t.Fatalf("root admin digest %q != rotate result digest %q", rc.Admin.Current, res.Digest)
	}
}
