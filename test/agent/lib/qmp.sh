#!/usr/bin/env bash
umask 077
# test/agent/lib/qmp.sh -- QMP/HMP control helpers for the Cua agent VM
# fixture (Phase 4, Task 3).
#
# Every helper talks to QEMU's QMP control socket (a Unix domain socket the
# fixture creates with `-qmp unix:<path>,server,nowait`) by speaking the real
# JSON protocol: it reads the server greeting, performs the mandatory
# `qmp_capabilities` negotiation, sends one command, and then reads back the
# JSON reply -- skipping the asynchronous QMP *events* that interleave with
# command replies -- and PARSES it as JSON with python3's stdlib json module.
#
# It never greps partial bytes off the socket: a QMP reply and a QMP event can
# arrive in either order and a `{"return": ...}` value can itself contain the
# word "error", so byte-grepping the stream is unsafe. json.loads on each
# newline-delimited message is the only correct way to tell a command reply
# from an event and to tell success from failure.
#
# All helpers take the socket path as $1. They exit nonzero (and print the QMP
# error object to stderr) on any protocol failure or error reply. Only
# python3 is required -- no socat, no qmp-shell, no extra build deps.
#
# Source order: source lib/common.sh FIRST (umask 077, the single EXIT trap,
# redaction) and then this file. Re-sourcing is a safe no-op.
set +x 2>/dev/null || true

if [ -n "${HDN_AGENT_QMP_SH_SOURCED:-}" ]; then
  return 0 2>/dev/null || exit 0
fi
HDN_AGENT_QMP_SH_SOURCED=1

# hdn_qmp <sock> <op> [arg] -- connect to the QMP Unix socket <sock>, negotiate
# capabilities, run one operation, and parse the JSON reply.
#
# Operations:
#   capabilities         -- greeting + qmp_capabilities handshake only.
#   query-status         -- print the guest run state string (e.g. "running").
#   screendump <file>    -- write a PPM screenshot of the scanout to <file>.
#   system_reset         -- hard-reset the guest.
#   system_powerdown     -- request an ACPI graceful shutdown.
#   sendkey <keys>       -- HMP `sendkey <keys>` via human-monitor-command,
#                           e.g. sendkey meta_l-shift-esc.
#
# The command JSON is always built inside python (json.dumps), so a filename or
# key sequence with shell-special characters can never corrupt the wire format.
hdn_qmp() {
  local sock="$1" op="$2" arg="${3:-}"
  [ -n "$sock" ] || { echo "hdn_qmp: socket path required" >&2; return 2; }
  python3 - "$sock" "$op" "$arg" <<'PY'
import json
import socket
import sys
import time

sock_path, op, arg = sys.argv[1], sys.argv[2], sys.argv[3]

s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(15)
for _ in range(40):
    try:
        s.connect(sock_path)
        break
    except OSError:
        time.sleep(0.25)
else:
    print("qmp: cannot connect to socket %s" % sock_path, file=sys.stderr)
    sys.exit(1)

buf = b""


def recv_json():
    global buf
    while b"\n" not in buf:
        chunk = s.recv(4096)
        if not chunk:
            raise EOFError("qmp connection closed")
        buf += chunk
    line, buf = buf.split(b"\n", 1)
    return json.loads(line.decode())


def send(obj):
    s.sendall((json.dumps(obj) + "\r\n").encode())


def wait_reply():
    # QMP interleaves asynchronous events with command replies; skip events
    # until a message carrying a "return" or "error" arrives.
    while True:
        msg = recv_json()
        if "return" in msg or "error" in msg:
            return msg


# 1. server greeting: {"QMP": {...}}
recv_json()

# 2. mandatory capabilities negotiation.
send({"execute": "qmp_capabilities"})
reply = wait_reply()
if "error" in reply:
    print("qmp_capabilities error: %s" % json.dumps(reply["error"]), file=sys.stderr)
    sys.exit(1)

# 3. the requested operation.
if op == "capabilities":
    cmd = None
elif op == "query-status":
    cmd = {"execute": "query-status"}
elif op == "screendump":
    if not arg:
        print("qmp screendump: filename required", file=sys.stderr)
        sys.exit(2)
    cmd = {"execute": "screendump", "arguments": {"filename": arg}}
elif op == "system_reset":
    cmd = {"execute": "system_reset"}
elif op == "system_powerdown":
    cmd = {"execute": "system_powerdown"}
elif op == "sendkey":
    keys = arg or "meta_l-shift-esc"
    cmd = {
        "execute": "human-monitor-command",
        "arguments": {"command-line": "sendkey %s" % keys},
    }
else:
    print("qmp: unknown operation %r" % op, file=sys.stderr)
    sys.exit(2)

if cmd is None:
    s.close()
    sys.exit(0)

send(cmd)
reply = wait_reply()
if "error" in reply:
    print("qmp %s error: %s" % (op, json.dumps(reply["error"])), file=sys.stderr)
    sys.exit(1)

ret = reply["return"]

if op == "query-status":
    # ret is like {"status": "running", "running": true, "singlestep": false}
    if isinstance(ret, dict) and "status" in ret:
        print(ret["status"])
    else:
        print("qmp query-status: unexpected return %r" % (ret,), file=sys.stderr)
        sys.exit(1)
elif op == "sendkey":
    # human-monitor-command returns a (usually empty) string.
    if isinstance(ret, str) and ret.strip():
        # A non-empty HMP payload here is the monitor complaining about the
        # command line; surface it as a failure rather than swallowing it.
        print("qmp sendkey: %s" % ret.strip(), file=sys.stderr)
        sys.exit(1)

s.close()
sys.exit(0)
PY
}

# Named wrappers matching the brief's helper list. Each takes the socket path.
qmp_capabilities()     { hdn_qmp "$1" capabilities; }
qmp_query_status()     { hdn_qmp "$1" query-status; }
qmp_screendump()       { hdn_qmp "$1" screendump "$2"; }
qmp_system_reset()     { hdn_qmp "$1" system_reset; }
qmp_system_powerdown() { hdn_qmp "$1" system_powerdown; }

# hmp_sendkey <sock> [keys] -- default key sequence is the appliance's
# "break glass" chord meta_l-shift-esc.
hmp_sendkey()          { hdn_qmp "$1" sendkey "${2:-meta_l-shift-esc}"; }
