#!/bin/sh
# Checks tools/install-firmware.sh and tools/install-sof.sh on a small fake
# linux-firmware and sof-bin tree, without downloading either:
#
# - a link at the top of the tree (iwlwifi-<card>.ucode, the name the driver
#   asks for) is made when its target under a pattern is installed, and a link
#   whose target is not installed is not;
# - --newest keeps the N newest APIs of each card, a c-API above a plain one;
# - --require fails when the kernel asks for a file that a pattern covers but
#   that was not installed, and passes otherwise;
# - a pattern that matches no file fails;
# - install-sof compresses the files, points the links at the .zst files and
#   leaves out the .ldc files.
#
# Needs zstd and coreutils realpath. CI runs it on the runner:
#
#   test/firmware/check-install-firmware.sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail=0
pass() { echo "PASS: $1"; }
bad() { echo "FAIL: $1"; fail=1; }
exists() { if [ -e "$2" ]; then pass "$1"; else bad "$1 ($2 missing)"; fi; }
absent() { if [ -e "$2" ] || [ -L "$2" ]; then bad "$1 ($2 present)"; else pass "$1"; fi; }

src="$work/lf"
mkdir -p "$src/intel/iwlwifi" "$src/amdgpu" "$src/other" "$src/nvidia/tu102/gsp"
for f in intel/iwlwifi/iwlwifi-so-a0-gf-a0-86.ucode intel/iwlwifi/iwlwifi-so-a0-gf-a0-89.ucode \
         intel/iwlwifi/iwlwifi-so-a0-gf-a0-77.ucode intel/iwlwifi/iwlwifi-gl-c0-fm-c0-101.ucode \
         intel/iwlwifi/iwlwifi-gl-c0-fm-c0-c101.ucode intel/iwlwifi/iwlwifi-gl-c0-fm-c0-c102.ucode \
         intel/iwlwifi/iwlwifi-so-a0-gf-a0.pnvm amdgpu/navi10_sos.bin other/x.bin \
         nvidia/tu102/gsp/gsp-535.113.01.bin nvidia/tu102/gsp/gsp-570.144.bin; do
  echo "$f" > "$src/$f"
done
cat > "$src/WHENCE" <<'EOF'
File: intel/iwlwifi/iwlwifi-so-a0-gf-a0-77.ucode
Link: iwlwifi-so-a0-gf-a0-77.ucode -> intel/iwlwifi/iwlwifi-so-a0-gf-a0-77.ucode
File: intel/iwlwifi/iwlwifi-so-a0-gf-a0-86.ucode
Link: iwlwifi-so-a0-gf-a0-86.ucode -> intel/iwlwifi/iwlwifi-so-a0-gf-a0-86.ucode
File: intel/iwlwifi/iwlwifi-so-a0-gf-a0-89.ucode
Link: iwlwifi-so-a0-gf-a0-89.ucode -> intel/iwlwifi/iwlwifi-so-a0-gf-a0-89.ucode
File: intel/iwlwifi/iwlwifi-gl-c0-fm-c0-101.ucode
File: intel/iwlwifi/iwlwifi-gl-c0-fm-c0-c101.ucode
File: intel/iwlwifi/iwlwifi-gl-c0-fm-c0-c102.ucode
Link: iwlwifi-gl-c0-fm-c0-c101.ucode -> intel/iwlwifi/iwlwifi-gl-c0-fm-c0-c101.ucode
File: intel/iwlwifi/iwlwifi-so-a0-gf-a0.pnvm
Link: iwlwifi-so-a0-gf-a0.pnvm -> intel/iwlwifi/iwlwifi-so-a0-gf-a0.pnvm
File: amdgpu/navi10_sos.bin
Link: amdgpu/navi14_sos.bin -> navi10_sos.bin
File: other/x.bin
Link: x.bin -> other/x.bin
File: nvidia/tu102/gsp/gsp-535.113.01.bin
File: nvidia/tu102/gsp/gsp-570.144.bin
Link: nvidia/tu104/gsp -> ../tu102/gsp
EOF

fw() { sh "$repo/tools/install-firmware.sh" "$@"; }

dest="$work/out1"
if fw --newest intel/iwlwifi/=2 "$src" "$dest" intel/iwlwifi/ amdgpu/ 'nvidia/*/gsp/*-570.144' > "$work/log" 2>&1; then
  pass "install succeeds"
else
  bad "install failed: $(cat "$work/log")"
fi
exists "file compressed" "$dest/amdgpu/navi10_sos.bin.zst"
exists "link inside a pattern" "$dest/amdgpu/navi14_sos.bin.zst"
exists "top-level iwlwifi link" "$dest/iwlwifi-so-a0-gf-a0-89.ucode.zst"
exists "top-level link resolves" "$(realpath "$dest/iwlwifi-so-a0-gf-a0-89.ucode.zst")"
exists "pnvm link" "$dest/iwlwifi-so-a0-gf-a0.pnvm.zst"
exists "--newest keeps the newest" "$dest/intel/iwlwifi/iwlwifi-so-a0-gf-a0-89.ucode.zst"
exists "--newest keeps the second newest" "$dest/intel/iwlwifi/iwlwifi-so-a0-gf-a0-86.ucode.zst"
absent "--newest drops the third" "$dest/intel/iwlwifi/iwlwifi-so-a0-gf-a0-77.ucode.zst"
absent "no link to a dropped file" "$dest/iwlwifi-so-a0-gf-a0-77.ucode.zst"
exists "c-API newer than plain API" "$dest/intel/iwlwifi/iwlwifi-gl-c0-fm-c0-c101.ucode.zst"
absent "plain API dropped below c-APIs" "$dest/intel/iwlwifi/iwlwifi-gl-c0-fm-c0-101.ucode.zst"
absent "file outside the patterns" "$dest/other/x.bin.zst"
absent "link to a file not installed" "$dest/x.bin.zst"
exists "glob pattern" "$dest/nvidia/tu102/gsp/gsp-570.144.bin.zst"
absent "glob pattern leaves the other version" "$dest/nvidia/tu102/gsp/gsp-535.113.01.bin.zst"
exists "directory link" "$dest/nvidia/tu104/gsp/gsp-570.144.bin.zst"

printf '%s\n' iwlwifi-so-a0-gf-a0-89.ucode amdgpu/navi14_sos.bin x.bin not-in-tree.bin \
  nvidia/tu104/gsp/gsp-570.144.bin > "$work/kernel-ok"
if fw --newest intel/iwlwifi/=2 --require "$work/kernel-ok" "$src" "$work/out2" \
     intel/iwlwifi/ amdgpu/ 'nvidia/*/gsp/*-570.144' > "$work/log" 2>&1; then
  pass "--require passes when every covered name is installed"
else
  bad "--require failed: $(cat "$work/log")"
fi
printf '%s\n' iwlwifi-so-a0-gf-a0-77.ucode > "$work/kernel-old"
if fw --newest intel/iwlwifi/=2 --require "$work/kernel-old" "$src" "$work/out3" intel/iwlwifi/ > "$work/log" 2>&1; then
  bad "--require passed although the kernel's API was dropped"
else
  pass "--require fails when --newest dropped the kernel's API"
fi
if fw "$src" "$work/out4" amdgpu/ nothing-here/ > "$work/log" 2>&1; then
  bad "a pattern with no file passed"
else
  pass "a pattern with no file fails"
fi

sof="$work/sof-bin"
mkdir -p "$sof/sof/intel-signed" "$sof/sof-ipc4/mtl/intel-signed" "$sof/sof-ipc4-tplg" "$sof/sof-tplg" "$sof/tools"
echo ri > "$sof/sof/intel-signed/sof-tgl.ri"
ln -s intel-signed/sof-tgl.ri "$sof/sof/sof-tgl.ri"
echo ldc > "$sof/sof/sof-tgl.ldc"
echo ri > "$sof/sof-ipc4/mtl/intel-signed/sof-mtl.ri"
echo tplg > "$sof/sof-ipc4-tplg/sof-mtl.tplg"
echo tplg > "$sof/sof-tplg/sof-tgl.tplg"
ln -s sof-ipc4-tplg "$sof/sof-ace-tplg"
if sh "$repo/tools/install-sof.sh" "$sof" "$work/out5" > "$work/log" 2>&1; then
  pass "install-sof succeeds"
else
  bad "install-sof failed: $(cat "$work/log")"
fi
exists "sof file compressed" "$work/out5/intel/sof/intel-signed/sof-tgl.ri.zst"
exists "sof link to the .zst" "$(realpath "$work/out5/intel/sof/sof-tgl.ri.zst")"
exists "sof directory link" "$work/out5/intel/sof-ace-tplg/sof-mtl.tplg.zst"
absent "sof .ldc left out" "$work/out5/intel/sof/sof-tgl.ldc.zst"

exit "$fail"
