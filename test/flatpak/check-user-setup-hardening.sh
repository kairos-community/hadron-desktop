#!/usr/bin/env bash
set -euo pipefail

IMAGE="${IMAGE:?set IMAGE to the Hadron image under test}"
CASE="${CASE:-all}"
result=0

case "$CASE" in
    all|query_failure|disable_failure|chmod_failure|clean_extra_descriptor|repair_extra_key|repair_trust_replacement|repair_tampered_descriptor|two_user_mktemp) ;;
    *)
        echo "unknown hardening case: $CASE" >&2
        exit 2
        ;;
esac

run_case() {
    name=$1
    if [ "$CASE" != all ] && [ "$CASE" != "$name" ]; then
        return 0
    fi
    if docker run --rm -i "$IMAGE" sh -eu -s; then
        printf 'PASS %s\n' "$name"
    else
        printf 'FAIL %s\n' "$name" >&2
        result=1
    fi
}

run_case query_failure <<'QUERY_FAILURE'
useradd -m -u 1000 -s /bin/sh fixture
su - fixture -c "flatpak --user remote-add --no-gpg-verify flathub https://dl.flathub.org/repo/"
mv /usr/bin/flatpak /usr/bin/flatpak.real
cat > /usr/bin/flatpak <<'WRAPPER'
#!/bin/sh
if [ "$1" = "--user" ] && [ "$2" = "remotes" ]; then
    echo "injected remote query failure" >&2
    exit 70
fi
exec /usr/bin/flatpak.real "$@"
WRAPPER
chmod +x /usr/bin/flatpak
if /usr/bin/hadron-user-setup; then
    echo "query failure unexpectedly succeeded" >&2
    exit 1
fi
details=$(su - fixture -c "/usr/bin/flatpak.real --user remotes --show-disabled --columns=name,options")
printf "%s\n" "$details" | awk -F "\t" '
  $1 == "flathub" && $2 ~ /disabled/ { found=1 }
  END { exit !found }
'
QUERY_FAILURE

run_case disable_failure <<'DISABLE_FAILURE'
useradd -m -u 1000 -s /bin/sh fixture
su - fixture -c "flatpak --user remote-add --no-gpg-verify flathub https://dl.flathub.org/repo/"
mv /usr/bin/flatpak /usr/bin/flatpak.real
cat > /usr/bin/flatpak <<'WRAPPER'
#!/bin/sh
if [ "$1" = "--user" ] && [ "$2" = "remote-modify" ] &&
   [ "$3" = "--disable" ] && [ ! -e /tmp/disable-failed ]; then
    touch /tmp/disable-failed
    echo "injected first disable failure" >&2
    exit 71
fi
exec /usr/bin/flatpak.real "$@"
WRAPPER
chmod +x /usr/bin/flatpak
if /usr/bin/hadron-user-setup; then
    echo "first disable failure unexpectedly succeeded" >&2
    exit 1
fi
details=$(su - fixture -c "/usr/bin/flatpak.real --user remotes --show-disabled --columns=name,options")
printf "%s\n" "$details" | awk -F "\t" '
  $1 == "flathub" && $2 ~ /disabled/ { found=1 }
  END { exit !found }
'
DISABLE_FAILURE

run_case chmod_failure <<'CHMOD_FAILURE'
useradd -m -u 1000 -s /bin/sh fixture
su - fixture -c "flatpak --user remote-add --no-gpg-verify flathub https://dl.flathub.org/repo/"
mkdir /fault-bin
cat > /fault-bin/chmod <<'WRAPPER'
#!/bin/sh
case "$1:$2" in
    700:/run/hadron-user-setup-gpg.*)
        details=$(su - fixture -c "flatpak --user remotes --show-disabled --columns=name,options")
        printf "%s\n" "$details" | awk -F "\t" '
          $1 == "flathub" && $2 ~ /disabled/ { found=1 }
          END { exit !found }
        ' && touch /tmp/chmod-saw-disabled
        echo "injected chmod failure" >&2
        exit 72
        ;;
esac
exec /usr/bin/chmod "$@"
WRAPPER
chmod +x /fault-bin/chmod
if PATH=/fault-bin:/usr/bin:/bin /usr/bin/hadron-user-setup; then
    echo "chmod failure unexpectedly succeeded" >&2
    exit 1
fi
test -e /tmp/chmod-saw-disabled
details=$(su - fixture -c "flatpak --user remotes --show-disabled --columns=name,options")
printf "%s\n" "$details" | awk -F "\t" '
  $1 == "flathub" && $2 ~ /disabled/ { found=1 }
  END { exit !found }
'
CHMOD_FAILURE

run_case clean_extra_descriptor <<'CLEAN_EXTRA_DESCRIPTOR'
useradd -m -u 1000 -s /bin/sh fixture
install -d -m700 /tmp/extra-home
GNUPGHOME=/tmp/extra-home gpg --batch --pinentry-mode loopback --passphrase '' --quick-generate-key 'Hadron migration extra key <extra@example.invalid>' ed25519 sign 0
GNUPGHOME=/tmp/extra-home gpg --batch --export > /tmp/extra.gpg
cat /usr/share/hadron/flathub.gpg /tmp/extra.gpg > /tmp/combined.gpg
encoded=$(base64 /tmp/combined.gpg | tr -d '\n')
sed -i "s|^GPGKey=.*|GPGKey=$encoded|" /usr/share/hadron/flathub.flatpakrepo
if /usr/bin/hadron-user-setup; then
    echo "extra descriptor key unexpectedly succeeded" >&2
    exit 1
fi
details=$(su - fixture -c "flatpak --user remotes --show-disabled --columns=name,options")
printf "%s\n" "$details" | awk -F "\t" '
  $1 == "flathub" && $2 !~ /disabled/ { bad=1 }
  END { exit bad }
'
CLEAN_EXTRA_DESCRIPTOR

run_case repair_extra_key <<'REPAIR_EXTRA_KEY'
useradd -m -u 1000 -s /bin/sh fixture
su - fixture -c "flatpak --user remote-add --no-gpg-verify flathub https://dl.flathub.org/repo/"
install -d -m700 /tmp/extra-home
GNUPGHOME=/tmp/extra-home gpg --batch --pinentry-mode loopback --passphrase '' --quick-generate-key 'Hadron migration extra key <extra@example.invalid>' ed25519 sign 0
GNUPGHOME=/tmp/extra-home gpg --batch --export > /tmp/extra.gpg
cat /tmp/extra.gpg >> /usr/share/hadron/flathub.gpg
if /usr/bin/hadron-user-setup; then
    echo "extra bootstrap key unexpectedly succeeded" >&2
    exit 1
fi
details=$(su - fixture -c "flatpak --user remotes --show-disabled --columns=name,options")
printf "%s\n" "$details" | awk -F "\t" '
  $1 == "flathub" && $2 ~ /disabled/ { found=1 }
  END { exit !found }
'
REPAIR_EXTRA_KEY

run_case repair_trust_replacement <<'REPAIR_TRUST_REPLACEMENT'
useradd -m -u 1000 -s /bin/sh fixture
su - fixture -c "flatpak --user remote-add flathub /usr/share/hadron/flathub.flatpakrepo"
install -d -m700 /tmp/extra-home
GNUPGHOME=/tmp/extra-home gpg --batch --pinentry-mode loopback --passphrase '' --quick-generate-key 'Hadron migration extra key <extra@example.invalid>' ed25519 sign 0
GNUPGHOME=/tmp/extra-home gpg --batch --export > /tmp/extra.gpg
su - fixture -c "flatpak --user remote-modify --gpg-import=/tmp/extra.gpg flathub"
repo=/home/fixture/.local/share/flatpak/repo
before_keys=$(GNUPGHOME=/tmp/extra-home gpg --batch --with-colons --show-keys "$repo/flathub.trustedkeys.gpg" 2>/dev/null |
  awk -F: '$1 == "pub" { primary=1; next }
    primary && $1 == "fpr" { print $10; primary=0 }')
test "$(printf "%s\n" "$before_keys" | wc -l)" -eq 2
metadata_before=$(su - fixture -c \
  "flatpak --user remotes --columns=name,title:full,comment:full,description:full,homepage:full,icon:full")
install -d -o fixture -g fixture /home/fixture/.local/share/flatpak/app/org.example.Preserved/current/test
printf "flathub\n" > /home/fixture/.local/share/flatpak/app/org.example.Preserved/current/test/origin
chown fixture:fixture /home/fixture/.local/share/flatpak/app/org.example.Preserved/current/test/origin
/usr/bin/hadron-user-setup
metadata_after=$(su - fixture -c \
  "flatpak --user remotes --columns=name,title:full,comment:full,description:full,homepage:full,icon:full")
test "$metadata_after" = "$metadata_before"
test "$(cat /home/fixture/.local/share/flatpak/app/org.example.Preserved/current/test/origin)" = flathub
after_keys=$(GNUPGHOME=/tmp/extra-home gpg --batch --with-colons --show-keys "$repo/flathub.trustedkeys.gpg" 2>/dev/null |
  awk -F: '$1 == "pub" { primary=1; next }
    primary && $1 == "fpr" { print $10; primary=0 }')
test "$after_keys" = 6E5C05D979C76DAF93C081354184DD4D907A7CAE
details=$(su - fixture -c "flatpak --user remotes --show-disabled --columns=name,url,options")
printf "%s\n" "$details" | awk -F "\t" '
  $1 == "flathub" && $2 == "https://dl.flathub.org/repo/" &&
  $3 !~ /no-gpg-verify/ && $3 !~ /disabled/ { found=1 }
  END { exit !found }
'
su - fixture -c "flatpak --user remote-info --show-commit flathub org.chromium.Chromium" >/dev/null
REPAIR_TRUST_REPLACEMENT

run_case repair_tampered_descriptor <<'REPAIR_TAMPERED_DESCRIPTOR'
useradd -m -u 1000 -s /bin/sh fixture
su - fixture -c "flatpak --user remote-add --no-gpg-verify flathub https://dl.flathub.org/repo/"
printf "\n# valid but tampered descriptor\n" >> /usr/share/hadron/flathub.flatpakrepo
if /usr/bin/hadron-user-setup; then
    echo "tampered descriptor repair unexpectedly succeeded" >&2
    exit 1
fi
details=$(su - fixture -c "flatpak --user remotes --show-disabled --columns=name,options")
printf "%s\n" "$details" | awk -F "\t" '
  $1 == "flathub" && $2 ~ /disabled/ { found=1 }
  END { exit !found }
'
REPAIR_TAMPERED_DESCRIPTOR

run_case two_user_mktemp <<'TWO_USER_MKTEMP'
useradd -m -u 1000 -s /bin/sh first
useradd -m -u 1001 -s /bin/sh second
su - first -c "flatpak --user remote-add --no-gpg-verify flathub https://dl.flathub.org/repo/"
su - second -c "flatpak --user remote-add --no-gpg-verify flathub https://dl.flathub.org/repo/"
mkdir /fault-bin
cat > /fault-bin/mktemp <<'WRAPPER'
#!/bin/sh
if [ ! -e /tmp/mktemp-failed ]; then
    touch /tmp/mktemp-failed
    details=$(su - first -c "flatpak --user remotes --show-disabled --columns=name,options")
    printf "%s\n" "$details" | awk -F "\t" '
      $1 == "flathub" && $2 ~ /disabled/ { found=1 }
      END { exit !found }
    ' && touch /tmp/mktemp-saw-disabled
    echo "injected mktemp failure" >&2
    exit 73
fi
exec /usr/bin/mktemp "$@"
WRAPPER
chmod +x /fault-bin/mktemp
if PATH=/fault-bin:/usr/bin:/bin /usr/bin/hadron-user-setup; then
    echo "two-user mktemp failure unexpectedly succeeded" >&2
    exit 1
fi
test -e /tmp/mktemp-saw-disabled
first_details=$(su - first -c "flatpak --user remotes --show-disabled --columns=name,options")
printf "%s\n" "$first_details" | awk -F "\t" '
  $1 == "flathub" && $2 ~ /disabled/ { found=1 }
  END { exit !found }
'
second_details=$(su - second -c "flatpak --user remotes --show-disabled --columns=name,url,options")
printf "%s\n" "$second_details" | awk -F "\t" '
  $1 == "flathub" && $2 == "https://dl.flathub.org/repo/" &&
  $3 !~ /no-gpg-verify/ && $3 !~ /disabled/ { found=1 }
  END { exit !found }
'
TWO_USER_MKTEMP

exit "$result"
