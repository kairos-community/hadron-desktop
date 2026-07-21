// Package smoke implements the public MCP smoke/privilege client for the
// Hadron Cua agent appliance. It drives the appliance's public HTTPS MCP
// gateway using ONLY the eight public MCP tools and asserts both the public
// contract (exactly eight tools, every tool callable) and the privilege
// boundaries that separate the unprivileged `agent` user from admin/root.
//
// The package is deliberately split so its assertion logic can be exercised
// both against a live fixture (the built mcp-smoke binary) and, in unit tests,
// against a real in-process gateway wired up over httptest TLS with no VM:
//
//   - descriptor.go loads and validates the frozen fixture descriptor;
//   - client.go builds a CA-pinned, bearer-injecting MCP client;
//   - checks.go is the contract suite: the credential-scoped assertions;
//   - run.go is the command entry point and the deterministic exit codes.
//
// It never logs, serializes, or otherwise emits a bearer value, a command
// line, or file contents into its results or diagnostics.
package smoke

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Descriptor is the frozen fixture descriptor emitted by the Phase-4 fixture
// (test/agent/run.sh) and validated against test/agent/fixture.schema.json.
// It is exactly the eight string fields of that schema; the smoke client
// consumes mcp_url, bearer_token_file, ca_certificate_file, and
// artifact_directory.
type Descriptor struct {
	MCPURL            string `json:"mcp_url"`
	BearerTokenFile   string `json:"bearer_token_file"`
	CACertificateFile string `json:"ca_certificate_file"`
	TLSFingerprint    string `json:"tls_fingerprint"`
	VNCAddress        string `json:"vnc_address"`
	NoVNCURL          string `json:"novnc_url"`
	QMPSocket         string `json:"qmp_socket"`
	ArtifactDirectory string `json:"artifact_directory"`
}

// LoadDescriptor reads and validates the descriptor JSON at path. A parse
// failure or a missing/malformed required field is a descriptor error, which
// the caller maps to exit code 2.
func LoadDescriptor(path string) (Descriptor, error) {
	var d Descriptor
	data, err := os.ReadFile(path)
	if err != nil {
		return d, fmt.Errorf("read descriptor: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, fmt.Errorf("parse descriptor: %w", err)
	}
	if err := d.validate(); err != nil {
		return d, err
	}
	return d, nil
}

// validate enforces the descriptor invariants the smoke client relies on: an
// https /mcp URL and absolute paths for the three files/directories it reads
// or writes.
func (d Descriptor) validate() error {
	if !strings.HasPrefix(d.MCPURL, "https://") || !strings.HasSuffix(d.MCPURL, "/mcp") {
		return fmt.Errorf("mcp_url must be https and end in /mcp, got %q", d.MCPURL)
	}
	for name, v := range map[string]string{
		"bearer_token_file":   d.BearerTokenFile,
		"ca_certificate_file": d.CACertificateFile,
		"artifact_directory":  d.ArtifactDirectory,
	} {
		if !strings.HasPrefix(v, "/") {
			return fmt.Errorf("%s must be an absolute path, got %q", name, v)
		}
	}
	return nil
}

// readBearer reads a bearer token from a mode-0600 file, trimming trailing
// whitespace. The value is never logged or returned in any error.
func readBearer(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read bearer file %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("bearer file %s is empty", path)
	}
	return tok, nil
}
