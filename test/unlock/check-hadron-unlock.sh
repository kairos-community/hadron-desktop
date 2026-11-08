#!/usr/bin/env bash
# hadron-unlock's recovery key handling, with a fake cryptsetup:
#   test/unlock/check-hadron-unlock.sh
# The prompt names no device, says "or recovery key" only when a
# systemd-recovery token is there, and the key opens the disk in any case and
# without its dashes, while a passphrase is used byte for byte.
set -u
repo=$(cd "$(dirname "$0")/../.." && pwd)
script=$repo/rootfs/usr/lib/dracut/modules.d/45hadron-unlock/hadron-unlock.sh
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fail=0
expect() { if [ "$2" = "$3" ]; then echo "ok: $1"; else echo "FAIL: $1: got [$2] want [$3]"; fail=1; fi; }

key=cbdefghi-jklnrtuv-cbdefghi-jklnrtuv-cbdefghi-jklnrtuv-cbdefghi-jklnrtuv
mkdir -p "$T/bin"
# luksDump prints a token list from $T/tokens; open accepts $T/secrets.
cat > "$T/bin/cryptsetup" <<SH
#!/bin/bash
case "\$1" in
  luksDump) printf 'Subsystem:\tkcrypt:passphrase\nTokens:\n'; cat "$T/tokens"; printf 'Digests:\n' ;;
  open) in=\$(cat); grep -qxF -- "\$in" "$T/secrets" && echo "\$in" >> "$T/opened" ;;
esac
SH
chmod +x "$T/bin/cryptsetup"
printf 'correct horse battery\n%s\n' "$key" > "$T/secrets"

# Load the functions only.
eval "$(sed -n '/^has_recovery_key()/,/^}/p; /^recovery_key_form()/,/^}/p; /^open_with()/,/^}/p' "$script")"
PATH=$T/bin:$PATH

: > "$T/tokens"
has_recovery_key /dev/vda5 && r=yes || r=no
expect "no token, no recovery key" "$r" no
printf '  0: systemd-recovery\n\tKeyslot:    1\n' > "$T/tokens"
has_recovery_key /dev/vda5 && r=yes || r=no
expect "a systemd-recovery token is a recovery key" "$r" yes

expect "canonical form from upper case without dashes" \
    "$(recovery_key_form "$(echo "$key" | tr -d - | tr '[:lower:]' '[:upper:]')")" "$key"
expect "a passphrase is not a key" "$(recovery_key_form 'correct horse battery' || echo no)" no

open_with /dev/vda5 vda5 'correct horse battery' 1 && r=yes || r=no
expect "the passphrase opens" "$r" yes
open_with /dev/vda5 vda5 "$(echo "$key" | tr -d -)" 1 && r=yes || r=no
expect "the key without dashes opens" "$r" yes
open_with /dev/vda5 vda5 "$(echo "$key" | tr -d -)" 0 && r=yes || r=no
expect "without a recovery key the text is used as typed" "$r" no
open_with /dev/vda5 vda5 'wrong' 1 && r=yes || r=no
expect "a wrong one does not" "$r" no

for p in 'Enter the disk passphrase' 'Enter the disk passphrase or recovery key' \
        'Wrong passphrase, try again' 'Wrong passphrase or recovery key, try again'; do
    expect "the script says: $p" "$(grep -c -F "\"$p\"" "$script")" 1
done
expect "no prompt names the device" "$(grep -c 'prompt=.*\${dev}' "$script")" 0
exit "$fail"
