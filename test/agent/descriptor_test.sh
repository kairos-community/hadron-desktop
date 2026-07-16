#!/usr/bin/env bash
# Tests for the frozen VM fixture descriptor (Phase 4, Task 1):
#
#   test/agent/fixture.schema.json  -- the exactly-8-field JSON Schema
#   test/agent/lib/common.sh        -- secret handling + process lifecycle
#
# Two layers:
#   PART 1: common.sh library behavior -- umask, the single EXIT trap
#           (including double-source safety), PID tracking/cleanup, the
#           hdn_u_/hdn_a_ redaction function, loopback port allocation, and a
#           static check that the file never enables xtrace.
#   PART 2: descriptor validation -- a schema+permission+leak checker
#           (hdn_check_descriptor) built from fixture.schema.json, exercised
#           against a valid descriptor and every required rejection case:
#           missing field, extra field, relative path, HTTP mcp_url, a
#           world-readable descriptor, a world-readable bearer file, and a
#           descriptor whose text contains token-shaped content.
#
# No new build dependencies: validation uses python3's stdlib json/re only
# (no jsonschema package), so this test is self-contained in CI.
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
schema="$repo_root/test/agent/fixture.schema.json"
common_sh="$repo_root/test/agent/lib/common.sh"

fail=0
err() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

# ---------------------------------------------------------------------------
# 0. syntax
# ---------------------------------------------------------------------------
if bash -n "$common_sh" 2>/dev/null; then
  ok "bash -n ${common_sh#"$repo_root"/}"
else
  err "bash -n failed: ${common_sh#"$repo_root"/}"
fi

if bash -n "${BASH_SOURCE[0]}" 2>/dev/null; then
  ok "bash -n ${BASH_SOURCE[0]#"$repo_root"/} (self)"
else
  err "bash -n failed on this test script"
fi

if python3 -c "import json; json.load(open('$schema'))" 2>/dev/null; then
  ok "fixture.schema.json is valid JSON"
else
  err "fixture.schema.json is not valid JSON"
fi

workdir="$(mktemp -d)"
export COMMON_SH_PATH="$common_sh"

# Use common.sh's own on_exit registry for teardown instead of calling
# `trap ... EXIT` ourselves, once we've sourced it below -- exercising the
# exact pattern later fixture scripts are expected to follow.
# shellcheck source=lib/common.sh
source "$common_sh"
hdn_agent_on_exit "rm -rf '$workdir'"

# ===========================================================================
# PART 1: common.sh library behavior
# ===========================================================================
echo "--- common.sh: umask ---"
got_umask="$(bash -c 'source "$COMMON_SH_PATH"; umask')"
case "$got_umask" in
  *077) ok "common.sh sets umask 077 (got $got_umask)" ;;
  *)    err "common.sh did not set umask 077 (got $got_umask)" ;;
esac

echo "--- common.sh: single EXIT trap ---"
trap_line="$(bash -c 'source "$COMMON_SH_PATH"; trap -p EXIT')"
case "$trap_line" in
  *hdn_agent_cleanup*) ok "installs an EXIT trap running hdn_agent_cleanup" ;;
  *)                   err "no hdn_agent_cleanup EXIT trap found (got: $trap_line)" ;;
esac

trap_count="$(bash -c 'source "$COMMON_SH_PATH"; trap -p EXIT' | grep -c .)"
if [ "$trap_count" -eq 1 ]; then
  ok "exactly one EXIT trap is registered"
else
  err "expected exactly 1 EXIT trap line, found $trap_count"
fi

double_source_count="$(bash -c 'source "$COMMON_SH_PATH"; source "$COMMON_SH_PATH"; trap -p EXIT' | grep -c .)"
if [ "$double_source_count" -eq 1 ]; then
  ok "double-sourcing common.sh does not stack a second EXIT trap"
else
  err "double-sourcing stacked traps (found $double_source_count EXIT trap lines)"
fi

echo "--- common.sh: never enables xtrace ---"
if grep -Ev '^[[:space:]]*#' "$common_sh" | grep -Eq '(^|[[:space:]])set[[:space:]]+-[a-zA-Z]*x|set[[:space:]]+-o[[:space:]]+xtrace'; then
  err "common.sh contains a 'set -x' / 'set -o xtrace' outside comments"
else
  ok "common.sh never enables xtrace"
fi

echo "--- common.sh: hdn_u_/hdn_a_ redaction ---"
export SAMPLE="pre token=hdn_u_AbCdEf0123456789-_ZZ mid admin=hdn_a_QwErTy9876543210_-YY post"
redacted="$(bash -c 'source "$COMMON_SH_PATH"; printf "%s" "$SAMPLE" | hdn_agent_redact')"
case "$redacted" in
  *hdn_u_AbCdEf0123456789-_ZZ*) err "redaction leaked the raw user token: $redacted" ;;
  *) ok "user bearer token is redacted from stream input" ;;
esac
case "$redacted" in
  *hdn_a_QwErTy9876543210_-YY*) err "redaction leaked the raw admin token: $redacted" ;;
  *) ok "admin bearer token is redacted from stream input" ;;
esac
case "$redacted" in
  *hdn_u_'<redacted>'*hdn_a_'<redacted>'*) ok "redaction output contains both placeholders" ;;
  *) err "redaction output missing expected placeholders (got: $redacted)" ;;
esac

redacted_str="$(bash -c 'source "$COMMON_SH_PATH"; hdn_agent_redact_str "$SAMPLE"')"
case "$redacted_str" in
  *hdn_u_AbCdEf0123456789-_ZZ*|*hdn_a_QwErTy9876543210_-YY*)
    err "hdn_agent_redact_str leaked a raw token: $redacted_str" ;;
  *) ok "hdn_agent_redact_str redacts a single argument" ;;
esac

echo "--- common.sh: loopback port allocation ---"
port="$(bash -c 'source "$COMMON_SH_PATH"; hdn_agent_alloc_port')"
case "$port" in
  ''|*[!0-9]*) err "hdn_agent_alloc_port did not print a bare number (got: $port)" ;;
  *)
    if [ "$port" -ge 1 ] && [ "$port" -le 65535 ]; then
      ok "hdn_agent_alloc_port returned a valid port ($port)"
    else
      err "hdn_agent_alloc_port returned an out-of-range port: $port"
    fi
    ;;
esac

echo "--- common.sh: PID tracking + EXIT-trap cleanup ---"
pid_result="$(bash -c '
  source "$COMMON_SH_PATH"
  sleep 30 &
  pid=$!
  hdn_agent_track_pid "$pid"
  hdn_agent_cleanup
  sleep 0.3
  if kill -0 "$pid" 2>/dev/null; then echo STILL_ALIVE; else echo KILLED; fi
')"
if [ "$pid_result" = "KILLED" ]; then
  ok "hdn_agent_cleanup terminates a tracked PID"
else
  err "tracked PID survived hdn_agent_cleanup (got: $pid_result)"
fi

echo "--- common.sh: hdn_agent_on_exit callback registry ---"
on_exit_result="$(bash -c '
  source "$COMMON_SH_PATH"
  marker="$(mktemp -u)"
  hdn_agent_on_exit "touch \"$marker\""
  hdn_agent_cleanup
  if [ -f "$marker" ]; then echo RAN; else echo MISSING; fi
  rm -f "$marker"
')"
if [ "$on_exit_result" = "RAN" ]; then
  ok "hdn_agent_on_exit callback runs during cleanup"
else
  err "hdn_agent_on_exit callback did not run (got: $on_exit_result)"
fi

# ===========================================================================
# PART 2: descriptor schema / permission / leak validation
# ===========================================================================

# A minimal, generic validator driven entirely by fixture.schema.json: no
# jsonschema package dependency, stdlib json + re only.
validator_py="$workdir/validate_descriptor.py"
cat > "$validator_py" <<'PY'
import json
import re
import sys

schema_path, descriptor_path = sys.argv[1], sys.argv[2]

with open(schema_path) as f:
    schema = json.load(f)
with open(descriptor_path) as f:
    raw = f.read()

try:
    descriptor = json.loads(raw)
except ValueError as e:
    print(f"not valid JSON: {e}")
    sys.exit(1)

if not isinstance(descriptor, dict):
    print("descriptor is not a JSON object")
    sys.exit(1)

required = set(schema["required"])
props = schema["properties"]
keys = set(descriptor.keys())

missing = sorted(required - keys)
extra = sorted(keys - set(props.keys()))
if missing:
    print(f"missing required field(s): {missing}")
    sys.exit(1)
if extra:
    print(f"unexpected field(s) not in schema: {extra}")
    sys.exit(1)

for key, value in descriptor.items():
    if not isinstance(value, str):
        print(f"{key}: not a string")
        sys.exit(1)
    pattern = props.get(key, {}).get("pattern")
    if pattern and not re.fullmatch(pattern, value):
        print(f"{key}: value does not match required pattern {pattern!r}: {value!r}")
        sys.exit(1)

print("VALID")
sys.exit(0)
PY

# Combined check: descriptor file mode, schema shape, token-leak scan, and
# (if the referenced bearer file exists on disk) its mode too.
hdn_check_descriptor() {
  local desc="$1" mode schema_out bearer_file bmode

  mode="$(stat -c '%a' "$desc" 2>/dev/null || stat -f '%OLp' "$desc" 2>/dev/null)"
  if [ -z "$mode" ]; then
    echo "cannot stat descriptor: $desc"
    return 1
  fi
  if [ "$mode" != "600" ]; then
    echo "descriptor mode $mode is not 600 (world/group accessible)"
    return 1
  fi

  if grep -Eq 'hdn_[ua]_[A-Za-z0-9_-]{8,}' "$desc"; then
    echo "descriptor text contains what looks like a bearer token"
    return 1
  fi

  if ! schema_out="$(python3 "$validator_py" "$schema" "$desc" 2>&1)"; then
    echo "schema validation failed: $schema_out"
    return 1
  fi

  bearer_file="$(python3 -c '
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    print(d.get("bearer_token_file", ""))
except Exception:
    print("")
' "$desc")"
  if [ -n "$bearer_file" ] && [ -e "$bearer_file" ]; then
    bmode="$(stat -c '%a' "$bearer_file" 2>/dev/null || stat -f '%OLp' "$bearer_file" 2>/dev/null)"
    if [ "$bmode" != "600" ]; then
      echo "bearer_token_file mode $bmode is not 600 (world/group accessible)"
      return 1
    fi
  fi

  echo "VALID"
  return 0
}

assert_accepts() {
  local file="$1" label="$2" reason
  if reason="$(hdn_check_descriptor "$file")"; then
    ok "$label: correctly accepted"
  else
    err "$label: expected acceptance but was rejected ($reason)"
  fi
}

assert_rejects() {
  local file="$1" label="$2" reason
  if reason="$(hdn_check_descriptor "$file")"; then
    err "$label: expected rejection but validated OK"
  else
    ok "$label: correctly rejected ($reason)"
  fi
}

mcp_port="$(hdn_agent_alloc_port)"
bearer_file="$workdir/bearer.token"
ca_file="$workdir/ca.pem"
qmp_sock="$workdir/qmp.sock"
artifact_dir="$workdir/artifacts"
mkdir -p "$artifact_dir"
fingerprint="$(python3 -c 'import secrets; print(secrets.token_hex(32))')"

(umask 077; printf 'hdn_u_%s\n' "$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')" > "$bearer_file")
printf -- '-----BEGIN CERTIFICATE-----\nplaceholder\n-----END CERTIFICATE-----\n' > "$ca_file"

base_descriptor_json() {
  python3 - "$mcp_port" "$bearer_file" "$ca_file" "$fingerprint" "$qmp_sock" "$artifact_dir" <<'PY'
import json
import sys

port, bearer, ca, fp, qmp, artifacts = sys.argv[1:7]
d = {
    "mcp_url": f"https://127.0.0.1:{port}/mcp",
    "bearer_token_file": bearer,
    "ca_certificate_file": ca,
    "tls_fingerprint": fp,
    "vnc_address": "127.0.0.1:5910",
    "novnc_url": "http://127.0.0.1:6080/vnc.html",
    "qmp_socket": qmp,
    "artifact_directory": artifacts,
}
print(json.dumps(d))
PY
}

# mutate.py is written to a real file (not a heredoc) because it must read
# the piped descriptor JSON from its OWN stdin: `python3 - <<HEREDOC` would
# consume stdin for the script source itself, leaving nothing for
# json.load(sys.stdin) to read.
mutate_py="$workdir/mutate.py"
cat > "$mutate_py" <<'PY'
import json
import sys

d = json.load(sys.stdin)
for op in sys.argv[1:]:
    if op.startswith("del:"):
        d.pop(op[4:], None)
    elif op.startswith("set:"):
        key, _, value = op[4:].partition("=")
        d[key] = value
json.dump(d, sys.stdout)
PY

mutate() {
  # mutate <op> [<op> ...] < base_json > mutated_json ; ops are "del:key" or
  # "set:key=value".
  python3 "$mutate_py" "$@"
}

write_desc() {
  # write_desc <out_file> <mode> <op> [<op> ...]
  local out="$1" mode="$2"; shift 2
  base_descriptor_json | mutate "$@" > "$out"
  chmod "$mode" "$out"
}

echo "--- descriptor: happy path ---"
write_desc "$workdir/valid.json" 600
assert_accepts "$workdir/valid.json" "valid descriptor (mode 0600)"

echo "--- descriptor: empty novnc_url is allowed ---"
write_desc "$workdir/empty_novnc.json" 600 'set:novnc_url='
assert_accepts "$workdir/empty_novnc.json" "descriptor with empty novnc_url"

echo "--- descriptor: missing required field ---"
write_desc "$workdir/missing_field.json" 600 'del:qmp_socket'
assert_rejects "$workdir/missing_field.json" "descriptor missing qmp_socket"

echo "--- descriptor: extra/unknown field ---"
write_desc "$workdir/extra_field.json" 600 'set:unexpected_field=nope'
assert_rejects "$workdir/extra_field.json" "descriptor with an extra field"

echo "--- descriptor: relative path ---"
write_desc "$workdir/relative_path.json" 600 'set:bearer_token_file=relative/bearer.token'
assert_rejects "$workdir/relative_path.json" "descriptor with a relative bearer_token_file path"

echo "--- descriptor: HTTP (non-HTTPS) mcp_url ---"
write_desc "$workdir/http_url.json" 600 "set:mcp_url=http://127.0.0.1:${mcp_port}/mcp"
assert_rejects "$workdir/http_url.json" "descriptor with an http:// mcp_url"

echo "--- descriptor: mcp_url not ending in /mcp ---"
write_desc "$workdir/bad_suffix.json" 600 "set:mcp_url=https://127.0.0.1:${mcp_port}/other"
assert_rejects "$workdir/bad_suffix.json" "descriptor whose mcp_url does not end in /mcp"

echo "--- descriptor: malformed tls_fingerprint ---"
write_desc "$workdir/bad_fingerprint.json" 600 'set:tls_fingerprint=DEADBEEF'
assert_rejects "$workdir/bad_fingerprint.json" "descriptor with a malformed tls_fingerprint"

echo "--- descriptor: world-readable descriptor file ---"
write_desc "$workdir/world_readable_desc.json" 644
assert_rejects "$workdir/world_readable_desc.json" "world-readable descriptor (mode 0644)"

echo "--- descriptor: world-readable bearer file ---"
world_bearer="$workdir/world_bearer.token"
(umask 077; printf 'hdn_u_%s\n' "$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')" > "$world_bearer")
chmod 644 "$world_bearer"
python3 - "$mcp_port" "$world_bearer" "$ca_file" "$fingerprint" "$qmp_sock" "$artifact_dir" <<'PY' > "$workdir/world_readable_bearer.json"
import json
import sys

port, bearer, ca, fp, qmp, artifacts = sys.argv[1:7]
print(json.dumps({
    "mcp_url": f"https://127.0.0.1:{port}/mcp",
    "bearer_token_file": bearer,
    "ca_certificate_file": ca,
    "tls_fingerprint": fp,
    "vnc_address": "127.0.0.1:5910",
    "novnc_url": "",
    "qmp_socket": qmp,
    "artifact_directory": artifacts,
}))
PY
chmod 600 "$workdir/world_readable_bearer.json"
assert_rejects "$workdir/world_readable_bearer.json" "descriptor referencing a world-readable bearer file"

echo "--- descriptor: text contains a bearer token ---"
leaked_token="hdn_u_$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')"
write_desc "$workdir/token_leak.json" 600 "set:novnc_url=http://127.0.0.1:6080/?t=${leaked_token}"
assert_rejects "$workdir/token_leak.json" "descriptor whose text contains a bearer token"

echo
if [ "$fail" -eq 0 ]; then
  echo "PASS: all fixture descriptor checks"
else
  echo "FAILED: one or more fixture descriptor checks failed" >&2
fi
exit "$fail"
