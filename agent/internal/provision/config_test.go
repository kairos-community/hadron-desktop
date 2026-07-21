package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validDigest is a syntactically valid "sha256:<64 lowercase hex>" digest,
// taken verbatim from the phase3-task-1 brief's example hadron_agent block.
const validDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// anotherValidDigest is a second, DIFFERENT valid digest used to exercise
// the "conflicting secret" merge rule.
const anotherValidDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func writeOEMFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// ---------------------------------------------------------------------------
// Merge across sorted /oem/*.yaml files
// ---------------------------------------------------------------------------

func TestLoadMergesFilesInSortedOrderLastScalarWins(t *testing.T) {
	dir := t.TempDir()
	// Filenames deliberately sort as 01 < 02 alphabetically; the LATER file
	// (02-override.yaml) must win for the scalars it redefines.
	writeOEMFile(t, dir, "01-base.yaml", `
hadron_agent:
  endpoint:
    listen: "127.0.0.1:9000"
    mdns: true
  limits:
    max_processes: 4
`)
	writeOEMFile(t, dir, "02-override.yaml", `
hadron_agent:
  endpoint:
    listen: "0.0.0.0:8000"
    mdns: false
`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Endpoint.Listen != "0.0.0.0:8000" {
		t.Errorf("Listen = %q, want last-defined %q", cfg.Endpoint.Listen, "0.0.0.0:8000")
	}
	if cfg.Endpoint.MDNS != false {
		t.Errorf("MDNS = %v, want last-defined false", cfg.Endpoint.MDNS)
	}
	// max_processes was only ever set in the first file, so it must survive
	// the merge unmodified.
	if cfg.Limits.MaxProcesses != 4 {
		t.Errorf("MaxProcesses = %d, want 4 (only set in 01-base.yaml)", cfg.Limits.MaxProcesses)
	}
}

func TestLoadProcessesFilesInLexicalFilenameOrderRegardlessOfCreationOrder(t *testing.T) {
	dir := t.TempDir()
	// Create the file that should win FIRST on disk, but give it a filename
	// that sorts LAST, to prove ordering is by filename, not mtime/creation.
	writeOEMFile(t, dir, "a-first-created-sorts-last.yaml", `
hadron_agent:
  endpoint:
    mdns: true
`)
	writeOEMFile(t, dir, "0-created-second-sorts-first.yaml", `
hadron_agent:
  endpoint:
    mdns: false
`)
	// Filename sort order: "0-created-second-sorts-first.yaml" < "a-first-created-sorts-last.yaml"
	// so the "a-..." file is applied LAST and must win.
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Endpoint.MDNS != true {
		t.Errorf("MDNS = %v, want true (last in sorted filename order)", cfg.Endpoint.MDNS)
	}
}

func TestLoadIgnoresUnrelatedKairosKeys(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01-kairos.yaml", `
users:
  - name: kairos
    passwd: kairos
stages:
  boot:
    - name: "setup"
      commands:
        - echo hello
hadron_agent:
  endpoint:
    listen: "0.0.0.0:7443"
  auth:
    user_token_hash: "`+validDigest+`"
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.UserTokenHash != validDigest {
		t.Errorf("UserTokenHash = %q, want %q", cfg.Auth.UserTokenHash, validDigest)
	}
	if !cfg.Enabled {
		t.Errorf("Enabled = false, want true")
	}
}

func TestLoadRejectsConflictingSecretAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01-first.yaml", `
hadron_agent:
  auth:
    user_token_hash: "`+validDigest+`"
`)
	writeOEMFile(t, dir, "02-second.yaml", `
hadron_agent:
  auth:
    user_token_hash: "`+anotherValidDigest+`"
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatalf("Load: want error for conflicting user_token_hash across files, got nil")
	}
	if !strings.Contains(err.Error(), "user_token_hash") {
		t.Errorf("error %q does not mention the conflicting field", err)
	}
}

func TestLoadAllowsSameSecretRepeatedAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01-first.yaml", `
hadron_agent:
  auth:
    user_token_hash: "`+validDigest+`"
`)
	writeOEMFile(t, dir, "02-second.yaml", `
hadron_agent:
  auth:
    user_token_hash: "`+validDigest+`"
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.UserTokenHash != validDigest {
		t.Errorf("UserTokenHash = %q, want %q", cfg.Auth.UserTokenHash, validDigest)
	}
}

// ---------------------------------------------------------------------------
// Opt-in default (absent section) vs explicit enabled=false
// ---------------------------------------------------------------------------

func TestLoadAbsentSectionResolvesEnabledWithDefaults(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01-unrelated.yaml", `
users:
  - name: kairos
    passwd: kairos
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Enabled {
		t.Errorf("Enabled = false, want true (opt-in default)")
	}
	if cfg.Endpoint.Listen != "0.0.0.0:7443" {
		t.Errorf("Listen = %q, want default %q", cfg.Endpoint.Listen, "0.0.0.0:7443")
	}
	if !cfg.Endpoint.MDNS {
		t.Errorf("MDNS = false, want default true")
	}
	if cfg.Endpoint.InsecureLoopback {
		t.Errorf("InsecureLoopback = true, want default false")
	}
	if cfg.Limits.MaxConcurrentCalls != 16 {
		t.Errorf("MaxConcurrentCalls = %d, want 16", cfg.Limits.MaxConcurrentCalls)
	}
	if cfg.Limits.MaxProcesses != 16 {
		t.Errorf("MaxProcesses = %d, want 16", cfg.Limits.MaxProcesses)
	}
	if cfg.Limits.MaxRequestBytes != 2097152 {
		t.Errorf("MaxRequestBytes = %d, want 2097152", cfg.Limits.MaxRequestBytes)
	}
}

func TestLoadEmptyOEMDirResolvesEnabledWithDefaults(t *testing.T) {
	dir := t.TempDir() // no *.yaml files at all
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Enabled {
		t.Errorf("Enabled = false, want true (opt-in default)")
	}
}

func TestLoadMissingOEMDirResolvesEnabledWithDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Enabled {
		t.Errorf("Enabled = false, want true (opt-in default)")
	}
}

func TestLoadExplicitDisabledSuppressesConfig(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01-disable.yaml", `
hadron_agent:
  enabled: false
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Enabled {
		t.Errorf("Enabled = true, want false (explicit opt-out)")
	}
}

// ---------------------------------------------------------------------------
// Full-schema validation: each reject rule
// ---------------------------------------------------------------------------

func TestValidateRejectsNonLoopbackListenerWithInsecureLoopback(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01.yaml", `
hadron_agent:
  endpoint:
    listen: "0.0.0.0:7443"
    insecure_loopback: true
`)
	if _, err := Load(dir); err == nil {
		t.Fatalf("Load: want error for insecure_loopback on non-loopback listener, got nil")
	}
}

func TestValidateAllowsLoopbackListenerWithInsecureLoopback(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01.yaml", `
hadron_agent:
  endpoint:
    listen: "127.0.0.1:7443"
    insecure_loopback: true
`)
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestValidateRejectsBadDigestPrefix(t *testing.T) {
	cases := map[string]string{
		"user_token_hash":           "md5:0000000000000000000000000000000000000000000000000000000000000000",
		"user_previous_token_hash":  "not-a-digest-at-all",
		"admin_token_hash":          "sha256:tooshort",
		"admin_previous_token_hash": "sha256:ZZZZ000000000000000000000000000000000000000000000000000000000000", // uppercase not allowed
	}
	for field, bad := range cases {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			extra := ""
			if strings.HasPrefix(field, "user_previous") || strings.HasPrefix(field, "admin_previous") {
				// previous_token_hash requires previous_valid_until and a
				// current token_hash to even reach digest validation for the
				// previous field itself; supply a valid current + valid_until
				// so only the bad digest under test trips validation.
				class := "user"
				if strings.HasPrefix(field, "admin") {
					class = "admin"
				}
				extra = "\n    " + class + "_token_hash: \"" + validDigest + "\"\n    " + class + "_previous_valid_until: \"" + time.Now().Add(time.Hour).Format(time.RFC3339) + "\""
			}
			writeOEMFile(t, dir, "01.yaml", `
hadron_agent:
  auth:
    `+field+`: "`+bad+`"`+extra+`
`)
			if _, err := Load(dir); err == nil {
				t.Fatalf("Load: want error for bad digest prefix on %s=%q, got nil", field, bad)
			}
		})
	}
}

func TestValidateAllowsEmptyDigestFields(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01.yaml", `
hadron_agent:
  enabled: true
`)
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestValidateRejectsRotationOverlapOverTwentyFourHours(t *testing.T) {
	dir := t.TempDir()
	tooFar := time.Now().Add(25 * time.Hour).Format(time.RFC3339)
	writeOEMFile(t, dir, "01.yaml", `
hadron_agent:
  auth:
    user_token_hash: "`+validDigest+`"
    user_previous_token_hash: "`+anotherValidDigest+`"
    user_previous_valid_until: "`+tooFar+`"
`)
	if _, err := Load(dir); err == nil {
		t.Fatalf("Load: want error for rotation overlap > 24h, got nil")
	}
}

func TestValidateAllowsRotationOverlapUnderTwentyFourHours(t *testing.T) {
	dir := t.TempDir()
	fine := time.Now().Add(23 * time.Hour).Format(time.RFC3339)
	writeOEMFile(t, dir, "01.yaml", `
hadron_agent:
  auth:
    user_token_hash: "`+validDigest+`"
    user_previous_token_hash: "`+anotherValidDigest+`"
    user_previous_valid_until: "`+fine+`"
`)
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestValidateRejectsPortZero(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01.yaml", `
hadron_agent:
  endpoint:
    listen: "0.0.0.0:0"
`)
	if _, err := Load(dir); err == nil {
		t.Fatalf("Load: want error for port zero, got nil")
	}
}

func TestValidateRejectsUnknownTopLevelField(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01.yaml", `
hadron_agent:
  enabled: true
  bogus_field: true
`)
	if _, err := Load(dir); err == nil {
		t.Fatalf("Load: want error for unknown top-level hadron_agent field, got nil")
	}
}

// TestValidateRejectsUnknownNestedField locks in a byproduct of decoding
// hadron_agent with yaml.Decoder.KnownFields(true): strictness applies to
// every nested struct in the subtree, not only the top level, so a typo
// inside e.g. "endpoint" is caught too.
func TestValidateRejectsUnknownNestedField(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01.yaml", `
hadron_agent:
  endpoint:
    listen: "0.0.0.0:7443"
    bogus_nested_field: true
`)
	if _, err := Load(dir); err == nil {
		t.Fatalf("Load: want error for unknown nested endpoint field, got nil")
	}
}

func TestValidateRejectsTLSOnlyCertificateSet(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01.yaml", `
hadron_agent:
  tls:
    certificate_pem: "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----"
`)
	if _, err := Load(dir); err == nil {
		t.Fatalf("Load: want error for certificate_pem without private_key_pem, got nil")
	}
}

func TestValidateRejectsTLSOnlyPrivateKeySet(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01.yaml", `
hadron_agent:
  tls:
    private_key_pem: "-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----"
`)
	if _, err := Load(dir); err == nil {
		t.Fatalf("Load: want error for private_key_pem without certificate_pem, got nil")
	}
}

func TestValidateAllowsTLSBothSet(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01.yaml", `
hadron_agent:
  tls:
    certificate_pem: "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----"
    private_key_pem: "-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----"
`)
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestValidateAllowsTLSNeitherSet(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01.yaml", `
hadron_agent:
  enabled: true
`)
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Full example from the brief round-trips cleanly.
// ---------------------------------------------------------------------------

func TestLoadFullBriefExampleRoundTrips(t *testing.T) {
	dir := t.TempDir()
	writeOEMFile(t, dir, "01.yaml", `
hadron_agent:
  enabled: true
  endpoint:
    listen: "0.0.0.0:7443"
    mdns: true
    insecure_loopback: false
  auth:
    user_token_hash: "`+validDigest+`"
    user_previous_token_hash: ""
    user_previous_valid_until: ""
    admin_token_hash: ""
    admin_previous_token_hash: ""
    admin_previous_valid_until: ""
  tls:
    certificate_pem: ""
    private_key_pem: ""
  limits:
    max_concurrent_calls: 16
    max_processes: 16
    max_request_bytes: 2097152
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Enabled {
		t.Error("Enabled = false, want true")
	}
	if cfg.Endpoint.Listen != "0.0.0.0:7443" {
		t.Errorf("Listen = %q", cfg.Endpoint.Listen)
	}
	if cfg.Auth.UserTokenHash != validDigest {
		t.Errorf("UserTokenHash = %q", cfg.Auth.UserTokenHash)
	}
	if cfg.Limits.MaxConcurrentCalls != 16 || cfg.Limits.MaxProcesses != 16 || cfg.Limits.MaxRequestBytes != 2097152 {
		t.Errorf("Limits = %+v", cfg.Limits)
	}
}

// TestConfigValidateDirect exercises Config.Validate() directly (not via
// Load) to confirm it is usable standalone on a hand-built Config.
func TestConfigValidateDirect(t *testing.T) {
	cfg := Config{
		Enabled: true,
		Endpoint: Endpoint{
			Listen: "0.0.0.0:7443",
			MDNS:   true,
		},
		Limits: Limits{MaxConcurrentCalls: 16, MaxProcesses: 16, MaxRequestBytes: 2097152},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	cfg.Endpoint.Listen = "0.0.0.0:0"
	if err := cfg.Validate(); err == nil {
		t.Fatalf("Validate: want error for port zero")
	}
}
