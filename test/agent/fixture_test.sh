#!/usr/bin/env bash
# Tests for the detachable VM fixture plumbing (Phase 4, Task 3):
#
#   test/agent/lib/qmp.sh       -- QMP/HMP control helpers (JSON protocol)
#   test/agent/lib/fixture.sh   -- descriptor emission + small probes
#   test/agent/run.sh           -- `fixture` / `stop` arg wiring
#   tools/vm.sh                 -- optional detachable-fixture knobs
#
# The parts that require a BOOTED guest (a multi-minute real appliance VM with
# KVM) are deliberately NOT exercised here; the controller runs that live-boot
# integration separately. What this suite proves without a guest:
#
#   PART 0: bash -n syntax of every script we touch, plus this test.
#   PART 1: the QMP helpers speak the real JSON protocol against a FAKE QMP
#           Unix-socket server -- greeting + qmp_capabilities handshake,
#           query-status parsed as JSON (not grepped) even with an async event
#           interleaved, screendump writing a file, system_reset/powerdown,
#           HMP sendkey, and a genuine error reply surfacing as a nonzero exit.
#   PART 2: hdn_fixture_emit_descriptor produces a descriptor that validates
#           against fixture.schema.json, is mode 0600, uses absolute paths,
#           carries no bearer-token text, and refuses token-shaped input.
#   PART 3: tools/vm.sh default preservation -- with none of the new knobs set
#           the QEMU argv is byte-for-byte identical to git HEAD (Global
#           Constraint), and with them set the expected args are injected.
#           (Gated on QEMU/OVMF availability; skipped cleanly otherwise.)
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
qmp_sh="$repo_root/test/agent/lib/qmp.sh"
fixture_sh="$repo_root/test/agent/lib/fixture.sh"
common_sh="$repo_root/test/agent/lib/common.sh"
schema="$repo_root/test/agent/fixture.schema.json"
run_sh="$repo_root/test/agent/run.sh"
vm_sh="$repo_root/tools/vm.sh"

fail=0
err() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }
have() { command -v "$1" >/dev/null 2>&1; }

assert_mode() {
  local path="$1" want="$2" label="$3" got
  got="$(stat -c '%a' "$path" 2>/dev/null || stat -f '%OLp' "$path" 2>/dev/null)"
  if [ "$got" = "$want" ]; then ok "$label: mode $got"; else err "$label: expected $want, got $got"; fi
}

# ---------------------------------------------------------------------------
# PART 0: syntax
# ---------------------------------------------------------------------------
echo "--- bash -n ---"
for f in "$qmp_sh" "$fixture_sh" "$run_sh" "$vm_sh" "${BASH_SOURCE[0]}"; do
  if bash -n "$f" 2>/dev/null; then ok "bash -n ${f#"$repo_root"/}"; else err "bash -n failed: ${f#"$repo_root"/}"; fi
done

if ! have python3; then
  echo "FATAL: python3 is required for this suite" >&2
  exit 1
fi

workdir="$(mktemp -d)"
export COMMON_SH_PATH="$common_sh"
# shellcheck source=lib/common.sh
source "$common_sh"
# shellcheck source=lib/qmp.sh
source "$qmp_sh"
# shellcheck source=lib/fixture.sh
source "$fixture_sh"
hdn_agent_on_exit "rm -rf '$workdir'"

# ===========================================================================
# PART 1: QMP helpers against a fake QMP server
# ===========================================================================
# The fake server speaks the real newline-delimited QMP JSON protocol so the
# helpers must actually parse JSON (and skip an interleaved async event) rather
# than grep raw bytes. mode=ok answers every command with a proper return;
# mode=err answers post-capabilities commands with an {"error": ...} object.
fake_qmp_py="$workdir/fake_qmp.py"
cat > "$fake_qmp_py" <<'PY'
import json
import os
import socket
import sys
import threading

sock_path, mode = sys.argv[1], sys.argv[2]

if os.path.exists(sock_path):
    os.unlink(sock_path)

srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
srv.bind(sock_path)
srv.listen(8)

# Signal readiness by creating a marker the parent waits on.
open(sock_path + ".ready", "w").close()


def handle(conn):
    conn.sendall(
        (json.dumps({"QMP": {"version": {"qemu": {"major": 8}}, "capabilities": []}}) + "\r\n").encode()
    )
    buf = b""
    while True:
        while b"\n" not in buf:
            chunk = conn.recv(4096)
            if not chunk:
                conn.close()
                return
            buf += chunk
        line, buf = buf.split(b"\n", 1)
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line.decode())
        except ValueError:
            continue
        exe = msg.get("execute")
        if exe == "qmp_capabilities":
            conn.sendall((json.dumps({"return": {}}) + "\r\n").encode())
            continue
        if mode == "err":
            conn.sendall(
                (json.dumps({"error": {"class": "GenericError", "desc": "injected failure"}}) + "\r\n").encode()
            )
            continue
        if exe == "query-status":
            # Interleave an async event BEFORE the reply: a byte-grepper would
            # trip on it; a JSON parser skips it.
            conn.sendall((json.dumps({"event": "RESUME", "timestamp": {"seconds": 1, "microseconds": 0}}) + "\r\n").encode())
            conn.sendall(
                (json.dumps({"return": {"status": "running", "running": True, "singlestep": False}}) + "\r\n").encode()
            )
        elif exe == "screendump":
            fn = msg.get("arguments", {}).get("filename")
            if fn:
                with open(fn, "wb") as f:
                    f.write(b"P6\n1 1\n255\n\x00\x00\x00")
            conn.sendall((json.dumps({"return": {}}) + "\r\n").encode())
        elif exe in ("system_reset", "system_powerdown"):
            conn.sendall((json.dumps({"return": {}}) + "\r\n").encode())
        elif exe == "human-monitor-command":
            conn.sendall((json.dumps({"return": ""}) + "\r\n").encode())
        else:
            conn.sendall(
                (json.dumps({"error": {"class": "CommandNotFound", "desc": "no %s" % exe}}) + "\r\n").encode()
            )


while True:
    try:
        conn, _ = srv.accept()
    except OSError:
        break
    threading.Thread(target=handle, args=(conn,), daemon=True).start()
PY

start_fake_qmp() {
  # start_fake_qmp <sock> <mode> -- launch the server, wait for readiness,
  # track its PID for cleanup.
  local sock="$1" mode="$2"
  python3 "$fake_qmp_py" "$sock" "$mode" &
  local pid=$!
  hdn_agent_track_pid "$pid"
  local tries=0
  while [ ! -e "${sock}.ready" ] && [ "$tries" -lt 50 ]; do sleep 0.1; tries=$((tries + 1)); done
  printf '%s' "$pid"
}

echo "--- QMP helpers: happy path (fake server) ---"
ok_sock="$workdir/qmp-ok.sock"
start_fake_qmp "$ok_sock" ok >/dev/null

if qmp_capabilities "$ok_sock"; then
  ok "qmp_capabilities completes the greeting + negotiation handshake"
else
  err "qmp_capabilities failed against the fake server"
fi

status="$(qmp_query_status "$ok_sock")"
if [ "$status" = "running" ]; then
  ok "qmp_query_status parses the JSON 'status' field as 'running' (async event skipped)"
else
  err "qmp_query_status returned '$status' (expected 'running')"
fi

shot="$workdir/shot.ppm"
if qmp_screendump "$ok_sock" "$shot" && [ -s "$shot" ]; then
  ok "qmp_screendump drives the command and the server wrote the PPM"
else
  err "qmp_screendump did not produce $shot"
fi

if qmp_system_reset "$ok_sock"; then ok "qmp_system_reset succeeds"; else err "qmp_system_reset failed"; fi
if qmp_system_powerdown "$ok_sock"; then ok "qmp_system_powerdown succeeds"; else err "qmp_system_powerdown failed"; fi
if hmp_sendkey "$ok_sock" "meta_l-shift-esc"; then
  ok "hmp_sendkey meta_l-shift-esc succeeds (empty HMP return)"
else
  err "hmp_sendkey failed"
fi

echo "--- QMP helpers: error reply surfaces as nonzero exit ---"
err_sock="$workdir/qmp-err.sock"
start_fake_qmp "$err_sock" err >/dev/null
if qmp_query_status "$err_sock" >/dev/null 2>&1; then
  err "qmp_query_status should have failed on an injected QMP error"
else
  ok "qmp_query_status exits nonzero when the server returns a QMP error"
fi

echo "--- QMP helpers: missing socket path is a usage error ---"
if hdn_qmp "" query-status >/dev/null 2>&1; then
  err "hdn_qmp with an empty socket path should fail"
else
  ok "hdn_qmp rejects an empty socket path"
fi

# ===========================================================================
# PART 2: descriptor emission (hdn_fixture_emit_descriptor)
# ===========================================================================
echo "--- descriptor emission: schema-valid, 0600, absolute, no token ---"

# Reuse Task 1's validation approach: a stdlib-only validator driven directly
# by fixture.schema.json (required / additionalProperties / pattern), plus the
# real jsonschema package when it happens to be installed.
if python3 -c 'import jsonschema' 2>/dev/null; then have_jsonschema=1; else have_jsonschema=0; fi

validator_py="$workdir/validate.py"
cat > "$validator_py" <<'PY'
import json
import re
import sys

schema = json.load(open(sys.argv[1]))
d = json.load(open(sys.argv[2]))
if not isinstance(d, dict):
    print("not an object"); sys.exit(1)
required = set(schema["required"])
props = schema["properties"]
keys = set(d.keys())
missing = sorted(required - keys)
extra = sorted(keys - set(props.keys()))
if missing:
    print("missing: %s" % missing); sys.exit(1)
if extra:
    print("extra: %s" % extra); sys.exit(1)
for k, v in d.items():
    if not isinstance(v, str):
        print("%s not a string" % k); sys.exit(1)
    pat = props.get(k, {}).get("pattern")
    if pat and not re.fullmatch(pat, v):
        print("%s does not match %r: %r" % (k, pat, v)); sys.exit(1)
print("VALID")
PY

validator_js_py="$workdir/validate_js.py"
cat > "$validator_js_py" <<'PY'
import json
import sys

import jsonschema

schema = json.load(open(sys.argv[1]))
d = json.load(open(sys.argv[2]))
errs = sorted(jsonschema.Draft7Validator(schema).iter_errors(d), key=str)
if errs:
    print("; ".join(e.message for e in errs)); sys.exit(1)
print("VALID")
PY

# Realistic fake values (no booted guest needed).
bearer_file="$workdir/user.token"
ca_file="$workdir/ca.pem"
qmp_sock_val="$workdir/qmp.sock"
art_dir="$workdir/art"
mkdir -p "$art_dir"
(umask 077; printf 'hdn_u_%s' "$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')" > "$bearer_file")
printf -- '-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n' > "$ca_file"
fingerprint="$(python3 -c 'import secrets; print(secrets.token_hex(32))')"

desc="$workdir/fixture.json"
if hdn_fixture_emit_descriptor "$desc" \
    "https://127.0.0.1:17443/mcp" \
    "$bearer_file" \
    "$ca_file" \
    "$fingerprint" \
    "127.0.0.1:5925" \
    "http://127.0.0.1:6090/vnc.html?autoconnect=1" \
    "$qmp_sock_val" \
    "$art_dir"; then
  ok "hdn_fixture_emit_descriptor wrote $desc"
else
  err "hdn_fixture_emit_descriptor failed"
fi

if out="$(python3 "$validator_py" "$schema" "$desc" 2>&1)" && [ "$out" = "VALID" ]; then
  ok "descriptor validates against fixture.schema.json (stdlib validator)"
else
  err "descriptor failed schema validation: $out"
fi
if [ "$have_jsonschema" -eq 1 ]; then
  if out="$(python3 "$validator_js_py" "$schema" "$desc" 2>&1)" && [ "$out" = "VALID" ]; then
    ok "descriptor validates against fixture.schema.json (jsonschema package)"
  else
    err "descriptor failed jsonschema validation: $out"
  fi
fi

assert_mode "$desc" 600 "descriptor"

# absolute paths for the path-typed fields
for field in bearer_token_file ca_certificate_file qmp_socket artifact_directory; do
  val="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$desc" "$field")"
  case "$val" in
    /*) ok "$field is an absolute path" ;;
    *)  err "$field is not absolute: $val" ;;
  esac
done

# no bearer-shaped token text anywhere in the descriptor
if grep -Eq 'hdn_[ua]_[A-Za-z0-9_-]{8,}' "$desc"; then
  err "descriptor text contains a bearer-shaped token"
else
  ok "descriptor contains no bearer-shaped token text"
fi

# mcp_url shape
mcp_url="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["mcp_url"])' "$desc")"
case "$mcp_url" in
  https://*/mcp) ok "mcp_url is https and ends in /mcp" ;;
  *) err "mcp_url has the wrong shape: $mcp_url" ;;
esac

echo "--- descriptor emission: empty novnc_url is accepted ---"
desc2="$workdir/fixture-nonovnc.json"
if hdn_fixture_emit_descriptor "$desc2" \
    "https://127.0.0.1:17443/mcp" "$bearer_file" "$ca_file" "$fingerprint" \
    "127.0.0.1:5925" "" "$qmp_sock_val" "$art_dir"; then
  if out="$(python3 "$validator_py" "$schema" "$desc2" 2>&1)" && [ "$out" = "VALID" ]; then
    ok "descriptor with empty novnc_url validates"
  else
    err "empty-novnc descriptor failed validation: $out"
  fi
else
  err "emit failed for empty novnc_url"
fi

echo "--- descriptor emission: refuses token-shaped input (atomic, no clobber) ---"
desc3="$workdir/fixture-leak.json"
leak_token="hdn_u_$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')"
if hdn_fixture_emit_descriptor "$desc3" \
    "https://127.0.0.1:17443/mcp" "$bearer_file" "$ca_file" "$fingerprint" \
    "127.0.0.1:5925" "http://127.0.0.1:6090/?t=${leak_token}" "$qmp_sock_val" "$art_dir" \
    >/dev/null 2>&1; then
  err "emit should have refused a novnc_url carrying a bearer token"
else
  ok "emit refuses to write a descriptor carrying a bearer-shaped token"
fi
if [ ! -e "$desc3" ] && ! ls "$workdir"/fixture-leak.json.tmp.* >/dev/null 2>&1; then
  ok "refused emit left neither a descriptor nor a temp file behind"
else
  err "refused emit left a partial file behind"
fi

# ===========================================================================
# PART 3: fixture probes + run.sh wiring + tools/vm.sh default preservation
# ===========================================================================
echo "--- fixture probes ---"
probe_port="$(hdn_agent_alloc_port)"
if hdn_fixture_tcp_open 127.0.0.1 "$probe_port"; then
  err "hdn_fixture_tcp_open reported an unused port as open"
else
  ok "hdn_fixture_tcp_open reports a closed port as closed"
fi
# open a listener and confirm it detects it
python3 - "$probe_port" <<'PY' &
import socket, sys, time
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", int(sys.argv[1]))); s.listen(1); time.sleep(3)
PY
listener_pid=$!
hdn_agent_track_pid "$listener_pid"
sleep 0.4
if hdn_fixture_tcp_open 127.0.0.1 "$probe_port"; then
  ok "hdn_fixture_tcp_open detects an open port"
else
  err "hdn_fixture_tcp_open failed to detect an open listener"
fi
kill "$listener_pid" 2>/dev/null || true

echo "--- run.sh subcommand dispatch ---"
# Capture into a variable first: run.sh exits nonzero on a usage error and
# `set -o pipefail` would otherwise propagate that through `... | grep`.
noarg_out="$("$run_sh" 2>&1 || true)"
case "$noarg_out" in *"usage:"*) ok "run.sh with no subcommand prints usage" ;; *) err "run.sh no-arg did not print usage" ;; esac
case "$noarg_out" in *fixture*) ok "run.sh usage advertises the fixture subcommand" ;; *) err "run.sh usage omits fixture" ;; esac
bogus_out="$("$run_sh" bogus 2>&1 || true)"
case "$bogus_out" in *"unknown subcommand"*) ok "run.sh rejects an unknown subcommand" ;; *) err "run.sh did not reject a bogus subcommand" ;; esac
# `fixture` with a bogus ISO must fail fast (no ISO found), not hang.
if FIXTURE_ISO="$workdir/nope.iso" timeout 30 "$run_sh" fixture >/dev/null 2>&1; then
  err "run.sh fixture with a missing ISO should fail"
else
  ok "run.sh fixture fails fast when the ISO is missing"
fi
# `stop` with no running fixture must exit cleanly (nothing to stop) and keep quiet on stdout.
stop_out="$(FIXTURE_RUNTIME="$workdir/empty-runtime" FIXTURE_ART="$workdir/empty-art" timeout 30 "$run_sh" stop 2>/dev/null)"
if [ -z "$stop_out" ]; then ok "run.sh stop prints nothing to stdout when there is nothing to stop"; else err "run.sh stop printed to stdout: $stop_out"; fi

echo "--- tools/vm.sh default preservation (Global Constraint) ---"
if have qemu-system-x86_64 && { [ -f /usr/share/OVMF/OVMF_CODE_4M.fd ] || [ -f /usr/share/OVMF/OVMF_CODE.fd ] || [ -f /usr/share/edk2-ovmf/x64/OVMF_CODE.fd ]; }; then
  fakebin="$workdir/fakebin"; mkdir -p "$fakebin"
  cat > "$fakebin/qemu-system-x86_64" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@"
EOF
  chmod +x "$fakebin/qemu-system-x86_64"
  run_vm() {
    local script="$1"; shift
    local root; root="$(mktemp -d)"
    mkdir -p "$root/tools" "$root/build/vm"
    cp "$script" "$root/tools/vm.sh"; chmod +x "$root/tools/vm.sh"
    : > "$root/build/vm/sway-disk.qcow2"
    env -i PATH="$fakebin:/usr/bin:/bin" HOME="$HOME" "$@" "$root/tools/vm.sh" run 2>/dev/null || true
    rm -rf "$root"
  }
  head_args="$workdir/vm-head.args"; work_args="$workdir/vm-work.args"
  git -C "$repo_root" show HEAD:tools/vm.sh > "$workdir/vm.head.sh" 2>/dev/null
  run_vm "$workdir/vm.head.sh" > "$head_args"
  run_vm "$vm_sh" > "$work_args"
  if [ -s "$head_args" ] && diff -q "$head_args" "$work_args" >/dev/null 2>&1; then
    ok "tools/vm.sh QEMU argv is byte-for-byte unchanged when no new knob is set"
  else
    err "tools/vm.sh default QEMU argv changed (Global Constraint violated)"
    diff "$head_args" "$work_args" >&2 || true
  fi
  # knobs inject the expected args
  : > "$workdir/seed.iso"
  knob_args="$(run_vm "$vm_sh" HOST_MCP_PORT=17443 QMP="$workdir/q.sock" SERIAL_LOG="$workdir/s.log" PID_FILE="$workdir/q.pid" SEED_ISO="$workdir/seed.iso")"
  echo "$knob_args" | grep -q "hostfwd=tcp:127.0.0.1:17443-:7443" && ok "HOST_MCP_PORT injects the loopback hostfwd" || err "HOST_MCP_PORT hostfwd missing"
  echo "$knob_args" | grep -q "unix:$workdir/q.sock,server,nowait" && ok "QMP injects the control socket" || err "QMP socket arg missing"
  echo "$knob_args" | grep -q "file:$workdir/s.log" && ok "SERIAL_LOG injects the serial file" || err "SERIAL_LOG arg missing"
  echo "$knob_args" | grep -q -- "-pidfile" && ok "PID_FILE injects -pidfile" || err "PID_FILE arg missing"
  echo "$knob_args" | grep -q "file=$workdir/seed.iso" && ok "SEED_ISO injects the read-only seed CD" || err "SEED_ISO arg missing"
else
  ok "SKIP tools/vm.sh argv check (qemu-system-x86_64/OVMF not available here)"
fi

echo
if [ "$fail" -eq 0 ]; then
  echo "PASS: all fixture / QMP checks"
else
  echo "FAILED: one or more fixture / QMP checks failed" >&2
fi
exit "$fail"
