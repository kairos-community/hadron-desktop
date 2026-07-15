package provision

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/auth"
)

// TestLoadGatewayConfigRoundTrip writes machine state via Materialize with a
// provisioned user+admin rotation and limits, then reads it back via
// LoadGatewayConfig and asserts every field the gateway service depends on
// survives the writer -> reader round trip byte-for-byte.
func TestLoadGatewayConfigRoundTrip(t *testing.T) {
	m, stateDir, _ := newTestMaterializer(t)

	// A CI-provided user+admin rotation with a bounded-overlap previous digest,
	// so the round trip exercises the full rotation shape (not just current).
	userCur, userPrev := digest("11"), digest("22")
	adminCur, adminPrev := digest("33"), digest("44")
	validUntil := testNow.Add(1 * time.Hour)

	cfg := enabledConfig()
	cfg.Endpoint.Listen = "127.0.0.1:7443"
	cfg.Endpoint.InsecureLoopback = true
	cfg.Limits = Limits{MaxConcurrentCalls: 7, MaxProcesses: 9, MaxRequestBytes: 4096}
	cfg.Auth = Auth{
		UserTokenHash:           userCur,
		UserPreviousTokenHash:   userPrev,
		UserPreviousValidUntil:  validUntil,
		AdminTokenHash:          adminCur,
		AdminPreviousTokenHash:  adminPrev,
		AdminPreviousValidUntil: validUntil,
	}

	if _, err := m.Materialize(cfg); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	gw, err := LoadGatewayConfig(filepath.Join(stateDir, "gateway", "config.json"))
	if err != nil {
		t.Fatalf("LoadGatewayConfig: %v", err)
	}

	if gw.Listen != "127.0.0.1:7443" {
		t.Errorf("Listen = %q, want 127.0.0.1:7443", gw.Listen)
	}
	if !gw.InsecureLoopback {
		t.Error("InsecureLoopback = false, want true")
	}
	if gw.TLSCertPath != filepath.Join(stateDir, "gateway", "tls.crt") {
		t.Errorf("TLSCertPath = %q, unexpected", gw.TLSCertPath)
	}
	if gw.TLSKeyPath != filepath.Join(stateDir, "gateway", "tls.key") {
		t.Errorf("TLSKeyPath = %q, unexpected", gw.TLSKeyPath)
	}
	if gw.Limits != (ServiceLimits{MaxConcurrentCalls: 7, MaxProcesses: 9, MaxRequestBytes: 4096}) {
		t.Errorf("Limits = %+v, want {7 9 4096}", gw.Limits)
	}
	wantUser := Rotation{Current: userCur, Previous: userPrev, PreviousValidUntil: validUntil.UTC().Format(time.RFC3339)}
	if gw.User != wantUser {
		t.Errorf("User rotation = %+v, want %+v", gw.User, wantUser)
	}
	wantAdmin := Rotation{Current: adminCur, Previous: adminPrev, PreviousValidUntil: validUntil.UTC().Format(time.RFC3339)}
	if gw.Admin != wantAdmin {
		t.Errorf("Admin rotation = %+v, want %+v", gw.Admin, wantAdmin)
	}

	// The persisted rotation must convert into a Verifier that accepts both the
	// current and the (unexpired) previous bearer of each class.
	userRot, err := gw.User.AuthRotation()
	if err != nil {
		t.Fatalf("user AuthRotation: %v", err)
	}
	if userRot.Current != auth.Digest(userCur) || userRot.Previous != auth.Digest(userPrev) {
		t.Errorf("user AuthRotation = %+v, want current/previous %s/%s", userRot, userCur, userPrev)
	}
	if !userRot.PreviousValidUntil.Equal(validUntil) {
		t.Errorf("user PreviousValidUntil = %v, want %v", userRot.PreviousValidUntil, validUntil)
	}
}

// TestLoadSessionAndRootConfigRoundTrip asserts the session and root readers
// decode the exact JSON the writer produced.
func TestLoadSessionAndRootConfigRoundTrip(t *testing.T) {
	m, stateDir, _ := newTestMaterializer(t)

	adminCur := digest("55")
	cfg := enabledConfig()
	cfg.Limits = Limits{MaxConcurrentCalls: 3, MaxProcesses: 5, MaxRequestBytes: 2048}
	cfg.Auth = Auth{AdminTokenHash: adminCur} // admin enabled -> root config written

	if _, err := m.Materialize(cfg); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	sc, err := LoadSessionConfig(filepath.Join(stateDir, "session", "config.json"))
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if sc.Limits != (ServiceLimits{MaxConcurrentCalls: 3, MaxProcesses: 5, MaxRequestBytes: 2048}) {
		t.Errorf("session Limits = %+v, want {3 5 2048}", sc.Limits)
	}

	rc, err := LoadRootConfig(filepath.Join(stateDir, "root", "config.json"))
	if err != nil {
		t.Fatalf("LoadRootConfig: %v", err)
	}
	if rc.Admin.Current != adminCur {
		t.Errorf("root admin current = %q, want %q", rc.Admin.Current, adminCur)
	}
}

// TestLoadGatewayConfigMissingAndCorrupt asserts a missing or malformed config
// is a hard error (a service must not start credential-less).
func TestLoadGatewayConfigMissingAndCorrupt(t *testing.T) {
	if _, err := LoadGatewayConfig(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error loading a missing gateway config")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write bad file: %v", err)
	}
	if _, err := LoadGatewayConfig(bad); err == nil {
		t.Fatal("expected error loading a corrupt gateway config")
	}
}

// TestAuthRotationInvalidPrevious covers the defensive rejections in
// AuthRotation: a previous digest without a valid-until, and a valid-until
// without a previous digest, are both rejected rather than silently accepted.
func TestAuthRotationInvalidPrevious(t *testing.T) {
	if _, err := (Rotation{Current: digest("aa"), Previous: digest("bb")}).AuthRotation(); err == nil {
		t.Error("expected error: previous without previous_valid_until")
	}
	if _, err := (Rotation{Current: digest("aa"), PreviousValidUntil: "2026-01-01T00:00:00Z"}).AuthRotation(); err == nil {
		t.Error("expected error: previous_valid_until without previous")
	}
	if _, err := (Rotation{Current: digest("aa"), Previous: digest("bb"), PreviousValidUntil: "not-a-time"}).AuthRotation(); err == nil {
		t.Error("expected error: unparseable previous_valid_until")
	}
	// A clean single-current rotation converts without error.
	rot, err := (Rotation{Current: digest("aa")}).AuthRotation()
	if err != nil {
		t.Fatalf("clean rotation: %v", err)
	}
	if rot.Current != auth.Digest(digest("aa")) || rot.Previous != "" {
		t.Errorf("rotation = %+v, want only current set", rot)
	}
}

// digest builds a format-valid "sha256:<64 hex>" digest by repeating a 2-char
// hex seed, distinct per seed, for use as a fake bearer digest in tests.
func digest(seed string) string {
	out := "sha256:"
	for len(out) < len("sha256:")+64 {
		out += seed
	}
	return out[:len("sha256:")+64]
}
