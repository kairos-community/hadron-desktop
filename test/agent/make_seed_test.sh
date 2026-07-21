#!/usr/bin/env bash
# Tests for test/agent/make-seed.sh (Phase 4, Task 2): minting VM bearer
# credentials and building a Kairos `cidata` (NoCloud) seed ISO.
#
# Cases:
#   0. bash -n syntax check of make-seed.sh and this test.
#   1. Happy path (install mode, with admin): exit 0, no `hdn_` substring in
#      combined stdout/stderr, both token files exist with the right
#      prefix/length, user-data contains only digests (never bearer text),
#      the digests in user-data match an independently recomputed SHA-256
#      of each token file's complete content, the install: block has the
#      brief's exact fields, and every plaintext-bearing file (plus the
#      seed dir) is mode 0600/0700.
#   2. Independent admin token: admin bearer/digest differ from the user
#      bearer/digest.
#   3. --no-admin: no admin.token file is created, admin_token_hash is
#      absent from user-data, user_token_hash is still present.
#   4. --live: the install: block (and every one of its fields) is entirely
#      absent from user-data.
#   5. ISO round-trip: `xorriso -indev ... -osirrox on -extract /user-data`
#      pulls back the exact same digest-only, bearer-free text, and the ISO
#      volume label is exactly `cidata`.
#   6. Basic CLI validation: missing seed-dir argument and an unknown flag
#      both fail with a non-zero exit and, again, leak no `hdn_` text.
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
make_seed="$repo_root/test/agent/make-seed.sh"

fail=0
err() { echo "FAIL: $*" >&2; fail=1; }
ok()  { echo "ok: $*"; }

have() { command -v "$1" >/dev/null 2>&1; }

hash_stdin() {
  if have sha256sum; then
    sha256sum | awk '{print $1}'
  else
    shasum -a 256 | awk '{print $1}'
  fi
}

# ---------------------------------------------------------------------------
# 0. syntax
# ---------------------------------------------------------------------------
if bash -n "$make_seed" 2>/dev/null; then
  ok "bash -n ${make_seed#"$repo_root"/}"
else
  err "bash -n failed: ${make_seed#"$repo_root"/}"
fi

if bash -n "${BASH_SOURCE[0]}" 2>/dev/null; then
  ok "bash -n ${BASH_SOURCE[0]#"$repo_root"/} (self)"
else
  err "bash -n failed on this test script"
fi

for bin in openssl xorriso; do
  if ! have "$bin"; then
    echo "FATAL: required tool not found in PATH: $bin" >&2
    echo "This suite is CI-runnable only where openssl and xorriso are installed." >&2
    exit 1
  fi
done

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------

# assert_mode <path> <expected-octal> <label>
assert_mode() {
  local path="$1" want="$2" label="$3" got
  got="$(stat -c '%a' "$path" 2>/dev/null || stat -f '%OLp' "$path" 2>/dev/null)"
  if [ "$got" = "$want" ]; then
    ok "$label: mode $got"
  else
    err "$label: expected mode $want, got $got"
  fi
}

# assert_no_token_leak <file> <label> -- fails if the file's raw bytes
# contain a bearer-shaped hdn_u_/hdn_a_ token.
assert_no_token_leak() {
  local file="$1" label="$2"
  if grep -Eq 'hdn_[ua]_[A-Za-z0-9_-]{8,}' "$file" 2>/dev/null; then
    err "$label: contains what looks like a bearer token"
  else
    ok "$label: no bearer-shaped token text"
  fi
}

# token_digest <file> -- complete-content SHA-256, lowercase hex, over the
# file's exact bytes (mirrors make-seed.sh's own hashing).
token_digest() {
  hash_stdin < "$1"
}

# extract_yaml_hash <user-data-file> <key> -- pulls the hex digest that
# follows `key_hash: "sha256:<hex>"` out of a user-data file, or prints
# nothing if the key is absent.
extract_yaml_hash() {
  local file="$1" key="$2"
  grep -E "^\s*${key}:" "$file" 2>/dev/null \
    | sed -E "s/.*sha256:([0-9a-f]{64})\".*/\1/"
}

# ===========================================================================
# 1. happy path: install mode, with admin
# ===========================================================================
echo "--- happy path: install mode, with admin ---"
seed1="$workdir/seed1"
out1="$workdir/out1.txt"
if "$make_seed" "$seed1" --mode install >"$out1" 2>&1; then
  ok "make-seed.sh exits 0 (install mode, with admin)"
else
  err "make-seed.sh exited nonzero (install mode, with admin); output follows:"
  cat "$out1" >&2
fi

if grep -Eq 'hdn_[ua]_' "$out1"; then
  err "command output contains an 'hdn_' bearer-prefixed string"
else
  ok "command output contains no 'hdn_' string"
fi

user_token="$seed1/user.token"
admin_token="$seed1/admin.token"
user_data="$seed1/user-data"
meta_data="$seed1/meta-data"
seed_iso="$seed1/seed.iso"

for f in "$user_token" "$admin_token" "$user_data" "$meta_data" "$seed_iso"; do
  [ -e "$f" ] && ok "exists: ${f#"$workdir"/}" || err "missing: $f"
done

echo "--- token prefix/length ---"
if grep -Eq '^hdn_u_[A-Za-z0-9_-]{43}$' "$user_token"; then
  ok "user.token matches hdn_u_ + 43-char unpadded base64url body"
else
  err "user.token does not match the expected hdn_u_<43 chars> shape"
fi
if grep -Eq '^hdn_a_[A-Za-z0-9_-]{43}$' "$admin_token"; then
  ok "admin.token matches hdn_a_ + 43-char unpadded base64url body"
else
  err "admin.token does not match the expected hdn_a_<43 chars> shape"
fi
user_len="$(wc -c < "$user_token" | tr -d ' ')"
admin_len="$(wc -c < "$admin_token" | tr -d ' ')"
[ "$user_len" -eq 49 ] && ok "user.token is exactly 49 bytes (no trailing newline)" \
  || err "user.token is $user_len bytes, expected 49"
[ "$admin_len" -eq 49 ] && ok "admin.token is exactly 49 bytes (no trailing newline)" \
  || err "admin.token is $admin_len bytes, expected 49"

echo "--- user-data: digests only, never bearer text ---"
assert_no_token_leak "$user_data" "user-data"
if grep -q 'user_token_hash: "sha256:' "$user_data"; then
  ok "user-data has a sha256-format user_token_hash"
else
  err "user-data missing a sha256-format user_token_hash"
fi
if grep -q 'admin_token_hash: "sha256:' "$user_data"; then
  ok "user-data has a sha256-format admin_token_hash"
else
  err "user-data missing a sha256-format admin_token_hash"
fi

echo "--- digests match an independent recomputation over the complete bearer ---"
want_user_digest="$(token_digest "$user_token")"
got_user_digest="$(extract_yaml_hash "$user_data" user_token_hash)"
if [ -n "$want_user_digest" ] && [ "$want_user_digest" = "$got_user_digest" ]; then
  ok "user_token_hash matches sha256(complete user.token content)"
else
  err "user_token_hash mismatch (yaml=$got_user_digest recomputed=$want_user_digest)"
fi
want_admin_digest="$(token_digest "$admin_token")"
got_admin_digest="$(extract_yaml_hash "$user_data" admin_token_hash)"
if [ -n "$want_admin_digest" ] && [ "$want_admin_digest" = "$got_admin_digest" ]; then
  ok "admin_token_hash matches sha256(complete admin.token content)"
else
  err "admin_token_hash mismatch (yaml=$got_admin_digest recomputed=$want_admin_digest)"
fi

echo "--- exact install authorization fields ---"
if grep -Eq '^install:$' "$user_data"; then
  ok "user-data has top-level 'install:'"
else
  err "user-data missing top-level 'install:'"
fi
grep -Fq '  auto: true' "$user_data" && ok "install.auto: true present" \
  || err "install.auto: true missing"
grep -Fq '  device: /dev/vda' "$user_data" && ok "install.device: /dev/vda present" \
  || err "install.device: /dev/vda missing"
grep -Fq '  reboot: true' "$user_data" && ok "install.reboot: true present" \
  || err "install.reboot: true missing"
grep -Fq 'hadron_agent:' "$user_data" && ok "hadron_agent: present" \
  || err "hadron_agent: missing"
grep -Fq '  enabled: true' "$user_data" && ok "hadron_agent.enabled: true present" \
  || err "hadron_agent.enabled: true missing"
grep -Fq 'listen: "0.0.0.0:7443"' "$user_data" && ok "endpoint.listen 0.0.0.0:7443 present" \
  || err "endpoint.listen 0.0.0.0:7443 missing"
grep -Fq 'mdns: false' "$user_data" && ok "endpoint.mdns: false present" \
  || err "endpoint.mdns: false missing"

echo "--- meta-data is empty ---"
if [ ! -s "$meta_data" ]; then
  ok "meta-data is empty"
else
  err "meta-data is not empty"
fi

echo "--- file modes: 0600/0700 ---"
assert_mode "$user_token" 600 "user.token"
assert_mode "$admin_token" 600 "admin.token"
assert_mode "$user_data" 600 "user-data"
assert_mode "$meta_data" 600 "meta-data"
assert_mode "$seed_iso" 600 "seed.iso"
assert_mode "$seed1" 700 "seed dir"

# ===========================================================================
# 2. admin token is independently generated (never derived from user token)
# ===========================================================================
echo "--- admin token is independent of the user token ---"
if [ "$want_user_digest" != "$want_admin_digest" ]; then
  ok "user and admin token digests differ"
else
  err "user and admin token digests are identical (admin not independently generated)"
fi
if ! diff -q "$user_token" "$admin_token" >/dev/null 2>&1; then
  ok "user.token and admin.token have different byte content"
else
  err "user.token and admin.token are byte-identical"
fi

# ===========================================================================
# 3. --no-admin: no admin bearer minted, no admin_token_hash in the YAML
# ===========================================================================
echo "--- --no-admin ---"
seed3="$workdir/seed3"
out3="$workdir/out3.txt"
if "$make_seed" "$seed3" --mode install --no-admin >"$out3" 2>&1; then
  ok "make-seed.sh exits 0 (--no-admin)"
else
  err "make-seed.sh exited nonzero (--no-admin); output follows:"
  cat "$out3" >&2
fi
grep -Eq 'hdn_[ua]_' "$out3" \
  && err "--no-admin command output contains an 'hdn_' bearer-prefixed string" \
  || ok "--no-admin command output contains no 'hdn_' string"

[ ! -e "$seed3/admin.token" ] && ok "--no-admin: no admin.token file created" \
  || err "--no-admin: admin.token file was created"
if grep -q 'admin_token_hash' "$seed3/user-data"; then
  err "--no-admin: user-data still contains admin_token_hash"
else
  ok "--no-admin: user-data omits admin_token_hash entirely"
fi
if grep -q 'user_token_hash: "sha256:' "$seed3/user-data"; then
  ok "--no-admin: user-data still has a valid user_token_hash"
else
  err "--no-admin: user-data lost its user_token_hash"
fi
assert_no_token_leak "$seed3/user-data" "--no-admin user-data"

# ===========================================================================
# 4. --live: install: block entirely omitted
# ===========================================================================
echo "--- --live (omit install: block) ---"
seed4="$workdir/seed4"
out4="$workdir/out4.txt"
if "$make_seed" "$seed4" --live >"$out4" 2>&1; then
  ok "make-seed.sh exits 0 (--live)"
else
  err "make-seed.sh exited nonzero (--live); output follows:"
  cat "$out4" >&2
fi
grep -Eq 'hdn_[ua]_' "$out4" \
  && err "--live command output contains an 'hdn_' bearer-prefixed string" \
  || ok "--live command output contains no 'hdn_' string"

if grep -Eq '^install:$' "$seed4/user-data"; then
  err "--live: user-data still has a top-level 'install:' block"
else
  ok "--live: user-data has no top-level 'install:' block"
fi
for needle in 'auto: true' 'device: /dev/vda' 'reboot: true'; do
  if grep -Fq "  $needle" "$seed4/user-data"; then
    err "--live: user-data still contains install field '$needle'"
  else
    ok "--live: user-data omits install field '$needle'"
  fi
done
grep -Fq 'hadron_agent:' "$seed4/user-data" && ok "--live: hadron_agent: still present" \
  || err "--live: hadron_agent: missing"
assert_no_token_leak "$seed4/user-data" "--live user-data"

# ===========================================================================
# 5. ISO round-trip via xorriso extraction; volume label is exactly cidata
# ===========================================================================
echo "--- xorriso ISO round-trip (seed1) ---"
extracted="$workdir/extracted-user-data"
if xorriso -indev "$seed_iso" -osirrox on -extract /user-data "$extracted" >"$workdir/xorriso-extract.log" 2>&1; then
  ok "xorriso extracted /user-data from the ISO"
else
  err "xorriso failed to extract /user-data from the ISO"
  cat "$workdir/xorriso-extract.log" >&2
fi

if [ -f "$extracted" ] && diff -q "$user_data" "$extracted" >/dev/null 2>&1; then
  ok "ISO's /user-data byte-for-byte matches the on-disk user-data"
else
  err "ISO's /user-data does not match the on-disk user-data"
fi

assert_no_token_leak "$extracted" "ISO-extracted user-data"
if grep -q 'user_token_hash: "sha256:' "$extracted" 2>/dev/null; then
  ok "ISO-extracted user-data contains the sha256 digest"
else
  err "ISO-extracted user-data is missing the sha256 digest"
fi

echo "--- ISO volume label is exactly 'cidata' ---"
toc_out="$workdir/toc.log"
xorriso -indev "$seed_iso" -toc >"$toc_out" 2>&1 || true
if grep -Eq "Volume id[[:space:]]*:[[:space:]]*'cidata'" "$toc_out"; then
  ok "ISO volume id is exactly 'cidata'"
else
  err "ISO volume id is not 'cidata' (see $toc_out)"
fi

# ===========================================================================
# 6. basic CLI validation
# ===========================================================================
echo "--- CLI validation ---"
out6="$workdir/out6.txt"
if "$make_seed" >"$out6" 2>&1; then
  err "make-seed.sh with no seed-dir argument unexpectedly succeeded"
else
  ok "make-seed.sh with no seed-dir argument fails"
fi
grep -Eq 'hdn_[ua]_' "$out6" \
  && err "no-arg failure output contains an 'hdn_' bearer-prefixed string" \
  || ok "no-arg failure output contains no 'hdn_' string"

out7="$workdir/out7.txt"
if "$make_seed" "$workdir/seed-unknown-flag" --bogus-flag >"$out7" 2>&1; then
  err "make-seed.sh with an unknown flag unexpectedly succeeded"
else
  ok "make-seed.sh with an unknown flag fails"
fi
grep -Eq 'hdn_[ua]_' "$out7" \
  && err "unknown-flag failure output contains an 'hdn_' bearer-prefixed string" \
  || ok "unknown-flag failure output contains no 'hdn_' string"

echo
if [ "$fail" -eq 0 ]; then
  echo "PASS: all make-seed.sh checks"
else
  echo "FAILED: one or more make-seed.sh checks failed" >&2
fi
exit "$fail"
