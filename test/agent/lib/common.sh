#!/usr/bin/env bash
umask 077
# test/agent/lib/common.sh -- shared secret-handling and process-lifecycle
# helpers for the Cua agent VM fixture (Phase 4).
#
# CONTRACT (do not violate; test/agent/descriptor_test.sh enforces parts of
# this mechanically):
#
#   - `umask 077` above is the very first executable statement in this file,
#     so every file any caller creates after sourcing it defaults to
#     owner-only (0600 for plain files, 0700 for directories) before any
#     token, descriptor, or seed material ever touches disk.
#
#   - Exactly one EXIT trap is installed by this file (see
#     hdn_agent__install_trap below). Sourcing it more than once in the same
#     shell is a no-op (guarded by HDN_AGENT_COMMON_SH_SOURCED) so it never
#     stacks a second trap. Callers that need their own teardown (temp dirs,
#     extra sockets, ...) MUST register it with hdn_agent_on_exit instead of
#     calling `trap ... EXIT` themselves -- a second direct `trap` call would
#     silently replace this file's handler and stop PIDs from being reaped.
#
#   - hdn_agent_redact (and hdn_agent_redact_str) is the only sanctioned way
#     to put fixture text -- logs, descriptors, command output, CI artifacts
#     -- anywhere it might be printed or uploaded. It scrubs both hdn_u_
#     (user-class) and hdn_a_ (admin-class) bearer tokens (see
#     agent/internal/auth/token.go).
#
#   - This file MUST NEVER `set -x` / `set -o xtrace` itself, and no caller
#     that has sourced it should re-enable tracing after a token has been
#     generated: tracing an hdn_u_/hdn_a_ value straight to a log or CI
#     console defeats every mode-0600 protection here. Defensively turn off
#     any tracing this shell may have inherited.
set +x 2>/dev/null || true

# Re-sourcing must be a safe no-op: don't re-run the trap install below, and
# don't clobber PID/cleanup state a caller may have already accumulated.
if [ -n "${HDN_AGENT_COMMON_SH_SOURCED:-}" ]; then
  return 0 2>/dev/null || exit 0
fi
HDN_AGENT_COMMON_SH_SOURCED=1

# --- process lifecycle: tracked PIDs + cleanup callbacks --------------------

# QEMU and noVNC (websockify) PIDs live here; hdn_agent_track_pid appends,
# the single EXIT trap below reaps them.
HDN_AGENT_PIDS=()

# Arbitrary extra cleanup (temp dirs, extra sockets, ...) registered via
# hdn_agent_on_exit. Run in LIFO order, before PID teardown, by
# hdn_agent_cleanup.
HDN_AGENT_CLEANUP_FNS=()

# hdn_agent_track_pid <pid> -- remember a background PID (QEMU, websockify)
# so the EXIT trap terminates it on exit.
hdn_agent_track_pid() {
  [ -n "${1:-}" ] || return 0
  HDN_AGENT_PIDS+=("$1")
}

# hdn_agent_on_exit <shell-command-string> -- register one extra cleanup
# action to run when the process exits. Commands are eval'd in LIFO order by
# hdn_agent_cleanup, before any tracked PID is signaled.
hdn_agent_on_exit() {
  [ -n "${1:-}" ] || return 0
  HDN_AGENT_CLEANUP_FNS+=("$1")
}

# hdn_agent_cleanup is the body of the single EXIT trap installed below. It
# is idempotent (safe to call again manually, e.g. from a `stop` subcommand,
# after the trap already ran) because it empties both lists once it has
# processed them.
hdn_agent_cleanup() {
  local i fn pid tries

  for (( i=${#HDN_AGENT_CLEANUP_FNS[@]}-1; i>=0; i-- )); do
    fn="${HDN_AGENT_CLEANUP_FNS[$i]}"
    eval "$fn" 2>/dev/null || true
  done
  HDN_AGENT_CLEANUP_FNS=()

  # Ask nicely first.
  for pid in "${HDN_AGENT_PIDS[@]:-}"; do
    [ -n "$pid" ] || continue
    kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null || true
  done
  # Give each tracked process up to ~2s to exit, then force it.
  for pid in "${HDN_AGENT_PIDS[@]:-}"; do
    [ -n "$pid" ] || continue
    tries=0
    while [ "$tries" -lt 10 ] && kill -0 "$pid" 2>/dev/null; do
      sleep 0.2
      tries=$((tries + 1))
    done
    kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null || true
  done
  HDN_AGENT_PIDS=()
}

hdn_agent__install_trap() {
  trap hdn_agent_cleanup EXIT
}
hdn_agent__install_trap

# --- loopback port allocation ------------------------------------------------

# hdn_agent_alloc_port -- binds an ephemeral TCP port on 127.0.0.1, reads
# back the OS-assigned port number, and releases it immediately. This is a
# TOCTOU race like any "find a free port" helper, but it is scoped to
# loopback only (never 0.0.0.0), so it can never reserve anything reachable
# off-host.
hdn_agent_alloc_port() {
  python3 - <<'PY'
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

# --- secret redaction ---------------------------------------------------------

# hdn_agent_redact -- reads text on stdin, writes it to stdout with every
# hdn_u_<...> / hdn_a_<...> bearer token replaced by a fixed placeholder.
# Tokens are 32 random bytes, base64url-encoded (see agent/internal/auth),
# so the body alphabet is [A-Za-z0-9_-]; match it greedily.
hdn_agent_redact() {
  sed -E \
    -e 's/hdn_u_[A-Za-z0-9_-]+/hdn_u_<redacted>/g' \
    -e 's/hdn_a_[A-Za-z0-9_-]+/hdn_a_<redacted>/g'
}

# hdn_agent_redact_str <text> -- same redaction, for a single argument
# instead of a stream.
hdn_agent_redact_str() {
  printf '%s' "${1:-}" | hdn_agent_redact
}
