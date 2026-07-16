#!/usr/bin/env bash
umask 077
# test/agent/lib/fixture.sh -- helpers shared by the `fixture` and `stop`
# subcommands of test/agent/run.sh (Phase 4, Task 3), factored out so the
# descriptor-emission logic can be unit-tested WITHOUT booting a guest.
#
# Source lib/common.sh FIRST (umask 077, single EXIT trap, redaction), then
# this file. Re-sourcing is a safe no-op.
set +x 2>/dev/null || true

if [ -n "${HDN_AGENT_FIXTURE_SH_SOURCED:-}" ]; then
  return 0 2>/dev/null || exit 0
fi
HDN_AGENT_FIXTURE_SH_SOURCED=1

# hdn_fixture_tcp_open <host> <port> -- exit 0 iff a TCP connect to
# host:port succeeds within a short timeout. Uses python3 (portable; no
# /dev/tcp, nc, or bashism dependency).
hdn_fixture_tcp_open() {
  python3 - "$1" "$2" <<'PY'
import socket
import sys

host, port = sys.argv[1], int(sys.argv[2])
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.settimeout(2)
sys.exit(0 if s.connect_ex((host, port)) == 0 else 1)
PY
}

# hdn_fixture_novnc_root -- print the directory that contains vnc.html for a
# local noVNC checkout/package, or nothing if none is found.
hdn_fixture_novnc_root() {
  local c
  for c in \
    "$HOME/novnc/vnc.html" \
    /usr/share/novnc/vnc.html \
    /usr/share/webapps/novnc/vnc.html; do
    if [ -f "$c" ]; then
      printf '%s' "${c%/vnc.html}"
      return 0
    fi
  done
  return 1
}

# hdn_fixture_emit_descriptor <out> <mcp_url> <bearer_file> <ca_file>
#                             <fingerprint> <vnc_addr> <novnc_url>
#                             <qmp_sock> <artifact_dir>
#
# Emit the frozen 8-field descriptor (test/agent/fixture.schema.json) to <out>
# ATOMICALLY and with mode 0600:
#   - the JSON is built with python3 json.dumps (no shell string splicing, so
#     values with odd characters can't corrupt the file);
#   - it is written to a same-directory temp file opened 0600, then
#     os.replace()d into place (atomic on the same filesystem);
#   - as defense in depth it REFUSES to write if any value contains a
#     bearer-shaped hdn_u_/hdn_a_ token -- the descriptor must only ever point
#     AT the token file, never carry the token text.
# Prints nothing on success; the caller owns printing the descriptor path.
hdn_fixture_emit_descriptor() {
  local out="$1"; shift
  python3 - "$out" "$@" <<'PY'
import json
import os
import re
import sys

out = sys.argv[1]
vals = sys.argv[2:]
if len(vals) != 8:
    print("emit: expected 8 field values, got %d" % len(vals), file=sys.stderr)
    sys.exit(2)

(mcp_url, bearer_file, ca_file, fingerprint, vnc_addr, novnc_url,
 qmp_sock, artifact_dir) = vals

d = {
    "mcp_url": mcp_url,
    "bearer_token_file": bearer_file,
    "ca_certificate_file": ca_file,
    "tls_fingerprint": fingerprint,
    "vnc_address": vnc_addr,
    "novnc_url": novnc_url,
    "qmp_socket": qmp_sock,
    "artifact_directory": artifact_dir,
}

blob = json.dumps(d, indent=2)

# Never let a bearer-shaped token reach the descriptor text.
if re.search(r"hdn_[ua]_[A-Za-z0-9_-]{8,}", blob):
    print("emit: descriptor would contain a bearer-shaped token; refusing",
          file=sys.stderr)
    sys.exit(1)

tmp = "%s.tmp.%d" % (out, os.getpid())
fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
try:
    with os.fdopen(fd, "w") as f:
        f.write(blob + "\n")
    os.replace(tmp, out)
    os.chmod(out, 0o600)
except Exception:
    try:
        os.unlink(tmp)
    except OSError:
        pass
    raise
PY
}
