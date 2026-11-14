#!/bin/sh
# Install part of an unpacked linux-firmware release, compressed with zstd.
#
#   install-firmware.sh [--newest PATTERN=N]... [--require LIST] SRC DEST PATTERN...
#
# SRC is the unpacked release tarball, DEST the firmware directory to fill
# (for example /firmware/usr/lib/firmware). Installs every file that
# SRC/WHENCE lists under a path matching one of the PATTERNs, the way
# upstream's `copy-firmware.sh --zstd` installs all of them: File entries are
# compressed to <name>.zst, RawFile entries are copied as they are. A PATTERN
# is a shell pattern for the start of the path: `intel/iwlwifi/` or
# `nvidia/*/gsp/*-570.144`. The release copy-firmware.sh has no option to
# install a subset, hence this script.
#
# Then it makes every Link entry of WHENCE whose target it installed, wherever
# the link itself is. The tarball has no symlinks of its own, and drivers
# often ask for the link and not the file: iwlwifi asks for
# iwlwifi-<card>-<api>.ucode at the top, which links to intel/iwlwifi/, and
# most nvidia/<chip> directories exist only as links.
#
# --newest PATTERN=N keeps, of the <name>-<api>.ucode files under PATTERN, the
# N newest API versions of each <name> (a "c" API, c101, is newer than 101).
# The kernel loads the newest API it supports, so the older ones are dead
# weight; N > 1 leaves room for a release that is ahead of the kernel.
#
# --require LIST names a file with one firmware path per line, the
# MODULE_FIRMWARE names of the kernel the blobs are for. Every name in LIST
# that SRC provides under one of the PATTERNs must be in DEST, or the install
# fails: this catches a missing link and a --newest that dropped the API the
# kernel asks for.
#
# A PATTERN that matches no File entry fails the install: upstream moves
# directories (iwlwifi-*.ucode moved to intel/iwlwifi/), and a pattern that
# silently matches nothing ships an image without that hardware's firmware.
set -eu

usage() {
  echo "usage: $0 [--newest PATTERN=N]... [--require LIST] SRC DEST PATTERN..." >&2
  exit 2
}

newest="" require=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --newest) [ "$#" -ge 2 ] || usage; newest="$newest$2
"; shift 2 ;;
    --require) [ "$#" -ge 2 ] || usage; require="$2"; shift 2 ;;
    --) shift; break ;;
    -*) usage ;;
    *) break ;;
  esac
done
[ "$#" -ge 3 ] || usage
src="$1" dest="$2"
shift 2
[ -z "$require" ] || require="$(cd "$(dirname "$require")" && pwd)/$(basename "$require")"
mkdir -p "$dest"
dest="$(cd "$dest" && pwd)"
cd "$src"
[ -f WHENCE ] || { echo "install-firmware: no WHENCE in $src" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
tab="$(printf '\t')"

# The whole of WHENCE as tab-separated kind, path, link target.
awk '
  /^(RawFile|File):/ {
    kind = $1; sub(/:$/, "", kind)
    line = $0; sub(/^[A-Za-z]+: */, "", line); gsub(/"/, "", line)
    printf "%s\t%s\t\n", kind, line
  }
  /^Link:/ {
    line = $0; sub(/^Link: */, "", line); gsub(/"/, "", line)
    n = split(line, a, / -> /)
    if (n == 2) printf "Link\t%s\t%s\n", a[1], a[2]
  }' WHENCE > "$work/whence"

matches() { # PATH PATTERN...
  _p="$1"; shift
  for _pat in "$@"; do
    # shellcheck disable=SC2254
    case "$_p" in $_pat*) return 0 ;; esac
  done
  return 1
}

# The files to install.
set -f
: > "$work/files"
for pattern in "$@"; do
  n=0
  while IFS="$tab" read -r kind path target; do
    [ "$kind" = Link ] && continue
    # shellcheck disable=SC2254
    case "$path" in $pattern*) printf '%s\t%s\n' "$kind" "$path" >> "$work/files"; n=$((n + 1)) ;; esac
  done < "$work/whence"
  if [ "$n" = 0 ]; then
    echo "install-firmware: WHENCE lists no file under '$pattern'" >&2
    exit 1
  fi
done
sort -u -o "$work/files" "$work/files"

# --newest: drop all but the N newest APIs of each <name>-<api>.ucode.
printf '%s' "$newest" | while IFS= read -r spec; do
  [ -n "$spec" ] || continue
  pattern="${spec%=*}" keep="${spec##*=}"
  case "$keep" in ''|*[!0-9]*) echo "install-firmware: bad --newest '$spec'" >&2; exit 2 ;; esac
  # The sort key puts c-APIs after plain ones: c101 -> 1101.
  while IFS="$tab" read -r kind path; do
    # shellcheck disable=SC2254
    case "$path" in $pattern*) ;; *) continue ;; esac
    printf '%s\n' "$path"
  done < "$work/files" | awk -v keep="$keep" '
    match($0, /-c?[0-9]+\.ucode$/) {
      name = substr($0, 1, RSTART - 1); api = substr($0, RSTART + 1, RLENGTH - 7)
      key = (api ~ /^c/) ? 1000 + substr(api, 2) : api + 0
      print name "\t" key "\t" $0
    }' | sort -t "$tab" -k1,1 -k2,2nr | awk -F "$tab" -v keep="$keep" '
    { if ($1 != last) { last = $1; n = 0 } if (++n > keep) print $3 }' >> "$work/drop"
done
if [ -s "$work/drop" ]; then
  awk -F "$tab" 'NR == FNR { drop[$0] = 1; next } !($2 in drop)' "$work/drop" "$work/files" > "$work/kept"
  mv "$work/kept" "$work/files"
fi
set +f

while IFS="$tab" read -r kind path; do
  mkdir -p "$dest/$(dirname "$path")"
  case "$kind" in
    File) zstd --compress --quiet --force "$path" -o "$dest/$path.zst" ;;
    RawFile) cp "$path" "$dest/$path" ;;
  esac
done < "$work/files"

# Every link whose target is installed. Repeat while links are made, because
# a link may point at a link (or through a directory link) that WHENCE lists
# later.
grep "^Link$tab" "$work/whence" > "$work/links" || true
while :; do
  made=0
  : > "$work/pending"
  while IFS="$tab" read -r kind path target; do
    dir="$dest/$(dirname "$path")"
    resolved="$(realpath -m -s "$dir/$target")"
    if [ -e "$resolved" ]; then
      # A directory link, or a link to a RawFile.
      mkdir -p "$dir"
      ln -sfn "$target" "$dest/$path"
      made=1
    elif [ -e "$resolved.zst" ]; then
      mkdir -p "$dir"
      ln -sfn "$target.zst" "$dest/$path.zst"
      made=1
    else
      printf '%s\t%s\t%s\n' "$kind" "$path" "$target" >> "$work/pending"
    fi
  done < "$work/links"
  mv "$work/pending" "$work/links"
  if [ "$made" = 0 ] || [ ! -s "$work/links" ]; then break; fi
done
# A link under one of the patterns whose target is not installed.
set -f
while IFS="$tab" read -r kind path target; do
  if matches "$path" "$@"; then
    echo "install-firmware: skipping link $path -> $target (target not installed)" >&2
  fi
done < "$work/links"
set +f

[ -n "$require" ] || exit 0

# --require: lay out all of SRC as empty files and links, to see where each
# name the kernel asks for leads.
ref="$work/ref"
mkdir -p "$ref"
while IFS="$tab" read -r kind path target; do
  case "$kind" in
    File|RawFile) mkdir -p "$ref/$(dirname "$path")"; : > "$ref/$path" ;;
  esac
done < "$work/whence"
grep "^Link$tab" "$work/whence" | while IFS="$tab" read -r kind path target; do
  mkdir -p "$ref/$(dirname "$path")"
  ln -sfn "$target" "$ref/$path"
done
checked=0 missing=0
set -f
while IFS= read -r name; do
  [ -n "$name" ] || continue
  [ -f "$ref/$name" ] || continue
  real="$(realpath "$ref/$name")"
  real="${real#"$(realpath "$ref")"/}"
  matches "$real" "$@" || continue
  checked=$((checked + 1))
  if [ ! -e "$dest/$name" ] && [ ! -e "$dest/$name.zst" ]; then
    echo "install-firmware: the kernel asks for $name ($real), which is not installed" >&2
    missing=$((missing + 1))
  fi
done < "$require"
set +f
echo "install-firmware: $checked firmware names the kernel asks for are installed, $missing missing"
[ "$missing" = 0 ] && [ "$checked" -gt 0 ]
