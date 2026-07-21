// Package auth generates, verifies, and rotates the bearer credentials that
// authenticate every hadron-agent MCP request. It defines the two bearer
// classes (user and admin), the digest format credentials are stored in at
// rest, and a Verifier that accepts a bounded rotation window between a
// current and previous digest.
//
// This package is the security boundary for hadron-agent: the gateway (a
// later Phase-2 task) calls Verifier.Verify on every /mcp request, and the
// root-helper independently re-verifies the raw admin bearer before
// forwarding a privileged call. Bearer values must never be logged; see
// Credential for how that is enforced even under %v/%s formatting.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// ---------------------------------------------------------------------------
// Bearer classes
// ---------------------------------------------------------------------------

// Class identifies which of the two bearer classes a credential belongs to.
// The class is encoded in the bearer's prefix and is not secret: it only
// says what kind of credential this is, not whether it is valid.
type Class string

const (
	// ClassUser is the bearer class presented by ordinary MCP clients
	// (the Cua agent loop) and carries the "hadron:user" scope.
	ClassUser Class = "user"
	// ClassAdmin is the bearer class presented by the root-helper's
	// privileged callers and carries the "hadron:admin" scope.
	ClassAdmin Class = "admin"
)

const bearerRandomBytes = 32

var prefixByClass = map[Class]string{
	ClassUser:  "hdn_u_",
	ClassAdmin: "hdn_a_",
}

// scopeForClass returns the MCP scope string a successfully verified bearer
// of class grants.
func scopeForClass(class Class) string {
	if class == ClassAdmin {
		return "hadron:admin"
	}
	return "hadron:user"
}

// ---------------------------------------------------------------------------
// Digest
// ---------------------------------------------------------------------------

// Digest is a credential's at-rest representation: the literal string
// "sha256:" followed by exactly 64 lowercase hexadecimal characters (the
// hex-encoded SHA-256 sum of a complete bearer string, prefix included).
// Digests, unlike bearers, are safe to store in configuration and logs.
type Digest string

const (
	digestPrefix = "sha256:"
	digestHexLen = 64
)

// Validate reports whether d has the exact "sha256:<64 lowercase hex>"
// format. It does not check that d corresponds to any real bearer.
func (d Digest) Validate() error {
	s := string(d)
	if !strings.HasPrefix(s, digestPrefix) {
		return fmt.Errorf("auth: digest must start with %q", digestPrefix)
	}
	h := s[len(digestPrefix):]
	if len(h) != digestHexLen {
		return fmt.Errorf("auth: digest hex part must be %d characters, got %d", digestHexLen, len(h))
	}
	for _, r := range h {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("auth: digest must be lowercase hex")
		}
	}
	return nil
}

// hex64 returns the 64 hex characters following the "sha256:" prefix.
// Callers must only call this on a Digest that has already passed
// Validate.
func (d Digest) hex64() string {
	return string(d)[len(digestPrefix):]
}

// digestBearer computes the at-rest Digest of a complete bearer string
// (prefix included).
func digestBearer(bearer string) Digest {
	sum := sha256.Sum256([]byte(bearer))
	return Digest(digestPrefix + hex.EncodeToString(sum[:]))
}

// ---------------------------------------------------------------------------
// Generate
// ---------------------------------------------------------------------------

// Generate creates a fresh bearer of the given class: "hdn_u_" or "hdn_a_"
// followed by 32 crypto/rand bytes encoded as unpadded base64url. It
// returns both the raw bearer (present this to the caller exactly once;
// never store it) and its Digest (store this).
func Generate(class Class) (bearer string, digest Digest, err error) {
	prefix, ok := prefixByClass[class]
	if !ok {
		return "", "", fmt.Errorf("auth: unknown bearer class %q", class)
	}
	buf := make([]byte, bearerRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("auth: generate bearer: %w", err)
	}
	bearer = prefix + base64.RawURLEncoding.EncodeToString(buf)
	return bearer, digestBearer(bearer), nil
}

// parseBearerClass validates rawBearer's format (a known class prefix
// followed by exactly bearerRandomBytes of unpadded base64url) and, if
// valid, returns the class its prefix names. The prefix itself is public
// metadata, so matching it does not need to run in constant time; only the
// secret digest comparison in Verify does.
func parseBearerClass(rawBearer string) (Class, error) {
	for class, prefix := range prefixByClass {
		if !strings.HasPrefix(rawBearer, prefix) {
			continue
		}
		payload := strings.TrimPrefix(rawBearer, prefix)
		if !isWellFormedPayload(payload) {
			return "", ErrMalformedBearer
		}
		return class, nil
	}
	return "", ErrMalformedBearer
}

// isWellFormedPayload reports whether payload is exactly the unpadded
// base64url encoding of bearerRandomBytes random bytes.
func isWellFormedPayload(payload string) bool {
	if len(payload) != base64.RawURLEncoding.EncodedLen(bearerRandomBytes) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	return len(decoded) == bearerRandomBytes
}

// ---------------------------------------------------------------------------
// Credential (private payload carried in TokenInfo.Extra)
// ---------------------------------------------------------------------------

// Credential is the value Verify places in TokenInfo.Extra["credential"].
// It carries the class and the complete raw bearer so that only code that
// deliberately imports this package and calls Bearer() -- the root-helper
// proxy, which must forward the exact bearer upstream -- can retrieve the
// secret. Ordinary consumers of TokenInfo (Scopes, UserID) never need it.
//
// Fields are unexported so json.Marshal and any struct-literal comparison
// outside this package cannot touch the raw bearer. That alone is *not*
// sufficient to stop a leak through fmt: Go's default struct formatter
// prints unexported field values via reflection even though external
// packages cannot read them directly (verified: fmt.Sprintf("%v", struct{
// s string }{"secret"}) prints "secret"). The only way to stop that is to
// implement Stringer/GoStringer ourselves, so -- deliberately, and for that
// reason -- Credential implements both, returning a fixed redacted
// placeholder instead of the class/bearer, so %v, %s, %+v, %#v, and the
// Print family can never echo a bearer into a log line.
type Credential struct {
	class  Class
	bearer string
}

// Class reports which bearer class the credential belongs to.
func (c Credential) Class() Class { return c.class }

// Bearer returns the complete raw bearer string. Callers must never log,
// print, or otherwise format this value themselves; it exists solely so
// the root-helper proxy can forward the same bearer to the privileged
// upstream it authenticates.
func (c Credential) Bearer() string { return c.bearer }

// String implements fmt.Stringer with a fixed placeholder so %v/%s/%+v and
// the Print family never reveal the class or the raw bearer.
func (c Credential) String() string { return "auth.Credential{REDACTED}" }

// GoString implements fmt.GoStringer for the same reason, covering %#v.
func (c Credential) GoString() string { return "auth.Credential{REDACTED}" }

// ---------------------------------------------------------------------------
// Rotation configuration
// ---------------------------------------------------------------------------

// MaxRotationOverlap is the longest a Previous digest may remain valid
// after a RotationConfig is constructed. NewVerifier rejects any
// configuration whose overlap window exceeds it.
const MaxRotationOverlap = 24 * time.Hour

// RotationConfig configures the digests a Verifier accepts for one bearer
// class. Current is always accepted. Previous, if set, is accepted only
// until PreviousValidUntil, giving credential rotation a bounded overlap
// window instead of letting the old digest remain valid forever.
type RotationConfig struct {
	// Current is the digest of the presently active bearer for this
	// class. Required.
	Current Digest
	// Previous is the digest of the bearer being rotated out. Leave the
	// zero value ("") when no rotation is in progress.
	Previous Digest
	// PreviousValidUntil is the instant Previous stops being accepted.
	// Required when Previous is set; must not be set otherwise.
	PreviousValidUntil time.Time
}

// validate checks cfg for internal consistency, using now as the reference
// instant for the overlap-window bound.
func (cfg RotationConfig) validate(now time.Time) error {
	if err := cfg.Current.Validate(); err != nil {
		return fmt.Errorf("current: %w", err)
	}
	if cfg.Previous == "" {
		if !cfg.PreviousValidUntil.IsZero() {
			return errors.New("previous_valid_until set without previous")
		}
		return nil
	}
	if err := cfg.Previous.Validate(); err != nil {
		return fmt.Errorf("previous: %w", err)
	}
	if cfg.Previous == cfg.Current {
		return errors.New("previous must differ from current")
	}
	if cfg.PreviousValidUntil.IsZero() {
		return errors.New("previous set without previous_valid_until")
	}
	if overlap := cfg.PreviousValidUntil.Sub(now); overlap > MaxRotationOverlap {
		return fmt.Errorf("rotation overlap %s exceeds maximum %s", overlap, MaxRotationOverlap)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Verify errors
// ---------------------------------------------------------------------------

var (
	// ErrMalformedBearer means the presented string is not a
	// syntactically valid bearer for either class (unknown prefix, or a
	// payload that is not exactly 32 bytes of unpadded base64url).
	ErrMalformedBearer = errors.New("auth: malformed bearer")
	// ErrWrongClass means the bearer is well-formed and of a known
	// class, but that class does not match the class the caller
	// required (a user bearer presented where admin was required, or
	// vice versa).
	ErrWrongClass = errors.New("auth: bearer class does not match required class")
	// ErrInvalidBearer means the bearer is well-formed and of the
	// required class, but its digest does not match any currently
	// accepted candidate (neither the current digest, nor an unexpired
	// previous digest).
	ErrInvalidBearer = errors.New("auth: bearer does not match any configured credential")
)

// Code maps an error returned by Verify to the stable api.ErrorCode the
// gateway should report in its response envelope. It returns "" for a nil
// error and api.CodeUnauthenticated for any error it does not specifically
// recognize, so a caller can always safely report *some* stable code.
func Code(err error) api.ErrorCode {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrWrongClass):
		return api.CodeForbidden
	default:
		return api.CodeUnauthenticated
	}
}

// ---------------------------------------------------------------------------
// Verifier
// ---------------------------------------------------------------------------

// Verifier holds the current rotation configuration for both bearer
// classes and verifies presented bearers against them.
type Verifier struct {
	user  RotationConfig
	admin RotationConfig
	now   func() time.Time
}

// NewVerifier constructs a Verifier from the user and admin rotation
// configurations. It rejects a configuration whose current digest is
// malformed, whose previous/previous_valid_until pairing is inconsistent,
// or whose rotation overlap window exceeds MaxRotationOverlap as measured
// from the moment NewVerifier is called.
func NewVerifier(user, admin RotationConfig) (*Verifier, error) {
	return newVerifierAt(time.Now(), user, admin)
}

// newVerifierAt is NewVerifier with an explicit reference instant, used by
// this package's own tests to exercise the rotation-overlap boundary and
// expiry behavior deterministically.
func newVerifierAt(now time.Time, user, admin RotationConfig) (*Verifier, error) {
	if err := user.validate(now); err != nil {
		return nil, fmt.Errorf("auth: user rotation config: %w", err)
	}
	if err := admin.validate(now); err != nil {
		return nil, fmt.Errorf("auth: admin rotation config: %w", err)
	}
	return &Verifier{user: user, admin: admin, now: time.Now}, nil
}

// configFor returns the RotationConfig governing class.
func (v *Verifier) configFor(class Class) RotationConfig {
	if class == ClassAdmin {
		return v.admin
	}
	return v.user
}

// testCompareHook, when non-nil, is invoked once per constant-time digest
// comparison performed inside Verify. It exists solely so this package's
// own tests can assert that every configured candidate digest is always
// compared -- current and, when configured, previous -- regardless of
// whether an earlier candidate already matched. Production code never sets
// it; it has no effect outside of _test.go files in this package.
var testCompareHook func()

// compareDigest reports, as 1 or 0, whether got and want are equal, using
// subtle.ConstantTimeCompare so the comparison time does not depend on
// where got and want first differ.
func compareDigest(got, want Digest) int {
	if testCompareHook != nil {
		testCompareHook()
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want))
}

// Verify checks rawBearer against the Verifier's configured digests for
// requiredClass and, on success, returns an *sdkauth.TokenInfo carrying the
// scope for that class, a UserID derived from the digest, and a private
// Credential (class + raw bearer) in Extra["credential"].
//
// Once rawBearer's format and class prefix are confirmed (public
// properties of the string itself, so this part is not constant-time),
// verification hashes the complete bearer and compares the resulting
// digest against every currently configured candidate -- the class's
// current digest, and its previous digest if one is configured -- using
// subtle.ConstantTimeCompare. Both comparisons always run and their
// results are combined with bitwise OR/AND rather than an early return, so
// neither how many candidates are configured nor which one (if any)
// matched is observable from the number or timing of comparisons
// performed.
func (v *Verifier) Verify(rawBearer string, requiredClass Class) (*sdkauth.TokenInfo, error) {
	class, err := parseBearerClass(rawBearer)
	if err != nil {
		return nil, err
	}
	if class != requiredClass {
		return nil, ErrWrongClass
	}

	cfg := v.configFor(class)
	digest := digestBearer(rawBearer)

	// currentMatch and previousMatch are computed independently and both
	// constant-time digest comparisons always execute -- see the
	// non-short-circuit contract documented above.
	currentMatch := compareDigest(digest, cfg.Current)

	previousMatch := 0
	if cfg.Previous != "" {
		rawMatch := compareDigest(digest, cfg.Previous)
		unexpired := 0
		if !v.now().After(cfg.PreviousValidUntil) {
			unexpired = 1
		}
		previousMatch = rawMatch & unexpired
	}

	if currentMatch|previousMatch != 1 {
		return nil, ErrInvalidBearer
	}

	hex := digest.hex64()
	return &sdkauth.TokenInfo{
		Scopes: []string{scopeForClass(class)},
		UserID: hex[:16],
		Extra: map[string]any{
			"credential": Credential{class: class, bearer: rawBearer},
		},
	}, nil
}
