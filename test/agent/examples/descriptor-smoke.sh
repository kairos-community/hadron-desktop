#!/usr/bin/env bash
# test/agent/examples/descriptor-smoke.sh -- the smallest complete downstream
# consumption of the agent VM fixture (Phase 4, Task 8).
#
# It does exactly what a downstream UI repository's CI does, minus the UI:
#
#   1. resolve a fixture descriptor (argument, HADRON_AGENT_FIXTURE, or the
#      default fixture artifact path);
#   2. validate it against test/agent/fixture.schema.json AND check the file
#      modes and token-leak rules that the schema alone cannot express;
#   3. build the public mcp-smoke client and run its `contract` suite, which
#      exercises all eight public MCP tools and the privilege boundaries.
#
# This script BOOTS NOTHING. Start the VM first, in another shell:
#
#   test/agent/run.sh fixture &
#   bash test/agent/examples/descriptor-smoke.sh
#   test/agent/run.sh stop
#
# Usage:
#   descriptor-smoke.sh [DESCRIPTOR]
#
# Environment:
#   HADRON_AGENT_FIXTURE  descriptor path (same variable the reusable CI
#                         workflow exports to the caller's test command)
#   ADMIN_TOKEN_FILE      admin bearer file. mcp-smoke requires one so it can
#                         prove that the DESCRIPTOR's ordinary bearer cannot
#                         reach root -- which is why the admin bearer is
#                         deliberately NOT a descriptor field. Defaults to the
#                         seed directory `run.sh fixture` mints into.
#   FIXTURE_RUNTIME       fixture runtime dir (default test/agent/runtime/fixture)
#   FIXTURE_ART           fixture artifact dir (default test/agent/artifacts/fixture)
#   MCP_SMOKE_BIN         prebuilt mcp-smoke binary; skips the Go build
#
# Exit codes are mcp-smoke's own, so this script is drop-in usable as a gate:
#   0 every assertion held      2 descriptor/arguments invalid
#   1 an assertion failed       3 transport/readiness failure
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AGENT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"          # test/agent
REPO_ROOT="$(cd "$AGENT_DIR/../.." && pwd)"

SCHEMA="$AGENT_DIR/fixture.schema.json"
FIXTURE_RUNTIME="${FIXTURE_RUNTIME:-$AGENT_DIR/runtime/fixture}"
FIXTURE_ART="${FIXTURE_ART:-$AGENT_DIR/artifacts/fixture}"

# Match the harness's own exit-code vocabulary (see agent/internal/smoke/run.go)
# so a caller can distinguish "the appliance is broken" from "we never got to
# ask it anything".
EXIT_ASSERTION=1
EXIT_DESCRIPTOR=2

log() { echo -e "\033[1;34m[smoke]\033[0m $*" >&2; }
err() { echo -e "\033[1;31m[smoke] $*\033[0m" >&2; }

# umask 077 + redaction, so anything this script creates or prints is safe.
# shellcheck source=test/agent/lib/common.sh disable=SC1091
source "$AGENT_DIR/lib/common.sh"

# ---------------------------------------------------------------------------
# 1. Resolve the descriptor
# ---------------------------------------------------------------------------
DESCRIPTOR="${1:-${HADRON_AGENT_FIXTURE:-$FIXTURE_ART/fixture.json}}"
if [ ! -f "$DESCRIPTOR" ]; then
  err "no descriptor at $DESCRIPTOR"
  err "start a fixture first:  $AGENT_DIR/run.sh fixture &"
  exit "$EXIT_DESCRIPTOR"
fi
log "Descriptor: $DESCRIPTOR"

# ---------------------------------------------------------------------------
# 2. Validate it
#
# Three separate properties, because the JSON Schema can only speak to the
# first: the descriptor must have exactly the eight fields in the right shapes;
# it and the bearer file it names must be mode 0600; and its own text must not
# contain a bearer. Only stdlib json/re is used, so this needs no pip install.
# ---------------------------------------------------------------------------
command -v python3 >/dev/null 2>&1 || { err "python3 is required"; exit "$EXIT_DESCRIPTOR"; }

if ! python3 - "$SCHEMA" "$DESCRIPTOR" <<'PY'
import json
import os
import re
import stat
import sys

schema_path, descriptor_path = sys.argv[1], sys.argv[2]

with open(schema_path) as f:
    schema = json.load(f)
with open(descriptor_path) as f:
    raw = f.read()

def fail(msg):
    print(f"descriptor invalid: {msg}", file=sys.stderr)
    sys.exit(1)

mode = stat.S_IMODE(os.stat(descriptor_path).st_mode)
if mode != 0o600:
    fail(f"mode {mode:o} is not 600 (group/world accessible)")

# Defense in depth: the descriptor points AT the token file, never carries it.
if re.search(r"hdn_[ua]_[A-Za-z0-9_-]{8,}", raw):
    fail("text contains a bearer-shaped token")

try:
    d = json.loads(raw)
except ValueError as e:
    fail(f"not valid JSON: {e}")
if not isinstance(d, dict):
    fail("not a JSON object")

required = set(schema["required"])
props = schema["properties"]
keys = set(d)
if required - keys:
    fail(f"missing required field(s): {sorted(required - keys)}")
if keys - set(props):
    fail(f"unexpected field(s): {sorted(keys - set(props))}")

for key, value in d.items():
    if not isinstance(value, str):
        fail(f"{key}: not a string")
    pattern = props[key].get("pattern")
    if pattern and not re.fullmatch(pattern, value):
        fail(f"{key}: {value!r} does not match {pattern!r}")

bearer = d["bearer_token_file"]
if not os.path.exists(bearer):
    fail(f"bearer_token_file does not exist: {bearer}")
bmode = stat.S_IMODE(os.stat(bearer).st_mode)
if bmode != 0o600:
    fail(f"bearer_token_file mode {bmode:o} is not 600")

if not os.path.exists(d["ca_certificate_file"]):
    fail(f"ca_certificate_file does not exist: {d['ca_certificate_file']}")

print(f"descriptor OK: {d['mcp_url']} (vnc {d['vnc_address']})")
PY
then
  err "descriptor validation failed"
  exit "$EXIT_DESCRIPTOR"
fi

# ---------------------------------------------------------------------------
# 3. Locate the admin bearer
# ---------------------------------------------------------------------------
ADMIN_TOKEN_FILE="${ADMIN_TOKEN_FILE:-$FIXTURE_RUNTIME/seed/admin.token}"
if [ ! -f "$ADMIN_TOKEN_FILE" ]; then
  err "no admin bearer at $ADMIN_TOKEN_FILE"
  err "set ADMIN_TOKEN_FILE, or point FIXTURE_RUNTIME at the running fixture"
  exit "$EXIT_DESCRIPTOR"
fi

# ---------------------------------------------------------------------------
# 4. Build mcp-smoke and run the contract suite
#
# The main package lives under test/agent so it ships with the harness, but it
# imports the agent module's internal/smoke package -- Go's internal rule means
# it can only build from INSIDE that module, so it is copied in and removed
# again. This mirrors _agent_build_mcp_smoke in run.sh.
# ---------------------------------------------------------------------------
SMOKE="${MCP_SMOKE_BIN:-}"
if [ -z "$SMOKE" ]; then
  command -v go >/dev/null 2>&1 \
    || { err "a Go toolchain is required to build mcp-smoke (or set MCP_SMOKE_BIN)"; exit "$EXIT_DESCRIPTOR"; }
  moddir="$REPO_ROOT/agent/cmd/mcp-smoke"
  mkdir -p "$moddir"
  cp -f "$AGENT_DIR/cmd/mcp-smoke/main.go" "$moddir/main.go" \
    || { err "could not stage mcp-smoke into the agent module"; exit "$EXIT_DESCRIPTOR"; }
  hdn_agent_on_exit "rm -rf '$moddir'"

  SMOKE="$(mktemp -d)/mcp-smoke"
  hdn_agent_on_exit "rm -rf '$(dirname "$SMOKE")'"
  log "Building mcp-smoke"
  ( cd "$REPO_ROOT/agent" && CGO_ENABLED=0 go build -trimpath -o "$SMOKE" ./cmd/mcp-smoke ) >&2 \
    || { err "mcp-smoke build failed"; exit "$EXIT_DESCRIPTOR"; }
fi

log "Running the mcp-smoke contract suite (eight public tools + privilege boundaries)"
"$SMOKE" --mode contract \
  --descriptor "$DESCRIPTOR" \
  --admin-bearer-file "$ADMIN_TOKEN_FILE"
rc=$?

case "$rc" in
  0) log "PASS: descriptor valid and every contract assertion held" ;;
  "$EXIT_ASSERTION") err "FAIL: a contract assertion was violated" ;;
  *) err "FAIL: mcp-smoke exited $rc (2 = descriptor/arguments, 3 = transport/readiness)" ;;
esac
exit "$rc"
