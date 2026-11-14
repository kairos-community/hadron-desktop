#!/bin/sh
# Install an unpacked sof-bin release (Sound Open Firmware for Intel audio
# DSPs), compressed with zstd.
#
#   install-sof.sh SRC DEST
#
# SRC is the unpacked release tarball, DEST the firmware directory (for
# example /firmware/usr/lib/firmware). Installs SRC/sof* into DEST/intel/, as
# the release's install.sh does, but compresses every file to <name>.zst and
# points each link at the compressed file. Leaves out the .ldc files, which
# only sof-logger reads, and the tools.
set -eu

[ "$#" = 2 ] || { echo "usage: $0 SRC DEST" >&2; exit 2; }
src="$1" dest="$2/intel"
mkdir -p "$dest"
dest="$(cd "$dest" && pwd)"
cd "$src"
ls -d sof sof-ipc4 sof-ipc4-tplg sof-tplg >/dev/null || {
  echo "install-sof: $src does not look like a sof-bin release" >&2; exit 1; }

find sof* -name '*.ldc' -prune -o -type d -print | while read -r d; do
  mkdir -p "$dest/$d"
done
find sof* -name '*.ldc' -prune -o -type f -print | while read -r f; do
  zstd --compress --quiet --force "$f" -o "$dest/$f.zst"
done
# A link to a directory (sof-ace-tplg -> sof-ipc4-tplg) stays as it is; a
# link to a file gains .zst on both sides.
find sof* -name '*.ldc' -prune -o -type l -print | while read -r l; do
  target="$(readlink "$l")"
  if [ -d "$l" ]; then
    ln -sfn "$target" "$dest/$l"
  else
    ln -sfn "$target.zst" "$dest/$l.zst"
  fi
done
# Every link must lead to an installed file or directory.
broken="$(find "$dest"/sof* -type l | while read -r l; do [ -e "$l" ] || echo "$l"; done)"
[ -z "$broken" ] || { echo "install-sof: broken links: $broken" >&2; exit 1; }
