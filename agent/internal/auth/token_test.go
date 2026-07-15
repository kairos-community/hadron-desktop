package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// ---------------------------------------------------------------------------
// Generate
// ---------------------------------------------------------------------------

func TestGenerate_UserPrefixAndPayload(t *testing.T) {
	bearer, digest, err := Generate(ClassUser)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(bearer, "hdn_u_") {
		t.Fatalf("bearer %q does not have hdn_u_ prefix", bearer)
	}
	checkBearerPayload(t, bearer, "hdn_u_")
	checkDigestMatches(t, bearer, digest)
}

func TestGenerate_AdminPrefixAndPayload(t *testing.T) {
	bearer, digest, err := Generate(ClassAdmin)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(bearer, "hdn_a_") {
		t.Fatalf("bearer %q does not have hdn_a_ prefix", bearer)
	}
	checkBearerPayload(t, bearer, "hdn_a_")
	checkDigestMatches(t, bearer, digest)
}

func TestGenerate_UnknownClassErrors(t *testing.T) {
	if _, _, err := Generate(Class("bogus")); err == nil {
		t.Fatal("expected error for unknown class")
	}
}

func TestGenerate_ProducesUniqueRandomBearers(t *testing.T) {
	seen := make(map[string]bool)
	const n = 500
	for i := 0; i < n; i++ {
		bearer, _, err := Generate(ClassUser)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if seen[bearer] {
			t.Fatalf("duplicate bearer generated: %q", bearer)
		}
		seen[bearer] = true
	}
}

// checkBearerPayload asserts the bearer's payload decodes to exactly 32
// random bytes via unpadded base64url, as required for a fresh bearer.
func checkBearerPayload(t *testing.T, bearer, prefix string) {
	t.Helper()
	payload := strings.TrimPrefix(bearer, prefix)
	if strings.ContainsAny(payload, "=+/") {
		t.Fatalf("payload %q is not unpadded base64url (contains padding or non-url chars)", payload)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload %q is not valid unpadded base64url: %v", payload, err)
	}
	if len(decoded) != 32 {
		t.Fatalf("decoded payload has %d bytes, want 32", len(decoded))
	}
}

// checkDigestMatches asserts digest is "sha256:" + 64 lowercase hex chars
// derived from the complete bearer string (prefix included).
func checkDigestMatches(t *testing.T, bearer string, digest Digest) {
	t.Helper()
	if err := digest.Validate(); err != nil {
		t.Fatalf("digest %q failed format validation: %v", digest, err)
	}
	want := digestBearer(bearer)
	if digest != want {
		t.Fatalf("digest %q does not match sha256 of complete bearer, want %q", digest, want)
	}
}

// ---------------------------------------------------------------------------
// Digest format
// ---------------------------------------------------------------------------

func TestDigest_Validate(t *testing.T) {
	valid := digestBearer("hdn_u_anything")
	if err := valid.Validate(); err != nil {
		t.Fatalf("expected valid digest, got error: %v", err)
	}

	cases := map[string]Digest{
		"missing prefix":     Digest(strings.Repeat("a", 64)),
		"wrong prefix":       Digest("md5:" + strings.Repeat("a", 64)),
		"too short":          Digest("sha256:" + strings.Repeat("a", 63)),
		"too long":           Digest("sha256:" + strings.Repeat("a", 65)),
		"uppercase hex":      Digest("sha256:" + strings.Repeat("A", 64)),
		"non-hex characters": Digest("sha256:" + strings.Repeat("g", 64)),
		"empty":              Digest(""),
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			if err := d.Validate(); err == nil {
				t.Fatalf("expected validation error for %s (%q)", name, d)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// RotationConfig / NewVerifier
// ---------------------------------------------------------------------------

func TestNewVerifier_RejectsOverlapLongerThan24h(t *testing.T) {
	now := time.Now()
	_, curDigest, _ := Generate(ClassUser)
	_, prevDigest, _ := Generate(ClassUser)

	cfg := RotationConfig{
		Current:            curDigest,
		Previous:           prevDigest,
		PreviousValidUntil: now.Add(24*time.Hour + time.Second),
	}
	if _, err := newVerifierAt(now, cfg, validAdminConfig(now)); err == nil {
		t.Fatal("expected error for rotation overlap longer than 24h")
	}
}

func TestNewVerifier_AcceptsOverlapUpTo24h(t *testing.T) {
	now := time.Now()
	_, curDigest, _ := Generate(ClassUser)
	_, prevDigest, _ := Generate(ClassUser)

	cfg := RotationConfig{
		Current:            curDigest,
		Previous:           prevDigest,
		PreviousValidUntil: now.Add(24 * time.Hour),
	}
	if _, err := newVerifierAt(now, cfg, validAdminConfig(now)); err != nil {
		t.Fatalf("expected 24h overlap to be accepted, got error: %v", err)
	}
}

func TestNewVerifier_RejectsInvalidCurrentDigest(t *testing.T) {
	cfg := RotationConfig{Current: Digest("not-a-digest")}
	if _, err := NewVerifier(cfg, validAdminConfig(time.Now())); err == nil {
		t.Fatal("expected error for malformed current digest")
	}
}

func TestNewVerifier_RejectsPreviousWithoutValidUntil(t *testing.T) {
	_, curDigest, _ := Generate(ClassUser)
	_, prevDigest, _ := Generate(ClassUser)
	cfg := RotationConfig{Current: curDigest, Previous: prevDigest}
	if _, err := NewVerifier(cfg, validAdminConfig(time.Now())); err == nil {
		t.Fatal("expected error when previous is set without previous_valid_until")
	}
}

func TestNewVerifier_RejectsValidUntilWithoutPrevious(t *testing.T) {
	_, curDigest, _ := Generate(ClassUser)
	cfg := RotationConfig{Current: curDigest, PreviousValidUntil: time.Now().Add(time.Hour)}
	if _, err := NewVerifier(cfg, validAdminConfig(time.Now())); err == nil {
		t.Fatal("expected error when previous_valid_until is set without previous")
	}
}

// validAdminConfig returns a minimally valid admin RotationConfig (current
// digest only, no rotation in progress) for tests that only care about the
// user-class configuration.
func validAdminConfig(_ time.Time) RotationConfig {
	_, digest, _ := Generate(ClassAdmin)
	return RotationConfig{Current: digest}
}

// ---------------------------------------------------------------------------
// Verify: malformed / wrong class
// ---------------------------------------------------------------------------

func TestVerify_RejectsMalformedTokens(t *testing.T) {
	now := time.Now()
	_, userDigest, _ := Generate(ClassUser)
	_, adminDigest, _ := Generate(ClassAdmin)
	v, err := newVerifierAt(now, RotationConfig{Current: userDigest}, RotationConfig{Current: adminDigest})
	if err != nil {
		t.Fatalf("newVerifierAt: %v", err)
	}

	cases := map[string]string{
		"empty string":            "",
		"no prefix":               "totallynotabearer",
		"unknown prefix":          "hdn_x_" + strings.Repeat("A", 43),
		"prefix only":             "hdn_u_",
		"invalid base64 char":     "hdn_u_" + strings.Repeat("A", 42) + "!",
		"padded base64":           "hdn_u_" + strings.Repeat("A", 42) + "=",
		"too short payload":       "hdn_u_" + strings.Repeat("A", 10),
		"too long payload":        "hdn_u_" + strings.Repeat("A", 60),
		"admin prefix short body": "hdn_a_AAAA",
	}
	for name, bearer := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(bearer, ClassUser); !errors.Is(err, ErrMalformedBearer) {
				t.Fatalf("Verify(%q) = %v, want ErrMalformedBearer", bearer, err)
			}
		})
	}
}

func TestVerify_RejectsWrongClass(t *testing.T) {
	now := time.Now()
	userBearer, userDigest, _ := Generate(ClassUser)
	_, adminDigest, _ := Generate(ClassAdmin)
	v, err := newVerifierAt(now, RotationConfig{Current: userDigest}, RotationConfig{Current: adminDigest})
	if err != nil {
		t.Fatalf("newVerifierAt: %v", err)
	}

	if _, err := v.Verify(userBearer, ClassAdmin); !errors.Is(err, ErrWrongClass) {
		t.Fatalf("Verify(user bearer, ClassAdmin) = %v, want ErrWrongClass", err)
	}

	adminBearer, _, _ := Generate(ClassAdmin)
	if _, err := v.Verify(adminBearer, ClassUser); !errors.Is(err, ErrWrongClass) {
		t.Fatalf("Verify(admin bearer, ClassUser) = %v, want ErrWrongClass", err)
	}
}

// ---------------------------------------------------------------------------
// Verify: current / previous acceptance and rotation expiry
// ---------------------------------------------------------------------------

func TestVerify_AcceptsCurrentDigest(t *testing.T) {
	now := time.Now()
	userBearer, userDigest, _ := Generate(ClassUser)
	_, adminDigest, _ := Generate(ClassAdmin)
	v, err := newVerifierAt(now, RotationConfig{Current: userDigest}, RotationConfig{Current: adminDigest})
	if err != nil {
		t.Fatalf("newVerifierAt: %v", err)
	}

	info, err := v.Verify(userBearer, ClassUser)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(info.Scopes) != 1 || info.Scopes[0] != "hadron:user" {
		t.Fatalf("Scopes = %v, want [hadron:user]", info.Scopes)
	}
	wantUserID := string(userDigest)[len("sha256:") : len("sha256:")+16]
	if info.UserID != wantUserID {
		t.Fatalf("UserID = %q, want %q", info.UserID, wantUserID)
	}
	cred, ok := info.Extra["credential"].(Credential)
	if !ok {
		t.Fatalf("Extra[credential] is %T, want Credential", info.Extra["credential"])
	}
	if cred.Class() != ClassUser || cred.Bearer() != userBearer {
		t.Fatalf("credential = %+v, want class=user bearer=%q", cred, userBearer)
	}
}

func TestVerify_AcceptsAdminCurrentDigest(t *testing.T) {
	now := time.Now()
	_, userDigest, _ := Generate(ClassUser)
	adminBearer, adminDigest, _ := Generate(ClassAdmin)
	v, err := newVerifierAt(now, RotationConfig{Current: userDigest}, RotationConfig{Current: adminDigest})
	if err != nil {
		t.Fatalf("newVerifierAt: %v", err)
	}

	info, err := v.Verify(adminBearer, ClassAdmin)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(info.Scopes) != 1 || info.Scopes[0] != "hadron:admin" {
		t.Fatalf("Scopes = %v, want [hadron:admin]", info.Scopes)
	}
}

func TestVerify_AcceptsUnexpiredPreviousDigest(t *testing.T) {
	now := time.Now()
	oldBearer, oldDigest, _ := Generate(ClassUser)
	_, newDigest, _ := Generate(ClassUser)
	_, adminDigest, _ := Generate(ClassAdmin)

	cfg := RotationConfig{
		Current:            newDigest,
		Previous:           oldDigest,
		PreviousValidUntil: now.Add(time.Hour),
	}
	v, err := newVerifierAt(now, cfg, RotationConfig{Current: adminDigest})
	if err != nil {
		t.Fatalf("newVerifierAt: %v", err)
	}

	if _, err := v.Verify(oldBearer, ClassUser); err != nil {
		t.Fatalf("Verify(old bearer) = %v, want success (previous unexpired)", err)
	}
}

func TestVerify_RejectsExpiredPreviousDigest(t *testing.T) {
	now := time.Now()
	oldBearer, oldDigest, _ := Generate(ClassUser)
	_, newDigest, _ := Generate(ClassUser)
	_, adminDigest, _ := Generate(ClassAdmin)

	cfg := RotationConfig{
		Current:            newDigest,
		Previous:           oldDigest,
		PreviousValidUntil: now.Add(time.Hour),
	}
	v, err := newVerifierAt(now, cfg, RotationConfig{Current: adminDigest})
	if err != nil {
		t.Fatalf("newVerifierAt: %v", err)
	}
	// Advance the verifier's clock past previous_valid_until.
	v.now = func() time.Time { return now.Add(2 * time.Hour) }

	if _, err := v.Verify(oldBearer, ClassUser); !errors.Is(err, ErrInvalidBearer) {
		t.Fatalf("Verify(expired old bearer) = %v, want ErrInvalidBearer", err)
	}
}

func TestVerify_RejectsUnknownBearer(t *testing.T) {
	now := time.Now()
	_, userDigest, _ := Generate(ClassUser)
	_, adminDigest, _ := Generate(ClassAdmin)
	v, err := newVerifierAt(now, RotationConfig{Current: userDigest}, RotationConfig{Current: adminDigest})
	if err != nil {
		t.Fatalf("newVerifierAt: %v", err)
	}

	stranger, _, _ := Generate(ClassUser)
	if _, err := v.Verify(stranger, ClassUser); !errors.Is(err, ErrInvalidBearer) {
		t.Fatalf("Verify(unknown bearer) = %v, want ErrInvalidBearer", err)
	}
}

// ---------------------------------------------------------------------------
// Non-short-circuit comparison
// ---------------------------------------------------------------------------

func TestVerify_ComparesEveryConfiguredDigest_NoMatch(t *testing.T) {
	now := time.Now()
	_, curDigest, _ := Generate(ClassUser)
	_, prevDigest, _ := Generate(ClassUser)
	_, adminDigest, _ := Generate(ClassAdmin)

	cfg := RotationConfig{
		Current:            curDigest,
		Previous:           prevDigest,
		PreviousValidUntil: now.Add(time.Hour),
	}
	v, err := newVerifierAt(now, cfg, RotationConfig{Current: adminDigest})
	if err != nil {
		t.Fatalf("newVerifierAt: %v", err)
	}

	stranger, _, _ := Generate(ClassUser)

	calls := 0
	testCompareHook = func() { calls++ }
	defer func() { testCompareHook = nil }()

	if _, err := v.Verify(stranger, ClassUser); !errors.Is(err, ErrInvalidBearer) {
		t.Fatalf("Verify(stranger) = %v, want ErrInvalidBearer", err)
	}
	if calls != 2 {
		t.Fatalf("compareDigest invoked %d times, want 2 (current and previous both compared even though neither matched)", calls)
	}
}

func TestVerify_ComparesEveryConfiguredDigest_MatchOnCurrentStillComparesPrevious(t *testing.T) {
	now := time.Now()
	curBearer, curDigest, _ := Generate(ClassUser)
	_, prevDigest, _ := Generate(ClassUser)
	_, adminDigest, _ := Generate(ClassAdmin)

	cfg := RotationConfig{
		Current:            curDigest,
		Previous:           prevDigest,
		PreviousValidUntil: now.Add(time.Hour),
	}
	v, err := newVerifierAt(now, cfg, RotationConfig{Current: adminDigest})
	if err != nil {
		t.Fatalf("newVerifierAt: %v", err)
	}

	calls := 0
	testCompareHook = func() { calls++ }
	defer func() { testCompareHook = nil }()

	if _, err := v.Verify(curBearer, ClassUser); err != nil {
		t.Fatalf("Verify(current bearer) = %v, want success", err)
	}
	// The verifier must not short-circuit: even though the bearer matched
	// on the very first (current) comparison, the previous digest is still
	// compared, so the number and timing of comparisons performed does not
	// reveal which candidate (if any) matched.
	if calls != 2 {
		t.Fatalf("compareDigest invoked %d times, want 2 (must not short-circuit after matching current)", calls)
	}
}

func TestVerify_ComparesEveryConfiguredDigest_MatchOnPrevious(t *testing.T) {
	now := time.Now()
	oldBearer, oldDigest, _ := Generate(ClassUser)
	_, newDigest, _ := Generate(ClassUser)
	_, adminDigest, _ := Generate(ClassAdmin)

	cfg := RotationConfig{
		Current:            newDigest,
		Previous:           oldDigest,
		PreviousValidUntil: now.Add(time.Hour),
	}
	v, err := newVerifierAt(now, cfg, RotationConfig{Current: adminDigest})
	if err != nil {
		t.Fatalf("newVerifierAt: %v", err)
	}

	calls := 0
	testCompareHook = func() { calls++ }
	defer func() { testCompareHook = nil }()

	if _, err := v.Verify(oldBearer, ClassUser); err != nil {
		t.Fatalf("Verify(old bearer) = %v, want success", err)
	}
	if calls != 2 {
		t.Fatalf("compareDigest invoked %d times, want 2", calls)
	}
}

// ---------------------------------------------------------------------------
// Credential: never leaks via Stringer/%v/%s formatting
// ---------------------------------------------------------------------------

func TestCredential_NeverLeaksRawBearerThroughFormatting(t *testing.T) {
	const secretBearer = "hdn_a_super-secret-value-do-not-log-me"
	cred := Credential{class: ClassAdmin, bearer: secretBearer}

	outputs := []string{
		fmt.Sprintf("%v", cred),
		fmt.Sprintf("%+v", cred),
		fmt.Sprintf("%#v", cred),
		fmt.Sprintf("%s", cred),
		fmt.Sprintln(cred),
	}
	for _, out := range outputs {
		if strings.Contains(out, secretBearer) {
			t.Fatalf("formatted output leaked raw bearer: %q", out)
		}
		if strings.Contains(out, "super-secret-value") {
			t.Fatalf("formatted output leaked bearer fragment: %q", out)
		}
	}

	// The accessor is the only sanctioned way to retrieve the raw bearer.
	if cred.Bearer() != secretBearer {
		t.Fatalf("Bearer() = %q, want %q", cred.Bearer(), secretBearer)
	}
	if cred.Class() != ClassAdmin {
		t.Fatalf("Class() = %q, want %q", cred.Class(), ClassAdmin)
	}
}

// ---------------------------------------------------------------------------
// Code: maps verify errors to stable api.ErrorCode values
// ---------------------------------------------------------------------------

func TestCode_MapsErrorsToAPICodes(t *testing.T) {
	cases := []struct {
		err  error
		want api.ErrorCode
	}{
		{nil, ""},
		{ErrMalformedBearer, api.CodeUnauthenticated},
		{ErrInvalidBearer, api.CodeUnauthenticated},
		{ErrWrongClass, api.CodeForbidden},
	}
	for _, c := range cases {
		if got := Code(c.err); got != c.want {
			t.Fatalf("Code(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
