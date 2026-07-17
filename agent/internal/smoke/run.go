package smoke

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Deterministic process exit codes. These are part of the smoke client's
// contract with the CI controller that runs it live.
const (
	// ExitPass means every assertion held.
	ExitPass = 0
	// ExitAssertion means at least one assertion was violated.
	ExitAssertion = 1
	// ExitDescriptor means the descriptor or command arguments were invalid.
	ExitDescriptor = 2
	// ExitTransport means the gateway was unreachable/not ready, or an MCP
	// transport error prevented an assertion from being evaluated.
	ExitTransport = 3
)

// artifactReport is the JSON document written to the fixture's artifact
// directory. It carries only redacted, operator-safe data: no bearer values,
// no command lines, no file contents.
type artifactReport struct {
	Mode     string        `json:"mode"`
	Started  time.Time     `json:"started"`
	Finished time.Time     `json:"finished"`
	ExitCode int           `json:"exit_code"`
	Passed   int           `json:"passed"`
	Failed   int           `json:"failed"`
	Checks   []CheckResult `json:"checks"`
}

// Main is the mcp-smoke command entry point. It parses args, loads the
// descriptor, drives the gateway, writes the redacted results artifact, and
// returns the deterministic exit code. It never emits a bearer, a command
// line, or file contents.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp-smoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	descriptorPath := fs.String("descriptor", "", "path to the fixture descriptor JSON (required)")
	adminBearerFile := fs.String("admin-bearer-file", "", "path to a file holding the admin (hdn_a_) bearer (required)")
	mode := fs.String("mode", "contract", "smoke mode: contract, persist-write, or persist-verify")
	marker := fs.String("marker", "", "opaque marker value for the persist-write/persist-verify modes")
	readyTimeout := fs.Duration("ready-timeout", 60*time.Second, "how long to wait for the gateway /readyz")
	callTimeout := fs.Duration("call-timeout", 30*time.Second, "per tool-call timeout")
	if err := fs.Parse(args); err != nil {
		return ExitDescriptor
	}

	switch *mode {
	case "contract", "persist-write", "persist-verify":
	default:
		fmt.Fprintf(stderr, "mcp-smoke: unknown mode %q (want: contract, persist-write, or persist-verify)\n", *mode)
		return ExitDescriptor
	}
	if *descriptorPath == "" {
		fmt.Fprintln(stderr, "mcp-smoke: --descriptor is required")
		return ExitDescriptor
	}
	if *adminBearerFile == "" {
		fmt.Fprintln(stderr, "mcp-smoke: --admin-bearer-file is required")
		return ExitDescriptor
	}
	if (*mode == "persist-write" || *mode == "persist-verify") && *marker == "" {
		fmt.Fprintf(stderr, "mcp-smoke: --marker is required for mode %q\n", *mode)
		return ExitDescriptor
	}

	desc, err := LoadDescriptor(*descriptorPath)
	if err != nil {
		fmt.Fprintf(stderr, "mcp-smoke: %v\n", err)
		return ExitDescriptor
	}

	userBearer, err := readBearer(desc.BearerTokenFile)
	if err != nil {
		fmt.Fprintf(stderr, "mcp-smoke: %v\n", err)
		return ExitDescriptor
	}
	adminBearer, err := readBearer(*adminBearerFile)
	if err != nil {
		fmt.Fprintf(stderr, "mcp-smoke: %v\n", err)
		return ExitDescriptor
	}

	client, err := NewTLSClient(desc.MCPURL, desc.CACertificateFile)
	if err != nil {
		fmt.Fprintf(stderr, "mcp-smoke: %v\n", err)
		return ExitDescriptor
	}

	// The artifact directory is part of the descriptor; an unwritable one is a
	// descriptor error surfaced before any network work.
	if err := os.MkdirAll(desc.ArtifactDirectory, 0o755); err != nil {
		fmt.Fprintf(stderr, "mcp-smoke: artifact directory: %v\n", err)
		return ExitDescriptor
	}

	ctx := context.Background()
	started := time.Now()

	if err := client.WaitReady(ctx, *readyTimeout); err != nil {
		fmt.Fprintf(stderr, "mcp-smoke: %v\n", err)
		// Still record a minimal artifact so the controller sees the outcome.
		writeArtifact(desc.ArtifactDirectory, artifactReport{
			Mode: *mode, Started: started, Finished: time.Now(), ExitCode: ExitTransport,
			Checks: []CheckResult{failTransport("readiness", "gateway not ready")},
		}, stderr)
		return ExitTransport
	}

	suite := &Suite{
		Client:      client,
		UserBearer:  userBearer,
		AdminBearer: adminBearer,
		CallTimeout: *callTimeout,
	}
	var report Report
	switch *mode {
	case "persist-write":
		report = suite.RunPersistWrite(ctx, *marker)
	case "persist-verify":
		report = suite.RunPersistVerify(ctx, *marker)
	default:
		report = suite.RunContract(ctx)
	}
	exit := report.Outcome()

	passed, failed := tally(report.Checks)
	writeArtifact(desc.ArtifactDirectory, artifactReport{
		Mode:     report.Mode,
		Started:  started,
		Finished: time.Now(),
		ExitCode: exit,
		Passed:   passed,
		Failed:   failed,
		Checks:   report.Checks,
	}, stderr)

	printSummary(stdout, report, exit)
	return exit
}

func tally(checks []CheckResult) (passed, failed int) {
	for _, c := range checks {
		if c.Passed {
			passed++
		} else {
			failed++
		}
	}
	return passed, failed
}

func printSummary(w io.Writer, report Report, exit int) {
	for _, c := range report.Checks {
		status := "PASS"
		if !c.Passed {
			status = "FAIL"
		}
		fmt.Fprintf(w, "[%s] %s: %s\n", status, c.Name, c.Detail)
	}
	passed, failed := tally(report.Checks)
	fmt.Fprintf(w, "mcp-smoke %s: %d passed, %d failed, exit %d\n", report.Mode, passed, failed, exit)
}

// writeArtifact writes the redacted results JSON to
// dir/mcp-<mode>.json (defaulting to mcp-smoke.json when the mode is empty).
// Naming the file per mode keeps the three install-gate invocations (contract,
// persist-write, persist-verify) from clobbering one another's results in a
// shared artifact directory. A write failure is reported to stderr but does not
// change the assertion outcome.
func writeArtifact(dir string, rep artifactReport, stderr io.Writer) {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "mcp-smoke: encode artifact: %v\n", err)
		return
	}
	name := "mcp-smoke.json"
	switch rep.Mode {
	case "persist-write":
		name = "mcp-persist-write.json"
	case "persist-verify":
		name = "mcp-persist-verify.json"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		fmt.Fprintf(stderr, "mcp-smoke: write artifact: %v\n", err)
	}
}
