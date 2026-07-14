#!/usr/bin/env bash
set -euo pipefail
IMAGE="${IMAGE:?set IMAGE to the Hadron image under test}"

test "$(docker run --rm "$IMAGE" stat -c %a /tmp)" = 1777

docker run --rm -i "$IMAGE" sh -eu -s <<'CLEAN'
useradd -m -u 1000 -s /bin/sh fixture
/usr/bin/hadron-user-setup
/usr/bin/hadron-user-setup
details=$(su - fixture -c \
  "flatpak --user remotes --show-disabled --columns=name,url,options")
printf "%s\n" "$details" | awk -F "\t" '
  $1 == "flathub" && $2 == "https://dl.flathub.org/repo/" &&
  $3 !~ /no-gpg-verify/ && $3 !~ /disabled/ { found=1 }
  END { exit !found }
'
su - fixture -c \
  "flatpak --user remote-info --show-commit flathub org.chromium.Chromium" \
  >/dev/null
CLEAN

docker run --rm -i "$IMAGE" sh -eu -s <<'REPAIR'
useradd -m -u 1000 -s /bin/sh fixture
su - fixture -c \
  "flatpak --user remote-add --no-gpg-verify flathub https://dl.flathub.org/repo/"
su - fixture -c \
  "flatpak --user remote-modify --url=https://example.invalid/repo/ flathub"
/usr/bin/hadron-user-setup
details=$(su - fixture -c \
  "flatpak --user remotes --show-disabled --columns=name,url,options")
printf "%s\n" "$details" | awk -F "\t" '
  $1 == "flathub" && $2 == "https://dl.flathub.org/repo/" &&
  $3 !~ /no-gpg-verify/ && $3 !~ /disabled/ { found=1 }
  END { exit !found }
'
su - fixture -c \
  "flatpak --user remote-info --show-commit flathub org.chromium.Chromium" \
  >/dev/null
REPAIR

docker run --rm -i "$IMAGE" sh -eu -s <<'FAIL_CLOSED'
useradd -m -u 1000 -s /bin/sh fixture
su - fixture -c \
  "flatpak --user remote-add --no-gpg-verify flathub https://dl.flathub.org/repo/"
printf "%s\n" not-a-public-key > /usr/share/hadron/flathub.gpg
if /usr/bin/hadron-user-setup; then
    echo "malformed key unexpectedly succeeded" >&2
    exit 1
fi
details=$(su - fixture -c \
  "flatpak --user remotes --show-disabled --columns=name,url,options")
printf "%s\n" "$details" | awk -F "\t" '
  $1 == "flathub" && $3 ~ /disabled/ { found=1 }
  END { exit !found }
'
FAIL_CLOSED

docker run --rm -i "$IMAGE" sh -eu -s <<'BAD_DESCRIPTOR'
useradd -m -u 1000 -s /bin/sh fixture
printf '%s\n' '[not-a-flatpak-repository' > \
  /usr/share/hadron/flathub.flatpakrepo
if /usr/bin/hadron-user-setup; then
    echo "malformed descriptor unexpectedly succeeded" >&2
    exit 1
fi
details=$(su - fixture -c \
  "flatpak --user remotes --show-disabled --columns=name,url,options")
printf "%s\n" "$details" | awk -F "\t" '
  $1 == "flathub" && $3 !~ /disabled/ { bad=1 }
  END { exit bad }
'
BAD_DESCRIPTOR
