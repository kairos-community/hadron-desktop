#!/usr/bin/env bash
# Tests for hadron-agent-first-run (Phase 3, Task 7).
#
#   1. `sh -n` syntax-checks the script.
#   2. Missing token -> the panel does NOT open (exits cleanly, `st` never
#      invoked, no output).
#   3. Token present + status ready -> the panel shows the MCP URL, the
#      certificate fingerprint, the token, and "shown once; store it now",
#      then unlinks the token file after Enter.
#   4. Status TIMEOUT (never becomes ready) -> the token file is RETAINED
#      (not unlinked), so the panel is retried on the next i3 start.
#   5. The captured token value never appears in stderr (or anywhere outside
#      the deliberately-displayed stdout of case 3), and this script never
#      shells out to `logger`/journald.
#
# `st` and `hadron-agent` are faked via a PATH-shadowing bin dir; Enter is fed
# via stdin. The fake `st` strips flags up to `-e` and execs the remaining
# command directly (mirrors how a real terminal emulator attaches the child's
# stdio to its own pty -- the parent script's own stdin is never read except
# by this exec'd child, so feeding Enter on the invoking process's stdin
# reaches it, exactly as it would reach a real st window's foreground reader).
set -u

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
bin="$repo_root/rootfs-agent/usr/bin"
script="$bin/hadron-agent-first-run"

fail=0
err() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

# ---------------------------------------------------------------------------
# 1. syntax
# ---------------------------------------------------------------------------
if sh -n "$script" 2>/dev/null; then
  ok "sh -n ${script#$repo_root/}"
else
  err "sh -n failed: ${script#$repo_root/}"
fi

# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

fakebin="$workdir/bin"
mkdir -p "$fakebin"

st_calls="$workdir/st.calls"

# Fake st: records that it was invoked, then strips flags up to `-e` and execs
# the remaining command directly (no real X window/pty needed for the test).
cat >"$fakebin/st" <<EOF
#!/bin/sh
echo "invoked" >>"$st_calls"
while [ \$# -gt 0 ]; do
  case "\$1" in
    -e) shift; break ;;
    *) shift ;;
  esac
done
exec "\$@"
EOF
chmod +x "$fakebin/st"

# Fake hadron-agent: prints a not-ready status line by default (used by the
# timeout case's CLI-fallback path, and to assert it stays not-ready).
cat >"$fakebin/hadron-agent" <<'EOF'
#!/bin/sh
echo "version: 0.0.0-test"
echo "ready:   false"
echo "paused:  false"
EOF
chmod +x "$fakebin/hadron-agent"

token_secret="hdn_u_TOPSECRET1234567890"

# run_first_run: invokes the script under a controlled environment. Args are
# extra VAR=val pairs. Feeds a single Enter on stdin. Captures stdout/stderr
# into $out/$err globals and the exit code into $code.
run_first_run() {
  : >"$st_calls"
  local stdout_file="$workdir/stdout.$$"
  local stderr_file="$workdir/stderr.$$"
  printf '\n' | env -i PATH="$fakebin:/usr/bin:/bin" HOME="$workdir" TMPDIR="$workdir" \
    "$@" sh "$script" >"$stdout_file" 2>"$stderr_file"
  code=$?
  out="$(cat "$stdout_file")"
  err_out="$(cat "$stderr_file")"
  rm -f "$stdout_file" "$stderr_file"
}

# ---------------------------------------------------------------------------
# 2. missing token -> no panel, clean exit
# ---------------------------------------------------------------------------
missing_token="$workdir/no-such-token"
rm -f "$missing_token"
run_first_run HADRON_AGENT_FIRST_RUN_TOKEN="$missing_token"
if [ "$code" -ne 0 ]; then
  err "missing token: expected exit 0, got $code"
else
  ok "missing token: exits 0"
fi
if [ -n "$out" ]; then
  err "missing token: expected no stdout, got: $out"
else
  ok "missing token: no stdout"
fi
if [ -s "$st_calls" ]; then
  err "missing token: st was invoked (panel opened) but should not have been"
else
  ok "missing token: st was never invoked (panel did not open)"
fi

# ---------------------------------------------------------------------------
# 3. token present + status ready -> shows everything, unlinks on Enter
# ---------------------------------------------------------------------------
status_dir="$workdir/status"
mkdir -p "$status_dir"
status_json="$status_dir/status.json"
cat >"$status_json" <<'EOF'
{
  "version": "0.0.0-test",
  "gateway": true,
  "session": true,
  "cua": true,
  "paused": false,
  "degraded": false,
  "computer_use_active": false,
  "ready": true,
  "cert_fingerprint": "sha256:deadbeefcafe0000000000000000000000000000000000000000000000",
  "mdns": true,
  "listen": "0.0.0.0:7443"
}
EOF

token_file="$workdir/first-run-token"
printf '%s' "$token_secret" >"$token_file"

run_first_run \
  HADRON_AGENT_FIRST_RUN_TOKEN="$token_file" \
  HADRON_AGENT_STATUS_JSON="$status_json"

if [ "$code" -ne 0 ]; then
  err "ready case: expected exit 0, got $code"
else
  ok "ready case: exits 0"
fi

if [ -s "$st_calls" ]; then
  ok "ready case: st was invoked (panel opened)"
else
  err "ready case: st was never invoked"
fi

case "$out" in
  *"https://"*"/mcp"*) ok "ready case: shows an MCP URL" ;;
  *) err "ready case: no MCP URL in output: $out" ;;
esac
case "$out" in
  *"sha256:deadbeefcafe"*) ok "ready case: shows the certificate fingerprint" ;;
  *) err "ready case: no fingerprint in output: $out" ;;
esac
case "$out" in
  *"$token_secret"*) ok "ready case: shows the ordinary token" ;;
  *) err "ready case: token not in output: $out" ;;
esac
case "$out" in
  *"shown once; store it now"*) ok "ready case: shows 'shown once; store it now'" ;;
  *) err "ready case: missing 'shown once; store it now': $out" ;;
esac

if [ -e "$token_file" ]; then
  err "ready case: token file was NOT unlinked after Enter"
else
  ok "ready case: token file unlinked after Enter"
fi

case "$err_out" in
  *"$token_secret"*) err "ready case: token value leaked into stderr!" ;;
  *) ok "ready case: token value does not appear in stderr" ;;
esac

# ---------------------------------------------------------------------------
# 4. status timeout -> token retained, no panel
# ---------------------------------------------------------------------------
timeout_token="$workdir/timeout-token"
printf '%s' "$token_secret" >"$timeout_token"
missing_status="$workdir/status/nope.json"

run_first_run \
  HADRON_AGENT_FIRST_RUN_TOKEN="$timeout_token" \
  HADRON_AGENT_STATUS_JSON="$missing_status" \
  HADRON_AGENT_FIRST_RUN_TIMEOUT=2 \
  HADRON_AGENT_FIRST_RUN_POLL=0

if [ "$code" -ne 0 ]; then
  err "timeout case: expected exit 0, got $code"
else
  ok "timeout case: exits 0"
fi

if [ -e "$timeout_token" ]; then
  ok "timeout case: token file RETAINED (not unlinked)"
else
  err "timeout case: token file was unlinked despite a status timeout"
fi

if [ -n "$out" ]; then
  err "timeout case: expected no stdout (no panel), got: $out"
else
  ok "timeout case: no panel output"
fi

case "$err_out" in
  *"$token_secret"*) err "timeout case: token value leaked into stderr!" ;;
  *) ok "timeout case: token value does not appear in stderr" ;;
esac

# The script must never shell out to logger/journalctl (comment lines
# mentioning the words as documentation, e.g. "NEVER calls logger", are fine
# -- only an actual invocation is checked).
if grep -Ev '^[[:space:]]*#' "$script" |
   grep -Eq '(^|[^a-zA-Z0-9_-])(logger|journalctl|systemd-cat)([^a-zA-Z0-9_-]|$)'; then
  err "script invokes logger/journalctl/systemd-cat"
else
  ok "script never invokes logger/journalctl/systemd-cat"
fi

# ---------------------------------------------------------------------------
if [ "$fail" -eq 0 ]; then
  echo "PASS: first-run tests"
else
  echo "FAILED: first-run tests" >&2
fi
exit "$fail"
