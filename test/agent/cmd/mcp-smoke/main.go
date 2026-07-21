// Command mcp-smoke is the public MCP smoke/privilege client for the Hadron Cua
// agent appliance. It drives the appliance's public HTTPS MCP gateway using
// ONLY the eight public MCP tools and asserts both the public contract and the
// privilege boundaries that separate the unprivileged `agent` user from
// admin/root.
//
// It is test-only and, like test/agent/cmd/cua-compat-probe, its main package
// lives OUTSIDE the agent Go module tree so it can sit under test/agent; it is
// COPIED INTO the module (…/agent/cmd/mcp-smoke) at build time to satisfy Go's
// internal-package import rule, since all assertion logic lives in the agent
// module's internal/smoke package (which is where its unit test also lives, so
// the suite can be exercised against a real in-process gateway with no VM).
//
// Build it statically:
//
//	CGO_ENABLED=0 go build -trimpath -o mcp-smoke ./cmd/mcp-smoke
//
// Run it against a fixture descriptor:
//
//	mcp-smoke --descriptor fixture.json --admin-bearer-file admin.token
//
// Exit codes are deterministic: 0 all assertions held; 1 an assertion failed;
// 2 a descriptor/arguments error; 3 a transport/readiness error.
package main

import (
	"os"

	"github.com/mudler/hadron-desktop/agent/internal/smoke"
)

func main() {
	os.Exit(smoke.Main(os.Args[1:], os.Stdout, os.Stderr))
}
