#!/usr/bin/env bash
set -euo pipefail

IMAGE="${IMAGE:?set IMAGE to the Hadron image under test}"
DESCRIPTOR_SHA=3371dd250e61d9e1633630073fefda153cd4426f72f4afa0c3373ae2e8fea03a
PRIMARY_FINGERPRINT=6E5C05D979C76DAF93C081354184DD4D907A7CAE
MAX_BYTES=20971520

docker run --rm -i "$IMAGE" sh -eu -s -- \
  "$DESCRIPTOR_SHA" "$PRIMARY_FINGERPRINT" "$MAX_BYTES" <<'CONTAINER'
descriptor_sha=$1
primary_fingerprint=$2
max_bytes=$3

for program in gpg gpgv gpgconf gpg-agent dirmngr; do
    command -v "$program" >/dev/null || {
        echo "missing required program: $program" >&2
        exit 1
    }
done

gpg --version | grep -F "gpg (GnuPG) 2.4.9" >/dev/null
gpgconf --list-components | grep -E "^gpg:OpenPGP:" >/dev/null
keyboxd=$(gpgconf --list-components |
    awk -F: '$1 == "keyboxd" { print $3; exit }')
test -n "$keyboxd" && test -x "$keyboxd"

for omitted in gpgsm scdaemon gpg-wks-client gpg-card sqlite3; do
    if command -v "$omitted" >/dev/null; then
        echo "unexpected GnuPG component: $omitted" >&2
        exit 1
    fi
done
test ! -e /usr/lib/gnupg/scdaemon
test ! -e /usr/lib/gnupg/tpm2daemon
test ! -e /usr/lib/gnupg/gpg-wks-client

: > /tmp/gnupg-ldd
for program in gpg gpgv gpgconf gpg-agent dirmngr; do
    ldd "$(command -v "$program")" >> /tmp/gnupg-ldd
done
ldd "$keyboxd" >> /tmp/gnupg-ldd
ldd /usr/lib/libgpgme.so.11 >> /tmp/gnupg-ldd
if grep -F "not found" /tmp/gnupg-ldd; then
    exit 1
fi

descriptor=/usr/share/hadron/flathub.flatpakrepo
key=/usr/share/hadron/flathub.gpg
test "$(sha256sum "$descriptor" | cut -d ' ' -f1)" = "$descriptor_sha"
install -d -m700 /tmp/runtime-gpg-home
fingerprint=$(GNUPGHOME=/tmp/runtime-gpg-home \
    gpg --batch --with-colons --show-keys "$key" 2>/dev/null |
    awk -F: '$1 == "fpr" {print $10; exit}')
test "$fingerprint" = "$primary_fingerprint"

size=$(cat /usr/share/hadron/gnupg-runtime-size)
case "$size" in (*[!0-9]*|"") exit 1;; esac
test "$size" -le "$max_bytes"
CONTAINER
