# Hadron desktop environment: Sway/Wayland or i3/XLibre.
#
# Builds a complete desktop on top of the Hadron base image. DESKTOP=sway keeps
# the existing Wayland/Sway stack; DESKTOP=i3 selects XLibre and i3. BuildKit
# only evaluates the selected desktop rootfs, so the other graphics stack is
# pruned from both the build graph and the resulting image.
# Everything is built from source against the Hadron musl toolchain.
#
# Build:    docker build -t sway-desktop:dev .
# XLibre:   docker build --build-arg DESKTOP=i3 -t i3-desktop:dev .
# Test:     test/run.sh   (full build -> boot -> assert loop)
#
# Milestone M1: Sway compositor under systemd-logind, rendering on tty1 for an
# ly-authenticated user, with a terminal (foot).

# Hadron release the desktop is built on. The base image and the toolchain MUST
# come from the same release (same musl/GCC ABI), so both derive from this one
# value. scripts/bump-hadron.sh rewrites it here, in Dockerfile.agent and in the
# Makefile; .github/workflows/follow-hadron.yml runs that when Hadron tags a
# new release.
ARG HADRON_VERSION=v0.6.4
ARG BASE_IMAGE=ghcr.io/kairos-io/hadron:${HADRON_VERSION}
# Select the display server + window manager pair. Supported values: sway, i3.
ARG DESKTOP=sway
# GPU=vm (default): software/virtual GL only (llvmpipe, virgl, softpipe, svga).
#   llvmpipe links libLLVM, so the llvm stage below is built (or taken from
#   LLVM_IMAGE), but not the SPIRV stages.
# GPU=full: also hardware GL (iris/radeonsi), which adds the SPIRV stack below.
ARG GPU=vm
# kairos-init image for the bootable layer (final `kairos` stage). kairos-init
# now lives in the kairos-io/kairos monorepo, whose master pipeline promotes
# this tag only after its own image tests pass. Global ARG so it is usable in
# that stage's FROM.
ARG KAIROS_INIT_IMAGE=ghcr.io/kairos-io/kairos/kairos-init:master

FROM ghcr.io/kairos-io/hadron-toolchain:${HADRON_VERSION} AS toolchain

# ---------------------------------------------------------------------------
# Boot splash (splash/). Installed as /usr/bin/kairos-splash: kairos-init leaves
# an existing binary at that path alone and runs it in both halves of the boot
# (its 50kairos-splash dracut module bakes it into the initramfs, and
# kairos-splash.service runs it before the login screen). It draws straight
# into the framebuffer, falling back to a text animation without one.
#
# `make test` checks the password agent protocol (agent.c). The smoke tests
# run the whole program three times: the non-tty fallback, one rendered
# frame and one with the passphrase panel, each a PPM of exactly the
# requested size.
# ---------------------------------------------------------------------------
FROM toolchain AS hadron-splash
COPY splash/ /build/splash/
RUN cd /build/splash && make clean && make && make test && \
    ./hadron-splash | grep -q HADRON && \
    ./hadron-splash --render=/tmp/frame.ppm --size=640x400 --at=2 && \
    [ "$(stat -c %s /tmp/frame.ppm)" -eq $((15 + 640 * 400 * 3)) ] && \
    ./hadron-splash --render=/tmp/prompt.ppm --size=640x400 --at=2 --prompt=retry && \
    [ "$(stat -c %s /tmp/prompt.ppm)" -eq $((15 + 640 * 400 * 3)) ]


FROM toolchain AS wayland
# renovate: datasource=gitlab-tags depName=wayland/wayland registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG WAYLAND_VERSION=1.24.0
# renovate: datasource=gitlab-tags depName=wayland/wayland-protocols registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG WAYLAND_PROTOCOLS_VERSION=1.46
RUN mkdir -p /wayland
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://gitlab.freedesktop.org/wayland/wayland/-/archive/${WAYLAND_VERSION}/wayland-${WAYLAND_VERSION}.tar -o wayland.tar && tar -xf wayland.tar && rm wayland.tar && mv wayland-* wayland-src
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://gitlab.freedesktop.org/wayland/wayland-protocols/-/archive/${WAYLAND_PROTOCOLS_VERSION}/wayland-protocols-${WAYLAND_PROTOCOLS_VERSION}.tar -o wayland-protocols.tar && tar -xf wayland-protocols.tar && rm wayland-protocols.tar && mv wayland-protocols-* wayland-protocols-src
RUN pip3 install meson ninja
WORKDIR /build/wayland-src
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=false -Ddocumentation=false -Dscanner=true -Dlibraries=true
RUN DESTDIR=/wayland ninja -C buildDir install
RUN ninja -C buildDir install
WORKDIR /build/wayland-protocols-src
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=false
RUN DESTDIR=/wayland ninja -C buildDir install


FROM toolchain AS libdrm
# renovate: datasource=gitlab-tags depName=mesa/drm registryUrl=https://gitlab.freedesktop.org extractVersion=^libdrm-(?<version>\d+(\.\d+)*)$
ARG LIBDRM_VERSION=2.4.129
RUN mkdir -p /libdrm
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://dri.freedesktop.org/libdrm/libdrm-${LIBDRM_VERSION}.tar.xz -o libdrm.tar.xz && tar -xf libdrm.tar.xz && rm libdrm.tar.xz && mv libdrm-* libdrm-src
WORKDIR /build/libdrm-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=false
RUN DESTDIR=/libdrm ninja -C buildDir install


# systemd/libudev in the Hadron musl toolchain is linked against libucontext.
# Build it explicitly so every stage using those toolchain libraries can link
# reliably, and copy it into both desktop runtime roots below.
FROM toolchain AS libucontext
# renovate: datasource=github-tags depName=kaniini/libucontext extractVersion=^libucontext-(?<version>\d+(\.\d+)*)$
ARG LIBUCONTEXT_VERSION=1.5.1
RUN mkdir -p /libucontext
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/kaniini/libucontext/archive/refs/tags/libucontext-${LIBUCONTEXT_VERSION}.tar.gz -o libucontext.tar.gz && \
    tar -xf libucontext.tar.gz && mv libucontext-* src
WORKDIR /build/src
RUN make -j"$(nproc)" ARCH=x86_64
RUN make ARCH=x86_64 DESTDIR=/libucontext install


# ===========================================================================
# LLVM / SPIRV stack for Mesa: llvmpipe (every GPU mode) and iris + radeonsi
# (GPU=full).
#
# Kept ENTIRELY inside this example so the main Hadron toolchain Dockerfile is
# never touched: these stages build LLVM/clang/libclc + SPIRV on top of the
# published hadron-toolchain (same musl ABI — no Alpine cross-mix). Every build
# uses the llvm stage, because Mesa's llvmpipe links libLLVM; without it, a VM
# without 3D or any GPU Mesa has no driver for falls back to softpipe, which
# takes seconds per frame. The SPIRV stages are compiled ONLY for GPU=full; the
# `llvm-stack` indirection at the bottom makes the GPU=vm build prune them
# (BuildKit only builds referenced stages). CI and release builds take the llvm
# stage from a prebuilt image (LLVM_IMAGE in the Makefile).
#
# Validated on hadron-toolchain (musl / GCC 15): LLVM 20.1.8 (LLVM 18 fails on
# GCC 15), amdgcn libclc, SPIRV-Tools, and llvm-spirv all compile; Mesa 25.3
# links iris/radeonsi against them.
# ===========================================================================

FROM toolchain AS spirv-tools
# renovate: datasource=github-tags depName=KhronosGroup/SPIRV-Headers versioning=regex:^vulkan-sdk-(?<major>\d+)\.(?<minor>\d+)\.(?<patch>\d+)\.(?<build>\d+)$
ARG SPIRV_HEADERS_VERSION=vulkan-sdk-1.4.309.0
# renovate: datasource=github-tags depName=KhronosGroup/SPIRV-Tools versioning=regex:^vulkan-sdk-(?<major>\d+)\.(?<minor>\d+)\.(?<patch>\d+)\.(?<build>\d+)$
ARG SPIRV_TOOLS_VERSION=vulkan-sdk-1.4.309.0
RUN pip3 install ninja
WORKDIR /b
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://github.com/KhronosGroup/SPIRV-Headers/archive/refs/tags/${SPIRV_HEADERS_VERSION}.tar.gz -o sh.tgz && \
    curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://github.com/KhronosGroup/SPIRV-Tools/archive/refs/tags/${SPIRV_TOOLS_VERSION}.tar.gz -o st.tgz && \
    tar -xf sh.tgz && mv SPIRV-Headers-* spirv-headers && \
    tar -xf st.tgz && mv SPIRV-Tools-* spirv-tools && \
    cp -a spirv-headers spirv-tools/external/spirv-headers
RUN cd spirv-headers && cmake -G Ninja -B build -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib && \
    DESTDIR=/spirv-tools ninja -C build install
RUN cd spirv-tools && cmake -G Ninja -B build -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib \
      -DSPIRV_SKIP_TESTS=ON -DBUILD_SHARED_LIBS=ON \
      -DSPIRV-Headers_SOURCE_DIR=/b/spirv-tools/external/spirv-headers && \
    ninja -C build -j$(nproc) && DESTDIR=/spirv-tools ninja -C build install

# LLVM + clang + amdgcn libclc. LTO off (brutal for LLVM); shared libLLVM dylib;
# in-tree SPIRV backend kept so clang can emit SPIR-V. The spirv libclc targets
# need llvm-spirv at configure time (circular dep on LLVM), so libclc builds the
# amdgcn targets only — enough for radeonsi + Mesa's libclc dependency.
FROM toolchain AS llvm
# renovate: datasource=github-tags depName=llvm/llvm-project extractVersion=^llvmorg-(?<version>\d+(\.\d+)*)$
ARG LLVM_VERSION=20.1.8
RUN pip3 install ninja
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://github.com/llvm/llvm-project/releases/download/llvmorg-${LLVM_VERSION}/llvm-project-${LLVM_VERSION}.src.tar.xz -o llvm.tar.xz && \
    tar -xf llvm.tar.xz && rm llvm.tar.xz && mv llvm-project-* llvm-project
WORKDIR /build/llvm-project
RUN cmake -G Ninja -S llvm -B build \
      -DCMAKE_BUILD_TYPE=Release -DCMAKE_C_FLAGS="-O2 -pipe" -DCMAKE_CXX_FLAGS="-O2 -pipe" \
      -DCMAKE_INSTALL_PREFIX=/usr -DLLVM_LIBDIR_SUFFIX="" \
      -DLLVM_TARGETS_TO_BUILD="X86;AMDGPU;SPIRV" \
      -DLLVM_ENABLE_PROJECTS="clang;libclc" \
      -DLIBCLC_TARGETS_TO_BUILD="amdgcn--;amdgcn--amdhsa" \
      -DLLVM_BUILD_LLVM_DYLIB=ON -DLLVM_LINK_LLVM_DYLIB=ON \
      -DLLVM_ENABLE_ZSTD=OFF -DLLVM_ENABLE_LIBXML2=OFF -DLLVM_ENABLE_TERMINFO=OFF \
      -DLLVM_INCLUDE_TESTS=OFF -DLLVM_INCLUDE_EXAMPLES=OFF -DLLVM_INCLUDE_BENCHMARKS=OFF \
      -DCLANG_ENABLE_STATIC_ANALYZER=OFF -DCLANG_ENABLE_ARCMT=OFF
RUN ninja -C build -j$(nproc) && DESTDIR=/llvm ninja -C build install

# Only the /llvm install tree, for CI: release.yml builds this target once per
# LLVM key (`make print-llvm-tag`) and pushes it, and later builds swap it in
# for the llvm stage (`make LLVM_IMAGE=...`), which skips the 3-hour compile.
FROM scratch AS llvm-dist
COPY --from=llvm /llvm /llvm

# SPIRV-LLVM-Translator (llvm-spirv). Its cmake otherwise git-clones SPIRV-Headers
# at a pinned commit; with no git/network in the build, feed it a local checkout
# at exactly that commit (its sources use newer SPIR-V caps than the spirv-tools
# headers tag).
FROM toolchain AS spirv-llvm-translator
# renovate: datasource=github-tags depName=KhronosGroup/SPIRV-LLVM-Translator extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG SPIRV_LLVM_TRANSLATOR_VERSION=20.1.3
ARG SPIRV_HEADERS_TRANSLATOR_COMMIT=0e710677989b4326ac974fd80c5308191ed80965
RUN pip3 install ninja
COPY --from=llvm /llvm /llvm
RUN rsync -aHAX --keep-dirlinks /llvm/. /
WORKDIR /b
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://github.com/KhronosGroup/SPIRV-Headers/archive/${SPIRV_HEADERS_TRANSLATOR_COMMIT}.tar.gz -o sh.tgz && \
    tar -xf sh.tgz && mv SPIRV-Headers-* spirv-headers && \
    curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://github.com/KhronosGroup/SPIRV-LLVM-Translator/archive/refs/tags/v${SPIRV_LLVM_TRANSLATOR_VERSION}.tar.gz -o tr.tgz && \
    tar -xf tr.tgz && mv SPIRV-LLVM-Translator-* tr
RUN cd tr && cmake -G Ninja -B build -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib \
      -DLLVM_DIR=/usr/lib/cmake/llvm -DBUILD_SHARED_LIBS=OFF \
      -DLLVM_EXTERNAL_SPIRV_HEADERS_SOURCE_DIR=/b/spirv-headers && \
    ninja -C build -j$(nproc) && DESTDIR=/translator ninja -C build install

# Merge the three installs into one /llvm-out tree (GPU=full path).
FROM toolchain AS llvm-stack-full
COPY --from=spirv-tools /spirv-tools /s
COPY --from=llvm /llvm /l
COPY --from=spirv-llvm-translator /translator /t
RUN mkdir -p /llvm-out && \
    rsync -aHAX /l/. /llvm-out && \
    rsync -aHAX /s/. /llvm-out && \
    rsync -aHAX /t/. /llvm-out

# LLVM only (GPU=vm path): what llvmpipe needs. Selecting this prunes the
# SPIRV-Tools and llvm-spirv stages above.
FROM toolchain AS llvm-stack-vm
COPY --from=llvm /llvm /llvm-out

# Pick the stack by GPU flag. For vm this resolves to the LLVM-only stage, so
# the SPIRV stages are never referenced and BuildKit skips them.
FROM llvm-stack-${GPU} AS llvm-stack


FROM toolchain AS mesa
COPY --from=libdrm /libdrm /
# LLVM stack: LLVM for GPU=vm, LLVM + libclc + SPIRV for GPU=full.
COPY --from=llvm-stack /llvm-out /llvm-out
RUN rsync -aHAX --keep-dirlinks /llvm-out/. / && rm -rf /llvm-out
COPY --from=wayland /wayland /
# renovate: datasource=gitlab-tags depName=mesa/mesa registryUrl=https://gitlab.freedesktop.org extractVersion=^mesa-(?<version>\d+(\.\d+)*)$
ARG MESA_VERSION=25.3.0
RUN mkdir -p /mesa
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://archive.mesa3d.org/mesa-${MESA_VERSION}.tar.xz -o mesa.tar.xz && tar -xf mesa.tar.xz && rm mesa.tar.xz && mv mesa-* mesa-src
WORKDIR /build/mesa-src
RUN pip3 install meson ninja setuptools mako pyyaml
# Every mode builds llvmpipe, the software rasterizer Mesa picks for any
# display it has no GPU driver for: a VM without 3D (virtio-gpu, QXL, -vga
# std) or, with GPU=vm, real hardware. softpipe stays as a fallback that
# GALLIUM_DRIVER=softpipe selects, for when llvmpipe itself is suspect.
# GPU=vm (default): software/virtual GL only (llvmpipe/virgl/softpipe/svga).
# GPU=full: also real hardware GL — iris (Intel) + radeonsi (AMD), linked
#   against the LLVM/libclc/SPIRV stack built by the stages above (rsync'd into
#   /usr). Pair with --build-arg FIRMWARE=true so the i915/amdgpu blobs are
#   present.
ARG GPU=vm
# The example's libLLVM is built without RTTI, so Mesa must disable it too
# (cpp_rtti=false).
RUN if [ "$GPU" = "full" ]; then \
      DRV="iris,radeonsi,llvmpipe,virgl,softpipe,svga"; \
    else \
      DRV="llvmpipe,virgl,softpipe,svga"; \
    fi; \
    meson setup buildDir ${COMMON_MESON_FLAGS} -Dplatforms=wayland \
    -Dgallium-drivers="$DRV" \
    -Dglx=disabled \
    -Dopengl=true \
    -Dgles1=enabled \
    -Dgles2=enabled \
    -Degl=enabled \
    -Dvulkan-drivers= \
    -Dllvm=enabled \
    -Dcpp_rtti=false \
    -Dbuild-tests=false
RUN DESTDIR=/mesa ninja -C buildDir install
# The megadriver links libLLVM at runtime (llvmpipe in every mode); with
# GPU=full it also links libelf (radeonsi) and the shared SPIRV-Tools
# libraries (iris). They come from the LLVM stack and the toolchain and aren't
# otherwise in the OS image, so bundle them: only the shared libraries, not
# the LLVM install (libLLVM is ~125MB, the cost of llvmpipe).
RUN mkdir -p /mesa/usr/lib && \
    cp -a /usr/lib/libLLVM.so* /mesa/usr/lib/ && \
    if [ "$GPU" = "full" ]; then \
      cp -a /usr/lib/libelf.so* /usr/lib/libSPIRV-Tools*.so* /mesa/usr/lib/ ; \
    fi


FROM toolchain AS xkeyboard-config
# renovate: datasource=gitlab-tags depName=xkeyboard-config/xkeyboard-config registryUrl=https://gitlab.freedesktop.org extractVersion=^xkeyboard-config-(?<version>\d+(\.\d+)*)$
ARG XKEYBOARD_CONFIG_VERSION=2.44
RUN mkdir -p /xkeyboard-config
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://gitlab.freedesktop.org/xkeyboard-config/xkeyboard-config/-/archive/xkeyboard-config-${XKEYBOARD_CONFIG_VERSION}/xkeyboard-config-xkeyboard-config-${XKEYBOARD_CONFIG_VERSION}.tar.gz -o xkeyboard-config.tar.gz && tar -xf xkeyboard-config.tar.gz && rm xkeyboard-config.tar.gz && mv xkeyboard-config-xkeyboard-config-* xkeyboard-config-src
WORKDIR /build/xkeyboard-config-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS}
RUN DESTDIR=/xkeyboard-config ninja -C buildDir install


FROM toolchain AS libxkb
COPY --from=wayland /wayland /
COPY --from=xkeyboard-config /xkeyboard-config /
# renovate: datasource=github-tags depName=xkbcommon/libxkbcommon extractVersion=^xkbcommon-(?<version>\d+(\.\d+)*)$
ARG LIBXKBCOMMON_VERSION=1.13.0
RUN mkdir -p /libxkb
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/xkbcommon/libxkbcommon/archive/refs/tags/xkbcommon-${LIBXKBCOMMON_VERSION}.tar.gz -o libxkbcommon.tar.gz && tar -xzf libxkbcommon.tar.gz && rm libxkbcommon.tar.gz && mv libxkbcommon-* libxkbcommon-src
WORKDIR /build/libxkbcommon-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Denable-x11=false -Denable-xkbregistry=true -Denable-bash-completion=false
RUN DESTDIR=/libxkb ninja -C buildDir install


FROM toolchain AS pixman
# renovate: datasource=gitlab-tags depName=pixman/pixman registryUrl=https://gitlab.freedesktop.org extractVersion=^pixman-(?<version>\d+(\.\d+)*)$
ARG PIXMAN_VERSION=0.46.0
RUN mkdir -p /pixman
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://www.cairographics.org/releases/pixman-${PIXMAN_VERSION}.tar.gz -o pixman.tar.gz && tar -xzf pixman.tar.gz && rm pixman.tar.gz && mv pixman-* pixman-src
WORKDIR /build/pixman-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS}
RUN DESTDIR=/pixman ninja -C buildDir install


FROM toolchain AS libevdev
# renovate: datasource=gitlab-tags depName=libevdev/libevdev registryUrl=https://gitlab.freedesktop.org extractVersion=^libevdev-(?<version>\d+(\.\d+)*)$
ARG LIBEVDEV_VERSION=1.13.5
RUN mkdir -p /libevdev
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://gitlab.freedesktop.org/libevdev/libevdev/-/archive/libevdev-${LIBEVDEV_VERSION}/libevdev-libevdev-${LIBEVDEV_VERSION}.tar.gz -o libevdev.tar.gz && tar -xzf libevdev.tar.gz && rm libevdev.tar.gz && mv libevdev-* libevdev-src
WORKDIR /build/libevdev-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=disabled -Ddocumentation=disabled -Dtools=disabled
RUN DESTDIR=/libevdev ninja -C buildDir install


FROM toolchain AS libinput
COPY --from=libucontext /libucontext /
COPY --from=libevdev /libevdev /
# renovate: datasource=gitlab-tags depName=libinput/libinput registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG LIBINPUT_VERSION=1.30.0
RUN mkdir -p /libinput
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://gitlab.freedesktop.org/libinput/libinput/-/archive/${LIBINPUT_VERSION}/libinput-${LIBINPUT_VERSION}.tar.gz -o libinput.tar.gz && tar -xf libinput.tar.gz && rm libinput.tar.gz && mv libinput-* libinput-src
WORKDIR /build/libinput-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dmtdev=false -Dlibwacom=false -Dtests=false -Ddebug-gui=false
RUN DESTDIR=/libinput ninja -C buildDir install


FROM toolchain AS hwdata
# renovate: datasource=github-tags depName=vcrhonek/hwdata extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG HWDATA_VERSION=0.401
RUN mkdir -p /hwdata
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/vcrhonek/hwdata/archive/v${HWDATA_VERSION}/hwdata-${HWDATA_VERSION}.tar.gz -o hwdata.tar.xz && tar -xf hwdata.tar.xz && rm hwdata.tar.xz && mv hwdata-* hwdata-src
WORKDIR /build/hwdata-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking
RUN make -j$(nproc) && make install DESTDIR=/hwdata


FROM toolchain AS libdisplay-info
COPY --from=hwdata /hwdata /
# renovate: datasource=gitlab-tags depName=emersion/libdisplay-info registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG LIBDISPLAY_INFO_VERSION=0.3.0
RUN mkdir -p /libdisplay-info
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://gitlab.freedesktop.org/emersion/libdisplay-info/-/releases/${LIBDISPLAY_INFO_VERSION}/downloads/libdisplay-info-${LIBDISPLAY_INFO_VERSION}.tar.xz -o libdisplay-info.tar.xz && tar -xf libdisplay-info.tar.xz && rm libdisplay-info.tar.xz && mv libdisplay-info-* libdisplay-info-src
WORKDIR /build/libdisplay-info-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS}
RUN DESTDIR=/libdisplay-info ninja -C buildDir install


# ===========================================================================
# Shared desktop libraries
# ===========================================================================

# PCRE2 — required by glib
FROM toolchain AS pcre2
# renovate: datasource=github-tags depName=PCRE2Project/pcre2 extractVersion=^pcre2-(?<version>\d+(\.\d+)*)$
ARG PCRE2_VERSION=10.44
RUN mkdir -p /pcre2
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/PCRE2Project/pcre2/releases/download/pcre2-${PCRE2_VERSION}/pcre2-${PCRE2_VERSION}.tar.gz -o pcre2.tar.gz && tar -xzf pcre2.tar.gz && rm pcre2.tar.gz && mv pcre2-* pcre2-src
WORKDIR /build/pcre2-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking --enable-pcre2-16 --enable-pcre2-32
RUN make -j$(nproc) && make install DESTDIR=/pcre2


# libpng — needed by grim (PNG screenshots)
FROM toolchain AS libpng
# renovate: datasource=github-tags depName=pnggroup/libpng extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG LIBPNG_VERSION=1.6.44
RUN mkdir -p /libpng
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://download.sourceforge.net/libpng/libpng-${LIBPNG_VERSION}.tar.xz -o libpng.tar.xz && tar -xf libpng.tar.xz && rm libpng.tar.xz && mv libpng-* libpng-src
WORKDIR /build/libpng-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking
# The Hadron cross-style target tuple causes libpng's generated DFA feature
# header to disable every read/write API. Use libpng's shipped, standard
# feature profile so Cairo, grim, and gdk-pixbuf get the normal PNG surface.
RUN sed -i 's/^pnglibconf\.h: pnglibconf\.out$/pnglibconf.h:/' Makefile && \
    cp scripts/pnglibconf.h.prebuilt pnglibconf.h && \
    make -j$(nproc) && make install DESTDIR=/libpng


# GLib — libffi/expat/zlib already provided by the toolchain
FROM toolchain AS glib2
COPY --from=pcre2 /pcre2 /
# renovate: datasource=gitlab-tags depName=GNOME/glib registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG GLIB_VERSION=2.82.5
RUN mkdir -p /glib2
WORKDIR /build
RUN GLIB_MAJOR="${GLIB_VERSION%.*}" && curl --retry 5 --retry-delay 3 --retry-all-errors -L https://download.gnome.org/sources/glib/${GLIB_MAJOR}/glib-${GLIB_VERSION}.tar.xz -o glib.tar.xz && tar -xf glib.tar.xz && rm glib.tar.xz && mv glib-* glib-src
WORKDIR /build/glib-src
RUN pip3 install meson ninja packaging
RUN meson setup buildDir ${COMMON_MESON_FLAGS} \
    -Dnls=disabled -Dtests=false -Dlibmount=disabled -Dselinux=disabled \
    -Dman-pages=disabled -Dintrospection=disabled -Dglib_debug=disabled \
    -Ddtrace=disabled -Dsysprof=disabled
RUN DESTDIR=/glib2 ninja -C buildDir install


# FreeType — built without harfbuzz (broken circular dep) and brotli. With
# libpng: Noto Color Emoji (the emoji stage) stores its glyphs as PNG images
# (CBDT), and a FreeType without PNG support cannot load them, so every app
# that draws text through FreeType (GTK, Pango, foot, fuzzel, dmenu, dunst)
# would show nothing for an emoji. libfreetype then links libpng16, so every
# stage that copies /freetype copies /libpng too (freetype2.pc names it), and
# every image already ships libpng.
FROM toolchain AS freetype
COPY --from=libpng /libpng /
RUN mkdir -p /freetype
WORKDIR /build
ARG FREETYPE_VERSION=2.13.3
ARG FREETYPE_SHA256=0550350666d427c74daeb85d5ac7bb353acba5f76956395995311a9c6f063289
# `-f` so a Savannah 5xx is not written to disk as the tarball: bare `curl -L`
# saved the HTML error page and the failure only surfaced later as the
# unhelpful `tar: invalid tar magic`. Savannah stays first as the canonical
# host but has returned 502 for days at a stretch, so SourceForge -- FreeType's
# other upstream distribution point -- is the fallback. The pinned checksum
# means neither host has to be trusted, matching how npth and sqlite are
# fetched in this Dockerfile.
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors \
      https://download.savannah.gnu.org/releases/freetype/freetype-${FREETYPE_VERSION}.tar.xz \
      -o freetype.tar.xz \
    || curl -fL --retry 5 --retry-delay 3 --retry-all-errors \
      https://downloads.sourceforge.net/freetype/freetype-${FREETYPE_VERSION}.tar.xz \
      -o freetype.tar.xz && \
    echo "${FREETYPE_SHA256}  freetype.tar.xz" | sha256sum -c - && \
    tar -xf freetype.tar.xz && rm freetype.tar.xz && mv freetype-* freetype-src
WORKDIR /build/freetype-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking --with-harfbuzz=no --with-png=yes --with-brotli=no --with-bzip2=no --with-zlib=yes
RUN make -j$(nproc) && make install DESTDIR=/freetype


# HarfBuzz — needs freetype + glib
FROM toolchain AS harfbuzz
COPY --from=freetype /freetype /
COPY --from=libpng /libpng /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
# renovate: datasource=github-tags depName=harfbuzz/harfbuzz extractVersion=^(?<version>\d+(\.\d+)*)$
ARG HARFBUZZ_VERSION=10.1.0
RUN mkdir -p /harfbuzz
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/harfbuzz/harfbuzz/releases/download/${HARFBUZZ_VERSION}/harfbuzz-${HARFBUZZ_VERSION}.tar.xz -o harfbuzz.tar.xz && tar -xf harfbuzz.tar.xz && rm harfbuzz.tar.xz && mv harfbuzz-* harfbuzz-src
WORKDIR /build/harfbuzz-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=disabled -Ddocs=disabled -Dintrospection=disabled -Dglib=enabled -Dfreetype=enabled -Dcairo=disabled -Dchafa=disabled
RUN DESTDIR=/harfbuzz ninja -C buildDir install


# FriBidi — bidirectional text, needed by pango
FROM toolchain AS fribidi
# renovate: datasource=github-tags depName=fribidi/fribidi extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG FRIBIDI_VERSION=1.0.16
RUN mkdir -p /fribidi
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/fribidi/fribidi/releases/download/v${FRIBIDI_VERSION}/fribidi-${FRIBIDI_VERSION}.tar.xz -o fribidi.tar.xz && tar -xf fribidi.tar.xz && rm fribidi.tar.xz && mv fribidi-* fribidi-src
WORKDIR /build/fribidi-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=false -Ddocs=false -Dbin=false
RUN DESTDIR=/fribidi ninja -C buildDir install


# gperf — build-time tool required by fontconfig
FROM toolchain AS gperf
ARG GPERF_VERSION=3.1
RUN mkdir -p /gperf
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://ftp.gnu.org/pub/gnu/gperf/gperf-${GPERF_VERSION}.tar.gz -o gperf.tar.gz && tar -xzf gperf.tar.gz && rm gperf.tar.gz && mv gperf-* gperf-src
WORKDIR /build/gperf-src
# gperf 3.1 ships an ancient K&R getopt.c; the toolchain GCC defaults to C23
# where `()` means `(void)`, so force the older language semantics.
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking
RUN make -j$(nproc) CFLAGS="-std=gnu17 -O2 -g" && make install DESTDIR=/gperf


# Fontconfig (+ DejaVu fonts) — expat from toolchain, freetype built above
FROM toolchain AS fontconfig
COPY --from=freetype /freetype /
COPY --from=libpng /libpng /
COPY --from=gperf /gperf /
# renovate: datasource=gitlab-tags depName=fontconfig/fontconfig registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG FONTCONFIG_VERSION=2.15.0
RUN mkdir -p /fontconfig
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://www.freedesktop.org/software/fontconfig/release/fontconfig-${FONTCONFIG_VERSION}.tar.xz -o fontconfig.tar.xz && tar -xf fontconfig.tar.xz && rm fontconfig.tar.xz && mv fontconfig-* fontconfig-src
WORKDIR /build/fontconfig-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=disabled -Dtools=disabled -Ddoc=disabled -Dnls=disabled -Dcache-build=disabled
RUN DESTDIR=/fontconfig ninja -C buildDir install
# Bundle DejaVu fonts
ARG DEJAVU_VERSION=2.37
RUN mkdir -p /fontconfig/usr/share/fonts/dejavu
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/dejavu-fonts/dejavu-fonts/releases/download/version_2_37/dejavu-fonts-ttf-${DEJAVU_VERSION}.tar.bz2 -o dejavu.tar.bz2 && \
    tar -xjf dejavu.tar.bz2 && \
    cp dejavu-fonts-ttf-*/ttf/*.ttf /fontconfig/usr/share/fonts/dejavu/ && \
    rm -rf dejavu.tar.bz2 dejavu-fonts-ttf-*


# Cairo — built with the FreeType + fontconfig backends (cairo-ft), required by
# pango. Defined here so freetype/fontconfig stages precede it.
FROM toolchain AS cairo
COPY --from=pixman /pixman /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pcre2 /pcre2 /
COPY --from=glib2 /glib2 /
# 1.18.4 fixes the COLRv1 / FreeType 2.13.x cairo-ft detection bug.
# renovate: datasource=gitlab-tags depName=cairo/cairo registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG CAIRO_VERSION=1.18.4
RUN mkdir -p /cairo
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://cairographics.org/releases/cairo-${CAIRO_VERSION}.tar.xz -o cairo.tar.xz && tar -xf cairo.tar.xz && rm cairo.tar.xz && mv cairo-* cairo-src
WORKDIR /build/cairo-src
RUN pip3 install meson ninja
# PNG enabled so cairo_image_surface_create_from_png is available (swaybar/swaybg).
# glib enabled so cairo-gobject is built (required by GTK3 / waybar).
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=disabled -Dglib=enabled -Dspectre=disabled \
    -Dfreetype=enabled -Dfontconfig=enabled -Dpng=enabled -Dxlib=disabled -Dxcb=disabled
RUN DESTDIR=/cairo ninja -C buildDir install


# Pango
FROM toolchain AS pango
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=harfbuzz /harfbuzz /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=fribidi /fribidi /
COPY --from=cairo /cairo /
COPY --from=pixman /pixman /
COPY --from=libpng /libpng /
# renovate: datasource=gitlab-tags depName=GNOME/pango registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG PANGO_VERSION=1.54.0
RUN mkdir -p /pango
WORKDIR /build
RUN PANGO_MAJOR="${PANGO_VERSION%.*}" && curl --retry 5 --retry-delay 3 --retry-all-errors -L https://download.gnome.org/sources/pango/${PANGO_MAJOR}/pango-${PANGO_VERSION}.tar.xz -o pango.tar.xz && tar -xf pango.tar.xz && rm pango.tar.xz && mv pango-* pango-src
WORKDIR /build/pango-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dintrospection=disabled -Dgtk_doc=false -Dfontconfig=enabled -Dfreetype=enabled -Dcairo=enabled
RUN DESTDIR=/pango ninja -C buildDir install


# ===========================================================================
# GTK3 stack (for waybar). Wayland-only, NO GObject introspection (keeps it
# tractable on musl). Network/audio/bluetooth are handled by waybar's native
# modules + fuzzel menus over nmcli/wpctl/bluetoothctl — no nm-applet/pavucontrol.
# ===========================================================================

# libepoxy — GL/EGL dispatch used by GTK's GL renderer.
FROM toolchain AS libepoxy
COPY --from=mesa /mesa /
# renovate: datasource=github-tags depName=anholt/libepoxy extractVersion=^(?<version>\d+(\.\d+)*)$
ARG LIBEPOXY_VERSION=1.5.10
RUN mkdir -p /libepoxy
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://github.com/anholt/libepoxy/archive/refs/tags/${LIBEPOXY_VERSION}.tar.gz -o epoxy.tar.gz && tar -xf epoxy.tar.gz && rm epoxy.tar.gz && mv libepoxy-* epoxy-src
WORKDIR /build/epoxy-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dglx=no -Dx11=false -Degl=yes -Dtests=false
RUN DESTDIR=/libepoxy ninja -C buildDir install

# gdk-pixbuf — image loading (PNG only is plenty for the bar/applets).
FROM toolchain AS gdk-pixbuf
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=libpng /libpng /
# renovate: datasource=gitlab-tags depName=GNOME/gdk-pixbuf registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG GDKPIXBUF_VERSION=2.42.12
RUN mkdir -p /gdk-pixbuf
WORKDIR /build
RUN GP_MAJOR="${GDKPIXBUF_VERSION%.*}" && curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://download.gnome.org/sources/gdk-pixbuf/${GP_MAJOR}/gdk-pixbuf-${GDKPIXBUF_VERSION}.tar.xz -o gp.tar.xz && tar -xf gp.tar.xz && rm gp.tar.xz && mv gdk-pixbuf-* gp-src
WORKDIR /build/gp-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtiff=disabled -Djpeg=disabled -Dothers=disabled \
    -Dintrospection=disabled -Dman=false -Ddocs=false -Dinstalled_tests=false -Dgio_sniffing=false
RUN DESTDIR=/gdk-pixbuf ninja -C buildDir install

# atk — accessibility toolkit (GTK links it; the at-spi2 bridge stays runtime-optional).
FROM toolchain AS atk
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
ARG ATK_VERSION=2.38.0
RUN mkdir -p /atk
WORKDIR /build
RUN ATK_MAJOR="${ATK_VERSION%.*}" && curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://download.gnome.org/sources/atk/${ATK_MAJOR}/atk-${ATK_VERSION}.tar.xz -o atk.tar.xz && tar -xf atk.tar.xz && rm atk.tar.xz && mv atk-* atk-src
WORKDIR /build/atk-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dintrospection=false
RUN DESTDIR=/atk ninja -C buildDir install

# GTK3 itself — wayland-only.
FROM toolchain AS gtk3
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=cairo /cairo /
COPY --from=pango /pango /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pixman /pixman /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=atk /atk /
COPY --from=libepoxy /libepoxy /
COPY --from=wayland /wayland /
COPY --from=libxkb /libxkb /
COPY --from=mesa /mesa /
# renovate: datasource=gitlab-tags depName=GNOME/gtk registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG GTK3_VERSION=3.24.43
RUN mkdir -p /gtk3
WORKDIR /build
RUN GTK_MAJOR="${GTK3_VERSION%.*}" && curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://download.gnome.org/sources/gtk+/${GTK_MAJOR}/gtk+-${GTK3_VERSION}.tar.xz -o gtk.tar.xz && tar -xf gtk.tar.xz && rm gtk.tar.xz && mv gtk+-* gtk-src
WORKDIR /build/gtk-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} \
    -Dx11_backend=false -Dwayland_backend=true -Dbroadway_backend=false \
    -Dintrospection=false -Dgtk_doc=false -Dman=false -Ddemos=false -Dexamples=false -Dtests=false \
    -Dprint_backends=file -Dcolord=no
RUN DESTDIR=/gtk3 ninja -C buildDir install

# --- gtkmm3 C++ wrappers (waybar links these) ------------------------------
FROM toolchain AS libsigcpp
# renovate: datasource=github-tags depName=libsigcplusplus/libsigcplusplus extractVersion=^(?<version>\d+(\.\d+)*)$
ARG LIBSIGCPP_VERSION=2.12.1
RUN mkdir -p /libsigcpp
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://download.gnome.org/sources/libsigc++/${LIBSIGCPP_VERSION%.*}/libsigc++-${LIBSIGCPP_VERSION}.tar.xz -o s.tar.xz && tar -xf s.tar.xz && rm s.tar.xz && mv libsigc++-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dbuild-documentation=false -Dbuild-examples=false
RUN DESTDIR=/libsigcpp ninja -C buildDir install

FROM toolchain AS glibmm
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=libsigcpp /libsigcpp /
# renovate: datasource=gitlab-tags depName=GNOME/glibmm registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG GLIBMM_VERSION=2.66.7
RUN mkdir -p /glibmm
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://download.gnome.org/sources/glibmm/${GLIBMM_VERSION%.*}/glibmm-${GLIBMM_VERSION}.tar.xz -o g.tar.xz && tar -xf g.tar.xz && rm g.tar.xz && mv glibmm-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dbuild-documentation=false
RUN DESTDIR=/glibmm ninja -C buildDir install

FROM toolchain AS cairomm
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=cairo /cairo /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pixman /pixman /
COPY --from=libsigcpp /libsigcpp /
# renovate: datasource=gitlab-tags depName=cairo/cairomm registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG CAIROMM_VERSION=1.14.5
RUN mkdir -p /cairomm
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://gitlab.freedesktop.org/cairo/cairomm/-/archive/${CAIROMM_VERSION}/cairomm-${CAIROMM_VERSION}.tar.gz -o c.tar.gz && tar -xf c.tar.gz && rm c.tar.gz && mv cairomm-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dbuild-documentation=false -Dbuild-tests=false
RUN DESTDIR=/cairomm ninja -C buildDir install

FROM toolchain AS pangomm
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=cairo /cairo /
COPY --from=pango /pango /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pixman /pixman /
COPY --from=libsigcpp /libsigcpp /
COPY --from=glibmm /glibmm /
COPY --from=cairomm /cairomm /
# renovate: datasource=gitlab-tags depName=GNOME/pangomm registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG PANGOMM_VERSION=2.46.4
RUN mkdir -p /pangomm
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://download.gnome.org/sources/pangomm/${PANGOMM_VERSION%.*}/pangomm-${PANGOMM_VERSION}.tar.xz -o p.tar.xz && tar -xf p.tar.xz && rm p.tar.xz && mv pangomm-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dbuild-documentation=false
RUN DESTDIR=/pangomm ninja -C buildDir install

FROM toolchain AS atkmm
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=atk /atk /
COPY --from=libsigcpp /libsigcpp /
COPY --from=glibmm /glibmm /
# renovate: datasource=gitlab-tags depName=GNOME/atkmm registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG ATKMM_VERSION=2.28.4
RUN mkdir -p /atkmm
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://download.gnome.org/sources/atkmm/${ATKMM_VERSION%.*}/atkmm-${ATKMM_VERSION}.tar.xz -o a.tar.xz && tar -xf a.tar.xz && rm a.tar.xz && mv atkmm-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dbuild-documentation=false
RUN DESTDIR=/atkmm ninja -C buildDir install

FROM toolchain AS gtkmm3
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=cairo /cairo /
COPY --from=pango /pango /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pixman /pixman /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=atk /atk /
COPY --from=libepoxy /libepoxy /
COPY --from=wayland /wayland /
COPY --from=libxkb /libxkb /
COPY --from=mesa /mesa /
COPY --from=gtk3 /gtk3 /
COPY --from=libsigcpp /libsigcpp /
COPY --from=glibmm /glibmm /
COPY --from=cairomm /cairomm /
COPY --from=pangomm /pangomm /
COPY --from=atkmm /atkmm /
# renovate: datasource=gitlab-tags depName=GNOME/gtkmm registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG GTKMM_VERSION=3.24.9
RUN mkdir -p /gtkmm3
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://download.gnome.org/sources/gtkmm/${GTKMM_VERSION%.*}/gtkmm-${GTKMM_VERSION}.tar.xz -o gm.tar.xz && tar -xf gm.tar.xz && rm gm.tar.xz && mv gtkmm-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dbuild-documentation=false -Dbuild-demos=false -Dbuild-tests=false
RUN DESTDIR=/gtkmm3 ninja -C buildDir install

# --- waybar support libraries ----------------------------------------------
FROM toolchain AS gtk-layer-shell
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=cairo /cairo /
COPY --from=pango /pango /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pixman /pixman /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=atk /atk /
COPY --from=libepoxy /libepoxy /
COPY --from=wayland /wayland /
COPY --from=libxkb /libxkb /
COPY --from=mesa /mesa /
COPY --from=gtk3 /gtk3 /
# renovate: datasource=github-tags depName=wmww/gtk-layer-shell extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG GTKLAYERSHELL_VERSION=0.8.2
RUN mkdir -p /gtk-layer-shell
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/wmww/gtk-layer-shell/archive/refs/tags/v${GTKLAYERSHELL_VERSION}.tar.gz -o g.tar.gz && tar -xf g.tar.gz && rm g.tar.gz && mv gtk-layer-shell-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dintrospection=false -Dvapi=false -Dexamples=false -Ddocs=false -Dtests=false
RUN DESTDIR=/gtk-layer-shell ninja -C buildDir install

FROM toolchain AS jsoncpp
# renovate: datasource=github-tags depName=open-source-parsers/jsoncpp extractVersion=^(?<version>\d+(\.\d+)*)$
ARG JSONCPP_VERSION=1.9.6
RUN mkdir -p /jsoncpp
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/open-source-parsers/jsoncpp/archive/refs/tags/${JSONCPP_VERSION}.tar.gz -o j.tar.gz && tar -xf j.tar.gz && rm j.tar.gz && mv jsoncpp-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=false
RUN DESTDIR=/jsoncpp ninja -C buildDir install

FROM toolchain AS libfmt
# renovate: datasource=github-tags depName=fmtlib/fmt extractVersion=^(?<version>\d+(\.\d+)*)$
ARG FMT_VERSION=10.2.1
RUN mkdir -p /libfmt
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/fmtlib/fmt/archive/refs/tags/${FMT_VERSION}.tar.gz -o f.tar.gz && tar -xf f.tar.gz && rm f.tar.gz && mv fmt-* src
WORKDIR /build/src
RUN cmake -B buildDir -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_BUILD_TYPE=MinSizeRel -DBUILD_SHARED_LIBS=ON -DFMT_TEST=OFF -DFMT_DOC=OFF
RUN cmake --build buildDir -j"$(nproc)" && DESTDIR=/libfmt cmake --install buildDir

FROM toolchain AS spdlog
COPY --from=libfmt /libfmt /
# renovate: datasource=github-tags depName=gabime/spdlog extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG SPDLOG_VERSION=1.13.0
RUN mkdir -p /spdlog
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/gabime/spdlog/archive/refs/tags/v${SPDLOG_VERSION}.tar.gz -o s.tar.gz && tar -xf s.tar.gz && rm s.tar.gz && mv spdlog-* src
WORKDIR /build/src
RUN cmake -B buildDir -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_BUILD_TYPE=MinSizeRel -DBUILD_SHARED_LIBS=ON -DSPDLOG_FMT_EXTERNAL=ON -DSPDLOG_BUILD_EXAMPLE=OFF -DSPDLOG_BUILD_TESTS=OFF
RUN cmake --build buildDir -j"$(nproc)" && DESTDIR=/spdlog cmake --install buildDir

# Howard Hinnant date — waybar's clock module timezone support (header + tz lib).
# Provided as a system dep so waybar's meson doesn't reach out to wrapdb.
FROM toolchain AS cpp-date
# renovate: datasource=github-tags depName=HowardHinnant/date extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG DATE_VERSION=3.0.1
RUN mkdir -p /cpp-date
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/HowardHinnant/date/archive/refs/tags/v${DATE_VERSION}.tar.gz -o d.tar.gz && tar -xf d.tar.gz && rm d.tar.gz && mv date-* src
WORKDIR /build/src
RUN cmake -B buildDir -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_BUILD_TYPE=MinSizeRel -DBUILD_SHARED_LIBS=ON -DBUILD_TZ_LIB=ON -DUSE_SYSTEM_TZ_DB=ON
RUN cmake --build buildDir -j"$(nproc)" && DESTDIR=/cpp-date cmake --install buildDir

# libdbusmenu-gtk3 — backend for waybar's SNI system tray (hosts external
# applet icons). The musl-from-source toolchain has no intltool/gettext, so we
# neuter the configure-time intltool version check (it only gates translation
# catalogs we don't ship) and build just the two library subdirs, skipping the
# po/ translation dir that actually needs intltool-merge at make time.
#
# The source and these stubs are shared by two builds: libdbusmenu, against
# the Wayland-only gtk3 (waybar on Sway), and libdbusmenu-gtk3-x11, against
# gtk3-x11 (snixembed on i3).
FROM toolchain AS libdbusmenu-src
ARG LIBDBUSMENU_VERSION=16.04.0
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://launchpad.net/libdbusmenu/16.04/${LIBDBUSMENU_VERSION}/+download/libdbusmenu-${LIBDBUSMENU_VERSION}.tar.gz -o d.tar.gz && tar -xf d.tar.gz && rm d.tar.gz && mv libdbusmenu-* src
WORKDIR /build/src
# Stub intltool: configure checks for intltool-update/merge/extract in PATH and
# parses --version. We don't build translations (only the two lib subdirs), so
# stubs that report a recent version and no-op are enough to get past configure.
RUN printf '#!/bin/sh\n[ "$1" = "--version" ] && echo "intltool-update (intltool) 0.51.0"\nexit 0\n' > /usr/bin/intltool-update \
    && printf '#!/bin/sh\nexit 0\n' > /usr/bin/intltool-merge \
    && printf '#!/bin/sh\nexit 0\n' > /usr/bin/intltool-extract \
    && chmod +x /usr/bin/intltool-update /usr/bin/intltool-merge /usr/bin/intltool-extract
# intltool also insists on GNU gettext tools; the toolchain has none. Stub the
# few configure probes for. po/ (which would actually run them) is not built.
RUN for t in msgfmt gmsgfmt xgettext msgmerge msgcat msguniq msginit msgconv msgen msgfilter; do \
        printf '#!/bin/sh\ncase "$1" in --version) echo "%s (GNU gettext-tools) 0.21" ;; esac\nexit 0\n' "$t" > /usr/bin/$t \
        && chmod +x /usr/bin/$t; \
    done
# intltool's configure probe `perl -e "require XML::Parser"`; provide a stub
# module so it resolves (only po/ would actually parse XML, and we skip po/).
RUN mkdir -p /usr/share/perl5/vendor_perl/XML \
    && printf 'package XML::Parser;\n$VERSION = "2.46";\n1;\n' > /usr/share/perl5/vendor_perl/XML/Parser.pm
# Disable the tests path leaves AM_CONDITIONALs (HAVE_VALGRIND, ...) defined only
# in that branch, so configure's "conditional X was never defined" sanity guards
# abort. Neuter those guards — the lib subdirs we build don't use the conditionals.
RUN sed -i 's/if test -z "${[A-Z_]*_TRUE}" && test -z "${[A-Z_]*_FALSE}"; then/if false; then/g' configure

FROM libdbusmenu-src AS libdbusmenu
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=cairo /cairo /
COPY --from=pango /pango /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pixman /pixman /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=atk /atk /
COPY --from=libepoxy /libepoxy /
COPY --from=wayland /wayland /
COPY --from=libxkb /libxkb /
COPY --from=mesa /mesa /
COPY --from=gtk3 /gtk3 /
RUN mkdir -p /libdbusmenu
RUN ./configure ${COMMON_CONFIGURE_ARGS} --with-gtk=3 --disable-introspection --disable-vala --disable-gtk-doc --disable-dumper --disable-tests --disable-static --disable-nls
# This 2016 codebase uses G_TYPE_INSTANCE_GET_PRIVATE, now a deprecation that
# -Werror promotes to a hard error against modern GLib. Strip -Werror.
RUN find . -name Makefile -exec sed -i 's/-Werror//g' {} +
# Build/install only the libraries (libdbusmenu-glib + libdbusmenu-gtk); the
# po/, docs/ and tools/ subdirs need intltool/gtk-doc we don't have.
RUN make -C libdbusmenu-glib -j"$(nproc)" && make -C libdbusmenu-gtk -j"$(nproc)" \
    && make -C libdbusmenu-glib DESTDIR=/libdbusmenu install \
    && make -C libdbusmenu-gtk  DESTDIR=/libdbusmenu install

# wlroots — compositor library (shared with doom)
# libseat — built with the systemd-logind backend (libsystemd from toolchain).
# MUST be defined before wlroots: wlroots links it for DRM session management,
# and without it at build time wlroots disables the session backend entirely
# ("Cannot create session: disabled at compile-time"), so sway can never open
# DRM and gets bounced straight back to the login manager.
FROM toolchain AS libseat
COPY --from=libucontext /libucontext /
# renovate: datasource=github-tags depName=kennylevinsen/seatd
ARG SEATD_VERSION=0.9.1
RUN mkdir -p /libseat
WORKDIR /build
RUN curl -f --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/kennylevinsen/seatd/archive/refs/tags/${SEATD_VERSION}.tar.gz -o seatd.tar.gz && tar -xf seatd.tar.gz && rm seatd.tar.gz && mv seatd-* seatd-src
WORKDIR /build/seatd-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} \
    -Dlibseat-logind=systemd -Dlibseat-seatd=enabled -Dlibseat-builtin=enabled -Dserver=enabled -Dexamples=disabled -Dman-pages=disabled
RUN DESTDIR=/libseat ninja -C buildDir install


FROM toolchain AS wlroots
COPY --from=libucontext /libucontext /
RUN mkdir -p /wlroots
COPY --from=libseat /libseat /
COPY --from=wayland /wayland /
COPY --from=libdrm /libdrm /
COPY --from=libxkb /libxkb /
COPY --from=pixman /pixman /
COPY --from=libinput /libinput /
COPY --from=libevdev /libevdev /
COPY --from=mesa /mesa /
COPY --from=hwdata /hwdata /
COPY --from=libdisplay-info /libdisplay-info /
# renovate: datasource=gitlab-tags depName=wlroots/wlroots registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG WLROOTS_VERSION=0.18.2
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://gitlab.freedesktop.org/wlroots/wlroots/-/archive/${WLROOTS_VERSION}/wlroots-${WLROOTS_VERSION}.tar -o wlroots.tar && tar -xf wlroots.tar && rm wlroots.tar && mv wlroots-* wlroots-src
WORKDIR /build/wlroots-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dexamples=false -Dwerror=false
RUN DESTDIR=/wlroots ninja -C buildDir install


# Sway
FROM toolchain AS sway
COPY --from=libucontext /libucontext /
COPY --from=wayland /wayland /
COPY --from=libdrm /libdrm /
COPY --from=libxkb /libxkb /
COPY --from=pixman /pixman /
COPY --from=libinput /libinput /
COPY --from=libevdev /libevdev /
COPY --from=mesa /mesa /
COPY --from=wlroots /wlroots /
COPY --from=libseat /libseat /
COPY --from=cairo /cairo /
COPY --from=pango /pango /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=harfbuzz /harfbuzz /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=fribidi /fribidi /
COPY --from=hwdata /hwdata /
COPY --from=libdisplay-info /libdisplay-info /
COPY --from=libpng /libpng /
# renovate: datasource=github-tags depName=swaywm/sway extractVersion=^(?<version>\d+(\.\d+)*)$
ARG SWAY_VERSION=1.10.1
RUN mkdir -p /sway
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/swaywm/sway/releases/download/${SWAY_VERSION}/sway-${SWAY_VERSION}.tar.gz -o sway.tar.gz && tar -xzf sway.tar.gz && rm sway.tar.gz && mv sway-* sway-src
WORKDIR /build/sway-src
RUN pip3 install meson ninja
# swaybar/swaynag image loading uses cairo PNG (libpng now available).
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dman-pages=disabled -Dgdk-pixbuf=disabled -Dtray=disabled -Dswaybar=true -Dswaynag=true -Dwerror=false
RUN DESTDIR=/sway ninja -C buildDir install


# tllist — foot dependency (header-only-ish)
FROM toolchain AS tllist
# renovate: datasource=gitea-tags depName=dnkl/tllist registryUrl=https://codeberg.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG TLLIST_VERSION=1.1.0
RUN mkdir -p /tllist
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://codeberg.org/dnkl/tllist/archive/${TLLIST_VERSION}.tar.gz -o tllist.tar.gz && tar -xzf tllist.tar.gz && rm tllist.tar.gz && mv tllist* tllist-src
WORKDIR /build/tllist-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS}
RUN DESTDIR=/tllist ninja -C buildDir install


# fcft — font loading/glyph rasterization for foot (shaping disabled)
FROM toolchain AS fcft
COPY --from=freetype /freetype /
COPY --from=libpng /libpng /
COPY --from=fontconfig /fontconfig /
COPY --from=pixman /pixman /
COPY --from=harfbuzz /harfbuzz /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=tllist /tllist /
# renovate: datasource=gitea-tags depName=dnkl/fcft registryUrl=https://codeberg.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG FCFT_VERSION=3.3.1
RUN mkdir -p /fcft
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://codeberg.org/dnkl/fcft/archive/${FCFT_VERSION}.tar.gz -o fcft.tar.gz && tar -xzf fcft.tar.gz && rm fcft.tar.gz && mv fcft* fcft-src
WORKDIR /build/fcft-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtest-text-shaping=false -Dgrapheme-shaping=disabled -Drun-shaping=disabled -Ddocs=disabled
RUN DESTDIR=/fcft ninja -C buildDir install


# foot — Wayland terminal emulator
FROM toolchain AS foot
COPY --from=wayland /wayland /
COPY --from=libxkb /libxkb /
COPY --from=pixman /pixman /
COPY --from=freetype /freetype /
COPY --from=libpng /libpng /
COPY --from=fontconfig /fontconfig /
COPY --from=fcft /fcft /
COPY --from=tllist /tllist /
COPY --from=harfbuzz /harfbuzz /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
# renovate: datasource=gitea-tags depName=dnkl/foot registryUrl=https://codeberg.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG FOOT_VERSION=1.20.2
RUN mkdir -p /foot
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://codeberg.org/dnkl/foot/archive/${FOOT_VERSION}.tar.gz -o foot.tar.gz && tar -xzf foot.tar.gz && rm foot.tar.gz && mv foot* foot-src
WORKDIR /build/foot-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Ddocs=disabled -Dgrapheme-clustering=disabled -Dthemes=false -Dterminfo=disabled -Dwerror=false
RUN DESTDIR=/foot ninja -C buildDir install


# grim — Wayland screenshot tool (used by the test harness and desktop)
FROM toolchain AS grim
COPY --from=wayland /wayland /
COPY --from=pixman /pixman /
COPY --from=libpng /libpng /
# renovate: datasource=gitlab-tags depName=emersion/grim registryUrl=https://gitlab.freedesktop.org extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG GRIM_VERSION=1.4.1
RUN mkdir -p /grim
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://gitlab.freedesktop.org/emersion/grim/-/archive/v${GRIM_VERSION}/grim-v${GRIM_VERSION}.tar.gz -o grim.tar.gz && tar -xzf grim.tar.gz && rm grim.tar.gz && mv grim-* grim-src
WORKDIR /build/grim-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Djpeg=disabled -Dman-pages=disabled
RUN DESTDIR=/grim ninja -C buildDir install


# ===========================================================================
# M2: networking — NetworkManager + wpa_supplicant + wifi
# ===========================================================================

# libnl — netlink library, needed by wpa_supplicant/hostapd (nl80211)
FROM toolchain AS libnl
ARG LIBNL_VERSION=3.11.0
RUN mkdir -p /libnl
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/thom311/libnl/releases/download/libnl3_11_0/libnl-${LIBNL_VERSION}.tar.gz -o libnl.tar.gz && tar -xzf libnl.tar.gz && rm libnl.tar.gz && mv libnl-* libnl-src
WORKDIR /build/libnl-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking
RUN make -j$(nproc) && make install DESTDIR=/libnl


# wpa_supplicant — wifi backend for NetworkManager (D-Bus control interface)
FROM toolchain AS wpa-supplicant
COPY --from=libnl /libnl /
ARG WPA_VERSION=2.11
RUN mkdir -p /wpa
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://w1.fi/releases/wpa_supplicant-${WPA_VERSION}.tar.gz -o wpa.tar.gz && tar -xzf wpa.tar.gz && rm wpa.tar.gz && mv wpa_supplicant-* wpa-src
WORKDIR /build/wpa-src/wpa_supplicant
RUN printf '%s\n' \
    'CONFIG_DRIVER_NL80211=y' \
    'CONFIG_LIBNL32=y' \
    'CONFIG_CTRL_IFACE=y' \
    'CONFIG_CTRL_IFACE_DBUS_NEW=y' \
    'CONFIG_CTRL_IFACE_DBUS_INTRO=y' \
    'CONFIG_BACKEND=file' \
    'CONFIG_TLS=openssl' \
    'CONFIG_IEEE80211W=y' \
    'CONFIG_IEEE80211AC=y' \
    'CONFIG_AP=y' \
    'CONFIG_WPS=y' \
    'CONFIG_EAP=y' \
    'CONFIG_PKCS12=y' \
    'CONFIG_DEBUG_FILE=y' \
    > .config
# Toolchain OpenSSL is built with OPENSSL_NO_MD4; drop the (unused for WPA2-PSK)
# EVP_md4 reference so the crypto backend compiles and links.
RUN sed -i 's/EVP_md4()/NULL/g' ../src/crypto/crypto_openssl.c
# Install to /usr/bin (the base image is usrmerged: /usr/sbin -> /usr/bin).
RUN make -j$(nproc) BINDIR=/usr/bin
RUN make install DESTDIR=/wpa BINDIR=/usr/bin
# D-Bus + systemd service files so NetworkManager can use wpa_supplicant.
# Written explicitly (the upstream .in templates use @BINDIR@ placeholders).
RUN mkdir -p /wpa/usr/share/dbus-1/system-services /wpa/usr/share/dbus-1/system.d /wpa/usr/lib/systemd/system && \
    cp dbus/dbus-wpa_supplicant.conf /wpa/usr/share/dbus-1/system.d/wpa_supplicant.conf && \
    printf '%s\n' \
      '[D-BUS Service]' \
      'Name=fi.w1.wpa_supplicant1' \
      'Exec=/usr/bin/wpa_supplicant -u -O /run/wpa_supplicant' \
      'User=root' \
      'SystemdService=wpa_supplicant.service' \
      > /wpa/usr/share/dbus-1/system-services/fi.w1.wpa_supplicant1.service && \
    printf '%s\n' \
      '[Unit]' \
      'Description=WPA supplicant' \
      'Before=network.target' \
      'After=dbus.service' \
      'Wants=network.target' \
      '[Service]' \
      'Type=dbus' \
      'BusName=fi.w1.wpa_supplicant1' \
      'ExecStart=/usr/bin/wpa_supplicant -u -O /run/wpa_supplicant' \
      'ExecReload=/bin/kill -HUP $MAINPID' \
      '[Install]' \
      'WantedBy=multi-user.target' \
      'Alias=dbus-fi.w1.wpa_supplicant1.service' \
      > /wpa/usr/lib/systemd/system/wpa_supplicant.service


# hostapd — test-only AP for exercising wifi association against mac80211_hwsim
FROM toolchain AS hostapd
COPY --from=libnl /libnl /
ARG HOSTAPD_VERSION=2.11
RUN mkdir -p /hostapd
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://w1.fi/releases/hostapd-${HOSTAPD_VERSION}.tar.gz -o hostapd.tar.gz && tar -xzf hostapd.tar.gz && rm hostapd.tar.gz && mv hostapd-* hostapd-src
WORKDIR /build/hostapd-src/hostapd
RUN printf '%s\n' \
    'CONFIG_DRIVER_NL80211=y' \
    'CONFIG_LIBNL32=y' \
    'CONFIG_IEEE80211N=y' \
    'CONFIG_IEEE80211AC=y' \
    > .config
RUN sed -i 's/EVP_md4()/NULL/g' ../src/crypto/crypto_openssl.c
RUN make -j$(nproc) BINDIR=/usr/bin
RUN make install DESTDIR=/hostapd BINDIR=/usr/bin


# libndp — IPv6 router/neighbour discovery, required by NetworkManager
FROM toolchain AS libndp
# renovate: datasource=github-tags depName=jpirko/libndp extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG LIBNDP_VERSION=1.9
RUN mkdir -p /libndp
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/jpirko/libndp/archive/refs/tags/v${LIBNDP_VERSION}.tar.gz -o libndp.tar.gz && tar -xzf libndp.tar.gz && rm libndp.tar.gz && mv libndp-* libndp-src
WORKDIR /build/libndp-src
RUN ./autogen.sh 2>/dev/null || true
# libndp 1.9 has musl pointer-type mismatches that GCC 14+ treats as errors.
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking \
    CFLAGS="-O2 -g -Wno-error=incompatible-pointer-types -Wno-error=int-conversion"
RUN make -j$(nproc) && make install DESTDIR=/libndp


# wireless-regdb — regulatory database (loaded by cfg80211 from /lib/firmware)
FROM toolchain AS wireless-regdb
ARG WIRELESS_REGDB_VERSION=2024.10.07
# Hadron points both /lib/firmware and /usr/lib/firmware at this persistent,
# canonical directory. Writing the target directly avoids replacing either
# symlink when the scratch rootfs is overlaid onto the base image.
RUN mkdir -p /wireless-regdb/usr/local/lib/firmware
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://www.kernel.org/pub/software/network/wireless-regdb/wireless-regdb-${WIRELESS_REGDB_VERSION}.tar.xz -o regdb.tar.xz && tar -xf regdb.tar.xz && rm regdb.tar.xz && mv wireless-regdb-* regdb-src
RUN cp regdb-src/regulatory.db regdb-src/regulatory.db.p7s /wireless-regdb/usr/local/lib/firmware/


# ncurses — provides the termcap library (libtinfo) that the toolchain's
# libreadline needs; without it nmcli fails to link (undefined tgetent, tputs…).
FROM toolchain AS ncurses
ARG NCURSES_VERSION=6.5
RUN mkdir -p /ncurses
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://ftp.gnu.org/gnu/ncurses/ncurses-${NCURSES_VERSION}.tar.gz -o ncurses.tar.gz && tar -xzf ncurses.tar.gz && rm ncurses.tar.gz && mv ncurses-* ncurses-src
WORKDIR /build/ncurses-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --with-shared --enable-termcap --disable-widec \
    --without-debug --without-ada --without-cxx --without-cxx-binding \
    --without-tests --disable-stripping --without-manpages
# install.libs installs just the libraries/headers (libncurses); the full
# `install` cross-runs `tic` to build the terminfo DB, which fails here and is
# not needed (we only want the termcap symbols for readline).
RUN make -j$(nproc) libs && make install.libs install.includes DESTDIR=/ncurses


# libxslt — provides xsltproc, a build-time requirement of NetworkManager
FROM toolchain AS libxslt
# renovate: datasource=gitlab-tags depName=GNOME/libxslt registryUrl=https://gitlab.gnome.org extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG LIBXSLT_VERSION=1.1.42
RUN mkdir -p /libxslt
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://download.gnome.org/sources/libxslt/${LIBXSLT_VERSION%.*}/libxslt-${LIBXSLT_VERSION}.tar.xz -o libxslt.tar.xz && tar -xf libxslt.tar.xz && rm libxslt.tar.xz && mv libxslt-* libxslt-src
WORKDIR /build/libxslt-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking --without-python
RUN make -j$(nproc) && make install DESTDIR=/libxslt


# NetworkManager
FROM toolchain AS networkmanager
COPY --from=libucontext /libucontext /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=libndp /libndp /
COPY --from=libnl /libnl /
COPY --from=libxslt /libxslt /
COPY --from=ncurses /ncurses /
# The toolchain's libreadline needs termcap symbols from libncurses; make the
# readline pkg-config dependency pull it in so nmcli links cleanly.
RUN sed -i 's/-lreadline/-lreadline -lncurses/' /usr/lib/pkgconfig/readline.pc /usr/lib64/pkgconfig/readline.pc 2>/dev/null || true
# renovate: datasource=gitlab-tags depName=NetworkManager/NetworkManager registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG NM_VERSION=1.50.0
RUN mkdir -p /networkmanager
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://download.gnome.org/sources/NetworkManager/${NM_VERSION%.*}/NetworkManager-${NM_VERSION}.tar.xz -o nm.tar.xz && tar -xf nm.tar.xz && rm nm.tar.xz && mv NetworkManager-* nm-src
WORKDIR /build/nm-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} --sbindir=bin \
    -Dpolkit=false -Dwifi=true -Diwd=false -Dwext=false \
    -Dnmcli=true -Dnmtui=false -Dmodem_manager=false -Dppp=false -Dovs=false \
    -Dintrospection=false -Ddocs=false -Dtests=no -Dlibpsl=false -Dqt=false \
    -Dselinux=false -Dvapi=false -Difcfg_rh=false -Dlibaudit=no \
    -Dsession_tracking=systemd -Dsuspend_resume=systemd -Dsystemd_journal=true \
    -Dcrypto=null -Dconfig_dns_rc_manager_default=symlink \
    -Dc_link_args=-lncurses
RUN DESTDIR=/networkmanager ninja -C buildDir install


# ===========================================================================
# M3: audio — PipeWire + WirePlumber
# ===========================================================================

# ALSA library
FROM toolchain AS alsa-lib
# renovate: datasource=github-tags depName=alsa-project/alsa-lib extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG ALSA_VERSION=1.2.12
RUN mkdir -p /alsa-lib
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://www.alsa-project.org/files/pub/lib/alsa-lib-${ALSA_VERSION}.tar.bz2 -o alsa-lib.tar.bz2 && tar -xjf alsa-lib.tar.bz2 && rm alsa-lib.tar.bz2 && mv alsa-lib-* alsa-lib-src
WORKDIR /build/alsa-lib-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking --disable-python
RUN make -j$(nproc) && make install DESTDIR=/alsa-lib


# ALSA UCM configuration (data files describing sound cards)
FROM toolchain AS alsa-ucm-conf
# renovate: datasource=github-tags depName=alsa-project/alsa-ucm-conf extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG ALSA_UCM_VERSION=1.2.12
RUN mkdir -p /alsa-ucm-conf
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://www.alsa-project.org/files/pub/lib/alsa-ucm-conf-${ALSA_UCM_VERSION}.tar.bz2 -o ucm.tar.bz2 && tar -xjf ucm.tar.bz2 && rm ucm.tar.bz2 && mv alsa-ucm-conf-* ucm-src
RUN mkdir -p /alsa-ucm-conf/usr/share/alsa && cp -a ucm-src/ucm2 /alsa-ucm-conf/usr/share/alsa/


# sbc — Bluetooth A2DP audio codec (used by PipeWire's bluez5 plugin)
FROM toolchain AS sbc
ARG SBC_VERSION=2.0
RUN mkdir -p /sbc
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://www.kernel.org/pub/linux/bluetooth/sbc-${SBC_VERSION}.tar.xz -o sbc.tar.xz && tar -xf sbc.tar.xz && rm sbc.tar.xz && mv sbc-* sbc-src
WORKDIR /build/sbc-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking --disable-tester
RUN make -j$(nproc) && make install DESTDIR=/sbc


# BlueZ — bluetoothd, bluetoothctl, btvirt (virtual HCI for the test)
FROM toolchain AS bluez
COPY --from=libucontext /libucontext /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=ncurses /ncurses /
RUN sed -i 's/-lreadline/-lreadline -lncurses/' /usr/lib/pkgconfig/readline.pc /usr/lib64/pkgconfig/readline.pc 2>/dev/null || true
# renovate: datasource=github-tags depName=bluez/bluez extractVersion=^(?<version>\d+(\.\d+)*)$
ARG BLUEZ_VERSION=5.79
RUN mkdir -p /bluez
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://www.kernel.org/pub/linux/bluetooth/bluez-${BLUEZ_VERSION}.tar.xz -o bluez.tar.xz && tar -xf bluez.tar.xz && rm bluez.tar.xz && mv bluez-* bluez-src
WORKDIR /build/bluez-src
# MAX_INPUT is gated behind _GNU_SOURCE in musl's <limits.h>; define it directly.
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking \
    --enable-client --enable-tools --enable-deprecated --enable-library \
    --enable-systemd --enable-udev --disable-obex --disable-mesh --disable-cups \
    --disable-manpages --enable-testing --disable-nfc --disable-sap --disable-midi \
    --with-dbusconfdir=/usr/share --with-systemdsystemunitdir=/usr/lib/systemd/system \
    --with-systemduserunitdir=/usr/lib/systemd/user \
    CFLAGS="-O2 -g -DMAX_INPUT=255" LIBS="-lncurses"
RUN make -j$(nproc) && make install DESTDIR=/bluez
# btvirt is a tool not installed by default; place it on PATH for the test
RUN cp -f tools/btvirt /bluez/usr/bin/ 2>/dev/null || cp -f emulator/btvirt /bluez/usr/bin/ 2>/dev/null || true


# PipeWire
FROM toolchain AS pipewire
COPY --from=libucontext /libucontext /
COPY --from=alsa-lib /alsa-lib /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=sbc /sbc /
COPY --from=bluez /bluez /
# renovate: datasource=gitlab-tags depName=pipewire/pipewire registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG PIPEWIRE_VERSION=1.2.7
RUN mkdir -p /pipewire
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://gitlab.freedesktop.org/pipewire/pipewire/-/archive/${PIPEWIRE_VERSION}/pipewire-${PIPEWIRE_VERSION}.tar.gz -o pipewire.tar.gz && tar -xzf pipewire.tar.gz && rm pipewire.tar.gz && mv pipewire-* pipewire-src
WORKDIR /build/pipewire-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} \
    -Dalsa=enabled -Dpipewire-alsa=enabled \
    -Ddbus=enabled -Dsystemd=enabled -Dsystemd-user-service=enabled \
    -Dbluez5=enabled -Djack=disabled -Dvulkan=disabled -Dv4l2=disabled \
    -Dlibcamera=disabled -Dexamples=disabled -Dtests=disabled -Dman=disabled \
    -Dgstreamer=disabled -Dsndfile=disabled -Dreadline=disabled -Draop=disabled \
    -Dlibpulse=disabled -Davahi=disabled -Dx11=disabled -Dlegacy-rtkit=true \
    -Dudev=enabled -Dsession-managers=[]
RUN DESTDIR=/pipewire ninja -C buildDir install


# WirePlumber (session manager; uses its bundled Lua)
FROM toolchain AS wireplumber
COPY --from=libucontext /libucontext /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=pipewire /pipewire /
COPY --from=alsa-lib /alsa-lib /
# renovate: datasource=gitlab-tags depName=pipewire/wireplumber registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG WIREPLUMBER_VERSION=0.5.7
RUN mkdir -p /wireplumber
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://gitlab.freedesktop.org/pipewire/wireplumber/-/archive/${WIREPLUMBER_VERSION}/wireplumber-${WIREPLUMBER_VERSION}.tar.gz -o wireplumber.tar.gz && tar -xzf wireplumber.tar.gz && rm wireplumber.tar.gz && mv wireplumber-* wireplumber-src
WORKDIR /build/wireplumber-src
RUN pip3 install meson ninja
# Pre-seed meson's wrap cache with lua (the wrap fetches it from www.lua.org,
# which is frequently unreachable). MacPorts mirrors the identical tarball
# (sha 164c7849…), and meson verifies the hash so the patch still applies.
RUN mkdir -p subprojects/packagecache && \
    curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://distfiles.macports.org/lua/lua-5.4.4.tar.gz -o subprojects/packagecache/lua-5.4.4.tar.gz
RUN meson setup buildDir ${COMMON_MESON_FLAGS} \
    -Dsystem-lua=false -Dintrospection=disabled -Ddoc=disabled -Dtests=false \
    -Delogind=disabled -Dsystemd=enabled -Dsystemd-user-service=true -Dsystemd-system-service=false
RUN DESTDIR=/wireplumber ninja -C buildDir install

# --- waybar — the status bar / panel ---------------------------------------
# Native modules drive NetworkManager / WirePlumber / BlueZ directly (clickable
# wifi, volume, bluetooth), plus an SNI system tray (libdbusmenu) for external
# applet icons. Click actions reuse the variant-aware Hadron network/audio menus.
# Defined after wireplumber/pipewire so every COPY --from resolves backwards.
FROM toolchain AS waybar
COPY --from=libucontext /libucontext /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=cairo /cairo /
COPY --from=pango /pango /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pixman /pixman /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=atk /atk /
COPY --from=libepoxy /libepoxy /
COPY --from=wayland /wayland /
COPY --from=libxkb /libxkb /
COPY --from=mesa /mesa /
COPY --from=gtk3 /gtk3 /
COPY --from=libsigcpp /libsigcpp /
COPY --from=glibmm /glibmm /
COPY --from=cairomm /cairomm /
COPY --from=pangomm /pangomm /
COPY --from=atkmm /atkmm /
COPY --from=gtkmm3 /gtkmm3 /
COPY --from=gtk-layer-shell /gtk-layer-shell /
COPY --from=jsoncpp /jsoncpp /
COPY --from=libfmt /libfmt /
COPY --from=spdlog /spdlog /
COPY --from=libnl /libnl /
COPY --from=cpp-date /cpp-date /
COPY --from=libdbusmenu /libdbusmenu /
COPY --from=pipewire /pipewire /
COPY --from=wireplumber /wireplumber /
# renovate: datasource=github-tags depName=Alexays/Waybar extractVersion=^(?<version>\d+(\.\d+)*)$
ARG WAYBAR_VERSION=0.10.4
RUN mkdir -p /waybar
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/Alexays/Waybar/archive/refs/tags/${WAYBAR_VERSION}.tar.gz -o w.tar.gz && tar -xf w.tar.gz && rm w.tar.gz && mv Waybar-* src
WORKDIR /build/src
RUN pip3 install meson ninja
# Enable the modules the desktop uses (tray=dbusmenu-gtk, network=libnl,
# audio=wireplumber); gtk-layer-shell is a hard dep. Disable the rest so meson
# doesn't reach for libs we don't ship (pulse/jack/mpd/sndio/cava).
RUN meson setup buildDir ${COMMON_MESON_FLAGS} \
      -Dlibcxx=false -Ddbusmenu-gtk=enabled -Dlibnl=enabled \
      -Dwireplumber=enabled -Dpulseaudio=disabled \
      -Dmpris=disabled -Djack=disabled -Dmpd=disabled -Dsndio=disabled \
      -Dcava=disabled -Dexperimental=false -Dman-pages=disabled -Dtests=disabled
RUN DESTDIR=/waybar ninja -C buildDir install

# Nerd Font symbols — waybar/foot glyphs (wifi/bt/volume icons). Symbols-only
# subset keeps it small; installed to /usr/share/fonts and picked up by fc-cache.
FROM toolchain AS nerdfont
# renovate: datasource=github-tags depName=ryanoasis/nerd-fonts extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG NERDFONT_VERSION=3.1.1
RUN mkdir -p /nerdfont/usr/share/fonts/nerd-fonts
WORKDIR /tmp
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/ryanoasis/nerd-fonts/releases/download/v${NERDFONT_VERSION}/NerdFontsSymbolsOnly.tar.xz -o n.tar.xz \
    && tar -xf n.tar.xz -C /nerdfont/usr/share/fonts/nerd-fonts SymbolsNerdFont-Regular.ttf SymbolsNerdFontMono-Regular.ttf \
    && rm n.tar.xz

# The Hadron typefaces: Inter for the interface, JetBrains Mono for terminals,
# both SIL OFL 1.1, with their licences. Data only: the variable TTFs (one file
# covers every weight), no static cuts, no web fonts. rootfs/etc/fonts/conf.d/
# 60-hadron.conf makes them the sans-serif and monospace defaults; DejaVu and
# the Nerd Font symbols stay as fallbacks. Neither project publishes checksums,
# so the release zips are checked against SHA256s computed when pinned: a
# Renovate bump fails here until the PR updates the matching *_SHA256 too.
# The toolchain has no unzip; Python's zipfile module extracts the zips.
FROM toolchain AS hadron-fonts
# renovate: datasource=github-releases depName=rsms/inter extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG INTER_VERSION=4.1
ARG INTER_SHA256=9883fdd4a49d4fb66bd8177ba6625ef9a64aa45899767dde3d36aa425756b11e
# renovate: datasource=github-releases depName=JetBrains/JetBrainsMono extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG JETBRAINS_MONO_VERSION=2.304
ARG JETBRAINS_MONO_SHA256=6f6376c6ed2960ea8a963cd7387ec9d76e3f629125bc33d1fdcd7eb7012f7bbf
WORKDIR /tmp
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://github.com/rsms/inter/releases/download/v${INTER_VERSION}/Inter-${INTER_VERSION}.zip -o inter.zip && \
    echo "${INTER_SHA256}  inter.zip" | sha256sum -c - && \
    curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://github.com/JetBrains/JetBrainsMono/releases/download/v${JETBRAINS_MONO_VERSION}/JetBrainsMono-${JETBRAINS_MONO_VERSION}.zip -o jbm.zip && \
    echo "${JETBRAINS_MONO_SHA256}  jbm.zip" | sha256sum -c - && \
    python3 -m zipfile -e inter.zip inter && python3 -m zipfile -e jbm.zip jbm && \
    f=/hadron-fonts/usr/share/fonts l=/hadron-fonts/usr/share/licenses && \
    install -Dm0644 -t $f/inter inter/InterVariable.ttf inter/InterVariable-Italic.ttf && \
    install -Dm0644 -t $l/inter inter/LICENSE.txt && \
    install -Dm0644 -t $f/jetbrains-mono 'jbm/fonts/variable/JetBrainsMono[wght].ttf' 'jbm/fonts/variable/JetBrainsMono-Italic[wght].ttf' && \
    install -Dm0644 -t $l/jetbrains-mono jbm/OFL.txt && \
    rm -rf inter.zip jbm.zip inter jbm

# Emoji, on every variant: the Noto Color Emoji font (SIL OFL 1.1) and the
# list that hadron-emoji, the emoji picker, shows. Data only. 60-hadron.conf
# adds the font as the last fallback of the generic families and as the
# "emoji" family. The font's glyphs are PNG images (CBDT), which is why the
# freetype stage is built with libpng.
# noto-emoji publishes no font in its release assets, so the font and its
# licence come from the tagged tree. The list is made from Unicode's
# emoji-test.txt by tools/gen-emoji-list.sh: one "<emoji> <name>" line per
# fully-qualified emoji, in /usr/share/hadron/emoji.txt. Nothing publishes
# checksums, so all three files are checked against SHA256s computed when
# pinned: a Renovate bump of NOTO_EMOJI_VERSION fails here until the PR
# updates NOTO_EMOJI_SHA256 too. Unicode has no tags Renovate can read; bump
# UNICODE_EMOJI_VERSION by hand with the font, once a year when Unicode
# releases (the font must have glyphs for the emoji the list shows).
FROM toolchain AS emoji
# renovate: datasource=github-releases depName=googlefonts/noto-emoji extractVersion=^v(?<version>\d+\.\d+)$
ARG NOTO_EMOJI_VERSION=2.051
ARG NOTO_EMOJI_SHA256=72a635cb3d2f3524c51620cdde406b217204e8a6a06c6a096ff8ed4b5fd6e27b
ARG NOTO_EMOJI_LICENSE_SHA256=6a73f9541c2de74158c0e7cf6b0a58ef774f5a780bf191f2d7ec9cc53efe2bf2
ARG UNICODE_EMOJI_VERSION=17.0.0
ARG UNICODE_EMOJI_SHA256=1d8a944f88d7952f7ef7c5167fef3c67995bcae24543949710231b03a201acda
COPY tools/gen-emoji-list.sh /build/gen-emoji-list.sh
WORKDIR /tmp
RUN n=https://raw.githubusercontent.com/googlefonts/noto-emoji/v${NOTO_EMOJI_VERSION} && \
    curl --retry 5 --retry-delay 3 --retry-all-errors -fL $n/fonts/NotoColorEmoji.ttf -o NotoColorEmoji.ttf && \
    echo "${NOTO_EMOJI_SHA256}  NotoColorEmoji.ttf" | sha256sum -c - && \
    curl --retry 5 --retry-delay 3 --retry-all-errors -fL $n/fonts/LICENSE -o OFL.txt && \
    echo "${NOTO_EMOJI_LICENSE_SHA256}  OFL.txt" | sha256sum -c - && \
    curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://www.unicode.org/Public/${UNICODE_EMOJI_VERSION}/emoji/emoji-test.txt -o emoji-test.txt && \
    echo "${UNICODE_EMOJI_SHA256}  emoji-test.txt" | sha256sum -c - && \
    install -Dm0644 -t /emoji/usr/share/fonts/noto-emoji NotoColorEmoji.ttf && \
    install -Dm0644 -t /emoji/usr/share/licenses/noto-emoji OFL.txt && \
    mkdir -p /emoji/usr/share/hadron && \
    sh /build/gen-emoji-list.sh emoji-test.txt > /emoji/usr/share/hadron/emoji.txt && \
    # A format change in emoji-test.txt must fail here, not ship an empty
    # picker: Unicode 17 has 3944 fully-qualified emoji.
    [ "$(wc -l < /emoji/usr/share/hadron/emoji.txt)" -gt 3000 ] && \
    grep -qx '😀 grinning face' /emoji/usr/share/hadron/emoji.txt && \
    rm -f NotoColorEmoji.ttf OFL.txt emoji-test.txt

# The Hadron wallpapers, drawn at build time from theme/palette.toml by
# tools/gen-wallpaper.py (stdlib Python, fixed dither seed, no network), so a
# palette edit reaches them and the repository carries no multi-megabyte PNGs.
# Every variant gets the night orbits (the desktop) and the plain night image
# (swaylock); niri and COSMIC, whose shells have a light mode, also get the day
# orbits. The 4K images are scaled down to fit smaller screens.
FROM toolchain AS hadron-wallpapers
COPY theme/palette.toml /build/theme/palette.toml
COPY tools/gen-wallpaper.py /build/tools/gen-wallpaper.py
RUN python3 /build/tools/gen-wallpaper.py /hadron-wallpapers/usr/share/backgrounds/hadron \
      hadron-orbits-night-3840x2160 hadron-plain-night-1920x1080

FROM toolchain AS hadron-wallpapers-day
COPY theme/palette.toml /build/theme/palette.toml
COPY tools/gen-wallpaper.py /build/tools/gen-wallpaper.py
RUN python3 /build/tools/gen-wallpaper.py /hadron-wallpapers-day/usr/share/backgrounds/hadron \
      hadron-orbits-day-3840x2160

# --- docker (static bundle) — rootful OCI containers ------------------------
# Docker's daemon + CLI + containerd + runc ship as fully-static Go binaries in
# the official static bundle (a musl-friendly, no-libc-dependency pattern).
# dockerd spawns its own containerd child when --containerd is not given, so a
# single docker.service is enough. The base image already provides iptables-nft
# and the kernel ships nf_conntrack/bridge/br_netfilter, so docker's bridge+NAT
# networking works without vendoring a separate network stack.
FROM toolchain AS docker
# renovate: datasource=github-tags depName=docker/cli extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG DOCKER_VERSION=29.5.3
RUN mkdir -p /docker/usr/bin
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://download.docker.com/linux/static/stable/x86_64/docker-${DOCKER_VERSION}.tgz -o d.tgz \
    && tar -xf d.tgz && rm d.tgz \
    && cp -a docker/dockerd docker/docker docker/containerd docker/containerd-shim-runc-v2 docker/runc docker/docker-init docker/docker-proxy docker/ctr /docker/usr/bin/ \
    && rm -rf docker

# --- distrobox — mutable dev-environment containers on docker ---------------
# distrobox is pure POSIX shell (no toolchain). Its install script lays the
# distrobox/distrobox-* scripts into <prefix>/bin and shared data into
# <prefix>/share. It drives the `docker` CLI directly as its container backend.
FROM toolchain AS distrobox
# renovate: datasource=github-tags depName=89luca89/distrobox extractVersion=^(?<version>\d+(\.\d+)*)$
ARG DISTROBOX_VERSION=1.8.2.5
RUN mkdir -p /distrobox/usr
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/89luca89/distrobox/archive/refs/tags/${DISTROBOX_VERSION}.tar.gz -o db.tgz \
    && tar -xf db.tgz && rm db.tgz && mv distrobox-* src
WORKDIR /build/src
RUN ./install --prefix /distrobox/usr

# --- flatpak support libraries ---------------------------------------------
# libarchive — OCI/bundle handling for ostree + flatpak (zlib/lzma/zstd/openssl
# all come from the toolchain).
FROM toolchain AS libarchive
# renovate: datasource=github-tags depName=libarchive/libarchive extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG LIBARCHIVE_VERSION=3.8.7
RUN mkdir -p /libarchive
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/libarchive/libarchive/releases/download/v${LIBARCHIVE_VERSION}/libarchive-${LIBARCHIVE_VERSION}.tar.gz -o a.tar.gz && tar -xf a.tar.gz && rm a.tar.gz && mv libarchive-* src
WORKDIR /build/src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-bsdtar --disable-bsdcpio --disable-bsdcat --without-xml2 --without-expat
RUN make -j"$(nproc)" && make DESTDIR=/libarchive install

FROM toolchain AS json-glib
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
# renovate: datasource=gitlab-tags depName=GNOME/json-glib registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG JSONGLIB_VERSION=1.10.8
RUN mkdir -p /json-glib
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://download.gnome.org/sources/json-glib/${JSONGLIB_VERSION%.*}/json-glib-${JSONGLIB_VERSION}.tar.xz -o j.tar.xz && tar -xf j.tar.xz && rm j.tar.xz && mv json-glib-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dintrospection=disabled -Ddocumentation=disabled -Dgtk_doc=disabled -Dman=false -Dtests=false
RUN DESTDIR=/json-glib ninja -C buildDir install

FROM toolchain AS bubblewrap
# renovate: datasource=github-tags depName=containers/bubblewrap extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG BWRAP_VERSION=0.11.2
RUN mkdir -p /bubblewrap
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/containers/bubblewrap/releases/download/v${BWRAP_VERSION}/bubblewrap-${BWRAP_VERSION}.tar.xz -o b.tar.xz && tar -xf b.tar.xz && rm b.tar.xz && mv bubblewrap-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dman=disabled -Dtests=false -Dselinux=disabled -Drequire_userns=false
RUN DESTDIR=/bubblewrap ninja -C buildDir install

FROM toolchain AS xdg-dbus-proxy
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
# renovate: datasource=github-tags depName=flatpak/xdg-dbus-proxy extractVersion=^(?<version>\d+(\.\d+)*)$
ARG XDP_VERSION=0.1.7
RUN mkdir -p /xdg-dbus-proxy
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/flatpak/xdg-dbus-proxy/releases/download/${XDP_VERSION}/xdg-dbus-proxy-${XDP_VERSION}.tar.xz -o x.tar.xz && tar -xf x.tar.xz && rm x.tar.xz && mv xdg-dbus-proxy-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dman=disabled
RUN DESTDIR=/xdg-dbus-proxy ninja -C buildDir install

# GPG support — flatpak 1.16 hard-requires libgpgme at build; runtime
# verification is supplied by signed-flatpak-runtime.
FROM toolchain AS libgpg-error
ARG LIBGPGERROR_VERSION=1.56
ARG LIBGPGERROR_SHA256=82c3d2deb4ad96ad3925d6f9f124fe7205716055ab50e291116ef27975d169c0
RUN mkdir -p /libgpg-error
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors \
      https://gnupg.org/ftp/gcrypt/libgpg-error/libgpg-error-${LIBGPGERROR_VERSION}.tar.bz2 \
      -o source.tar.bz2 && \
    echo "${LIBGPGERROR_SHA256}  source.tar.bz2" | sha256sum -c - && \
    tar -xf source.tar.bz2 && rm source.tar.bz2 && mv libgpg-error-* src
WORKDIR /build/src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-doc --disable-tests \
      --enable-install-gpg-error-config
RUN make -j"$(nproc)" && make DESTDIR=/libgpg-error install

FROM toolchain AS libassuan
COPY --from=libgpg-error /libgpg-error /
ARG LIBASSUAN_VERSION=3.0.2
RUN mkdir -p /libassuan
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://www.gnupg.org/ftp/gcrypt/libassuan/libassuan-${LIBASSUAN_VERSION}.tar.bz2 -o a.tar.bz2 && tar -xf a.tar.bz2 && rm a.tar.bz2 && mv libassuan-* src
WORKDIR /build/src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-doc
RUN make -j"$(nproc)" && make DESTDIR=/libassuan install

FROM toolchain AS gpgme
COPY --from=libgpg-error /libgpg-error /
COPY --from=libassuan /libassuan /
ARG GPGME_VERSION=1.24.3
RUN mkdir -p /gpgme
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://www.gnupg.org/ftp/gcrypt/gpgme/gpgme-${GPGME_VERSION}.tar.bz2 -o g.tar.bz2 && tar -xf g.tar.bz2 && rm g.tar.bz2 && mv gpgme-* src
WORKDIR /build/src
# musl lacks the glibc transitional ino64_t/off64_t types gpgme's posix-io.c
# uses; _LARGEFILE64_SOURCE makes musl expose them as off_t aliases.
RUN CPPFLAGS="-D_LARGEFILE64_SOURCE" ./configure ${COMMON_CONFIGURE_ARGS} --disable-gpg-test --disable-gpgsm-test --disable-g13-test --enable-languages=cpp --disable-doc
RUN make -j"$(nproc)" && make DESTDIR=/gpgme install

FROM toolchain AS libgcrypt
COPY --from=libgpg-error /libgpg-error /
ARG LIBGCRYPT_VERSION=1.12.2
ARG LIBGCRYPT_SHA256=7ce33c2492221a0436f96a8500215e9f3e3dcb5fd26a757cd415e7a843babd5e
RUN mkdir -p /libgcrypt
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors \
      https://gnupg.org/ftp/gcrypt/libgcrypt/libgcrypt-${LIBGCRYPT_VERSION}.tar.bz2 \
      -o source.tar.bz2 && \
    echo "${LIBGCRYPT_SHA256}  source.tar.bz2" | sha256sum -c - && \
    tar -xf source.tar.bz2 && rm source.tar.bz2 && mv libgcrypt-* src
WORKDIR /build/src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-doc
RUN make -j"$(nproc)" && make DESTDIR=/libgcrypt install

FROM toolchain AS libksba
COPY --from=libgpg-error /libgpg-error /
ARG LIBKSBA_VERSION=1.8.0
ARG LIBKSBA_SHA256=296b9db9095749f2aa104202d7ab7fd09ad10710e00780a709c9754b1a1d9292
RUN mkdir -p /libksba
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors \
      https://gnupg.org/ftp/gcrypt/libksba/libksba-${LIBKSBA_VERSION}.tar.bz2 \
      -o source.tar.bz2 && \
    echo "${LIBKSBA_SHA256}  source.tar.bz2" | sha256sum -c - && \
    tar -xf source.tar.bz2 && rm source.tar.bz2 && mv libksba-* src
WORKDIR /build/src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-doc
RUN make -j"$(nproc)" && make DESTDIR=/libksba install

FROM toolchain AS npth
ARG NPTH_VERSION=1.8
ARG NPTH_SHA256=8bd24b4f23a3065d6e5b26e98aba9ce783ea4fd781069c1b35d149694e90ca3e
RUN mkdir -p /npth
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors \
      https://gnupg.org/ftp/gcrypt/npth/npth-${NPTH_VERSION}.tar.bz2 \
      -o source.tar.bz2 && \
    echo "${NPTH_SHA256}  source.tar.bz2" | sha256sum -c - && \
    tar -xf source.tar.bz2 && rm source.tar.bz2 && mv npth-* src
WORKDIR /build/src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-tests
RUN make -j"$(nproc)" && make DESTDIR=/npth install

FROM toolchain AS sqlite-gnupg
ARG SQLITE_VERSION=3490200
ARG SQLITE_SHA256=5c6d8697e8a32a1512a9be5ad2b2e7a891241c334f56f8b0fb4fc6051e1652e8
RUN mkdir -p /sqlite-gnupg
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors \
      https://sqlite.org/2025/sqlite-autoconf-${SQLITE_VERSION}.tar.gz \
      -o source.tar.gz && \
    echo "${SQLITE_SHA256}  source.tar.gz" | sha256sum -c - && \
    tar -xf source.tar.gz && rm source.tar.gz && mv sqlite-autoconf-* src
WORKDIR /build/src
RUN CFLAGS="-O2 -pipe -flto" ./configure --prefix=/usr \
      --host=x86_64-hadron-linux-musl --build=x86_64-hadron-linux-musl \
      --enable-shared --disable-static --disable-readline --disable-static-shell
RUN make -j"$(nproc)" && make DESTDIR=/sqlite-gnupg install

FROM toolchain AS ntbtls
COPY --from=libgpg-error /libgpg-error /
COPY --from=libgcrypt /libgcrypt /
COPY --from=libksba /libksba /
ARG NTBTLS_VERSION=0.3.2
ARG NTBTLS_SHA256=bdfcb99024acec9c6c4b998ad63bb3921df4cfee4a772ad6c0ca324dbbf2b07c
RUN mkdir -p /ntbtls
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors \
      https://gnupg.org/ftp/gcrypt/ntbtls/ntbtls-${NTBTLS_VERSION}.tar.bz2 \
      -o source.tar.bz2 && \
    echo "${NTBTLS_SHA256}  source.tar.bz2" | sha256sum -c - && \
    tar -xf source.tar.bz2 && rm source.tar.bz2 && mv ntbtls-* src
WORKDIR /build/src
RUN ./configure ${COMMON_CONFIGURE_ARGS}
RUN make -j"$(nproc)" && make DESTDIR=/ntbtls install

FROM toolchain AS gnupg
COPY --from=libgpg-error /libgpg-error /
COPY --from=libassuan /libassuan /
COPY --from=libgcrypt /libgcrypt /
COPY --from=libksba /libksba /
COPY --from=npth /npth /
COPY --from=ntbtls /ntbtls /
COPY --from=sqlite-gnupg /sqlite-gnupg /
ARG GNUPG_VERSION=2.4.9
ARG GNUPG_SHA256=dd17ab2e9a04fd79d39d853f599cbc852062ddb9ab52a4ddeb4176fd8b302964
RUN mkdir -p /gnupg
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors \
      https://gnupg.org/ftp/gcrypt/gnupg/gnupg-${GNUPG_VERSION}.tar.bz2 \
      -o source.tar.bz2 && \
    echo "${GNUPG_SHA256}  source.tar.bz2" | sha256sum -c - && \
    tar -xf source.tar.bz2 && rm source.tar.bz2 && mv gnupg-* src
WORKDIR /build/src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --libexecdir=/usr/lib/gnupg \
      --disable-gpgsm --disable-scdaemon --disable-wks-tools \
      --disable-card-support --disable-tpm2d \
      --disable-doc --disable-nls --disable-ldap --disable-libdns \
      --disable-bzip2
RUN make -j"$(nproc)" && make DESTDIR=/gnupg install && \
    rm -f /gnupg/usr/bin/gpg-card /gnupg/usr/bin/gpg-wks-client \
      /gnupg/usr/lib/gnupg/gpg-wks-client
RUN test -x /gnupg/usr/bin/gpg && test -x /gnupg/usr/bin/gpgv && \
    test -x /gnupg/usr/bin/gpgconf && test -x /gnupg/usr/bin/gpg-agent && \
    test -x /gnupg/usr/bin/dirmngr && \
    test -x /gnupg/usr/lib/gnupg/keyboxd && \
    test ! -e /gnupg/usr/bin/gpgsm && test ! -e /gnupg/usr/bin/gpg-card && \
    test ! -e /gnupg/usr/bin/gpg-wks-client && \
    test ! -e /gnupg/usr/lib/gnupg/gpg-wks-client && \
    test ! -e /gnupg/usr/lib/gnupg/scdaemon && \
    test ! -e /gnupg/usr/lib/gnupg/tpm2daemon

FROM toolchain AS signed-flatpak-runtime
COPY --from=libgpg-error /libgpg-error /
COPY --from=libassuan /libassuan /
COPY --from=libgcrypt /libgcrypt /
COPY --from=libksba /libksba /
COPY --from=npth /npth /
COPY --from=ntbtls /ntbtls /
COPY --from=sqlite-gnupg /sqlite-gnupg /
COPY --from=gnupg /gnupg /
COPY --from=gnupg /gnupg /signed-flatpak-runtime
RUN mkdir -p /signed-flatpak-runtime/usr/lib \
      /signed-flatpak-runtime/usr/share/hadron && \
    cp -a /usr/lib/libgcrypt.so* /signed-flatpak-runtime/usr/lib/ && \
    cp -a /usr/lib/libksba.so* /signed-flatpak-runtime/usr/lib/ && \
    cp -a /usr/lib/libnpth.so* /signed-flatpak-runtime/usr/lib/ && \
    cp -a /usr/lib/libntbtls.so* /signed-flatpak-runtime/usr/lib/ && \
    cp -a /usr/lib/libsqlite3.so* /signed-flatpak-runtime/usr/lib/ && \
    rm -rf /signed-flatpak-runtime/usr/share/doc \
      /signed-flatpak-runtime/usr/share/info \
      /signed-flatpak-runtime/usr/share/man \
      /signed-flatpak-runtime/usr/share/locale
ARG FLATHUB_DESCRIPTOR_SHA256=3371dd250e61d9e1633630073fefda153cd4426f72f4afa0c3373ae2e8fea03a
ARG FLATHUB_PRIMARY_FINGERPRINT=6E5C05D979C76DAF93C081354184DD4D907A7CAE
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors \
      https://dl.flathub.org/repo/flathub.flatpakrepo \
      -o /signed-flatpak-runtime/usr/share/hadron/flathub.flatpakrepo && \
    echo "${FLATHUB_DESCRIPTOR_SHA256}  /signed-flatpak-runtime/usr/share/hadron/flathub.flatpakrepo" | \
      sha256sum -c - && \
    test "$(grep -c '^GPGKey=' \
      /signed-flatpak-runtime/usr/share/hadron/flathub.flatpakrepo)" -eq 1 && \
    sed -n 's/^GPGKey=//p' \
      /signed-flatpak-runtime/usr/share/hadron/flathub.flatpakrepo | \
      base64 -d > /signed-flatpak-runtime/usr/share/hadron/flathub.gpg && \
    install -d -m700 /tmp/gnupg-home && \
    test "$(GNUPGHOME=/tmp/gnupg-home gpg --batch --with-colons --show-keys \
      /signed-flatpak-runtime/usr/share/hadron/flathub.gpg 2>/dev/null | \
      awk -F: '$1 == "fpr" {print $10; exit}')" = \
      "${FLATHUB_PRIMARY_FINGERPRINT}"
RUN : > /signed-flatpak-runtime/usr/share/hadron/gnupg-runtime-size && \
    bytes="$(du -sb /signed-flatpak-runtime | awk '{print $1}')" && \
    printf '%s\n' "$bytes" > \
      /signed-flatpak-runtime/usr/share/hadron/gnupg-runtime-size && \
    bytes="$(du -sb /signed-flatpak-runtime | awk '{print $1}')" && \
    test "$bytes" -le 20971520 && \
    printf '%s\n' "$bytes" > \
      /signed-flatpak-runtime/usr/share/hadron/gnupg-runtime-size

# AppStream trio — flatpak 1.16 hard-requires libappstream at build.
FROM toolchain AS libyaml
# renovate: datasource=github-tags depName=yaml/libyaml extractVersion=^(?<version>\d+(\.\d+)*)$
ARG LIBYAML_VERSION=0.2.5
RUN mkdir -p /libyaml
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/yaml/libyaml/releases/download/${LIBYAML_VERSION}/yaml-${LIBYAML_VERSION}.tar.gz -o y.tar.gz && tar -xf y.tar.gz && rm y.tar.gz && mv yaml-* src
WORKDIR /build/src
RUN ./configure ${COMMON_CONFIGURE_ARGS}
RUN make -j"$(nproc)" && make DESTDIR=/libyaml install

FROM toolchain AS libxmlb
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
# renovate: datasource=github-tags depName=hughsie/libxmlb extractVersion=^(?<version>\d+(\.\d+)*)$
ARG LIBXMLB_VERSION=0.3.22
RUN mkdir -p /libxmlb
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/hughsie/libxmlb/releases/download/${LIBXMLB_VERSION}/libxmlb-${LIBXMLB_VERSION}.tar.xz -o x.tar.xz && tar -xf x.tar.xz && rm x.tar.xz && mv libxmlb-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dintrospection=false -Dgtkdoc=false -Dtests=false -Dstemmer=false -Dcli=false
RUN DESTDIR=/libxmlb ninja -C buildDir install

FROM toolchain AS appstream
COPY --from=gperf /gperf /
COPY --from=libxslt /libxslt /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=libyaml /libyaml /
COPY --from=libxmlb /libxmlb /
# renovate: datasource=github-tags depName=ximion/appstream extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG APPSTREAM_VERSION=1.0.4
RUN mkdir -p /appstream
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://www.freedesktop.org/software/appstream/releases/AppStream-${APPSTREAM_VERSION}.tar.xz -o as.tar.xz && tar -xf as.tar.xz && rm as.tar.xz && mv AppStream-* src
WORKDIR /build/src
RUN pip3 install meson ninja
# No gettext in the toolchain: meson's i18n.gettext() returns void and the po/
# build aborts. Stub the gettext tools so it resolves (translations are no-ops).
RUN for t in xgettext msgmerge msginit msgcat msgconv msguniq; do \
        printf '#!/bin/sh\ncase "$1" in --version) echo "%s (GNU gettext-tools) 0.21" ;; esac\nexit 0\n' "$t" > /usr/bin/$t && chmod +x /usr/bin/$t; \
    done
# msgfmt must emit a *valid* (empty) .mo so meson's po install step finds the
# files it declared; a no-op would leave them missing.
RUN printf '#!/bin/sh\ncase "$1" in --version) echo "msgfmt (GNU gettext-tools) 0.21"; exit 0;; esac\nout=""\nwhile [ $# -gt 0 ]; do case "$1" in -o) shift; out="$1";; --output-file=*) out="${1#*=}";; esac; shift; done\n[ -n "$out" ] && python3 -c "import struct,sys;open(sys.argv[1],\"wb\").write(struct.pack(\"<7I\",0x950412de,0,0,28,28,0,28))" "$out"\nexit 0\n' > /usr/bin/msgfmt && chmod +x /usr/bin/msgfmt
# itstool only translates appstream-cli's own metainfo (not used by libappstream
# that flatpak links). Stub it to copy the untranslated -j source to -o output.
RUN printf '#!/bin/sh\nsrc=""; out=""\nwhile [ $# -gt 0 ]; do case "$1" in -j) shift; src="$1";; -o) shift; out="$1";; esac; shift; done\n[ -n "$out" ] && { [ -n "$src" ] && cp "$src" "$out" || : > "$out"; }\nexit 0\n' > /usr/bin/itstool && chmod +x /usr/bin/itstool
# docs/ (man pages needing DocBook XSL), data/ (appstream-cli's translated
# metainfo) and po/ (translations needing real gettext) are all unused by
# libappstream — the only thing flatpak links — and each trips the missing
# gettext/docbook toolchain, so drop all three subdirs.
RUN : > docs/meson.build && : > data/meson.build && : > po/meson.build
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dstemming=false -Dsystemd=false -Dvapi=false -Dqt=false -Dcompose=false -Dgir=false -Dsvg-support=false -Ddocs=false -Dapidocs=false -Dinstall-docs=false -Dapt-support=false
RUN DESTDIR=/appstream ninja -C buildDir install

# libfuse3 — flatpak requires fuse (revokefs-fuse + the document portal).
FROM toolchain AS libfuse3
# renovate: datasource=github-tags depName=libfuse/libfuse extractVersion=^fuse-(?<version>\d+(\.\d+)*)$
ARG LIBFUSE_VERSION=3.16.2
RUN mkdir -p /libfuse3
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/libfuse/libfuse/releases/download/fuse-${LIBFUSE_VERSION}/fuse-${LIBFUSE_VERSION}.tar.gz -o f.tar.gz && tar -xf f.tar.gz && rm f.tar.gz && mv fuse-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dexamples=false -Dtests=false -Duseroot=false
RUN DESTDIR=/libfuse3 ninja -C buildDir install
# mount.fuse3 installs to /usr/sbin, but the image's /usr/sbin is a symlink to
# bin — a real sbin dir here breaks the COPY into downstream stages. Relocate.
RUN if [ -d /libfuse3/usr/sbin ]; then mkdir -p /libfuse3/usr/bin && cp -a /libfuse3/usr/sbin/. /libfuse3/usr/bin/ && rm -rf /libfuse3/usr/sbin; fi

# e2fsprogs — ostree hard-requires libe2p (ext2 attribute flags). Build only the
# libraries; reuse the toolchain's libuuid/libblkid instead of e2fsprogs' own.
FROM toolchain AS e2fsprogs
# renovate: datasource=github-tags depName=tytso/e2fsprogs extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG E2FSPROGS_VERSION=1.47.2
RUN mkdir -p /e2fsprogs
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://mirrors.edge.kernel.org/pub/linux/kernel/people/tytso/e2fsprogs/v${E2FSPROGS_VERSION}/e2fsprogs-${E2FSPROGS_VERSION}.tar.gz -o e.tar.gz && tar -xf e.tar.gz && rm e.tar.gz && mv e2fsprogs-* src
WORKDIR /build/src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --enable-elf-shlibs --disable-nls --disable-libuuid --disable-libblkid --disable-fuse2fs --disable-fsck
RUN make -j"$(nproc)" libs && make install-libs DESTDIR=/e2fsprogs

# --- ostree — the content store flatpak deploys apps from -------------------
# libcurl/openssl/zlib/lzma/libmount/libsystemd all come from the toolchain.
FROM toolchain AS ostree
COPY --from=libucontext /libucontext /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=libarchive /libarchive /
COPY --from=libgpg-error /libgpg-error /
COPY --from=libassuan /libassuan /
COPY --from=gpgme /gpgme /
COPY --from=e2fsprogs /e2fsprogs /
# renovate: datasource=github-tags depName=ostreedev/ostree extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG OSTREE_VERSION=2026.1
RUN mkdir -p /ostree
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/ostreedev/ostree/releases/download/v${OSTREE_VERSION}/libostree-${OSTREE_VERSION}.tar.xz -o ot.tar.xz && tar -xf ot.tar.xz && rm ot.tar.xz && mv libostree-* src
WORKDIR /build/src
RUN ./configure ${COMMON_CONFIGURE_ARGS} \
      --with-gpgme --with-curl --without-soup --with-crypto=openssl \
      --with-libarchive --with-libsystemd --disable-rofiles-fuse \
      --disable-introspection --disable-gtk-doc --disable-man --without-selinux
RUN make -j"$(nproc)" && make DESTDIR=/ostree install

# --- flatpak ---------------------------------------------------------------
# Sandboxed app runtime. Uses our system bubblewrap / xdg-dbus-proxy /
# fusermount3 (no bundled copies). libseccomp/libcap/libcurl/libxml2/libsystemd
# come from the toolchain. The GnuPG engine and trust bootstrap for runtime
# verification are supplied by signed-flatpak-runtime.
FROM toolchain AS flatpak
COPY --from=libucontext /libucontext /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=libpng /libpng /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=libarchive /libarchive /
COPY --from=json-glib /json-glib /
COPY --from=libgpg-error /libgpg-error /
COPY --from=libassuan /libassuan /
COPY --from=gpgme /gpgme /
COPY --from=libyaml /libyaml /
COPY --from=libxmlb /libxmlb /
COPY --from=appstream /appstream /
COPY --from=libfuse3 /libfuse3 /
COPY --from=bubblewrap /bubblewrap /
COPY --from=xdg-dbus-proxy /xdg-dbus-proxy /
COPY --from=ostree /ostree /
# renovate: datasource=github-tags depName=flatpak/flatpak extractVersion=^(?<version>\d+(\.\d+)*)$
ARG FLATPAK_VERSION=1.16.6
RUN mkdir -p /flatpak
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/flatpak/flatpak/releases/download/${FLATPAK_VERSION}/flatpak-${FLATPAK_VERSION}.tar.xz -o fp.tar.xz && tar -xf fp.tar.xz && rm fp.tar.xz && mv flatpak-* src
WORKDIR /build/src
RUN pip3 install meson ninja pyparsing
# --libexecdir=lib keeps flatpak's helpers (session-helper, portal, ...) in
# /usr/lib (pure image content) rather than /usr/libexec, which Kairos lists in
# PERSISTENT_STATE_PATHS and could shadow on the installed system.
RUN meson setup buildDir ${COMMON_MESON_FLAGS} --libexecdir=lib \
      -Dhttp_backend=curl -Dgir=disabled -Dgtkdoc=disabled -Ddocbook_docs=disabled \
      -Dman=disabled -Dseccomp=enabled -Dselinux_module=disabled -Dmalcontent=disabled \
      -Ddconf=disabled -Dsystem_helper=disabled -Dtests=false -Dinstalled_tests=false \
      -Dwayland_security_context=disabled -Dxauth=disabled \
      -Dsystem_bubblewrap=bwrap -Dsystem_dbus_proxy=xdg-dbus-proxy -Dsystem_fusermount=fusermount3
RUN DESTDIR=/flatpak ninja -C buildDir install

# --- shared-mime-info — the XDG MIME database -------------------------------
# Without /usr/share/mime, GLib's g_content_type_guess() can't identify files,
# so libxmlb (and thus appstream / `flatpak search`) fails to recognise the
# gzipped Flathub catalog and never decompresses it. Build the database and the
# update-mime-database tool; compile the binary cache at build time.
FROM toolchain AS shared-mime-info
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
# renovate: datasource=gitlab-tags depName=xdg/shared-mime-info registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG SMI_VERSION=2.4
RUN mkdir -p /shared-mime-info
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://gitlab.freedesktop.org/xdg/shared-mime-info/-/archive/${SMI_VERSION}/shared-mime-info-${SMI_VERSION}.tar.gz -o smi.tar.gz && tar -xf smi.tar.gz && rm smi.tar.gz && mv shared-mime-info-* src
WORKDIR /build/src
RUN pip3 install meson ninja
# No gettext/itstool/xmlto. Stub them: msgfmt must report a GNU version (meson's
# i18n.merge_file checks) and, in --xml mode, pass the template through so the
# (untranslated) freedesktop.org.xml is still produced.
RUN for t in xgettext msgmerge itstool xmlto; do printf '#!/bin/sh\nexit 0\n' > /usr/bin/$t && chmod +x /usr/bin/$t; done
RUN cat > /usr/bin/msgfmt <<'MFEOF' && chmod +x /usr/bin/msgfmt
#!/bin/sh
template=""; out=""
while [ $# -gt 0 ]; do
    case "$1" in
        --version) echo "msgfmt (GNU gettext-tools) 0.21"; exit 0 ;;
        --template) shift; template="$1" ;;
        --template=*) template="${1#*=}" ;;
        -o) shift; out="$1" ;;
        --output-file=*) out="${1#*=}" ;;
    esac
    shift
done
[ -n "$out" ] && { [ -n "$template" ] && cp "$template" "$out" || python3 -c "import struct,sys;open(sys.argv[1],'wb').write(struct.pack('<7I',0x950412de,0,0,28,28,0,28))" "$out"; }
exit 0
MFEOF
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dbuild-tools=true -Dupdate-mimedb=false -Dbuild-translations=false -Dbuild-tests=false
RUN DESTDIR=/shared-mime-info ninja -C buildDir install
# Compile the binary mime cache into the DESTDIR (the in-tree update-mimedb
# post-install step is skipped above because it isn't DESTDIR-aware).
RUN /shared-mime-info/usr/bin/update-mime-database /shared-mime-info/usr/share/mime 2>&1 | tail -1; \
    ls -la /shared-mime-info/usr/share/mime/mime.cache


# ===========================================================================
# M5: desktop polish — wallpaper, notifications, launcher, clipboard, etc.
# ===========================================================================

# swaybg — wallpaper daemon
FROM toolchain AS swaybg
COPY --from=wayland /wayland /
COPY --from=cairo /cairo /
COPY --from=pixman /pixman /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
# renovate: datasource=github-tags depName=swaywm/swaybg extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG SWAYBG_VERSION=1.2.1
RUN mkdir -p /swaybg
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/swaywm/swaybg/releases/download/v${SWAYBG_VERSION}/swaybg-${SWAYBG_VERSION}.tar.gz -o swaybg.tar.gz && tar -xzf swaybg.tar.gz && rm swaybg.tar.gz && mv swaybg-* swaybg-src
WORKDIR /build/swaybg-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dman-pages=disabled -Dgdk-pixbuf=disabled -Dwerror=false
RUN DESTDIR=/swaybg ninja -C buildDir install


# mako — notification daemon
FROM toolchain AS mako
COPY --from=libucontext /libucontext /
COPY --from=wayland /wayland /
COPY --from=cairo /cairo /
COPY --from=pango /pango /
COPY --from=pixman /pixman /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=harfbuzz /harfbuzz /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=fribidi /fribidi /
COPY --from=libpng /libpng /
# renovate: datasource=github-tags depName=emersion/mako extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG MAKO_VERSION=1.9.0
RUN mkdir -p /mako
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/emersion/mako/releases/download/v${MAKO_VERSION}/mako-${MAKO_VERSION}.tar.gz -o mako.tar.gz && tar -xzf mako.tar.gz && rm mako.tar.gz && mv mako-* mako-src
WORKDIR /build/mako-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dman-pages=disabled -Dsd-bus-provider=libsystemd
RUN DESTDIR=/mako ninja -C buildDir install


# fuzzel — application launcher
FROM toolchain AS fuzzel
COPY --from=wayland /wayland /
COPY --from=libxkb /libxkb /
COPY --from=pixman /pixman /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=fcft /fcft /
COPY --from=tllist /tllist /
COPY --from=harfbuzz /harfbuzz /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=libpng /libpng /
# renovate: datasource=gitea-tags depName=dnkl/fuzzel registryUrl=https://codeberg.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG FUZZEL_VERSION=1.11.0
RUN mkdir -p /fuzzel
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://codeberg.org/dnkl/fuzzel/archive/${FUZZEL_VERSION}.tar.gz -o fuzzel.tar.gz && tar -xzf fuzzel.tar.gz && rm fuzzel.tar.gz && mv fuzzel* fuzzel-src
WORKDIR /build/fuzzel-src
RUN pip3 install meson ninja
# Drop the man-page subdir (needs scdoc, which we don't build).
RUN sed -i "/subdir('doc')/d" meson.build
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Denable-cairo=disabled -Dpng-backend=libpng -Dsvg-backend=none -Dwerror=false
RUN DESTDIR=/fuzzel ninja -C buildDir install


# wl-clipboard — clipboard utilities. 2.3.0 or later: `wl-paste --watch` sets
# CLIPBOARD_STATE=sensitive for a copy marked x-kde-passwordManagerHint, which
# is how cliphist knows not to record it.
FROM toolchain AS wl-clipboard
COPY --from=wayland /wayland /
# renovate: datasource=github-tags depName=bugaevc/wl-clipboard extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG WLCLIP_VERSION=2.3.0
RUN mkdir -p /wl-clipboard
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/bugaevc/wl-clipboard/archive/refs/tags/v${WLCLIP_VERSION}.tar.gz -o wlclip.tar.gz && tar -xzf wlclip.tar.gz && rm wlclip.tar.gz && mv wl-clipboard-* wlclip-src
WORKDIR /build/wlclip-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS}
RUN DESTDIR=/wl-clipboard ninja -C buildDir install


# cliphist — the clipboard history of Sway and COSMIC (hadron-clipboard).
# `wl-paste --watch cliphist store` records each copy. Static Go binary
# (CGO_ENABLED=0), like hadron-update, so it runs on musl with no runtime
# linkage. niri needs none: Noctalia keeps its own history.
FROM golang:1.25.0-alpine3.22 AS cliphist
# renovate: datasource=github-tags depName=sentriz/cliphist extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG CLIPHIST_VERSION=0.7.0
WORKDIR /src
RUN wget -qO- https://github.com/sentriz/cliphist/archive/refs/tags/v${CLIPHIST_VERSION}.tar.gz | tar -xz --strip-components=1 && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /cliphist/usr/bin/cliphist . && \
    [ "$(/cliphist/usr/bin/cliphist version 2>&1 | awk '$1 == "version" { print $2 }')" = "${CLIPHIST_VERSION}" ]


# slurp — region selection (pairs with grim)
FROM toolchain AS slurp
COPY --from=wayland /wayland /
COPY --from=cairo /cairo /
COPY --from=pixman /pixman /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=libxkb /libxkb /
# renovate: datasource=github-tags depName=emersion/slurp extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG SLURP_VERSION=1.5.0
RUN mkdir -p /slurp
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/emersion/slurp/releases/download/v${SLURP_VERSION}/slurp-${SLURP_VERSION}.tar.gz -o slurp.tar.gz && tar -xzf slurp.tar.gz && rm slurp.tar.gz && mv slurp-* slurp-src
WORKDIR /build/slurp-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dman-pages=disabled
RUN DESTDIR=/slurp ninja -C buildDir install


# swayidle — idle management
FROM toolchain AS swayidle
COPY --from=libucontext /libucontext /
COPY --from=wayland /wayland /
# renovate: datasource=github-tags depName=swaywm/swayidle extractVersion=^(?<version>\d+(\.\d+)*)$
ARG SWAYIDLE_VERSION=1.8.0
RUN mkdir -p /swayidle
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/swaywm/swayidle/releases/download/${SWAYIDLE_VERSION}/swayidle-${SWAYIDLE_VERSION}.tar.gz -o swayidle.tar.gz && tar -xzf swayidle.tar.gz && rm swayidle.tar.gz && mv swayidle-* swayidle-src
WORKDIR /build/swayidle-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dman-pages=disabled -Dlogind=enabled
RUN DESTDIR=/swayidle ninja -C buildDir install


# swaylock — screen locker, run by the Super+Alt+L binding and by swayidle on
# idle and before sleep. Built against PAM: it checks the password through the
# service in rootfs-sway/etc/pam.d/swaylock (pam_unix's setuid unix_chkpwd
# helper reads /etc/shadow for it), so the binary itself is not setuid.
# Colours come from rootfs-sway/etc/swaylock/config.
FROM toolchain AS swaylock
COPY --from=wayland /wayland /
COPY --from=libxkb /libxkb /
COPY --from=cairo /cairo /
COPY --from=pixman /pixman /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
# renovate: datasource=github-tags depName=swaywm/swaylock extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG SWAYLOCK_VERSION=1.8.6
RUN mkdir -p /swaylock
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://github.com/swaywm/swaylock/releases/download/v${SWAYLOCK_VERSION}/swaylock-${SWAYLOCK_VERSION}.tar.gz -o swaylock.tar.gz && tar -xzf swaylock.tar.gz && rm swaylock.tar.gz && mv swaylock-* swaylock-src
WORKDIR /build/swaylock-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dpam=enabled -Dgdk-pixbuf=disabled -Dman-pages=disabled \
      -Dzsh-completions=false -Dbash-completions=false -Dfish-completions=false
RUN DESTDIR=/swaylock ninja -C buildDir install


# wlsunset — the sway night light, started by hadron-night-light. It warms the
# screen through wlr-gamma-control, at fixed times (Hadron has no location).
# niri does not need it: Noctalia has its own night light.
FROM toolchain AS wlsunset
COPY --from=wayland /wayland /
# renovate: datasource=github-tags depName=kennylevinsen/wlsunset extractVersion=^(?<version>\d+(\.\d+)*)$
ARG WLSUNSET_VERSION=0.4.0
RUN mkdir -p /wlsunset
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://github.com/kennylevinsen/wlsunset/archive/refs/tags/${WLSUNSET_VERSION}.tar.gz -o wlsunset.tar.gz && tar -xzf wlsunset.tar.gz && rm wlsunset.tar.gz && mv wlsunset-* wlsunset-src
WORKDIR /build/wlsunset-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dman-pages=disabled
RUN DESTDIR=/wlsunset ninja -C buildDir install


# X protocol libs — ly links libxcb (X11 session support) even in a Wayland setup
FROM toolchain AS xorgproto
# renovate: datasource=gitlab-tags depName=xorg/proto/xorgproto registryUrl=https://gitlab.freedesktop.org extractVersion=^xorgproto-(?<version>\d+(\.\d+)*)$
ARG XORGPROTO_VERSION=2025.1
RUN mkdir -p /xorgproto
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/X11Libre/mirror.fdo.xorgproto/archive/refs/tags/xorgproto-${XORGPROTO_VERSION}.tar.gz -o xorgproto.tar.gz && tar -xf xorgproto.tar.gz && rm xorgproto.tar.gz && mv mirror.fdo.xorgproto-* xorgproto-src
WORKDIR /build/xorgproto-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS}
RUN DESTDIR=/xorgproto ninja -C buildDir install

FROM toolchain AS libxau
COPY --from=xorgproto /xorgproto /
# renovate: datasource=gitlab-tags depName=xorg/lib/libxau registryUrl=https://gitlab.freedesktop.org extractVersion=^libXau-(?<version>\d+(\.\d+)*)$
ARG LIBXAU_VERSION=1.0.12
RUN mkdir -p /libxau
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://gitlab.freedesktop.org/xorg/lib/libxau/-/archive/libXau-${LIBXAU_VERSION}/libxau-libXau-${LIBXAU_VERSION}.tar.gz -o libxau.tar.gz && tar -xf libxau.tar.gz && rm libxau.tar.gz && mv libxau-libXau-* libxau-src
WORKDIR /build/libxau-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS}
RUN DESTDIR=/libxau ninja -C buildDir install

FROM toolchain AS xcbproto
# renovate: datasource=gitlab-tags depName=xorg/proto/xcbproto registryUrl=https://gitlab.freedesktop.org extractVersion=^xcb-proto-(?<version>\d+(\.\d+)*)$
ARG XCB_VERSION=1.17.0
RUN mkdir -p /xcbproto
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://xcb.freedesktop.org/dist/xcb-proto-${XCB_VERSION}.tar.gz -o xcb-proto.tar.gz && tar -xzf xcb-proto.tar.gz && rm xcb-proto.tar.gz && mv xcb-proto-* xcb-src
WORKDIR /build/xcb-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --build=${BUILD} --disable-dependency-tracking
RUN make -j$(nproc) && make install DESTDIR=/xcbproto

FROM toolchain AS libxcb
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
# renovate: datasource=gitlab-tags depName=xorg/lib/libxcb registryUrl=https://gitlab.freedesktop.org extractVersion=^libxcb-(?<version>\d+(\.\d+)*)$
ARG LIBXCB_VERSION=1.17.0
RUN mkdir -p /libxcb
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://xcb.freedesktop.org/dist/libxcb-${LIBXCB_VERSION}.tar.gz -o libxcb.tar.gz && tar -xzf libxcb.tar.gz && rm libxcb.tar.gz && mv libxcb-* libxcb-src
WORKDIR /build/libxcb-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --build=${BUILD} --disable-dependency-tracking
RUN make -j$(nproc) && make install DESTDIR=/libxcb


# Rust compiler from the pinned Alpine image: build time only, nothing it links
# ships. Used by the i3 screenshot tools and the COSMIC stages, the same way
# Dockerfile.agent builds cua-driver. It is declared before both because
# BuildKit resolves --from only against stages defined above the consumer.
FROM rust:1.97.0-alpine3.22 AS rust-toolchain


# ===========================================================================
# XLibre + i3 display stack (selected with DESKTOP=i3)
#
# XLibre intentionally remains isolated from the Sway graph. The i3-rootfs
# stage below is the only consumer, so BuildKit prunes this entire section from
# the default DESKTOP=sway build.
# ===========================================================================

# X11 client/server libraries. Keep these in one cached stage: each package is
# installed both into the live builder prefix (for the next package) and into
# /x11-libs (the rootfs artifact). Protocol headers/libxcb come from the shared
# stages above because ly also uses them in the Sway image.
FROM toolchain AS x11-libs
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=freetype /freetype /
COPY --from=libpng /libpng /
COPY --from=fontconfig /fontconfig /
RUN mkdir -p /x11-libs
WORKDIR /build

# renovate: datasource=gitlab-tags depName=xorg/lib/libxtrans registryUrl=https://gitlab.freedesktop.org extractVersion=^xtrans-(?<version>\d+(\.\d+)*)$
ARG XTRANS_VERSION=1.6.0
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/xtrans-${XTRANS_VERSION}.tar.xz -o xtrans.tar.xz && \
    tar -xf xtrans.tar.xz && cd xtrans-${XTRANS_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-docs && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libx11 registryUrl=https://gitlab.freedesktop.org extractVersion=^libX11-(?<version>\d+(\.\d+)*)$
ARG LIBX11_VERSION=1.8.12
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libX11-${LIBX11_VERSION}.tar.xz -o libX11.tar.xz && \
    tar -xf libX11.tar.xz && cd libX11-${LIBX11_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-specs --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxext registryUrl=https://gitlab.freedesktop.org extractVersion=^libXext-(?<version>\d+(\.\d+)*)$
ARG LIBXEXT_VERSION=1.3.6
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libXext-${LIBXEXT_VERSION}.tar.xz -o libXext.tar.xz && \
    tar -xf libXext.tar.xz && cd libXext-${LIBXEXT_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-specs --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxrender registryUrl=https://gitlab.freedesktop.org extractVersion=^libXrender-(?<version>\d+(\.\d+)*)$
ARG LIBXRENDER_VERSION=0.9.12
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libXrender-${LIBXRENDER_VERSION}.tar.xz -o libXrender.tar.xz && \
    tar -xf libXrender.tar.xz && cd libXrender-${LIBXRENDER_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxfixes registryUrl=https://gitlab.freedesktop.org extractVersion=^libXfixes-(?<version>\d+(\.\d+)*)$
ARG LIBXFIXES_VERSION=6.0.1
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libXfixes-${LIBXFIXES_VERSION}.tar.xz -o libXfixes.tar.xz && \
    tar -xf libXfixes.tar.xz && cd libXfixes-${LIBXFIXES_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxdamage registryUrl=https://gitlab.freedesktop.org extractVersion=^libXdamage-(?<version>\d+(\.\d+)*)$
ARG LIBXDAMAGE_VERSION=1.1.6
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libXdamage-${LIBXDAMAGE_VERSION}.tar.xz -o libXdamage.tar.xz && \
    tar -xf libXdamage.tar.xz && cd libXdamage-${LIBXDAMAGE_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxcomposite registryUrl=https://gitlab.freedesktop.org extractVersion=^libXcomposite-(?<version>\d+(\.\d+)*)$
ARG LIBXCOMPOSITE_VERSION=0.4.6
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libXcomposite-${LIBXCOMPOSITE_VERSION}.tar.xz -o libXcomposite.tar.xz && \
    tar -xf libXcomposite.tar.xz && cd libXcomposite-${LIBXCOMPOSITE_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxrandr registryUrl=https://gitlab.freedesktop.org extractVersion=^libXrandr-(?<version>\d+(\.\d+)*)$
ARG LIBXRANDR_VERSION=1.5.4
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libXrandr-${LIBXRANDR_VERSION}.tar.xz -o libXrandr.tar.xz && \
    tar -xf libXrandr.tar.xz && cd libXrandr-${LIBXRANDR_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxi registryUrl=https://gitlab.freedesktop.org extractVersion=^libXi-(?<version>\d+(\.\d+)*)$
ARG LIBXI_VERSION=1.8.2
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libXi-${LIBXI_VERSION}.tar.xz -o libXi.tar.xz && \
    tar -xf libXi.tar.xz && cd libXi-${LIBXI_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxinerama registryUrl=https://gitlab.freedesktop.org extractVersion=^libXinerama-(?<version>\d+(\.\d+)*)$
ARG LIBXINERAMA_VERSION=1.1.5
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libXinerama-${LIBXINERAMA_VERSION}.tar.xz -o libXinerama.tar.xz && \
    tar -xf libXinerama.tar.xz && cd libXinerama-${LIBXINERAMA_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxcursor registryUrl=https://gitlab.freedesktop.org extractVersion=^libXcursor-(?<version>\d+(\.\d+)*)$
ARG LIBXCURSOR_VERSION=1.2.3
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libXcursor-${LIBXCURSOR_VERSION}.tar.xz -o libXcursor.tar.xz && \
    tar -xf libXcursor.tar.xz && cd libXcursor-${LIBXCURSOR_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxft registryUrl=https://gitlab.freedesktop.org extractVersion=^libXft-(?<version>\d+(\.\d+)*)$
ARG LIBXFT_VERSION=2.3.9
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libXft-${LIBXFT_VERSION}.tar.xz -o libXft.tar.xz && \
    tar -xf libXft.tar.xz && cd libXft-${LIBXFT_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxscrnsaver registryUrl=https://gitlab.freedesktop.org extractVersion=^libXScrnSaver-(?<version>\d+(\.\d+)*)$
ARG LIBXSS_VERSION=1.2.4
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libXScrnSaver-${LIBXSS_VERSION}.tar.xz -o libXScrnSaver.tar.xz && \
    tar -xf libXScrnSaver.tar.xz && cd libXScrnSaver-${LIBXSS_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# XLibre's mandatory font/XKB support libraries and the runtime xkb compiler.
# renovate: datasource=gitlab-tags depName=xorg/lib/libfontenc registryUrl=https://gitlab.freedesktop.org extractVersion=^libfontenc-(?<version>\d+(\.\d+)*)$
ARG LIBFONTENC_VERSION=1.1.8
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libfontenc-${LIBFONTENC_VERSION}.tar.xz -o libfontenc.tar.xz && \
    tar -xf libfontenc.tar.xz && cd libfontenc-${LIBFONTENC_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxfont registryUrl=https://gitlab.freedesktop.org extractVersion=^libXfont2-(?<version>\d+(\.\d+)*)$
ARG LIBXFONT2_VERSION=2.0.7
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libXfont2-${LIBXFONT2_VERSION}.tar.xz -o libXfont2.tar.xz && \
    tar -xf libXfont2.tar.xz && cd libXfont2-${LIBXFONT2_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-devel-docs --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxkbfile registryUrl=https://gitlab.freedesktop.org extractVersion=^libxkbfile-(?<version>\d+(\.\d+)*)$
ARG LIBXKBFILE_VERSION=1.1.3
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libxkbfile-${LIBXKBFILE_VERSION}.tar.xz -o libxkbfile.tar.xz && \
    tar -xf libxkbfile.tar.xz && cd libxkbfile-${LIBXKBFILE_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/app/xkbcomp registryUrl=https://gitlab.freedesktop.org extractVersion=^xkbcomp-(?<version>\d+(\.\d+)*)$
ARG XKBCOMP_VERSION=1.4.7
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/app/xkbcomp-${XKBCOMP_VERSION}.tar.xz -o xkbcomp.tar.xz && \
    tar -xf xkbcomp.tar.xz && cd xkbcomp-${XKBCOMP_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# DRM/modesetting support used by the XLibre server.
RUN pip3 install meson ninja
# renovate: datasource=gitlab-tags depName=xorg/lib/libxcvt registryUrl=https://gitlab.freedesktop.org extractVersion=^libxcvt-(?<version>\d+(\.\d+)*)$
ARG LIBXCVT_VERSION=0.1.3
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libxcvt-${LIBXCVT_VERSION}.tar.xz -o libxcvt.tar.xz && \
    tar -xf libxcvt.tar.xz && cd libxcvt-${LIBXCVT_VERSION} && \
    meson setup buildDir ${COMMON_MESON_FLAGS} && \
    ninja -C buildDir install && DESTDIR=/x11-libs ninja -C buildDir install

# renovate: datasource=gitlab-tags depName=xorg/lib/libpciaccess registryUrl=https://gitlab.freedesktop.org extractVersion=^libpciaccess-(?<version>\d+(\.\d+)*)$
ARG LIBPCIACCESS_VERSION=0.18.1
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libpciaccess-${LIBPCIACCESS_VERSION}.tar.xz -o libpciaccess.tar.xz && \
    tar -xf libpciaccess.tar.xz && cd libpciaccess-${LIBPCIACCESS_VERSION} && \
    meson setup buildDir ${COMMON_MESON_FLAGS} -Dzlib=disabled && \
    ninja -C buildDir install && DESTDIR=/x11-libs ninja -C buildDir install

# renovate: datasource=gitlab-tags depName=xorg/lib/libxshmfence registryUrl=https://gitlab.freedesktop.org extractVersion=^libxshmfence-(?<version>\d+(\.\d+)*)$
ARG LIBXSHMFENCE_VERSION=1.3.3
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libxshmfence-${LIBXSHMFENCE_VERSION}.tar.xz -o libxshmfence.tar.xz && \
    tar -xf libxshmfence.tar.xz && cd libxshmfence-${LIBXSHMFENCE_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# i3's XCB utility dependencies.
# renovate: datasource=gitlab-tags depName=xorg/lib/libxcb-util registryUrl=https://gitlab.freedesktop.org extractVersion=^xcb-util-(?<version>\d+(\.\d+)*)$
ARG XCB_UTIL_VERSION=0.4.1
RUN curl --retry-all-errors -fL --retry 5 https://xcb.freedesktop.org/dist/xcb-util-${XCB_UTIL_VERSION}.tar.xz -o xcb-util.tar.xz && \
    tar -xf xcb-util.tar.xz && cd xcb-util-${XCB_UTIL_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxcb-image registryUrl=https://gitlab.freedesktop.org extractVersion=^xcb-util-image-(?<version>\d+(\.\d+)*)$
ARG XCB_UTIL_IMAGE_VERSION=0.4.1
RUN curl --retry-all-errors -fL --retry 5 https://xcb.freedesktop.org/dist/xcb-util-image-${XCB_UTIL_IMAGE_VERSION}.tar.xz -o xcb-image.tar.xz && \
    tar -xf xcb-image.tar.xz && cd xcb-util-image-${XCB_UTIL_IMAGE_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxcb-render-util registryUrl=https://gitlab.freedesktop.org extractVersion=^xcb-util-renderutil-(?<version>\d+(\.\d+)*)$
ARG XCB_UTIL_RENDERUTIL_VERSION=0.3.10
RUN curl --retry-all-errors -fL --retry 5 https://xcb.freedesktop.org/dist/xcb-util-renderutil-${XCB_UTIL_RENDERUTIL_VERSION}.tar.xz -o xcb-renderutil.tar.xz && \
    tar -xf xcb-renderutil.tar.xz && cd xcb-util-renderutil-${XCB_UTIL_RENDERUTIL_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxcb-cursor registryUrl=https://gitlab.freedesktop.org extractVersion=^xcb-util-cursor-(?<version>\d+(\.\d+)*)$
ARG XCB_UTIL_CURSOR_VERSION=0.1.5
RUN curl --retry-all-errors -fL --retry 5 https://xcb.freedesktop.org/dist/xcb-util-cursor-${XCB_UTIL_CURSOR_VERSION}.tar.xz -o xcb-cursor.tar.xz && \
    tar -xf xcb-cursor.tar.xz && cd xcb-util-cursor-${XCB_UTIL_CURSOR_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxcb-keysyms registryUrl=https://gitlab.freedesktop.org extractVersion=^xcb-util-keysyms-(?<version>\d+(\.\d+)*)$
ARG XCB_UTIL_KEYSYMS_VERSION=0.4.1
RUN curl --retry-all-errors -fL --retry 5 https://xcb.freedesktop.org/dist/xcb-util-keysyms-${XCB_UTIL_KEYSYMS_VERSION}.tar.xz -o xcb-keysyms.tar.xz && \
    tar -xf xcb-keysyms.tar.xz && cd xcb-util-keysyms-${XCB_UTIL_KEYSYMS_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=gitlab-tags depName=xorg/lib/libxcb-wm registryUrl=https://gitlab.freedesktop.org extractVersion=^xcb-util-wm-(?<version>\d+(\.\d+)*)$
ARG XCB_UTIL_WM_VERSION=0.4.2
RUN curl --retry-all-errors -fL --retry 5 https://xcb.freedesktop.org/dist/xcb-util-wm-${XCB_UTIL_WM_VERSION}.tar.xz -o xcb-wm.tar.xz && \
    tar -xf xcb-wm.tar.xz && cd xcb-util-wm-${XCB_UTIL_WM_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# xcb-util-xrm uses the BSD queue macros. musl does not ship sys/queue.h, so
# install the header-only compatibility pair from libbsd for this build graph.
# renovate: datasource=gitlab-tags depName=libbsd/libbsd registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG LIBBSD_HEADERS_VERSION=0.12.2
RUN curl --retry-all-errors -fL --retry 5 https://libbsd.freedesktop.org/releases/libbsd-${LIBBSD_HEADERS_VERSION}.tar.xz -o libbsd.tar.xz && \
    tar -xf libbsd.tar.xz && \
    mkdir -p /usr/include/sys /usr/include/bsd/sys /x11-libs/usr/include/sys /x11-libs/usr/include/bsd/sys && \
    cp libbsd-${LIBBSD_HEADERS_VERSION}/include/bsd/sys/queue.h /usr/include/sys/queue.h && \
    cp libbsd-${LIBBSD_HEADERS_VERSION}/include/bsd/sys/cdefs.h /usr/include/bsd/sys/cdefs.h && \
    cp /usr/include/sys/queue.h /x11-libs/usr/include/sys/queue.h && \
    cp /usr/include/bsd/sys/cdefs.h /x11-libs/usr/include/bsd/sys/cdefs.h

# renovate: datasource=github-tags depName=Airblader/xcb-util-xrm extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG XCB_UTIL_XRM_VERSION=1.3
RUN curl --retry-all-errors -fL --retry 5 https://github.com/Airblader/xcb-util-xrm/releases/download/v${XCB_UTIL_XRM_VERSION}/xcb-util-xrm-${XCB_UTIL_XRM_VERSION}.tar.gz -o xcb-xrm.tar.gz && \
    tar -xf xcb-xrm.tar.gz && cd xcb-util-xrm-${XCB_UTIL_XRM_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG STARTUP_NOTIFICATION_VERSION=0.12
RUN curl --retry-all-errors -fL --retry 5 https://www.freedesktop.org/software/startup-notification/releases/startup-notification-${STARTUP_NOTIFICATION_VERSION}.tar.gz -o startup-notification.tar.gz && \
    tar -xf startup-notification.tar.gz && cd startup-notification-${STARTUP_NOTIFICATION_VERSION} && \
    ./configure --quiet --prefix=/usr --enable-shared --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBEV_VERSION=4.33
RUN curl --retry-all-errors -fL --retry 5 https://dist.schmorp.de/libev/libev-${LIBEV_VERSION}.tar.gz -o libev.tar.gz && \
    tar -xf libev.tar.gz && cd libev-${LIBEV_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# renovate: datasource=github-tags depName=lloyd/yajl extractVersion=^(?<version>\d+(\.\d+)*)$
ARG YAJL_VERSION=2.1.0
RUN curl --retry-all-errors -fL --retry 5 https://github.com/lloyd/yajl/archive/refs/tags/${YAJL_VERSION}.tar.gz -o yajl.tar.gz && \
    tar -xf yajl.tar.gz && cd yajl-${YAJL_VERSION} && \
    sed -i '/ADD_SUBDIRECTORY(test)/,$d' CMakeLists.txt && \
    cmake -G Ninja -B buildDir -DCMAKE_POLICY_VERSION_MINIMUM=3.5 -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DBUILD_SHARED_LIBS=ON && \
    ninja -C buildDir && ninja -C buildDir install && DESTDIR=/x11-libs ninja -C buildDir install

# renovate: datasource=gitlab-tags depName=xorg/lib/libxxf86vm registryUrl=https://gitlab.freedesktop.org extractVersion=^libXxf86vm-(?<version>\d+(\.\d+)*)$
ARG LIBXXF86VM_VERSION=1.1.7
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/lib/libXxf86vm-${LIBXXF86VM_VERSION}.tar.xz -o libXxf86vm.tar.xz && \
    tar -xf libXxf86vm.tar.xz && cd libXxf86vm-${LIBXXF86VM_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs


# libxkbcommon with its X11/XCB bridge (the Sway variant keeps the lean,
# Wayland-only build from the earlier libxkb stage).
FROM toolchain AS libxkb-x11
COPY --from=xkeyboard-config /xkeyboard-config /
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
# renovate: datasource=github-tags depName=xkbcommon/libxkbcommon extractVersion=^xkbcommon-(?<version>\d+(\.\d+)*)$
ARG LIBXKBCOMMON_VERSION=1.13.0
RUN mkdir -p /libxkb-x11
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://github.com/xkbcommon/libxkbcommon/archive/refs/tags/xkbcommon-${LIBXKBCOMMON_VERSION}.tar.gz -o libxkbcommon.tar.gz && \
    tar -xf libxkbcommon.tar.gz && mv libxkbcommon-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Denable-x11=true -Denable-wayland=false -Denable-xkbregistry=true -Denable-bash-completion=false
RUN DESTDIR=/libxkb-x11 ninja -C buildDir install


# Mesa rebuilt for the X11 platform and DRI GLX. GPU retains the same vm/full
# semantics as the Sway Mesa stage, and every mode builds llvmpipe.
FROM toolchain AS mesa-x11
COPY --from=libdrm /libdrm /
COPY --from=llvm-stack /llvm-out /llvm-out
RUN rsync -aHAX --keep-dirlinks /llvm-out/. / && rm -rf /llvm-out
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
# renovate: datasource=gitlab-tags depName=mesa/mesa registryUrl=https://gitlab.freedesktop.org extractVersion=^mesa-(?<version>\d+(\.\d+)*)$
ARG MESA_VERSION=25.3.0
ARG GPU=vm
RUN mkdir -p /mesa-x11
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://archive.mesa3d.org/mesa-${MESA_VERSION}.tar.xz -o mesa.tar.xz && \
    tar -xf mesa.tar.xz && mv mesa-* src
WORKDIR /build/src
RUN pip3 install meson ninja setuptools mako pyyaml
RUN if [ "$GPU" = "full" ]; then \
      DRV="iris,radeonsi,llvmpipe,virgl,softpipe,svga"; \
    else \
      DRV="llvmpipe,virgl,softpipe,svga"; \
    fi; \
    meson setup buildDir ${COMMON_MESON_FLAGS} -Dplatforms=x11 \
      -Dgallium-drivers="$DRV" -Dglx=dri -Dopengl=true -Dgles1=enabled -Dgles2=enabled \
      -Degl=enabled -Dvulkan-drivers= -Dllvm=enabled -Dcpp_rtti=false -Dbuild-tests=false
RUN DESTDIR=/mesa-x11 ninja -C buildDir install
RUN mkdir -p /mesa-x11/usr/lib && cp -a /usr/lib/libLLVM.so* /mesa-x11/usr/lib/ && \
    if [ "$GPU" = "full" ]; then cp -a /usr/lib/libelf.so* /mesa-x11/usr/lib/; fi


FROM toolchain AS libepoxy-x11
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=mesa-x11 /mesa-x11 /
# renovate: datasource=github-tags depName=anholt/libepoxy extractVersion=^(?<version>\d+(\.\d+)*)$
ARG LIBEPOXY_VERSION=1.5.10
RUN mkdir -p /libepoxy-x11
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://github.com/anholt/libepoxy/archive/refs/tags/${LIBEPOXY_VERSION}.tar.gz -o epoxy.tar.gz && \
    tar -xf epoxy.tar.gz && mv libepoxy-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dglx=yes -Dx11=true -Degl=yes -Dtests=false
RUN DESTDIR=/libepoxy-x11 ninja -C buildDir install


# X11-enabled Cairo/Pango pair used by i3 and dunst. Keeping these separate
# avoids adding libX11 dependencies to Sway's Cairo runtime.
FROM toolchain AS cairo-x11
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=pixman /pixman /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pcre2 /pcre2 /
COPY --from=glib2 /glib2 /
# renovate: datasource=gitlab-tags depName=cairo/cairo registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG CAIRO_VERSION=1.18.4
RUN mkdir -p /cairo-x11
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://cairographics.org/releases/cairo-${CAIRO_VERSION}.tar.xz -o cairo.tar.xz && \
    tar -xf cairo.tar.xz && mv cairo-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=disabled -Dglib=enabled -Dspectre=disabled \
    -Dfreetype=enabled -Dfontconfig=enabled -Dpng=enabled -Dxlib=enabled -Dxcb=enabled
RUN DESTDIR=/cairo-x11 ninja -C buildDir install

FROM toolchain AS pango-x11
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=harfbuzz /harfbuzz /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=fribidi /fribidi /
COPY --from=cairo-x11 /cairo-x11 /
COPY --from=pixman /pixman /
COPY --from=libpng /libpng /
# renovate: datasource=gitlab-tags depName=GNOME/pango registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG PANGO_VERSION=1.54.0
RUN mkdir -p /pango-x11
WORKDIR /build
RUN PANGO_MAJOR="${PANGO_VERSION%.*}" && curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://download.gnome.org/sources/pango/${PANGO_MAJOR}/pango-${PANGO_VERSION}.tar.xz -o pango.tar.xz && \
    tar -xf pango.tar.xz && mv pango-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dintrospection=disabled -Dgtk_doc=false -Dfontconfig=enabled -Dfreetype=enabled -Dcairo=enabled
RUN DESTDIR=/pango-x11 ninja -C buildDir install


# XLibre X server. 25.2.0 is the current stable branch and includes the Xorg
# modesetting DDX; only the external libinput DDX is needed below.
FROM toolchain AS xlibre
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=freetype /freetype /
COPY --from=libpng /libpng /
COPY --from=libucontext /libucontext /
COPY --from=x11-libs /x11-libs /
COPY --from=xkeyboard-config /xkeyboard-config /
COPY --from=pixman /pixman /
COPY --from=libdrm /libdrm /
COPY --from=mesa-x11 /mesa-x11 /
COPY --from=libepoxy-x11 /libepoxy-x11 /
COPY --from=libseat /libseat /
# renovate: datasource=github-tags depName=X11Libre/xserver extractVersion=^xlibre-xserver-(?<version>\d+(\.\d+)*)$
ARG XLIBRE_VERSION=25.2.0
RUN mkdir -p /xlibre
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/X11Libre/xserver/archive/refs/tags/xlibre-xserver-${XLIBRE_VERSION}.tar.gz -o xlibre.tar.gz && \
    tar -xf xlibre.tar.gz && mv xserver-* src && \
    sed -i '/#include <stddef.h>/a #include <sys/random.h>' src/os/osdep.h && \
    sed -i 's/while (pos < len)/while (pos < nbytes)/; s/getrandom(buf + pos/getrandom((unsigned char *)buf + pos/' src/os/osdep.h
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} \
      -Dxorg=true -Dxorg-sdk=true -Dxnest=false -Dxephyr=false -Dxfbdev=false -Dxvfb=true \
      -Dglamor=true -Dgbm=true -Dglx=true -Dglx_dri=true \
      -Ddri1=false -Ddri2=true -Ddri3=true -Ddrm=true \
      -Dxdmcp=false -Dxdm-auth-1=false -Dint10=false -Dagp=false -Ddga=false \
      -Dudev=true -Dudev_kms=true -Dsystemd_notify=false -Dsystemd_logind=false -Dseatd_libseat=true \
      -Dsuid_wrapper=false -Dlisten_tcp=false -Dsha1=libcrypto -Dxselinux=false \
      -Dxkb_dir=/usr/share/X11/xkb -Dxkb_output_dir=/var/lib/xkb -Dxkb_bin_dir=/usr/bin \
      -Dxf86-input-inputtest=false -Dtests=false -Ddocs=false -Ddevel-docs=false
RUN DESTDIR=/xlibre ninja -C buildDir install


FROM toolchain AS xlibre-input-libinput
COPY --from=xorgproto /xorgproto /
COPY --from=libevdev /libevdev /
COPY --from=libinput /libinput /
COPY --from=libucontext /libucontext /
COPY --from=pixman /pixman /
COPY --from=freetype /freetype /
COPY --from=libpng /libpng /
COPY --from=x11-libs /x11-libs /
COPY --from=xlibre /xlibre /
# renovate: datasource=github-tags depName=X11Libre/xf86-input-libinput extractVersion=^xlibre-xf86-input-libinput-(?<version>\d+(\.\d+)*)$
ARG XLIBRE_INPUT_LIBINPUT_VERSION=25.0.1
RUN mkdir -p /xlibre-input-libinput
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/X11Libre/xf86-input-libinput/archive/refs/tags/xlibre-xf86-input-libinput-${XLIBRE_INPUT_LIBINPUT_VERSION}.tar.gz -o driver.tar.gz && \
    tar -xf driver.tar.gz && mv xf86-input-libinput-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS}
RUN DESTDIR=/xlibre-input-libinput ninja -C buildDir install


FROM toolchain AS i3
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=libxkb-x11 /libxkb-x11 /
COPY --from=cairo-x11 /cairo-x11 /
COPY --from=pango-x11 /pango-x11 /
COPY --from=pixman /pixman /
COPY --from=pcre2 /pcre2 /
COPY --from=glib2 /glib2 /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
# renovate: datasource=github-tags depName=i3/i3 extractVersion=^(?<version>\d+(\.\d+)*)$
ARG I3_VERSION=4.25.1
RUN mkdir -p /i3
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://i3wm.org/downloads/i3-${I3_VERSION}.tar.xz -o i3.tar.xz && \
    tar -xf i3.tar.xz && mv i3-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Ddocs=false -Dmans=false
RUN DESTDIR=/i3 ninja -C buildDir install


# Native X11 desktop tools: xauth for Ly's X session cookie, st terminal, dmenu
# launcher, xsel clipboard and clipnotify for the clipboard history. These
# replace foot/fuzzel/wl-clipboard/cliphist; the hadron-xroot stage below
# replaces swaybg.
FROM toolchain AS x11-desktop-tools
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=freetype /freetype /
COPY --from=libpng /libpng /
COPY --from=fontconfig /fontconfig /
RUN mkdir -p /x11-desktop-tools/usr/bin
WORKDIR /build
# renovate: datasource=gitlab-tags depName=xorg/app/xauth registryUrl=https://gitlab.freedesktop.org extractVersion=^xauth-(?<version>\d+(\.\d+)*)$
ARG XAUTH_VERSION=1.1.5
# xauth only uses XmuGetHostname from libXmuu; that function is a thin
# gethostname(2) wrapper. Use libc directly instead of pulling libXt and the
# rest of libXmu's legacy widget dependency chain into this small image.
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/app/xauth-${XAUTH_VERSION}.tar.xz -o xauth.tar.xz && \
    tar -xf xauth.tar.xz && cd xauth-${XAUTH_VERSION} && \
    sed -i -e 's|#include <X11/Xmu/SysUtil.h>|#include <unistd.h>|' \
           -e 's/XmuGetHostname/gethostname/' \
           -e '/(void) gethostname (buf, maxlen);/a\    buf[maxlen - 1] = '\''\\0'\'';' parsedpy.c && \
    XAUTH_CFLAGS="-I/usr/include $(pkg-config --cflags x11 xau xext)" \
    XAUTH_LIBS="$(pkg-config --libs x11 xau xext)" \
    ./configure ${COMMON_CONFIGURE_ARGS} && \
    make -j"$(nproc)" && make install DESTDIR=/x11-desktop-tools

# renovate: datasource=git-tags depName=https://git.suckless.org/st extractVersion=^(?<version>\d+(\.\d+)*)$
ARG ST_VERSION=0.9.3
RUN curl --retry-all-errors -fL --retry 5 https://dl.suckless.org/st/st-${ST_VERSION}.tar.gz -o st.tar.gz && \
    tar -xf st.tar.gz && cd st-${ST_VERSION} && \
    sed -i -e 's|^PREFIX = .*|PREFIX = /usr|' -e 's|^X11INC = .*|X11INC = /usr/include|' -e 's|^X11LIB = .*|X11LIB = /usr/lib|' config.mk && \
    sed -i -e 's|Liberation Mono:pixelsize=12:antialias=true:autohint=true|JetBrains Mono:pixelsize=15:antialias=true:autohint=true|' \
           -e 's|static int borderpx = 2;|static int borderpx = 10;|' \
           -e 's|char \*termname = "st-256color";|char *termname = "xterm-256color";|' \
           -e 's|"gray90", /\* default foreground colour \*/|"#c0caf5", /* default foreground colour */|' \
           -e 's|"black", /\* default background colour \*/|"#1a1b26", /* default background colour */|' config.def.h && \
    grep -q '"JetBrains Mono:pixelsize=15:' config.def.h && \
    make -j"$(nproc)" CC=cc && install -m0755 st /x11-desktop-tools/usr/bin/st

# renovate: datasource=git-tags depName=https://git.suckless.org/dmenu extractVersion=^(?<version>\d+(\.\d+)*)$
ARG DMENU_VERSION=5.3
RUN curl --retry-all-errors -fL --retry 5 https://dl.suckless.org/tools/dmenu-${DMENU_VERSION}.tar.gz -o dmenu.tar.gz && \
    tar -xf dmenu.tar.gz && cd dmenu-${DMENU_VERSION} && \
    sed -i -e 's|^PREFIX = .*|PREFIX = /usr|' -e 's|^X11INC = .*|X11INC = /usr/include|' -e 's|^X11LIB = .*|X11LIB = /usr/lib|' config.mk && \
    sed -i -e 's|"monospace:size=10"|"Inter Variable:size=11"|' config.def.h && \
    grep -q '"Inter Variable:size=11"' config.def.h && \
    make -j"$(nproc)" CC=cc && make install CC=cc DESTDIR=/x11-desktop-tools

# renovate: datasource=github-tags depName=kfish/xsel extractVersion=^(?<version>\d+(\.\d+)*)$
ARG XSEL_VERSION=1.2.1
RUN curl --retry-all-errors -fL --retry 5 https://github.com/kfish/xsel/archive/refs/tags/${XSEL_VERSION}.tar.gz -o xsel.tar.gz && \
    tar -xf xsel.tar.gz && cd xsel-${XSEL_VERSION} && \
    autoreconf -fi && ./configure ${COMMON_CONFIGURE_ARGS} && \
    make -j"$(nproc)" && make install DESTDIR=/x11-desktop-tools

# xclip puts the i3 screenshots on the clipboard. xsel above cannot: it only
# offers text targets, and a pasted screenshot needs image/png (`xclip -t`).
# Like xauth, xclip only takes two atom macros from libXmu; define them with
# XInternAtom instead of pulling libXmu, libXt, libSM and libICE into the image.
# The Xmu header also brought in Xresource.h and string.h, so include those.
# renovate: datasource=github-tags depName=astrand/xclip extractVersion=^(?<version>\d+(\.\d+)*)$
ARG XCLIP_VERSION=0.13
RUN curl --retry-all-errors -fL --retry 5 https://github.com/astrand/xclip/archive/refs/tags/${XCLIP_VERSION}.tar.gz -o xclip.tar.gz && \
    tar -xf xclip.tar.gz && cd xclip-${XCLIP_VERSION} && \
    sed -i -e '/X11\/Xmu\/Atoms.h/d' -e '/X11\/Intrinsic.h/d' -e '/AC_CHECK_LIB(Xmu/d' configure.ac && \
    sed -i 's/ -lXmu//' Makefile.in && \
    sed -i 's|^#include <X11/Xmu/Atoms.h>$|#include <X11/Xresource.h>\n#include <string.h>\n#define XA_CLIPBOARD(d) XInternAtom((d), "CLIPBOARD", False)\n#define XA_UTF8_STRING(d) XInternAtom((d), "UTF8_STRING", False)|' xclip.c && \
    autoreconf -fi && ./configure ${COMMON_CONFIGURE_ARGS} && \
    make -j"$(nproc)" && install -m0755 xclip /x11-desktop-tools/usr/bin/xclip

# xsct is the i3 night light: hadron-night-light runs it every minute, and it
# sets the X RandR gamma once and exits. One C file against libX11 and
# libXrandr, which the i3 image already has.
# renovate: datasource=github-tags depName=faf0/sct extractVersion=^(?<version>\d+(\.\d+)*)$
ARG XSCT_VERSION=2.3
RUN curl --retry-all-errors -fL --retry 5 https://github.com/faf0/sct/archive/refs/tags/${XSCT_VERSION}.tar.gz -o sct.tar.gz && \
    tar -xf sct.tar.gz && cd sct-${XSCT_VERSION} && \
    make CC=cc LDFLAGS=-s && install -m0755 xsct /x11-desktop-tools/usr/bin/xsct
# clipnotify waits for the next clipboard change (XFixes) and exits; the
# hadron-clipboard daemon runs it in a loop to record the i3 clipboard
# history, reading the text with xsel and the types with xclip. One C file.
# Not clipmenu: its C rewrite has no release tag to pin, and its last tagged
# release is a bash script.
# renovate: datasource=github-tags depName=cdown/clipnotify extractVersion=^(?<version>\d+(\.\d+)*)$
ARG CLIPNOTIFY_VERSION=1.0.2
RUN curl --retry-all-errors -fL --retry 5 https://github.com/cdown/clipnotify/archive/refs/tags/${CLIPNOTIFY_VERSION}.tar.gz -o clipnotify.tar.gz && \
    tar -xf clipnotify.tar.gz && cd clipnotify-${CLIPNOTIFY_VERSION} && \
    cc ${CFLAGS:-} clipnotify.c -o clipnotify ${LDFLAGS:-} -lX11 -lXfixes && \
    install -m0755 clipnotify /x11-desktop-tools/usr/bin/clipnotify


# hadron-xroot: the i3 wallpaper. Paints a PNG scaled to fill each monitor on
# the X root window, with cairo (already in the i3 image for i3 and dunst).
FROM toolchain AS hadron-xroot
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=pixman /pixman /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=cairo-x11 /cairo-x11 /
COPY tools/hadron-xroot.c /build/hadron-xroot.c
RUN mkdir -p /hadron-xroot/usr/bin && \
    cc -O2 -Wall -Wextra -Werror /build/hadron-xroot.c $(pkg-config --cflags --libs cairo-xlib cairo-png xrandr x11) -lm \
      -o /hadron-xroot/usr/bin/hadron-xroot


# X11 notification daemon replacing mako.
FROM toolchain AS dunst
COPY --from=libucontext /libucontext /
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=cairo-x11 /cairo-x11 /
COPY --from=pango-x11 /pango-x11 /
COPY --from=pixman /pixman /
COPY --from=pcre2 /pcre2 /
COPY --from=glib2 /glib2 /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=gdk-pixbuf /gdk-pixbuf /
# renovate: datasource=github-tags depName=dunst-project/dunst extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG DUNST_VERSION=1.13.0
RUN mkdir -p /dunst
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/dunst-project/dunst/archive/refs/tags/v${DUNST_VERSION}.tar.gz -o dunst.tar.gz && \
    tar -xf dunst.tar.gz && mv dunst-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dx11=enabled -Dwayland=disabled -Ddunstify=disabled -Dsystemd=enabled -Ddocs=disabled
RUN DESTDIR=/dunst ninja -C buildDir install


# i3lock — the i3 screen locker, run by Super+Alt+L and by xss-lock. Every
# library it needs is already in the i3 image (XCB utils, xcb-xrm and libev
# from x11-libs, xkbcommon-x11, X11 Cairo). It checks the password through PAM
# (rootfs-i3/etc/pam.d/i3lock), so the binary is not setuid.
FROM toolchain AS i3lock
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=libxkb-x11 /libxkb-x11 /
COPY --from=cairo-x11 /cairo-x11 /
COPY --from=pixman /pixman /
COPY --from=pcre2 /pcre2 /
COPY --from=glib2 /glib2 /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
# renovate: datasource=github-tags depName=i3/i3lock extractVersion=^(?<version>\d+(\.\d+)*)$
ARG I3LOCK_VERSION=2.16
RUN mkdir -p /i3lock
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/i3/i3lock/archive/refs/tags/${I3LOCK_VERSION}.tar.gz -o i3lock.tar.gz && \
    tar -xf i3lock.tar.gz && mv i3lock-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS}
RUN DESTDIR=/i3lock ninja -C buildDir install


# xss-lock — runs i3lock when the X screen saver activates (idle) and when
# logind asks: before suspend (holding a sleep delay lock until the screen is
# locked) and on `loginctl lock-session`. It needs only GLib/GIO and XCB.
FROM toolchain AS xss-lock
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=pcre2 /pcre2 /
COPY --from=glib2 /glib2 /
# renovate: datasource=github-tags depName=xdbob/xss-lock extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG XSS_LOCK_VERSION=0.3.0
RUN mkdir -p /xss-lock
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/xdbob/xss-lock/archive/refs/tags/v${XSS_LOCK_VERSION}.tar.gz -o xss-lock.tar.gz && \
    tar -xf xss-lock.tar.gz && mv xss-lock-* src
WORKDIR /build/src
RUN pip3 install ninja
RUN cmake -G Ninja -B buildDir -DCMAKE_POLICY_VERSION_MINIMUM=3.5 -DCMAKE_BUILD_TYPE=MinSizeRel -DCMAKE_INSTALL_PREFIX=/usr && \
    ninja -C buildDir && DESTDIR=/xss-lock ninja -C buildDir install && \
    rm -rf /xss-lock/usr/share


# i3 screenshots: shotgun captures the screen or an area to PNG, hacksaw lets
# the user drag out that area. They replace grim and slurp. Both are small Rust
# programs: shotgun speaks X11 through pure-Rust x11rb, so it links only musl,
# and hacksaw links libxcb's shape and xkb extensions, already in the image.
# maim and slop do the same job but would add libjpeg, libwebp, glm and C++ libs
# to the image. RUSTFLAGS links musl dynamically, like the COSMIC binaries.
FROM toolchain AS x11-screenshot
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=rust-toolchain /usr/local/cargo/ /usr/local/cargo/
COPY --from=rust-toolchain /usr/local/rustup/ /usr/local/rustup/
ENV CARGO_HOME=/usr/local/cargo \
    RUSTUP_HOME=/usr/local/rustup \
    PATH=/usr/local/cargo/bin:${PATH} \
    RUSTFLAGS="-C target-feature=-crt-static" \
    CARGO_INCREMENTAL=0
# renovate: datasource=crate depName=shotgun
ARG SHOTGUN_VERSION=2.5.1
# renovate: datasource=crate depName=hacksaw
ARG HACKSAW_VERSION=1.0.4
RUN mkdir -p /x11-screenshot/usr/bin
RUN cargo install shotgun --version ${SHOTGUN_VERSION} --locked --root /build/cargo && \
    cargo install hacksaw --version ${HACKSAW_VERSION} --locked --root /build/cargo && \
    install -m0755 /build/cargo/bin/shotgun /build/cargo/bin/hacksaw /x11-screenshot/usr/bin/


# ===========================================================================
# Login manager — ly (TUI display manager). Built with the prebuilt musl Zig
# toolchain (Hadron's toolchain has no Zig). ly authenticates a user via PAM,
# creates the logind session, and launches either Wayland directly or XLibre
# plus the selected X session.
# ===========================================================================
FROM toolchain AS ly
COPY --from=libxcb /libxcb /
COPY --from=libxau /libxau /
COPY --from=xorgproto /xorgproto /
ARG ZIG_VERSION=0.16.0
# ly 1.4.x needs Zig 0.16.x.
# renovate: datasource=github-tags depName=fairyglade/ly extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG LY_VERSION=1.4.1
RUN mkdir -p /ly
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://ziglang.org/download/${ZIG_VERSION}/zig-x86_64-linux-${ZIG_VERSION}.tar.xz -o zig.tar.xz && tar -xf zig.tar.xz && rm zig.tar.xz && mv zig-x86_64-linux-* /opt/zig
ENV PATH=/opt/zig:$PATH
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -L https://github.com/fairyglade/ly/archive/refs/tags/v${LY_VERSION}.tar.gz -o ly.tar.gz && tar -xzf ly.tar.gz && rm ly.tar.gz && mv ly-* ly-src
WORKDIR /build/ly-src
# -Dcpu=baseline: Zig builds for the CPU of the build machine unless told
# otherwise. ly built on a runner with AVX-512 died with SIGILL ("trap invalid
# opcode") 8 ms after start on every CPU without it (AMD EPYC 7763 in CI), so
# the desktop never came up there. Build for the generic x86-64 baseline, like
# the C and Rust code in this image.
RUN zig build -Dcpu=baseline
RUN zig build installexe -Dcpu=baseline -Dinit_system=systemd -Ddest_directory=/ly


# ===========================================================================
# COSMIC desktop (DESKTOP=cosmic). See
# docs/superpowers/specs/2026-11-03-cosmic-desktop-design.md.
#
# Every COSMIC component is Rust, built from its epoch tag against Hadron's
# musl and the libraries this Dockerfile builds. Rust itself comes from the
# pinned Alpine image (only the toolchain; nothing it links ships), the same
# way Dockerfile.agent builds cua-driver. libclang for bindgen comes from the
# llvm stage and is used at build time only. BuildKit only evaluates these
# stages for DESKTOP=cosmic. The rust-toolchain stage is declared above the
# XLibre section, whose i3 screenshot tools use it too.
# ===========================================================================

# duktape: the small JavaScript engine polkit evaluates its rules with (the
# alternative is mozjs, a browser engine's worth of build).
FROM toolchain AS duktape
# renovate: datasource=github-tags depName=svaarala/duktape extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG DUKTAPE_VERSION=2.7.0
RUN mkdir -p /duktape
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://duktape.org/duktape-${DUKTAPE_VERSION}.tar.xz -o d.tar.xz && tar -xf d.tar.xz && mv duktape-* src
WORKDIR /build/src
RUN make -f Makefile.sharedlibrary INSTALL_PREFIX=/usr CC=cc && \
    make -f Makefile.sharedlibrary INSTALL_PREFIX=/duktape/usr install && \
    mkdir -p /duktape/usr/lib/pkgconfig && \
    printf 'prefix=/usr\nlibdir=${prefix}/lib\nincludedir=${prefix}/include\n\nName: duktape\nDescription: Embeddable Javascript engine\nVersion: %s\nLibs: -L${libdir} -lduktape\nLibs.private: -lm\nCflags: -I${includedir}\n' "${DUKTAPE_VERSION}" \
      > /duktape/usr/lib/pkgconfig/duktape.pc

# gettext's tools (msgfmt): polkit and accountsservice merge translations into
# their policy files at build time even with translations off. Build-only;
# nothing from this stage ships.
FROM toolchain AS gettext-tools
ARG GETTEXT_VERSION=0.22.5
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://ftp.gnu.org/pub/gnu/gettext/gettext-${GETTEXT_VERSION}.tar.xz -o g.tar.xz && tar -xf g.tar.xz && \
    cd gettext-${GETTEXT_VERSION} && \
    ./configure --prefix=/usr --disable-java --disable-csharp --disable-libasprintf \
      --disable-openmp --disable-acl --disable-curses --without-emacs --without-git \
      --disable-static --enable-shared && \
    make -j"$(nproc)" && make install DESTDIR=/gettext-tools

# polkit: lets an unprivileged desktop session ask for privileged actions
# (date and time, language, users) with a password prompt. cosmic-osd is the
# agent that shows the prompt. Without it, COSMIC Settings' system panels and
# cosmic-initial-setup silently fail. --libexecdir=lib for the same reason as
# flatpak: /usr/libexec is a persistent path on Kairos.
FROM toolchain AS polkit
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=duktape /duktape /
COPY --from=gettext-tools /gettext-tools /
COPY --from=libucontext /libucontext /
# renovate: datasource=github-tags depName=polkit-org/polkit extractVersion=^(?<version>\d+(\.\d+)*)$
ARG POLKIT_VERSION=126
RUN mkdir -p /polkit
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/polkit-org/polkit/archive/refs/tags/${POLKIT_VERSION}.tar.gz -o p.tar.gz && tar -xzf p.tar.gz && mv polkit-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} --libexecdir=lib \
      -Dsession_tracking=logind -Dsystemdsystemunitdir=/usr/lib/systemd/system \
      -Dauthfw=pam -Dpam_include=system-auth -Dgettext=false \
      -Dintrospection=false -Dman=false -Dgtk_doc=false -Dexamples=false -Dtests=false \
      -Dpolkitd_user=polkitd -Dprivileged_group=admin
RUN DESTDIR=/polkit ninja -C buildDir install

# accountsservice: the user-account D-Bus service COSMIC Settings' Users page
# and the greeter's user list talk to.
FROM toolchain AS accountsservice
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=duktape /duktape /
COPY --from=polkit /polkit /
COPY --from=gettext-tools /gettext-tools /
COPY --from=libucontext /libucontext /
# The GitLab release Alpine builds: it carries the musl fallbacks (shadow
# entries without fgetspent_r, the wtmp path) that the last tarball lacks.
# Its version script reads the version from the source directory's name.
# renovate: datasource=gitlab-tags depName=accountsservice/accountsservice registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG ACCOUNTSSERVICE_VERSION=26.27.3
RUN mkdir -p /accountsservice
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://gitlab.freedesktop.org/accountsservice/accountsservice/-/archive/${ACCOUNTSSERVICE_VERSION}/accountsservice-${ACCOUNTSSERVICE_VERSION}.tar.gz -o a.tar.gz && tar -xzf a.tar.gz
WORKDIR /build/accountsservice-${ACCOUNTSSERVICE_VERSION}
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} --libexecdir=lib \
      -Dsystemdsystemunitdir=/usr/lib/systemd/system -Delogind=false -Dlibxcrypt=false \
      -Dintrospection=false -Dvapi=false -Ddocbook=false -Dgtk_doc=false -Dtests=false \
      -Dadmin_group=admin -Dwtmpfile=/var/log/wtmp
RUN DESTDIR=/accountsservice ninja -C buildDir install

# libsndfile + libpulse: the PulseAudio client library the audio applet, OSD
# and settings link against. Only the client: PipeWire's pipewire-pulse is the
# server.
FROM toolchain AS libsndfile
# renovate: datasource=github-tags depName=libsndfile/libsndfile extractVersion=^(?<version>\d+(\.\d+)*)$
ARG LIBSNDFILE_VERSION=1.2.2
RUN mkdir -p /libsndfile
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/libsndfile/libsndfile/releases/download/${LIBSNDFILE_VERSION}/libsndfile-${LIBSNDFILE_VERSION}.tar.xz -o s.tar.xz && tar -xf s.tar.xz && mv libsndfile-* src
WORKDIR /build/src
RUN pip3 install ninja
RUN cmake -B build -G Ninja -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_BUILD_TYPE=MinSizeRel -DCMAKE_POLICY_VERSION_MINIMUM=3.5 \
      -DBUILD_SHARED_LIBS=ON -DENABLE_EXTERNAL_LIBS=OFF -DENABLE_MPEG=OFF \
      -DBUILD_PROGRAMS=OFF -DBUILD_EXAMPLES=OFF -DBUILD_TESTING=OFF -DENABLE_CPACK=OFF && \
    cmake --build build && DESTDIR=/libsndfile cmake --install build

FROM toolchain AS libpulse
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=libsndfile /libsndfile /
# renovate: datasource=gitlab-tags depName=pulseaudio/pulseaudio registryUrl=https://gitlab.freedesktop.org extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG PULSEAUDIO_VERSION=17.0
RUN mkdir -p /libpulse
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://www.freedesktop.org/software/pulseaudio/releases/pulseaudio-${PULSEAUDIO_VERSION}.tar.xz -o p.tar.xz && tar -xf p.tar.xz && mv pulseaudio-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} \
      -Ddaemon=false -Dclient=true -Ddoxygen=false -Dman=false -Dtests=false -Ddatabase=simple \
      -Dalsa=disabled -Dasyncns=disabled -Davahi=disabled -Dbluez5=disabled -Ddbus=enabled \
      -Dfftw=disabled -Dglib=enabled -Dgsettings=disabled -Dgtk=disabled -Djack=disabled \
      -Dlirc=disabled -Dopenssl=disabled -Dorc=disabled -Dsamplerate=disabled -Dsoxr=disabled \
      -Dspeex=disabled -Dsystemd=disabled -Dudev=disabled -Dx11=disabled -Dgstreamer=disabled \
      -Dwebrtc-aec=disabled -Dtcpwrap=disabled -Dvalgrind=disabled -Delogind=disabled \
      -Doss-output=disabled -Dconsolekit=disabled -Dbashcompletiondir=no -Dzshcompletiondir=no
RUN DESTDIR=/libpulse ninja -C buildDir install

# dav1d: AV1 decoding for the AVIF wallpapers COSMIC ships. No assembly, so no
# nasm in the toolchain; the C path is plenty for a few wallpapers.
FROM toolchain AS dav1d
# renovate: datasource=gitlab-tags depName=videolan/dav1d registryUrl=https://code.videolan.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG DAV1D_VERSION=1.5.1
RUN mkdir -p /dav1d
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://downloads.videolan.org/pub/videolan/dav1d/${DAV1D_VERSION}/dav1d-${DAV1D_VERSION}.tar.xz -o d.tar.xz && tar -xf d.tar.xz && mv dav1d-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Denable_asm=false -Denable_tools=false -Denable_tests=false -Denable_examples=false
RUN DESTDIR=/dav1d ninja -C buildDir install

# iso-codes: only the JSON tables and the .pc file COSMIC Settings' region page
# reads. Its build wants gettext for the translations, which this toolchain does
# not have and nothing here needs.
FROM toolchain AS iso-codes
# renovate: datasource=gitlab-tags depName=iso-codes-team/iso-codes registryUrl=https://salsa.debian.org extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG ISO_CODES_VERSION=4.18.0
RUN mkdir -p /iso-codes/usr/share/iso-codes/json /iso-codes/usr/share/pkgconfig
WORKDIR /build
# salsa makes the archive on request and can answer 200 with a truncated body,
# which curl does not retry, so download until the archive reads to the end.
RUN for i in 1 2 3 4 5; do \
      curl --retry-all-errors -fL --retry 5 https://salsa.debian.org/iso-codes-team/iso-codes/-/archive/v${ISO_CODES_VERSION}/iso-codes-v${ISO_CODES_VERSION}.tar.gz -o i.tar.gz && \
        tar -tzf i.tar.gz > /dev/null && break; \
      [ "$i" = 5 ] && exit 1; sleep 10; \
    done && tar -xzf i.tar.gz && \
    cp iso-codes-*/data/*.json /iso-codes/usr/share/iso-codes/json/ && \
    printf 'prefix=/usr\ndatarootdir=${prefix}/share\ndatadir=${datarootdir}\ndomains=iso_639-2 iso_639-3 iso_639-5 iso_3166-1 iso_3166-2 iso_3166-3 iso_4217 iso_15924\n\nName: iso-codes\nDescription: ISO country, subdivision, language, currency and script codes and their translations\nVersion: %s\n' "${ISO_CODES_VERSION}" \
      > /iso-codes/usr/share/pkgconfig/iso-codes.pc

# GStreamer core + base libraries: xdg-desktop-portal-cosmic records the screen
# with it for screen sharing. Plugins beyond the base libraries are off.
FROM toolchain AS gstreamer
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
# renovate: datasource=gitlab-tags depName=gstreamer/gstreamer registryUrl=https://gitlab.freedesktop.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG GSTREAMER_VERSION=1.26.7
RUN mkdir -p /gstreamer
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://gstreamer.freedesktop.org/src/gstreamer/gstreamer-${GSTREAMER_VERSION}.tar.xz -o g.tar.xz && tar -xf g.tar.xz && \
    curl --retry-all-errors -fL --retry 5 https://gstreamer.freedesktop.org/src/gst-plugins-base/gst-plugins-base-${GSTREAMER_VERSION}.tar.xz -o b.tar.xz && tar -xf b.tar.xz
RUN pip3 install meson ninja
WORKDIR /build/gstreamer-${GSTREAMER_VERSION}
RUN meson setup buildDir ${COMMON_MESON_FLAGS} --libexecdir=lib \
      -Dintrospection=disabled -Dnls=disabled -Dtests=disabled -Dexamples=disabled \
      -Dbenchmarks=disabled -Dtools=disabled -Ddoc=disabled -Dptp-helper=disabled \
      -Dlibunwind=disabled -Dlibdw=disabled -Dbash-completion=disabled -Dcheck=enabled && \
    ninja -C buildDir install && DESTDIR=/gstreamer ninja -C buildDir install
WORKDIR /build/gst-plugins-base-${GSTREAMER_VERSION}
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dauto_features=disabled \
      -Dintrospection=disabled -Dnls=disabled -Dtests=disabled -Dexamples=disabled \
      -Dtools=disabled -Ddoc=disabled -Dapp=enabled -Dvideoconvertscale=enabled \
      -Daudioconvert=enabled -Dvideorate=enabled -Daudioresample=enabled && \
    DESTDIR=/gstreamer ninja -C buildDir install

# xdg-desktop-portal: the broker Flatpak apps open files, take screenshots and
# share the screen through. xdg-desktop-portal-cosmic is its COSMIC backend.
FROM toolchain AS xdg-desktop-portal
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=json-glib /json-glib /
COPY --from=libfuse3 /libfuse3 /
COPY --from=pipewire /pipewire /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=libpng /libpng /
COPY --from=flatpak /flatpak /
COPY --from=libarchive /libarchive /
COPY --from=libxmlb /libxmlb /
COPY --from=appstream /appstream /
COPY --from=libyaml /libyaml /
COPY --from=ostree /ostree /
COPY --from=gpgme /gpgme /
COPY --from=libassuan /libassuan /
COPY --from=libgpg-error /libgpg-error /
COPY --from=bubblewrap /bubblewrap /
COPY --from=gstreamer /gstreamer /
# Hadron's libsystemd links libucontext; the linker needs it to resolve it.
COPY --from=libucontext /libucontext /
# renovate: datasource=github-tags depName=flatpak/xdg-desktop-portal extractVersion=^(?<version>\d+(\.\d+)*)$
ARG XDG_DESKTOP_PORTAL_VERSION=1.20.3
RUN mkdir -p /xdg-desktop-portal
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/flatpak/xdg-desktop-portal/releases/download/${XDG_DESKTOP_PORTAL_VERSION}/xdg-desktop-portal-${XDG_DESKTOP_PORTAL_VERSION}.tar.xz -o x.tar.xz && tar -xf x.tar.xz && mv xdg-desktop-portal-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} --libexecdir=lib \
      -Dsystemd=enabled -Ddocumentation=disabled -Dman-pages=disabled -Dtests=disabled \
      -Dgeoclue=disabled -Dsandboxed-image-validation=disabled -Dsandboxed-sound-validation=disabled \
      -Dflatpak-interfaces=enabled
RUN DESTDIR=/xdg-desktop-portal ninja -C buildDir install

# Xwayland: X11 apps (several popular Flatpaks still are) on the COSMIC
# compositor. XLibre does not ship it, so this is the upstream X.org release,
# reusing the X11 libraries built for the i3 variant.
FROM toolchain AS xwayland
COPY --from=wayland /wayland /
COPY --from=libdrm /libdrm /
COPY --from=mesa /mesa /
COPY --from=libepoxy /libepoxy /
COPY --from=pixman /pixman /
COPY --from=xorgproto /xorgproto /
COPY --from=libxau /libxau /
COPY --from=xcbproto /xcbproto /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=freetype /freetype /
COPY --from=libpng /libpng /
COPY --from=libucontext /libucontext /
# renovate: datasource=gitlab-tags depName=xorg/xserver registryUrl=https://gitlab.freedesktop.org extractVersion=^xwayland-(?<version>\d+(\.\d+)*)$
ARG XWAYLAND_VERSION=24.1.8
RUN mkdir -p /xwayland
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://xorg.freedesktop.org/archive/individual/xserver/xwayland-${XWAYLAND_VERSION}.tar.xz -o x.tar.xz && tar -xf x.tar.xz && mv xwayland-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} \
      -Dxvfb=false -Dxdmcp=false -Dxdm-auth-1=false -Dxcsecurity=true -Ddri3=true -Dglamor=true -Dglx=false \
      -Dsha1=libcrypto -Dsecure-rpc=false -Dlibdecor=false \
      -Dxwayland_ei=false -Ddocs=false -Ddevel-docs=false -Dlisten_tcp=false \
      -Dxkb_dir=/usr/share/X11/xkb -Dxkb_output_dir=/var/lib/xkb -Dxkb_bin_dir=/usr/bin
RUN DESTDIR=/xwayland ninja -C buildDir install

# The shared COSMIC build environment: every library a component links
# against, Rust, just, and libclang.
FROM toolchain AS cosmic-build
RUN rmdir /usr/lib/firmware 2>/dev/null || true
COPY --from=wayland /wayland /
COPY --from=libdrm /libdrm /
COPY --from=libucontext /libucontext /
COPY --from=mesa /mesa /
COPY --from=xkeyboard-config /xkeyboard-config /
COPY --from=libxkb /libxkb /
COPY --from=pixman /pixman /
COPY --from=libevdev /libevdev /
COPY --from=libinput /libinput /
COPY --from=hwdata /hwdata /
COPY --from=libdisplay-info /libdisplay-info /
COPY --from=libseat /libseat /
COPY --from=pcre2 /pcre2 /
COPY --from=glib2 /glib2 /
COPY --from=freetype /freetype /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=xorgproto /xorgproto /
COPY --from=libxau /libxau /
COPY --from=xcbproto /xcbproto /
COPY --from=libxcb /libxcb /
COPY --from=alsa-lib /alsa-lib /
COPY --from=pipewire /pipewire /
COPY --from=libsndfile /libsndfile /
COPY --from=libpulse /libpulse /
COPY --from=dav1d /dav1d /
COPY --from=iso-codes /iso-codes /
COPY --from=gstreamer /gstreamer /
COPY --from=xdg-desktop-portal /xdg-desktop-portal /
COPY --from=json-glib /json-glib /
COPY --from=libfuse3 /libfuse3 /
COPY --from=libgpg-error /libgpg-error /
COPY --from=libassuan /libassuan /
COPY --from=gpgme /gpgme /
COPY --from=libyaml /libyaml /
COPY --from=libxmlb /libxmlb /
COPY --from=appstream /appstream /
COPY --from=libarchive /libarchive /
COPY --from=ostree /ostree /
COPY --from=flatpak /flatpak /
COPY --from=llvm /llvm /
COPY --from=rust-toolchain /usr/local/cargo/ /usr/local/cargo/
COPY --from=rust-toolchain /usr/local/rustup/ /usr/local/rustup/
COPY --from=rust-toolchain /sbin/ldconfig /sbin/ldconfig
RUN sed -i '$a exit 0' /sbin/ldconfig && ldconfig
ENV CARGO_HOME=/usr/local/cargo \
    RUSTUP_HOME=/usr/local/rustup \
    PATH=/usr/local/cargo/bin:${PATH} \
    RUSTFLAGS="-C target-feature=-crt-static" \
    PKG_CONFIG_PATH=/usr/lib/pkgconfig:/usr/share/pkgconfig \
    LIBCLANG_PATH=/usr/lib \
    CFLAGS="-O2 -pipe" CXXFLAGS="-O2 -pipe" LDFLAGS="-Wl,--as-needed" \
    CARGO_PROFILE_RELEASE_LTO=thin \
    CARGO_INCREMENTAL=0
# renovate: datasource=crate depName=just
ARG JUST_VERSION=1.43.0
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry \
    cargo install just --version ${JUST_VERSION} --locked --root /usr/local
COPY tools/cosmic-build.sh /usr/local/bin/cosmic-build
WORKDIR /build

# greetd, the login daemon cosmic-greeter runs under, and pop-launcher, the
# search backend behind the COSMIC launcher.
FROM cosmic-build AS greetd
# renovate: datasource=github-tags depName=kennylevinsen/greetd
ARG GREETD_VERSION=0.10.3
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry \
    curl --retry-all-errors -fsSL --retry 5 https://github.com/kennylevinsen/greetd/archive/refs/tags/${GREETD_VERSION}.tar.gz | tar -xz && \
    cd greetd-${GREETD_VERSION} && cargo build --release --locked && \
    install -Dm755 target/release/greetd /greetd/usr/bin/greetd && \
    install -Dm755 target/release/agreety /greetd/usr/bin/agreety && \
    ! ldd /greetd/usr/bin/greetd | grep 'not found'

FROM cosmic-build AS pop-launcher
# renovate: datasource=github-tags depName=pop-os/cosmic-session extractVersion=^epoch-(?<version>\d+(\.\d+)*)$
ARG COSMIC_VERSION=1.11.0
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry \
    curl --retry-all-errors -fsSL --retry 5 https://github.com/pop-os/launcher/archive/refs/tags/epoch-${COSMIC_VERSION}.tar.gz | tar -xz && \
    cd launcher-epoch-${COSMIC_VERSION} && just build-release --locked && \
    just rootdir=/pop-launcher install && \
    ! ldd /pop-launcher/usr/bin/pop-launcher | grep 'not found'

# The COSMIC components, one RUN each so a failure re-runs only that one. They
# build in a cargo target dir on a cache mount, so nothing but the installed
# files reaches a layer, and each step empties it when done: the components'
# release profiles differ (panic strategy, LTO), so little compiled code is
# shared between them, and keeping it would grow by gigabytes per component.
#
# They are split in two stages, the shell (cosmic-shell) and the applications
# (cosmic-apps), of about two and a half hours each on a CI runner. One job
# building both ran past GitHub's six-hour limit. release.yml builds each half
# in its own job and pushes it as the cosmic-shell-dist or cosmic-apps-dist
# image, tagged by `make print-stage-key`; the cosmic build then swaps them in
# (`make COSMIC_SHELL_IMAGE=... COSMIC_APPS_IMAGE=...`).
FROM cosmic-build AS cosmic-shell
# renovate: datasource=github-tags depName=pop-os/cosmic-session extractVersion=^epoch-(?<version>\d+(\.\d+)*)$
ARG COSMIC_VERSION=1.11.0
ENV COSMIC_VERSION=${COSMIC_VERSION} COSMIC_OUT=/cosmic
ENV CARGO_TARGET_DIR=/cargo-target
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-comp
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-session
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-settings-daemon
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-panel && rm -rf /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-applets && rm -rf /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-bg && rm -rf /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-osd && rm -rf /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-notifications && rm -rf /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-launcher && rm -rf /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-app-library && rm -rf /cargo-target/*
# cosmic-workspaces installs with make, not just.
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked \
    mkdir -p /build/ws && curl --retry-all-errors -fsSL --retry 5 https://github.com/pop-os/cosmic-workspaces-epoch/archive/refs/tags/epoch-${COSMIC_VERSION}.tar.gz | tar -xz --strip-components=1 -C /build/ws && \
    cd /build/ws && cargo build --release --locked --bin cosmic-workspaces && \
    make install DESTDIR=/cosmic CARGO_TARGET_DIR=/cargo-target && \
    ! ldd /cosmic/usr/bin/cosmic-workspaces | grep 'not found' && \
    cd /build && rm -rf /build/ws /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-idle && rm -rf /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-randr && rm -rf /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-screenshot && rm -rf /cargo-target/*
FROM cosmic-build AS cosmic-apps
# renovate: datasource=github-tags depName=pop-os/cosmic-session extractVersion=^epoch-(?<version>\d+(\.\d+)*)$
ARG COSMIC_VERSION=1.11.0
ENV COSMIC_VERSION=${COSMIC_VERSION} COSMIC_OUT=/cosmic
ENV CARGO_TARGET_DIR=/cargo-target
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build xdg-desktop-portal-cosmic && rm -rf /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-settings && rm -rf /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked COSMIC_CARGO_ARGS="--package cosmic-files --package cosmic-files-applet --package cosmic-files-thumbnailer" cosmic-build cosmic-files && rm -rf /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-term && rm -rf /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-edit && rm -rf /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked cosmic-build cosmic-store && rm -rf /cargo-target/*
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked COSMIC_CARGO_ARGS="--all" cosmic-build cosmic-greeter && rm -rf /cargo-target/*
# cosmic-initial-setup reads the system locale from systemd-localed
# (org.freedesktop.locale1), which Hadron builds without, and gives up on the
# whole language page when it is absent: an empty list and no way past it. The
# sed makes a missing localed mean "no system locale set", so the page still
# lists the languages `locale -a` reports. Its crabtime macros also insist the
# target dir is named `target`, hence the nested CARGO_TARGET_DIR.
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry --mount=type=cache,id=cosmic-target,target=/cargo-target,sharing=locked \
    mkdir -p /build/is && curl --retry-all-errors -fsSL --retry 5 https://github.com/pop-os/cosmic-initial-setup/archive/refs/tags/epoch-${COSMIC_VERSION}.tar.gz | tar -xz --strip-components=1 -C /build/is && \
    cd /build/is && \
    sed -i 's/\.wrap_err("could not get locale from locale1")?/.unwrap_or_default()/' src/page/language.rs && \
    grep -q 'unwrap_or_default()' src/page/language.rs && ! grep -q 'could not get locale from locale1' src/page/language.rs && \
    export CARGO_TARGET_DIR=/cargo-target/target VERGEN_GIT_COMMIT_DATE=1970-01-01 VERGEN_GIT_SHA=epoch-${COSMIC_VERSION} && \
    cargo build --release --locked && just rootdir=/cosmic install && \
    ! ldd /cosmic/usr/bin/cosmic-initial-setup | grep 'not found' && \
    cd /build && rm -rf /build/is /cargo-target/*
# Data-only components: the icon theme (just) and the wallpapers (make).
RUN mkdir -p /build/icons /build/wallpapers && \
    curl --retry-all-errors -fsSL --retry 5 https://github.com/pop-os/cosmic-icons/archive/refs/tags/epoch-${COSMIC_VERSION}.tar.gz | tar -xz --strip-components=1 -C /build/icons && \
    curl --retry-all-errors -fsSL --retry 5 https://github.com/pop-os/cosmic-wallpapers/archive/refs/tags/epoch-${COSMIC_VERSION}.tar.gz | tar -xz --strip-components=1 -C /build/wallpapers && \
    (cd /build/icons && just rootdir=/cosmic install) && \
    (cd /build/wallpapers && make install DESTDIR=/cosmic prefix=/usr) && \
    rm -rf /build/icons /build/wallpapers

# Only the installed trees, for CI to push (see cosmic-shell above).
FROM scratch AS cosmic-shell-dist
COPY --from=cosmic-shell /cosmic /cosmic

FROM scratch AS cosmic-apps-dist
COPY --from=cosmic-apps /cosmic /cosmic

# Both halves in one tree. The applications are copied last, as they were
# installed last when one stage built everything.
FROM scratch AS cosmic
COPY --from=cosmic-shell /cosmic /cosmic
COPY --from=cosmic-apps /cosmic /cosmic

# musl-locales: the `locale` command musl lacks. cosmic-initial-setup and
# COSMIC Settings build their language lists from `locale -a`; without it the
# first-run wizard shows an empty list and cannot go on. Alpine's patch makes
# `locale -a` list every language, since musl has no compiled locales to
# enumerate (its C library messages stay English; app UIs are translated).
# Each name gets the .UTF-8 suffix glibc systems report, which is what
# cosmic-initial-setup filters on; musl accepts any locale name.
FROM toolchain AS musl-locales
COPY --from=gettext-tools /gettext-tools /
# renovate: datasource=gitlab-tags depName=adelie/musl-locales registryUrl=https://git.adelielinux.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG MUSL_LOCALES_VERSION=0.1.0
ARG APORTS_COMMIT=65780305dd2517157357121ccddbe95c84e6d5a4
WORKDIR /build
# git.adelielinux.org refuses connections from GitHub's runners (a release
# build waited 14 minutes for it). Alpine mirrors the same tarball (identical
# sha512, see aports main/musl-locales/APKBUILD) in its distfiles.
RUN { curl --connect-timeout 20 --retry 2 --retry-all-errors -fsSL -o musl-locales.tar.bz2 \
        https://git.adelielinux.org/adelie/musl-locales/-/archive/${MUSL_LOCALES_VERSION}/musl-locales-${MUSL_LOCALES_VERSION}.tar.bz2 || \
      curl --retry-all-errors -fsSL --retry 5 -o musl-locales.tar.bz2 \
        https://distfiles.alpinelinux.org/distfiles/v3.22/musl-locales-${MUSL_LOCALES_VERSION}.tar.bz2; } && \
    tar -xjf musl-locales.tar.bz2 && rm musl-locales.tar.bz2 && \
    cd musl-locales-${MUSL_LOCALES_VERSION} && \
    curl --retry-all-errors -fsSL --retry 5 https://raw.githubusercontent.com/alpinelinux/aports/${APORTS_COMMIT}/main/musl-locales/add-all-available-locales.patch | patch -p1 && \
    sed -i '/printf("a_DJ\\n/s/\\n/.UTF-8\\n/g' locale.c && grep -q 'de_DE.UTF-8' locale.c && \
    pip3 install ninja && \
    cmake -B build -G Ninja -DCMAKE_BUILD_TYPE=MinSizeRel -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_POLICY_VERSION_MINIMUM=3.5 && \
    cmake --build build && DESTDIR=/musl-locales cmake --install build && \
    /musl-locales/usr/bin/locale -a | grep -qx de_DE.UTF-8

# hicolor: the fallback icon theme every other theme inherits from, and where
# applications install their own icons. Only its index.theme (and the empty
# size directories it declares); without it no app icon resolves.
FROM toolchain AS hicolor-icon-theme
# renovate: datasource=gitlab-tags depName=xdg/default-icon-theme registryUrl=https://gitlab.freedesktop.org extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG HICOLOR_VERSION=0.18
WORKDIR /build
RUN curl --retry-all-errors -fsSL --retry 5 https://icon-theme.freedesktop.org/releases/hicolor-icon-theme-${HICOLOR_VERSION}.tar.xz | tar -xJ && \
    cd hicolor-icon-theme-${HICOLOR_VERSION} && pip3 install meson ninja && \
    meson setup build --prefix=/usr && DESTDIR=/hicolor-icon-theme ninja -C build install && \
    test -f /hicolor-icon-theme/usr/share/icons/hicolor/index.theme

# The UI fonts libcosmic defaults to (Open Sans, Noto Sans Mono), installed
# system-wide so Flatpak and Xwayland apps match the shell. Pinned commits.
FROM toolchain AS cosmic-fonts
ARG GOOGLE_FONTS_COMMIT=8b0a1d0f5983c89bc2b93f1b5fb55f9e252744b5
ARG NOTO_FONTS_COMMIT=8b36ddb234b06c091320e28985a3c96b48ce2596
RUN d=/cosmic-fonts/usr/share/fonts/cosmic && mkdir -p $d && \
    for f in "OpenSans%5Bwdth,wght%5D.ttf" "OpenSans-Italic%5Bwdth,wght%5D.ttf"; do \
      curl --retry-all-errors -fsSL --retry 5 "https://raw.githubusercontent.com/google/fonts/${GOOGLE_FONTS_COMMIT}/ofl/opensans/$f" -o "$d/$(printf %s "$f" | sed 's/%5B/[/;s/%5D/]/')" || exit 1; \
    done && \
    for f in NotoSansMono-Regular.ttf NotoSansMono-Bold.ttf; do \
      curl --retry-all-errors -fsSL --retry 5 "https://raw.githubusercontent.com/notofonts/notofonts.github.io/${NOTO_FONTS_COMMIT}/fonts/NotoSansMono/hinted/ttf/$f" -o "$d/$f" || exit 1; \
    done

# ===========================================================================
# niri + Noctalia desktop (DESKTOP=niri). See
# docs/superpowers/specs/2026-11-04-niri-noctalia-design.md.
#
# niri is a scrollable-tiling Wayland compositor (Rust); Noctalia is the shell
# around it: bar, dock, launcher, control center, notifications, lock screen,
# wallpaper and the polkit agent (C++23, no Qt or GTK). Both build from tagged
# releases against Hadron's musl. niri reuses COSMIC's Rust build environment;
# Noctalia's libraries below are plain C/C++ and build on the toolchain.
# BuildKit only evaluates these stages for DESKTOP=niri.
# ===========================================================================

# Header-only: stb's image resize and write headers, from a pinned commit.
# Upstream has since moved the resize header under deprecated/.
FROM toolchain AS stb
ARG STB_COMMIT=d14d46f6b44456eb88beac58cba833a4135d4c89
RUN mkdir -p /stb/usr/include/stb && \
    for h in deprecated/stb_image_resize2.h stb_image_write.h; do \
      curl --retry-all-errors -fsSL --retry 5 "https://raw.githubusercontent.com/nothings/stb/${STB_COMMIT}/$h" -o "/stb/usr/include/stb/${h##*/}" || exit 1; \
    done

FROM toolchain AS nlohmann-json
# renovate: datasource=github-tags depName=nlohmann/json extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG NLOHMANN_JSON_VERSION=3.12.0
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/nlohmann/json/releases/download/v${NLOHMANN_JSON_VERSION}/json.tar.xz -o j.tar.xz && tar -xf j.tar.xz && mv json src
WORKDIR /build/src
RUN cmake -B build -DCMAKE_INSTALL_PREFIX=/usr -DJSON_BuildTests=OFF -DJSON_Install=ON && \
    DESTDIR=/nlohmann-json cmake --install build

FROM toolchain AS tomlplusplus
# renovate: datasource=github-tags depName=marzer/tomlplusplus extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG TOMLPLUSPLUS_VERSION=3.4.0
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/marzer/tomlplusplus/archive/refs/tags/v${TOMLPLUSPLUS_VERSION}.tar.gz -o t.tar.gz && tar -xzf t.tar.gz && mv tomlplusplus-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dbuild_tests=false -Dbuild_examples=false && \
    DESTDIR=/tomlplusplus ninja -C buildDir install

FROM toolchain AS md4c
# renovate: datasource=github-tags depName=mity/md4c extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG MD4C_VERSION=0.6.0
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/mity/md4c/archive/refs/tags/v${MD4C_VERSION}.tar.gz -o m.tar.gz && tar -xzf m.tar.gz && mv md4c-* src
WORKDIR /build/src
RUN cmake -B build -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_BUILD_TYPE=MinSizeRel -DBUILD_SHARED_LIBS=ON && \
    cmake --build build && DESTDIR=/md4c cmake --install build

FROM toolchain AS libsodium
# renovate: datasource=github-tags depName=jedisct1/libsodium extractVersion=^(?<version>\d+(\.\d+)*)-RELEASE$
ARG LIBSODIUM_VERSION=1.0.22
RUN mkdir -p /libsodium
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/jedisct1/libsodium/releases/download/${LIBSODIUM_VERSION}-RELEASE/libsodium-${LIBSODIUM_VERSION}.tar.gz -o s.tar.gz && tar -xzf s.tar.gz && mv libsodium-* src
WORKDIR /build/src
RUN ./configure ${COMMON_CONFIGURE_ARGS} && make -j"$(nproc)" && make DESTDIR=/libsodium install

# sdbus-c++: the C++ D-Bus binding Noctalia talks to system services with.
# Hadron's libsystemd links libucontext, so the linker needs it too.
FROM toolchain AS sdbus-cpp
COPY --from=libucontext /libucontext /
# renovate: datasource=github-tags depName=Kistler-Group/sdbus-cpp extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG SDBUS_CPP_VERSION=2.3.1
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/Kistler-Group/sdbus-cpp/archive/refs/tags/v${SDBUS_CPP_VERSION}.tar.gz -o s.tar.gz && tar -xzf s.tar.gz && mv sdbus-cpp-* src
WORKDIR /build/src
RUN cmake -B build -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_BUILD_TYPE=MinSizeRel \
      -DSDBUSCPP_BUILD_DOCS=OFF -DSDBUSCPP_BUILD_TESTS=OFF -DSDBUSCPP_BUILD_CODEGEN=OFF -DSDBUSCPP_BUILD_EXAMPLES=OFF && \
    cmake --build build && DESTDIR=/sdbus-cpp cmake --install build

# libical: calendar events (the clock panel's agenda, CalDAV). System time
# zone data, no GLib binding.
FROM toolchain AS libical
# renovate: datasource=github-tags depName=libical/libical extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG LIBICAL_VERSION=4.0.7
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/libical/libical/archive/refs/tags/v${LIBICAL_VERSION}.tar.gz -o i.tar.gz && tar -xzf i.tar.gz && mv libical-* src
WORKDIR /build/src
RUN pip3 install ninja
RUN cmake -B build -G Ninja -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_BUILD_TYPE=MinSizeRel \
      -DLIBICAL_GLIB=OFF -DLIBICAL_GOBJECT_INTROSPECTION=OFF -DLIBICAL_JAVA_BINDINGS=OFF -DLIBICAL_CXX_BINDINGS=OFF \
      -DLIBICAL_BUILD_DOCS=OFF -DLIBICAL_BUILD_TESTING=OFF -DLIBICAL_BUILD_EXAMPLES=OFF -DLIBICAL_STATIC=OFF \
      -DCMAKE_DISABLE_FIND_PACKAGE_ICU=ON -DCMAKE_DISABLE_FIND_PACKAGE_BerkeleyDB=ON && \
    cmake --build build && DESTDIR=/libical cmake --install build

FROM toolchain AS libwebp
# renovate: datasource=github-tags depName=webmproject/libwebp extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG LIBWEBP_VERSION=1.6.0
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://storage.googleapis.com/downloads.webmproject.org/releases/webp/libwebp-${LIBWEBP_VERSION}.tar.gz -o w.tar.gz && tar -xzf w.tar.gz && mv libwebp-* src
WORKDIR /build/src
RUN cmake -B build -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_BUILD_TYPE=MinSizeRel -DBUILD_SHARED_LIBS=ON \
      -DWEBP_BUILD_ANIM_UTILS=OFF -DWEBP_BUILD_CWEBP=OFF -DWEBP_BUILD_DWEBP=OFF -DWEBP_BUILD_GIF2WEBP=OFF \
      -DWEBP_BUILD_IMG2WEBP=OFF -DWEBP_BUILD_VWEBP=OFF -DWEBP_BUILD_WEBPINFO=OFF -DWEBP_BUILD_WEBPMUX=OFF \
      -DWEBP_BUILD_EXTRAS=OFF && \
    cmake --build build && DESTDIR=/libwebp cmake --install build

# libjxl (JPEG XL) and the three libraries it needs, all from the system rather
# than its git submodules, which a release tarball does not carry.
FROM toolchain AS brotli
# renovate: datasource=github-tags depName=google/brotli extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG BROTLI_VERSION=1.2.1
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/google/brotli/archive/refs/tags/v${BROTLI_VERSION}.tar.gz -o b.tar.gz && tar -xzf b.tar.gz && mv brotli-* src
WORKDIR /build/src
RUN cmake -B build -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_BUILD_TYPE=MinSizeRel -DBUILD_SHARED_LIBS=ON -DBROTLI_DISABLE_TESTS=ON && \
    cmake --build build && DESTDIR=/brotli cmake --install build

FROM toolchain AS highway
# renovate: datasource=github-tags depName=google/highway extractVersion=^(?<version>\d+(\.\d+)*)$
ARG HIGHWAY_VERSION=1.4.0
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/google/highway/archive/refs/tags/${HIGHWAY_VERSION}.tar.gz -o h.tar.gz && tar -xzf h.tar.gz && mv highway-* src
WORKDIR /build/src
RUN cmake -B build -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=ON \
      -DBUILD_TESTING=OFF -DHWY_ENABLE_TESTS=OFF -DHWY_ENABLE_EXAMPLES=OFF -DHWY_ENABLE_CONTRIB=OFF -DHWY_ENABLE_INSTALL=ON && \
    cmake --build build && DESTDIR=/highway cmake --install build

FROM toolchain AS lcms2
# renovate: datasource=github-tags depName=mm2/Little-CMS extractVersion=^lcms(?<version>\d+(\.\d+)*)$
ARG LCMS2_VERSION=2.20
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/mm2/Little-CMS/releases/download/lcms${LCMS2_VERSION}/lcms2-${LCMS2_VERSION}.tar.gz -o l.tar.gz && tar -xzf l.tar.gz && mv lcms2-* src
WORKDIR /build/src
RUN ./configure ${COMMON_CONFIGURE_ARGS} && make -j"$(nproc)" && make DESTDIR=/lcms2 install

FROM toolchain AS libjxl
COPY --from=brotli /brotli /
COPY --from=highway /highway /
COPY --from=lcms2 /lcms2 /
# renovate: datasource=github-tags depName=libjxl/libjxl extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG LIBJXL_VERSION=0.12.0
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/libjxl/libjxl/archive/refs/tags/v${LIBJXL_VERSION}.tar.gz -o j.tar.gz && tar -xzf j.tar.gz && mv libjxl-* src
WORKDIR /build/src
RUN pip3 install ninja
RUN cmake -B build -G Ninja -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=ON \
      -DBUILD_TESTING=OFF -DJPEGXL_ENABLE_TOOLS=OFF -DJPEGXL_ENABLE_DOXYGEN=OFF -DJPEGXL_ENABLE_MANPAGES=OFF \
      -DJPEGXL_ENABLE_BENCHMARK=OFF -DJPEGXL_ENABLE_EXAMPLES=OFF -DJPEGXL_ENABLE_JNI=OFF -DJPEGXL_ENABLE_SJPEG=OFF \
      -DJPEGXL_ENABLE_OPENEXR=OFF -DJPEGXL_ENABLE_SKCMS=OFF -DJPEGXL_ENABLE_JPEGLI=OFF -DJPEGXL_ENABLE_FUZZERS=OFF \
      -DJPEGXL_FORCE_SYSTEM_BROTLI=ON -DJPEGXL_FORCE_SYSTEM_HWY=ON -DJPEGXL_FORCE_SYSTEM_LCMS2=ON && \
    cmake --build build && DESTDIR=/libjxl cmake --install build

# libqalculate: the launcher's calculator (arithmetic, units, currencies). Its
# number types sit on GMP and MPFR. 5.13.1 is the newest release with a
# generated source tarball.
FROM toolchain AS gmp
ARG GMP_VERSION=6.3.0
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://ftp.gnu.org/gnu/gmp/gmp-${GMP_VERSION}.tar.xz -o g.tar.xz && tar -xf g.tar.xz && mv gmp-* src
WORKDIR /build/src
# GMP's configure probes predate C23, where `void g()` means no arguments.
RUN CFLAGS="-O2 -std=gnu17" ./configure --prefix=/usr --enable-cxx --enable-shared --disable-static && \
    make -j"$(nproc)" && make DESTDIR=/gmp install

FROM toolchain AS mpfr
COPY --from=gmp /gmp /
ARG MPFR_VERSION=4.2.2
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://ftp.gnu.org/gnu/mpfr/mpfr-${MPFR_VERSION}.tar.xz -o m.tar.xz && tar -xf m.tar.xz && mv mpfr-* src
WORKDIR /build/src
# Not COMMON_CONFIGURE_ARGS: MPFR rejects --enable-lto for a shared library.
RUN ./configure --prefix=/usr --enable-shared --disable-static && make -j"$(nproc)" && make DESTDIR=/mpfr install

FROM toolchain AS libqalculate
COPY --from=gmp /gmp /
COPY --from=mpfr /mpfr /
COPY --from=gettext-tools /gettext-tools /
# renovate: datasource=github-tags depName=Qalculate/libqalculate extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG LIBQALCULATE_VERSION=5.13.1
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/Qalculate/libqalculate/releases/download/v${LIBQALCULATE_VERSION}/libqalculate-${LIBQALCULATE_VERSION}.tar.gz -o q.tar.gz && tar -xzf q.tar.gz && mv libqalculate-* src
WORKDIR /build/src
RUN ./configure --prefix=/usr --enable-shared --disable-static --disable-textport --without-icu --without-readline --disable-nls && \
    make -j"$(nproc)" && make DESTDIR=/libqalculate install

# libsecret: the Secret Service client library (stored credentials).
FROM toolchain AS libsecret
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=libgpg-error /libgpg-error /
COPY --from=libgcrypt /libgcrypt /
# renovate: datasource=gitlab-tags depName=GNOME/libsecret registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG LIBSECRET_VERSION=0.21.8
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://download.gnome.org/sources/libsecret/${LIBSECRET_VERSION%.*}/libsecret-${LIBSECRET_VERSION}.tar.xz -o s.tar.xz && tar -xf s.tar.xz && mv libsecret-* src
WORKDIR /build/src
RUN pip3 install meson ninja
# secret-file-collection.c calls open() relying on glibc to pull in fcntl.h;
# fixed on libsecret's main branch, not yet in a release.
RUN sed -i 's|^#include "config.h"|&\n#include <fcntl.h>|' libsecret/secret-file-collection.c
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dmanpage=false -Dvapi=false -Dgtk_doc=false -Dintrospection=false \
      -Dbash_completion=disabled -Dtpm2=false -Dpam=false -Dcrypto=libgcrypt && \
    DESTDIR=/libsecret ninja -C buildDir install

# gnome-keyring: the Secret Service (org.freedesktop.secrets) on every variant,
# so Electron and Chromium apps, Flatpak ones included, keep passwords in an
# encrypted keyring instead of a plain text file. pam_gnome_keyring.so unlocks
# the "login" keyring with the login password: ly's and greetd's PAM services
# already call it as an optional module. It needs gcr 3 (gck-1, gcr-base-3),
# which needs p11-kit. No gcr-prompter (that is GTK): a keyring that the login
# did not unlock cannot be unlocked by a dialog.
FROM toolchain AS gnome-keyring
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=libgpg-error /libgpg-error /
COPY --from=libgcrypt /libgcrypt /
COPY --from=gettext-tools /gettext-tools /
# Hadron's libsystemd links libucontext (musl has no getcontext).
COPY --from=libucontext /libucontext /
# renovate: datasource=github-releases depName=p11-glue/p11-kit
ARG P11_KIT_VERSION=0.26.5
# renovate: datasource=gitlab-tags depName=GNOME/gcr registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>3\.\d+(\.\d+)*)$
ARG GCR3_VERSION=3.41.2
# renovate: datasource=gitlab-tags depName=GNOME/gnome-keyring registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG GNOME_KEYRING_VERSION=51.1
RUN pip3 install meson ninja
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/p11-glue/p11-kit/releases/download/${P11_KIT_VERSION}/p11-kit-${P11_KIT_VERSION}.tar.xz -o p.tar.xz && \
    curl --retry-all-errors -fL --retry 5 https://download.gnome.org/sources/gcr/${GCR3_VERSION%.*}/gcr-${GCR3_VERSION}.tar.xz -o g.tar.xz && \
    curl --retry-all-errors -fL --retry 5 https://download.gnome.org/sources/gnome-keyring/${GNOME_KEYRING_VERSION%.*}/gnome-keyring-${GNOME_KEYRING_VERSION}.tar.xz -o k.tar.xz && \
    for f in p g k; do tar -xf $f.tar.xz && rm $f.tar.xz; done
# Each project installs into the build root too, so the next one finds it.
# DESTDIR=/ for that: without a DESTDIR gcr's post-install script runs
# gtk-update-icon-cache, which is not here.
# p11-kit without its trust module: Hadron's CA bundle comes from the base
# image, and gcr only needs the PKCS#11 module loader.
RUN cd p11-kit-* && \
    meson setup buildDir ${COMMON_MESON_FLAGS} -Dtrust_module=disabled -Dlibffi=disabled \
      -Dsystemd=disabled -Dbash_completion=disabled -Dzsh_completion=disabled \
      -Dgtk_doc=false -Dman=false -Dnls=false -Dtest=false && \
    DESTDIR=/gnome-keyring ninja -C buildDir install && DESTDIR=/ ninja -C buildDir install
# gpg_path is only a string gcr would run for OpenPGP keys; the keyring never
# does, and Hadron's gpg is not in this stage. gcr 3.41.2 asks for the path of
# ssh-add and ssh-agent even with ssh_agent=false, so build-only stubs stand in.
# musl declares getpass() (console-interaction.c) only under _GNU_SOURCE.
RUN printf '#!/bin/sh\nexit 1\n' > /usr/local/bin/ssh-add && cp /usr/local/bin/ssh-add /usr/local/bin/ssh-agent && \
    chmod +x /usr/local/bin/ssh-add /usr/local/bin/ssh-agent && \
    cd gcr-* && \
    CFLAGS="${CFLAGS:-} -D_GNU_SOURCE" meson setup buildDir ${COMMON_MESON_FLAGS} -Dgtk=false -Dintrospection=false \
      -Dgtk_doc=false -Dssh_agent=false -Dsystemd=disabled -Dgpg_path=/usr/bin/gpg && \
    DESTDIR=/gnome-keyring ninja -C buildDir install && DESTDIR=/ ninja -C buildDir install
# The toolchain has libsystemd.pc but no systemd.pc, which gnome-keyring reads
# the user unit directory from.
RUN printf 'prefix=/usr\nsystemduserunitdir=${prefix}/lib/systemd/user\n\nName: systemd\nDescription: systemd\nVersion: 257\n' \
      > /usr/lib/pkgconfig/systemd.pc
RUN cd gnome-keyring-* && \
    meson setup buildDir ${COMMON_MESON_FLAGS} -Dpam=true -Dssh-agent=false -Dsystemd=enabled \
      -Dselinux=disabled -Dlibcap-ng=disabled -Ddebug-mode=false -Dmanpage=false && \
    DESTDIR=/gnome-keyring ninja -C buildDir install
# Headers, pkg-config files and the test-only modules (devel/) are not
# shipped. gcr installs D-Bus activation files for gcr-prompter even when it
# does not build it (-Dgtk=false).
RUN rm -rf /gnome-keyring/usr/include /gnome-keyring/usr/lib/pkgconfig \
      /gnome-keyring/usr/share/gtk-doc /gnome-keyring/usr/share/vala /gnome-keyring/usr/share/GConf \
      /gnome-keyring/usr/share/dbus-1/services/org.gnome.keyring.SystemPrompter.service \
      /gnome-keyring/usr/share/dbus-1/services/org.gnome.keyring.PrivatePrompter.service \
      /gnome-keyring/usr/lib/gnome-keyring/devel && \
    test -x /gnome-keyring/usr/bin/gnome-keyring-daemon && \
    test -f /gnome-keyring/usr/lib/security/pam_gnome_keyring.so && \
    test -f /gnome-keyring/usr/lib/systemd/user/gnome-keyring-daemon.socket && \
    test -f /gnome-keyring/usr/share/dbus-1/services/org.freedesktop.secrets.service

# niri's build environment: COSMIC's (Rust, libclang, the Wayland/input/GPU
# stack, PipeWire) plus cairo and pango, which niri draws its overlays with.
FROM cosmic-build AS niri-build
COPY --from=pixman /pixman /
COPY --from=cairo /cairo /
COPY --from=pango /pango /
COPY --from=x11-libs /x11-libs /
COPY --from=xwayland /xwayland /
RUN ldconfig

# librsvg (SVG icons and wallpapers) is Rust behind a C API, built with
# cargo-c. cargo-c itself is compiled here, against Hadron's libraries.
FROM niri-build AS librsvg
# renovate: datasource=crate depName=cargo-c
ARG CARGO_C_VERSION=0.10.26
# renovate: datasource=gitlab-tags depName=GNOME/librsvg registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG LIBRSVG_VERSION=2.63.4
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry \
    cargo install cargo-c --version ${CARGO_C_VERSION} --locked --features vendored-openssl --root /usr/local
RUN curl --retry-all-errors -fL --retry 5 https://download.gnome.org/sources/librsvg/${LIBRSVG_VERSION%.*}/librsvg-${LIBRSVG_VERSION}.tar.xz -o r.tar.xz && tar -xf r.tar.xz && mv librsvg-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry \
    meson setup buildDir ${COMMON_MESON_FLAGS} -Dintrospection=disabled -Dpixbuf=disabled -Dpixbuf-loader=disabled \
      -Drsvg-convert=disabled -Ddocs=disabled -Dvala=disabled -Dtests=false -Davif=disabled && \
    DESTDIR=/librsvg ninja -C buildDir install && \
    rm -rf buildDir && \
    ! ldd /librsvg/usr/lib/librsvg-2.so.2 | grep 'not found'

# niri's release profile keeps line-table debug info (~130 MB binary); the
# installed copy is stripped.
FROM niri-build AS niri
# renovate: datasource=github-tags depName=YaLTeR/niri extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG NIRI_VERSION=26.04
ENV CARGO_TARGET_DIR=/cargo-target
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=niri-target,target=/cargo-target,sharing=locked \
    curl --retry-all-errors -fsSL --retry 5 https://github.com/YaLTeR/niri/archive/refs/tags/v${NIRI_VERSION}.tar.gz | tar -xz && \
    cd niri-${NIRI_VERSION} && \
    cargo build --release --locked && \
    install -s -Dm755 /cargo-target/release/niri /niri/usr/bin/niri && \
    install -Dm755 resources/niri-session /niri/usr/bin/niri-session && \
    install -Dm644 resources/niri.desktop /niri/usr/share/wayland-sessions/niri.desktop && \
    install -Dm644 resources/niri-portals.conf /niri/usr/share/xdg-desktop-portal/niri-portals.conf && \
    install -Dm644 resources/niri.service /niri/usr/lib/systemd/user/niri.service && \
    install -Dm644 resources/niri-shutdown.target /niri/usr/lib/systemd/user/niri-shutdown.target && \
    rm -rf /cargo-target/* && \
    ! ldd /niri/usr/bin/niri | grep 'not found' && \
    /niri/usr/bin/niri --version

# xwayland-satellite: runs Xwayland as an ordinary Wayland client, so X11 apps
# work under niri. niri starts it on demand when it is on PATH.
FROM niri-build AS xwayland-satellite
# renovate: datasource=github-tags depName=Supreeeme/xwayland-satellite extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG XWAYLAND_SATELLITE_VERSION=0.8.3
ENV CARGO_TARGET_DIR=/cargo-target
RUN --mount=type=cache,id=cosmic-cargo-registry,target=/usr/local/cargo/registry \
    --mount=type=cache,id=niri-target,target=/cargo-target,sharing=locked \
    curl --retry-all-errors -fsSL --retry 5 https://github.com/Supreeeme/xwayland-satellite/archive/refs/tags/v${XWAYLAND_SATELLITE_VERSION}.tar.gz | tar -xz && \
    cd xwayland-satellite-${XWAYLAND_SATELLITE_VERSION} && \
    cargo build --release --locked -F systemd && \
    install -s -Dm755 /cargo-target/release/xwayland-satellite /xwayland-satellite/usr/bin/xwayland-satellite && \
    rm -rf /cargo-target/* && \
    ! ldd /xwayland-satellite/usr/bin/xwayland-satellite | grep 'not found'

# Noctalia, the desktop shell. Its runtime assets (fonts, translations,
# templates, sounds) install under /usr/share/noctalia and must ship with it.
FROM toolchain AS noctalia
COPY --from=wayland /wayland /
COPY --from=libdrm /libdrm /
COPY --from=libucontext /libucontext /
COPY --from=mesa /mesa /
COPY --from=libxkb /libxkb /
COPY --from=xkeyboard-config /xkeyboard-config /
COPY --from=pcre2 /pcre2 /
COPY --from=glib2 /glib2 /
COPY --from=freetype /freetype /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pixman /pixman /
COPY --from=cairo /cairo /
COPY --from=pango /pango /
COPY --from=libepoxy /libepoxy /
COPY --from=duktape /duktape /
COPY --from=polkit /polkit /
COPY --from=alsa-lib /alsa-lib /
COPY --from=pipewire /pipewire /
COPY --from=wireplumber /wireplumber /
COPY --from=libsndfile /libsndfile /
COPY --from=libgpg-error /libgpg-error /
COPY --from=libgcrypt /libgcrypt /
COPY --from=libsecret /libsecret /
COPY --from=libsodium /libsodium /
COPY --from=sdbus-cpp /sdbus-cpp /
COPY --from=librsvg /librsvg /
COPY --from=gmp /gmp /
COPY --from=mpfr /mpfr /
COPY --from=libqalculate /libqalculate /
COPY --from=md4c /md4c /
COPY --from=nlohmann-json /nlohmann-json /
COPY --from=tomlplusplus /tomlplusplus /
COPY --from=libical /libical /
COPY --from=stb /stb /
COPY --from=libwebp /libwebp /
COPY --from=brotli /brotli /
COPY --from=highway /highway /
COPY --from=lcms2 /lcms2 /
COPY --from=libjxl /libjxl /
# renovate: datasource=github-tags depName=noctalia-dev/noctalia extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG NOCTALIA_VERSION=5.4.1
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/noctalia-dev/noctalia/archive/refs/tags/v${NOCTALIA_VERSION}.tar.gz -o n.tar.gz && tar -xzf n.tar.gz && mv noctalia-${NOCTALIA_VERSION} src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir --prefix=/usr --libdir=lib --buildtype=release -Dstrip=true -Dtests=disabled -Djemalloc=disabled && \
    ninja -C buildDir && DESTDIR=/noctalia ninja -C buildDir install
RUN ! ldd /noctalia/usr/bin/noctalia | grep 'not found' && \
    test -f /noctalia/usr/share/noctalia/assets/emoji.json

# The Adwaita cursor theme, cursors only (not the icon set), registered as the
# system default. Without a cursor theme sway, i3, niri and GTK apps fall back
# to a bare built-in arrow and no resize or text cursors.
FROM toolchain AS adwaita-cursors
# renovate: datasource=gitlab-tags depName=GNOME/adwaita-icon-theme registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG ADWAITA_ICON_THEME_VERSION=51.0
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://download.gnome.org/sources/adwaita-icon-theme/${ADWAITA_ICON_THEME_VERSION%%.*}/adwaita-icon-theme-${ADWAITA_ICON_THEME_VERSION}.tar.xz -o a.tar.xz && tar -xf a.tar.xz && mv adwaita-icon-theme-* src
RUN d=/adwaita-cursors/usr/share/icons && mkdir -p $d/Adwaita $d/default && \
    cp -a src/Adwaita/cursors $d/Adwaita/cursors && \
    printf '[Icon Theme]\nName=Adwaita\nComment=The Adwaita cursor theme\n' > $d/Adwaita/index.theme && \
    printf '[Icon Theme]\nInherits=Adwaita\n' > $d/default/index.theme && \
    test -e $d/Adwaita/cursors/default

# GSettings schemas for the GTK portal's Settings interface, which is how
# Flatpak apps learn the colour scheme (org.freedesktop.appearance
# color-scheme) and, for GTK3 apps, the theme and fonts. xdg-desktop-portal-gtk
# reads them from org.gnome.desktop.interface. Hadron's defaults are a vendor
# override: dark colour scheme, Adwaita-dark for GTK3, and the Hadron fonts
# (the hadron-fonts stage) by their real family names, because a Flatpak sees
# the host's font files but not its fontconfig aliases. GTK3 has the dark
# Adwaita built in only as a variant, so the theme named Adwaita-dark is a
# one-line stylesheet that imports it.
#
# No dconf: without it GLib uses its in-memory backend, which still reads the
# compiled schemas with this override, so every app and portal sees the dark
# defaults. dconf would only add per-user changes saved across logins, for a
# daemon, a GIO module and its cache in every image. The default stage
# compiles the schemas (glib-compile-schemas); installs under DESTDIR skip it.
FROM toolchain AS gsettings-desktop-schemas
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
# renovate: datasource=gitlab-tags depName=GNOME/gsettings-desktop-schemas registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG GSETTINGS_SCHEMAS_VERSION=47.1
RUN mkdir -p /gsettings-desktop-schemas
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://download.gnome.org/sources/gsettings-desktop-schemas/${GSETTINGS_SCHEMAS_VERSION%.*}/gsettings-desktop-schemas-${GSETTINGS_SCHEMAS_VERSION}.tar.xz -o s.tar.xz && tar -xf s.tar.xz && rm s.tar.xz && mv gsettings-desktop-schemas-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dintrospection=false && \
    DESTDIR=/gsettings-desktop-schemas ninja -C buildDir install
RUN d=/gsettings-desktop-schemas/usr/share && \
    printf '%s\n' '# Hadron Desktop defaults: dark apps and the Hadron fonts, including Flatpak apps through the portal.' \
      '[org.gnome.desktop.interface]' "color-scheme='prefer-dark'" "gtk-theme='Adwaita-dark'" \
      "font-name='Inter Variable 10.5'" "document-font-name='Inter Variable 10.5'" \
      "monospace-font-name='JetBrains Mono 10.5'" \
      > $d/glib-2.0/schemas/90_hadron.gschema.override && \
    mkdir -p $d/themes/Adwaita-dark/gtk-3.0 && \
    printf '@import url("resource:///org/gtk/libgtk/theme/Adwaita/gtk-contained-dark.css");\n' \
      > $d/themes/Adwaita-dark/gtk-3.0/gtk.css && \
    cp -a $d/glib-2.0/schemas /tmp/schemas && glib-compile-schemas --strict /tmp/schemas && \
    test "$(gsettings --schemadir /tmp/schemas get org.gnome.desktop.interface color-scheme 2>/dev/null)" = "'prefer-dark'" && \
    test "$(gsettings --schemadir /tmp/schemas get org.gnome.desktop.interface font-name 2>/dev/null)" = "'Inter Variable 10.5'" && \
    rm -rf /tmp/schemas

# xdg-desktop-portal-gtk: the file chooser (and app chooser, print, settings)
# Flatpak apps open through the portal, on Sway and niri (i3 has its own X11
# build below). niri's portals.conf prefers gnome, then gtk; the GNOME backend
# needs GTK4 and mutter interfaces, so gtk it is. Its Settings interface serves
# the dark colour scheme from the gsettings-desktop-schemas stage.
FROM toolchain AS xdg-desktop-portal-gtk
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=cairo /cairo /
COPY --from=pango /pango /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pixman /pixman /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=atk /atk /
COPY --from=libepoxy /libepoxy /
COPY --from=wayland /wayland /
COPY --from=libxkb /libxkb /
COPY --from=mesa /mesa /
COPY --from=libdrm /libdrm /
COPY --from=gtk3 /gtk3 /
COPY --from=xdg-desktop-portal /xdg-desktop-portal /
COPY --from=gettext-tools /gettext-tools /
COPY --from=gsettings-desktop-schemas /gsettings-desktop-schemas /
# renovate: datasource=github-tags depName=flatpak/xdg-desktop-portal-gtk extractVersion=^(?<version>\d+(\.\d+)*)$
ARG XDG_DESKTOP_PORTAL_GTK_VERSION=1.15.3
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/flatpak/xdg-desktop-portal-gtk/releases/download/${XDG_DESKTOP_PORTAL_GTK_VERSION}/xdg-desktop-portal-gtk-${XDG_DESKTOP_PORTAL_GTK_VERSION}.tar.xz -o x.tar.xz && tar -xf x.tar.xz && mv xdg-desktop-portal-gtk-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} --libexecdir=lib -Dwallpaper=disabled -Dsettings=enabled -Dlockdown=disabled && \
    DESTDIR=/xdg-desktop-portal-gtk ninja -C buildDir install && \
    ! ldd /xdg-desktop-portal-gtk/usr/lib/xdg-desktop-portal-gtk | grep 'not found'

# inih: the INI parser xdg-desktop-portal-wlr reads its config with.
FROM toolchain AS inih
# renovate: datasource=github-tags depName=benhoyt/inih extractVersion=^r(?<version>\d+)$
ARG INIH_VERSION=62
RUN mkdir -p /inih
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://github.com/benhoyt/inih/archive/refs/tags/r${INIH_VERSION}.tar.gz -o inih.tar.gz && tar -xf inih.tar.gz && rm inih.tar.gz && mv inih-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Ddefault_library=shared -Ddistro_install=true && \
    DESTDIR=/inih ninja -C buildDir install

# xdg-desktop-portal-wlr: the Screenshot and ScreenCast backends on Sway. It
# captures through wlroots' screencopy protocol and streams through PipeWire;
# a screen share asks which output with slurp.
FROM toolchain AS xdg-desktop-portal-wlr
COPY --from=libucontext /libucontext /
COPY --from=wayland /wayland /
COPY --from=libdrm /libdrm /
COPY --from=mesa /mesa /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=alsa-lib /alsa-lib /
COPY --from=pipewire /pipewire /
COPY --from=inih /inih /
# renovate: datasource=github-tags depName=emersion/xdg-desktop-portal-wlr extractVersion=^v(?<version>\d+(\.\d+)*)$
ARG XDG_DESKTOP_PORTAL_WLR_VERSION=0.8.4
RUN mkdir -p /xdg-desktop-portal-wlr
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://github.com/emersion/xdg-desktop-portal-wlr/releases/download/v${XDG_DESKTOP_PORTAL_WLR_VERSION}/xdg-desktop-portal-wlr-${XDG_DESKTOP_PORTAL_WLR_VERSION}.tar.gz -o x.tar.gz && tar -xf x.tar.gz && rm x.tar.gz && mv xdg-desktop-portal-wlr-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} --libexecdir=lib -Dsd-bus-provider=libsystemd -Dsystemd=enabled -Dman-pages=disabled && \
    DESTDIR=/xdg-desktop-portal-wlr ninja -C buildDir install && \
    ! ldd /xdg-desktop-portal-wlr/usr/lib/xdg-desktop-portal-wlr | grep 'not found'

# at-spi2-core: ATK and the AT-SPI bridge. GTK3's X11 backend requires the
# bridge (the Wayland build above does not). Same version as Dockerfile.agent,
# whose X11-enabled build overwrites these libraries in the agent image; this
# one leaves X11 off so the i3 image needs no libXtst. --libexecdir=lib for
# the same reason as flatpak: /usr/libexec is a persistent path on Kairos.
FROM toolchain AS at-spi2-core
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=libucontext /libucontext /
# renovate: datasource=gitlab-tags depName=GNOME/at-spi2-core registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG ATSPI_VERSION=2.54.0
RUN mkdir -p /at-spi2-core
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://download.gnome.org/sources/at-spi2-core/${ATSPI_VERSION%.*}/at-spi2-core-${ATSPI_VERSION}.tar.xz -o a.tar.xz && tar -xf a.tar.xz && rm a.tar.xz && mv at-spi2-core-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} --libexecdir=lib \
      -Ddefault_bus=dbus-daemon -Ddbus_daemon=/usr/bin/dbus-daemon -Duse_systemd=true \
      -Dgtk2_atk_adaptor=false -Ddocs=false -Dintrospection=disabled -Dx11=disabled && \
    DESTDIR=/at-spi2-core ninja -C buildDir install

# GTK3 with the X11 backend, for the i3 image only: xdg-desktop-portal-gtk
# and snixembed need a GTK that can open a window on XLibre. The Wayland variants keep the
# Wayland-only gtk3 stage, so they gain no X11 libraries.
FROM toolchain AS gtk3-x11
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=cairo-x11 /cairo-x11 /
COPY --from=pango-x11 /pango-x11 /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pixman /pixman /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=at-spi2-core /at-spi2-core /
COPY --from=libdrm /libdrm /
COPY --from=mesa-x11 /mesa-x11 /
COPY --from=libepoxy-x11 /libepoxy-x11 /
COPY --from=libucontext /libucontext /
# renovate: datasource=gitlab-tags depName=GNOME/gtk registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG GTK3_VERSION=3.24.43
RUN mkdir -p /gtk3-x11
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://download.gnome.org/sources/gtk+/${GTK3_VERSION%.*}/gtk+-${GTK3_VERSION}.tar.xz -o gtk.tar.xz && tar -xf gtk.tar.xz && rm gtk.tar.xz && mv gtk+-* gtk-src
WORKDIR /build/gtk-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} \
    -Dx11_backend=true -Dwayland_backend=false -Dbroadway_backend=false \
    -Dintrospection=false -Dgtk_doc=false -Dman=false -Ddemos=false -Dexamples=false -Dtests=false \
    -Dprint_backends=file -Dcolord=no
RUN DESTDIR=/gtk3-x11 ninja -C buildDir install

# xdg-desktop-portal-gtk again, built against gtk3-x11 for i3. Same source and
# options as the stage above.
FROM toolchain AS xdg-desktop-portal-gtk-x11
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=cairo-x11 /cairo-x11 /
COPY --from=pango-x11 /pango-x11 /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pixman /pixman /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=at-spi2-core /at-spi2-core /
COPY --from=libdrm /libdrm /
COPY --from=mesa-x11 /mesa-x11 /
COPY --from=libepoxy-x11 /libepoxy-x11 /
COPY --from=libucontext /libucontext /
COPY --from=gtk3-x11 /gtk3-x11 /
COPY --from=xdg-desktop-portal /xdg-desktop-portal /
COPY --from=gettext-tools /gettext-tools /
COPY --from=gsettings-desktop-schemas /gsettings-desktop-schemas /
# renovate: datasource=github-tags depName=flatpak/xdg-desktop-portal-gtk extractVersion=^(?<version>\d+(\.\d+)*)$
ARG XDG_DESKTOP_PORTAL_GTK_VERSION=1.15.3
WORKDIR /build
RUN curl --retry-all-errors -fL --retry 5 https://github.com/flatpak/xdg-desktop-portal-gtk/releases/download/${XDG_DESKTOP_PORTAL_GTK_VERSION}/xdg-desktop-portal-gtk-${XDG_DESKTOP_PORTAL_GTK_VERSION}.tar.xz -o x.tar.xz && tar -xf x.tar.xz && mv xdg-desktop-portal-gtk-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} --libexecdir=lib -Dwallpaper=disabled -Dsettings=enabled -Dlockdown=disabled && \
    DESTDIR=/xdg-desktop-portal-gtk-x11 ninja -C buildDir install && \
    ! ldd /xdg-desktop-portal-gtk-x11/usr/lib/xdg-desktop-portal-gtk | grep 'not found'

# libdbusmenu-gtk3 again, against gtk3-x11 for snixembed on i3. Same source,
# stubs and options as the libdbusmenu stage.
FROM libdbusmenu-src AS libdbusmenu-gtk3-x11
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=cairo-x11 /cairo-x11 /
COPY --from=pango-x11 /pango-x11 /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pixman /pixman /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=at-spi2-core /at-spi2-core /
COPY --from=libdrm /libdrm /
COPY --from=mesa-x11 /mesa-x11 /
COPY --from=libepoxy-x11 /libepoxy-x11 /
COPY --from=libucontext /libucontext /
COPY --from=gtk3-x11 /gtk3-x11 /
RUN mkdir -p /libdbusmenu-gtk3-x11
RUN ./configure ${COMMON_CONFIGURE_ARGS} --with-gtk=3 --disable-introspection --disable-vala --disable-gtk-doc --disable-dumper --disable-tests --disable-static --disable-nls
RUN find . -name Makefile -exec sed -i 's/-Werror//g' {} +
RUN make -C libdbusmenu-glib -j"$(nproc)" && make -C libdbusmenu-gtk -j"$(nproc)" \
    && make -C libdbusmenu-glib DESTDIR=/libdbusmenu-gtk3-x11 install \
    && make -C libdbusmenu-gtk  DESTDIR=/libdbusmenu-gtk3-x11 install

# Vala compiler, to build snixembed. Build-time only: no rootfs copies this
# stage. The release tarball carries valac's own sources already translated to
# C, so it needs no earlier valac; valadoc is off (it needs graphviz).
# GI_GIRDIR is only where vapigen would look for .gir files; there is no
# GObject Introspection here, and configure fails without a value.
FROM toolchain AS vala
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=libucontext /libucontext /
# renovate: datasource=gitlab-tags depName=GNOME/vala registryUrl=https://gitlab.gnome.org extractVersion=^(?<version>\d+(\.\d+)*)$
ARG VALA_VERSION=0.56.19
RUN mkdir -p /vala
WORKDIR /build
RUN curl --retry 5 --retry-delay 3 --retry-all-errors -fL https://download.gnome.org/sources/vala/${VALA_VERSION%.*}/vala-${VALA_VERSION}.tar.xz -o vala.tar.xz && tar -xf vala.tar.xz && rm vala.tar.xz && mv vala-* src
WORKDIR /build/src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-valadoc GI_GIRDIR=/usr/share/gir-1.0 && \
    make -j"$(nproc)" && make install DESTDIR=/vala

# snixembed: shows StatusNotifierItems (the update tray's icon) as XEmbed tray
# icons, the only kind i3bar hosts. It draws them with GTK3's GtkStatusIcon,
# which exists only in the X11 backend, so it builds against gtk3-x11 and
# libdbusmenu-gtk3-x11 rather than the Wayland gtk3. Upstream is on sourcehut
# only (no GitHub mirror), so the archive comes from git.sr.ht. When git.sr.ht
# refuses the connection (it did for a whole agent gate run), the same archive
# comes from Debian's pool: snixembed_<version>.orig.tar.gz is byte-identical
# to the sourcehut tarball (same sha256 for 0.3.3).
#
# libdbusmenu ships its Vala bindings only when built with GObject
# Introspection, which Hadron does not build. snixembed uses one thing from
# them, DbusmenuGtk.Menu's constructor, so the two bindings it names
# (--pkg Dbusmenu-0.4 DbusmenuGtk3-0.4) are written here: an empty one and one
# declaring that class. The C code still includes libdbusmenu's real header
# and links its library, so a wrong function name fails the build.
FROM toolchain AS snixembed
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=cairo-x11 /cairo-x11 /
COPY --from=pango-x11 /pango-x11 /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=pixman /pixman /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=at-spi2-core /at-spi2-core /
COPY --from=libdrm /libdrm /
COPY --from=mesa-x11 /mesa-x11 /
COPY --from=libepoxy-x11 /libepoxy-x11 /
COPY --from=libucontext /libucontext /
COPY --from=gtk3-x11 /gtk3-x11 /
COPY --from=libdbusmenu-gtk3-x11 /libdbusmenu-gtk3-x11 /
COPY --from=vala /vala /
# renovate: datasource=git-tags depName=https://git.sr.ht/~steef/snixembed extractVersion=^(?<version>\d+(\.\d+)*)$
ARG SNIXEMBED_VERSION=0.3.3
RUN mkdir -p /snixembed
WORKDIR /build
RUN { curl --connect-timeout 20 --retry 5 --retry-delay 3 --retry-all-errors -fL -o s.tar.gz \
        https://git.sr.ht/~steef/snixembed/archive/${SNIXEMBED_VERSION}.tar.gz || \
      curl --retry 5 --retry-delay 3 --retry-all-errors -fL -o s.tar.gz \
        https://deb.debian.org/debian/pool/main/s/snixembed/snixembed_${SNIXEMBED_VERSION}.orig.tar.gz; } && \
    tar -xzf s.tar.gz && rm s.tar.gz && mv snixembed-* src
WORKDIR /build/src
RUN v=/usr/share/vala/vapi && mkdir -p $v && \
    printf '%s\n' '/* Empty: snixembed uses nothing from libdbusmenu-glib directly. */' \
      'namespace Dbusmenu {}' > $v/Dbusmenu-0.4.vapi && \
    printf '%s\n' '/* The part of libdbusmenu-gtk3 snixembed uses (see the Dockerfile). */' \
      '[CCode (cprefix = "DbusmenuGtk", lower_case_cprefix = "dbusmenu_gtk_")]' \
      'namespace DbusmenuGtk {' \
      '	[CCode (cheader_filename = "libdbusmenu-gtk/menu.h", type_id = "dbusmenu_gtkmenu_get_type ()")]' \
      '	public class Menu : Gtk.Menu {' \
      '		[CCode (cname = "dbusmenu_gtkmenu_new", has_construct_function = false)]' \
      '		public Menu (string dbus_name, string dbus_object);' \
      '	}' \
      '}' > $v/DbusmenuGtk3-0.4.vapi
RUN make && make install DESTDIR=/snixembed PREFIX=/usr && \
    rm -rf /snixembed/usr/share/man && \
    /snixembed/usr/bin/snixembed --version | grep -qx "version: ${SNIXEMBED_VERSION}" && \
    ! ldd /snixembed/usr/bin/snixembed | grep 'not found'

# ===========================================================================
# M6: real-hardware firmware (optional)
#
# Real wifi / bluetooth / GPU hardware needs firmware blobs. This is OFF by
# default (the QEMU test path and slim images need no blobs); build with
# --build-arg FIRMWARE=true to bundle a curated linux-firmware subset for
# common laptop and desktop hardware. Not tested in CI: QEMU has none of it.
#
# The blobs go to /usr/lib/firmware, never /usr/local/lib/firmware. In Hadron
# (kairos-io/kairos#4290, in v0.6.4) the kernel finds /usr/local/lib/firmware
# only through /usr/lib/firmware/updates, and on an installed system
# COS_PERSISTENT is mounted over /usr/local, which hides every blob the image
# put there. Only the live ISO could load them.
#
# tools/install-firmware.sh installs each directory the way upstream's
# copy-firmware.sh --zstd installs all of them: it compresses the blobs with
# zstd (about half the size; Hadron's kernel has CONFIG_FW_LOADER_COMPRESS_ZSTD
# and Hadron's own blobs are .zst too) and creates the links WHENCE declares,
# which the release tarball does not contain (most nvidia/<chip> directories
# are links). A prefix that matches nothing fails the build, so a directory
# upstream moves (iwlwifi-*.ucode moved to intel/iwlwifi/, and the old pattern
# here then shipped no Intel Wi-Fi firmware at all) is noticed.
# ===========================================================================
FROM alpine:3 AS firmware
ARG FIRMWARE=false
# The tarball is checked against a pinned SHA256, so this is updated by hand
# (scripts/check-renovate-pins.sh): take both values from
# https://cdn.kernel.org/pub/linux/kernel/firmware/sha256sums.asc.
ARG LINUX_FIRMWARE_VERSION=20261019
ARG LINUX_FIRMWARE_SHA256=a10d1ab5baf2c74ba92e8c7cf313e691696c26bddfa6df00db795e48d70cd4ca
RUN mkdir -p /firmware/usr/lib/firmware
# intel/iwlwifi, intel/ibt-: Intel Wi-Fi and Bluetooth. ath*, qca: Qualcomm
# Atheros. rtw*, rtl*: Realtek. brcm, mrvl, mediatek: Broadcom, Marvell,
# MediaTek; cypress, because brcm links into it. i915, xe: Intel graphics (xe
# from Lunar Lake on). amdgpu, radeon: AMD graphics (radeon for GCN 1 and 2 on
# the radeon kernel driver). nvidia:
# nouveau's display firmware; Mesa has no NVIDIA driver here, so those GPUs
# render with llvmpipe. Hadron's base image already ships regulatory.db.
COPY tools/install-firmware.sh /usr/local/bin/install-firmware
RUN if [ "$FIRMWARE" = "true" ]; then \
      apk add --no-cache curl tar xz zstd coreutils && \
      cd /tmp && \
      curl -fL --retry 5 --retry-delay 3 --retry-all-errors \
        https://cdn.kernel.org/pub/linux/kernel/firmware/linux-firmware-${LINUX_FIRMWARE_VERSION}.tar.xz \
        -o lf.tar.xz && \
      echo "${LINUX_FIRMWARE_SHA256}  lf.tar.xz" | sha256sum -c - && \
      prefixes="intel/iwlwifi/ intel/ibt- ath9k_htc/ ath10k/ ath11k/ ath12k/ qca/ \
        rtw88/ rtw89/ rtlwifi/ rtl_bt/ rtl_nic/ brcm/ mrvl/ mediatek/ \
        cypress/ i915/ xe/ amdgpu/ radeon/ nvidia/" && \
      top="linux-firmware-${LINUX_FIRMWARE_VERSION}" && \
      set -f && \
      tar -xJf lf.tar.xz --wildcards "$top/WHENCE" \
        $(for p in $prefixes; do printf '%s/%s* ' "$top" "$p"; done) && \
      set +f && \
      rm lf.tar.xz && \
      install-firmware "$top" /firmware/usr/lib/firmware $prefixes && \
      rm -rf "$top" ; \
    fi


# ===========================================================================
# Final assembly
# ===========================================================================

FROM scratch AS sway-rootfs
COPY --from=wayland /wayland /
COPY --from=libdrm /libdrm /
COPY --from=libucontext /libucontext /
COPY --from=mesa /mesa /
COPY --from=xkeyboard-config /xkeyboard-config /
COPY --from=libxkb /libxkb /
COPY --from=pixman /pixman /
COPY --from=libevdev /libevdev /
COPY --from=libinput /libinput /
COPY --from=hwdata /hwdata /
COPY --from=libdisplay-info /libdisplay-info /
COPY --from=cairo /cairo /
COPY --from=wlroots /wlroots /
COPY --from=pcre2 /pcre2 /
COPY --from=glib2 /glib2 /
COPY --from=freetype /freetype /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=fontconfig /fontconfig /
COPY --from=pango /pango /
COPY --from=libseat /libseat /
COPY --from=sway /sway /
COPY --from=tllist /tllist /
COPY --from=fcft /fcft /
COPY --from=foot /foot /
COPY --from=libpng /libpng /
COPY --from=grim /grim /
# M2: networking
COPY --from=libnl /libnl /
COPY --from=wpa-supplicant /wpa /
COPY --from=hostapd /hostapd /
COPY --from=libndp /libndp /
COPY --from=networkmanager /networkmanager /
COPY --from=wireless-regdb /wireless-regdb /
COPY --from=ncurses /ncurses /
# M3: audio
COPY --from=alsa-lib /alsa-lib /
COPY --from=alsa-ucm-conf /alsa-ucm-conf /
COPY --from=pipewire /pipewire /
COPY --from=wireplumber /wireplumber /
# M4: bluetooth
COPY --from=sbc /sbc /
COPY --from=bluez /bluez /
# M5: desktop polish
COPY --from=swaybg /swaybg /
COPY --from=mako /mako /
COPY --from=fuzzel /fuzzel /
COPY --from=wl-clipboard /wl-clipboard /
COPY --from=cliphist /cliphist /
COPY --from=slurp /slurp /
COPY --from=swayidle /swayidle /
COPY --from=swaylock /swaylock /
COPY --from=wlsunset /wlsunset /
# waybar — status bar + its GTK3/gtkmm/tray runtime stack and Nerd Font glyphs
COPY --from=gtk3 /gtk3 /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=atk /atk /
COPY --from=libepoxy /libepoxy /
COPY --from=libsigcpp /libsigcpp /
COPY --from=glibmm /glibmm /
COPY --from=cairomm /cairomm /
COPY --from=pangomm /pangomm /
COPY --from=atkmm /atkmm /
COPY --from=gtkmm3 /gtkmm3 /
COPY --from=gtk-layer-shell /gtk-layer-shell /
COPY --from=jsoncpp /jsoncpp /
COPY --from=libfmt /libfmt /
COPY --from=spdlog /spdlog /
COPY --from=cpp-date /cpp-date /
COPY --from=libdbusmenu /libdbusmenu /
COPY --from=waybar /waybar /
COPY --from=nerdfont /nerdfont /
COPY --from=hadron-fonts /hadron-fonts /
COPY --from=emoji /emoji /
COPY --from=hadron-wallpapers /hadron-wallpapers /
# Fallback icon theme and the Adwaita cursors (system default via
# /usr/share/icons/default), as on niri.
COPY --from=hicolor-icon-theme /hicolor-icon-theme /
COPY --from=adwaita-cursors /adwaita-cursors /
# Login manager (ly) + its X protocol libs
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=ly /ly /
# Portals, so Flatpak apps get file dialogs and the dark colour scheme (gtk)
# and screenshots and screen sharing (wlr). sway-portals.conf picks them.
COPY --from=gstreamer /gstreamer /
COPY --from=inih /inih /
COPY --from=xdg-desktop-portal /xdg-desktop-portal /
COPY --from=xdg-desktop-portal-gtk /xdg-desktop-portal-gtk /
COPY --from=xdg-desktop-portal-wlr /xdg-desktop-portal-wlr /
COPY --from=gsettings-desktop-schemas /gsettings-desktop-schemas /
# polkit, which answers hadron-updated's "may this user install updates?"
COPY --from=duktape /duktape /
COPY --from=polkit /polkit /


# XLibre/i3 root. This mirrors the shared networking/audio/container-facing
# runtime from sway-rootfs but swaps every display-specific artifact.
FROM scratch AS i3-rootfs
COPY --from=libdrm /libdrm /
COPY --from=libucontext /libucontext /
COPY --from=mesa-x11 /mesa-x11 /
COPY --from=xkeyboard-config /xkeyboard-config /
COPY --from=libxkb-x11 /libxkb-x11 /
COPY --from=pixman /pixman /
COPY --from=libevdev /libevdev /
COPY --from=libinput /libinput /
COPY --from=hwdata /hwdata /
COPY --from=cairo-x11 /cairo-x11 /
COPY --from=pcre2 /pcre2 /
COPY --from=glib2 /glib2 /
COPY --from=freetype /freetype /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=fontconfig /fontconfig /
COPY --from=pango-x11 /pango-x11 /
COPY --from=libpng /libpng /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=libseat /libseat /
# XLibre server, input DDX, client libraries, and i3-native desktop tools.
COPY --from=xorgproto /xorgproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=libepoxy-x11 /libepoxy-x11 /
COPY --from=xlibre /xlibre /
COPY --from=xlibre-input-libinput /xlibre-input-libinput /
COPY --from=i3 /i3 /
COPY --from=x11-desktop-tools /x11-desktop-tools /
COPY --from=hadron-xroot /hadron-xroot /
COPY --from=dunst /dunst /
COPY --from=i3lock /i3lock /
COPY --from=xss-lock /xss-lock /
COPY --from=x11-screenshot /x11-screenshot /
# M2: networking
COPY --from=libnl /libnl /
COPY --from=wpa-supplicant /wpa /
COPY --from=hostapd /hostapd /
COPY --from=libndp /libndp /
COPY --from=networkmanager /networkmanager /
COPY --from=wireless-regdb /wireless-regdb /
COPY --from=ncurses /ncurses /
# M3: audio
COPY --from=alsa-lib /alsa-lib /
COPY --from=alsa-ucm-conf /alsa-ucm-conf /
COPY --from=pipewire /pipewire /
COPY --from=wireplumber /wireplumber /
# M4: bluetooth
COPY --from=sbc /sbc /
COPY --from=bluez /bluez /
# Shared visual assets + login manager.
COPY --from=nerdfont /nerdfont /
COPY --from=hadron-fonts /hadron-fonts /
COPY --from=emoji /emoji /
COPY --from=hadron-wallpapers /hadron-wallpapers /
COPY --from=hicolor-icon-theme /hicolor-icon-theme /
COPY --from=adwaita-cursors /adwaita-cursors /
COPY --from=ly /ly /
# Portals, so Flatpak apps get file dialogs and the dark colour scheme. The GTK
# backend needs a GTK3 with the X11 backend (and its AT-SPI bridge) here.
COPY --from=gstreamer /gstreamer /
COPY --from=at-spi2-core /at-spi2-core /
COPY --from=gtk3-x11 /gtk3-x11 /
COPY --from=xdg-desktop-portal /xdg-desktop-portal /
COPY --from=xdg-desktop-portal-gtk-x11 /xdg-desktop-portal-gtk-x11 /
COPY --from=gsettings-desktop-schemas /gsettings-desktop-schemas /
# polkit, which answers hadron-updated's "may this user install updates?", and
# the update tray's host: snixembed shows its StatusNotifierItem in i3bar,
# which hosts only XEmbed icons. It uses the same GTK3 X11 build as the portal.
COPY --from=duktape /duktape /
COPY --from=polkit /polkit /
COPY --from=libdbusmenu-gtk3-x11 /libdbusmenu-gtk3-x11 /
COPY --from=snixembed /snixembed /


# COSMIC root. Shares the networking/audio/bluetooth runtime with the other
# variants; the desktop itself is the cosmic stage's output plus the services it
# needs (polkit, accountsservice, the portal, greetd, Xwayland).
FROM scratch AS cosmic-rootfs
COPY --from=wayland /wayland /
COPY --from=libdrm /libdrm /
COPY --from=libucontext /libucontext /
COPY --from=mesa /mesa /
COPY --from=xkeyboard-config /xkeyboard-config /
COPY --from=libxkb /libxkb /
COPY --from=pixman /pixman /
COPY --from=libevdev /libevdev /
COPY --from=libinput /libinput /
COPY --from=hwdata /hwdata /
COPY --from=libdisplay-info /libdisplay-info /
COPY --from=libseat /libseat /
COPY --from=pcre2 /pcre2 /
COPY --from=glib2 /glib2 /
COPY --from=freetype /freetype /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=libepoxy /libepoxy /
# Xwayland and the X11 libraries it (and xkbcomp) need.
COPY --from=xorgproto /xorgproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=xwayland /xwayland /
# M2: networking
COPY --from=libnl /libnl /
COPY --from=wpa-supplicant /wpa /
COPY --from=hostapd /hostapd /
COPY --from=libndp /libndp /
COPY --from=networkmanager /networkmanager /
COPY --from=wireless-regdb /wireless-regdb /
COPY --from=ncurses /ncurses /
# M3: audio
COPY --from=alsa-lib /alsa-lib /
COPY --from=alsa-ucm-conf /alsa-ucm-conf /
COPY --from=pipewire /pipewire /
COPY --from=wireplumber /wireplumber /
COPY --from=libsndfile /libsndfile /
COPY --from=libpulse /libpulse /
# M4: bluetooth
COPY --from=sbc /sbc /
COPY --from=bluez /bluez /
# COSMIC and the services it talks to.
COPY --from=duktape /duktape /
COPY --from=polkit /polkit /
COPY --from=accountsservice /accountsservice /
COPY --from=dav1d /dav1d /
COPY --from=iso-codes /iso-codes /
COPY --from=gstreamer /gstreamer /
COPY --from=json-glib /json-glib /
COPY --from=xdg-desktop-portal /xdg-desktop-portal /
COPY --from=greetd /greetd /
COPY --from=pop-launcher /pop-launcher /
COPY --from=cosmic /cosmic /
# fuzzel and the font library it links: the picker of the Hadron menu
# (hadron-pick) and the emoji picker.
COPY --from=fcft /fcft /
COPY --from=fuzzel /fuzzel /
# wl-clipboard: wl-copy puts the emoji picker's choice on the clipboard
# (hadron-emoji), and wl-paste --watch feeds cliphist the clipboard history
# (hadron-clipboard). cosmic-comp offers the data-control protocols to every
# client that is not in a sandbox, so wl-paste needs no setting.
COPY --from=wl-clipboard /wl-clipboard /
COPY --from=cliphist /cliphist /
COPY --from=cosmic-fonts /cosmic-fonts /
COPY --from=hicolor-icon-theme /hicolor-icon-theme /
COPY --from=musl-locales /musl-locales /
COPY --from=nerdfont /nerdfont /
COPY --from=hadron-fonts /hadron-fonts /
COPY --from=emoji /emoji /
COPY --from=hadron-wallpapers /hadron-wallpapers /
COPY --from=hadron-wallpapers-day /hadron-wallpapers-day /


FROM scratch AS niri-rootfs
COPY --from=wayland /wayland /
COPY --from=libdrm /libdrm /
COPY --from=libucontext /libucontext /
COPY --from=mesa /mesa /
COPY --from=xkeyboard-config /xkeyboard-config /
COPY --from=libxkb /libxkb /
COPY --from=pixman /pixman /
COPY --from=libevdev /libevdev /
COPY --from=libinput /libinput /
COPY --from=hwdata /hwdata /
COPY --from=libdisplay-info /libdisplay-info /
COPY --from=libseat /libseat /
COPY --from=pcre2 /pcre2 /
COPY --from=glib2 /glib2 /
COPY --from=freetype /freetype /
COPY --from=harfbuzz /harfbuzz /
COPY --from=fribidi /fribidi /
COPY --from=fontconfig /fontconfig /
COPY --from=libpng /libpng /
COPY --from=cairo /cairo /
COPY --from=pango /pango /
COPY --from=gdk-pixbuf /gdk-pixbuf /
COPY --from=atk /atk /
COPY --from=libepoxy /libepoxy /
COPY --from=gtk3 /gtk3 /
# Xwayland (run by xwayland-satellite) and the X11 libraries it needs.
COPY --from=xorgproto /xorgproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=xwayland /xwayland /
COPY --from=xwayland-satellite /xwayland-satellite /
# M2: networking
COPY --from=libnl /libnl /
COPY --from=wpa-supplicant /wpa /
COPY --from=hostapd /hostapd /
COPY --from=libndp /libndp /
COPY --from=networkmanager /networkmanager /
COPY --from=wireless-regdb /wireless-regdb /
COPY --from=ncurses /ncurses /
# M3: audio
COPY --from=alsa-lib /alsa-lib /
COPY --from=alsa-ucm-conf /alsa-ucm-conf /
COPY --from=pipewire /pipewire /
COPY --from=wireplumber /wireplumber /
COPY --from=libsndfile /libsndfile /
# M4: bluetooth
COPY --from=sbc /sbc /
COPY --from=bluez /bluez /
# niri, Noctalia and what Noctalia links.
COPY --from=niri /niri /
COPY --from=duktape /duktape /
COPY --from=polkit /polkit /
COPY --from=libgcrypt /libgcrypt /
COPY --from=libsecret /libsecret /
COPY --from=libsodium /libsodium /
COPY --from=sdbus-cpp /sdbus-cpp /
COPY --from=librsvg /librsvg /
COPY --from=gmp /gmp /
COPY --from=mpfr /mpfr /
COPY --from=libqalculate /libqalculate /
COPY --from=md4c /md4c /
COPY --from=tomlplusplus /tomlplusplus /
COPY --from=libical /libical /
COPY --from=libwebp /libwebp /
COPY --from=brotli /brotli /
COPY --from=highway /highway /
COPY --from=lcms2 /lcms2 /
COPY --from=libjxl /libjxl /
COPY --from=noctalia /noctalia /
# Portals, so Flatpak apps get file dialogs.
COPY --from=json-glib /json-glib /
COPY --from=gstreamer /gstreamer /
COPY --from=xdg-desktop-portal /xdg-desktop-portal /
COPY --from=xdg-desktop-portal-gtk /xdg-desktop-portal-gtk /
COPY --from=gsettings-desktop-schemas /gsettings-desktop-schemas /
# Terminal, clipboard, fonts, icons, login screen.
COPY --from=tllist /tllist /
COPY --from=fcft /fcft /
COPY --from=foot /foot /
# fuzzel: the picker of the Hadron menu (hadron-pick).
COPY --from=fuzzel /fuzzel /
COPY --from=wl-clipboard /wl-clipboard /
COPY --from=cosmic-fonts /cosmic-fonts /
COPY --from=nerdfont /nerdfont /
COPY --from=hadron-fonts /hadron-fonts /
COPY --from=emoji /emoji /
COPY --from=hadron-wallpapers /hadron-wallpapers /
COPY --from=hadron-wallpapers-day /hadron-wallpapers-day /
COPY --from=hicolor-icon-theme /hicolor-icon-theme /
COPY --from=adwaita-cursors /adwaita-cursors /
COPY --from=ly /ly /


# Static configuration is also selected as a rootfs stage. rootfs/ is shared;
# the overlay contains only files for the chosen session, so the i3 image never
# advertises a broken Sway session (and vice versa).
FROM scratch AS common-config
COPY rootfs/ /

FROM common-config AS sway-config
COPY rootfs-sway/ /

FROM common-config AS i3-config
COPY rootfs-i3/ /

FROM common-config AS cosmic-config
COPY rootfs-cosmic/ /

FROM common-config AS niri-config
COPY rootfs-niri/ /

FROM ${DESKTOP}-rootfs AS desktop-rootfs
FROM ${DESKTOP}-config AS desktop-config


# ---------------------------------------------------------------------------
# Installer TUI. A static CGO_ENABLED=0 Go binary, so it runs on musl without
# any runtime linkage. Replaces the POSIX-sh wizard: it collects the same
# answers, writes the same cloud-config, then runs `kairos-agent install` as a
# subprocess and renders a branded progress screen instead of raw log spew.
#
# Must be declared before `default` (the stage that COPYies it in), not merely
# before `kairos`: BuildKit resolves --from against stages defined ABOVE the
# consuming stage.
#
# Deliberately NOT in the Makefile's --no-cache-filter list. That list exists
# because the kairos stage's `dracut -f` bakes the splash branding and every
# other initramfs input behind BuildKit's back, and because it follows the
# moving kairos-init:master tag. Here the cache key is honest: `COPY install-ui/ ./` is
# content-addressed, so any source change invalidates the build, and the output
# is COPYied straight into the image rather than swallowed by a later step. Busting
# it every build would re-run `go mod download` (network) for no correctness gain.
# ---------------------------------------------------------------------------
FROM golang:1.25.0-alpine3.22 AS install-ui-build
WORKDIR /src
COPY install-ui/go.mod install-ui/go.sum ./
RUN go mod download
COPY install-ui/ ./
# The trailing check is a smoke test: the binary must run and reject an unknown
# flag with exit 2, so a binary that segfaults on startup fails the build here
# rather than on tty1 during an install. Braced so a FAILED `go build` still
# fails the RUN -- written as `... && bin --nonsense; [ $? -eq 2 ]` the RUN's
# status would be the test's alone, and a build that happened to exit 2 would
# "pass" having produced no binary at all.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hadron-install-ui . \
    && { /out/hadron-install-ui --nonsense 2>/dev/null; [ $? -eq 2 ]; }


# ---------------------------------------------------------------------------
# hadron-update: release checker, desktop notifier, `kairos-agent upgrade`
# wrapper and the root update service on the system bus (hadron-updated). Static
# (CGO_ENABLED=0), like the installer TUI, and for the same reasons kept out of
# the --no-cache-filter list. Not stdlib-only any more: the D-Bus service needs
# godbus, downloaded in its own layer so a source change does not refetch it.
# The unit tests run here so a broken updater fails the image build, not a
# user's upgrade; the ones that need a dbus-daemon skip, as this stage has none.
# ---------------------------------------------------------------------------
FROM golang:1.25.0-alpine3.22 AS update-build
WORKDIR /src
COPY update/go.mod update/go.sum ./
RUN go mod download
COPY update/ ./
RUN CGO_ENABLED=0 go test ./... \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hadron-update . \
    && { /out/hadron-update --nonsense >/dev/null 2>&1; [ $? -eq 2 ]; }


FROM ${BASE_IMAGE} AS default
# Built desktop stack
COPY --from=desktop-rootfs / /
# Runtime libraries provided by the toolchain but not the base image
COPY --from=toolchain /usr/lib/libffi.so* /usr/lib/
COPY --from=toolchain /usr/lib/libexpat.so* /usr/lib/
COPY --from=toolchain /usr/lib/libjson-c.so* /usr/lib/
COPY --from=toolchain /usr/lib/libstdc++.so.6* /usr/lib/
COPY --from=toolchain /usr/lib/libgcc_s.so* /usr/lib/
COPY --from=toolchain /usr/lib/libreadline.so* /usr/lib/
# libxml2 — pulled by libxkbregistry (waybar links it; sway uses libxkbcommon
# and never loads it, which is why this only surfaces with waybar present).
COPY --from=toolchain /usr/lib/libxml2.so* /usr/lib/
# Flatpak + its from-source stack (libcurl/seccomp/zstd/lzma/cap/mount/crypto
# already ship in the base image; glib2/gdk-pixbuf/libxml2 are pulled in above
# by the waybar stack).
COPY --from=libgpg-error /libgpg-error /
COPY --from=libassuan /libassuan /
COPY --from=gpgme /gpgme /
COPY --from=signed-flatpak-runtime /signed-flatpak-runtime /
COPY --from=libyaml /libyaml /
COPY --from=libxmlb /libxmlb /
COPY --from=appstream /appstream /
COPY --from=libarchive /libarchive /
COPY --from=json-glib /json-glib /
COPY --from=libfuse3 /libfuse3 /
# Only libe2p (ostree's libe2p dep) — NOT the rest of e2fsprogs, which would
# clobber the base image's libext2fs and break the installer's own mkfs.ext4.
COPY --from=e2fsprogs /e2fsprogs/usr/lib/libe2p.so* /usr/lib/
COPY --from=bubblewrap /bubblewrap /
COPY --from=xdg-dbus-proxy /xdg-dbus-proxy /
COPY --from=ostree /ostree /
COPY --from=flatpak /flatpak /
# The Secret Service (gnome-keyring, with gcr 3 and p11-kit), on every variant.
COPY --from=libgcrypt /libgcrypt /
COPY --from=gnome-keyring /gnome-keyring /
# XDG MIME database — required for GLib content-type detection, which appstream/
# libxmlb rely on to decompress the gzipped Flathub catalog (fixes flatpak search).
COPY --from=shared-mime-info /shared-mime-info /
# Docker (static bundle): dockerd/docker/containerd/runc + helpers -> /usr/bin.
COPY --from=docker /docker /
# Distrobox (POSIX shell) — mutable dev containers on docker.
COPY --from=distrobox /distrobox /
# M6: optional real-hardware firmware (empty unless FIRMWARE=true)
COPY --from=firmware /firmware /
# Shared + selected session config / launch layer
COPY --from=desktop-config / /
# The boot splash, at the path kairos-init's units and dracut module exec. It
# must be in place before the kairos stage runs kairos-init, which then keeps it
# instead of linking the stock `kairos splash` there.
COPY --from=hadron-splash /build/splash/hadron-splash /usr/bin/kairos-splash

# System setup. NOTE: no user is created here — the desktop user is defined at
# install time via a Kairos cloud-config (see cloud-config.yaml) and lives on
# the persistent /home. We only ensure the groups it will join exist, enable
# the system services, and configure the ly display manager on tty1.
RUN chmod 1777 /tmp; \
    ldconfig 2>/dev/null || true; \
    # Register the bundled fonts for the selected bar, terminal, and launcher.
    fc-cache -f 2>/dev/null || true; \
    # Compile the GSettings schemas the variant ships (GTK's, and on Sway, i3
    # and niri gsettings-desktop-schemas with Hadron's dark override): the
    # stages install them under DESTDIR, where meson skips this, and GLib
    # aborts a GTK app that opens a file chooser with no compiled schemas. On
    # i3 the AT-SPI bus launcher, which GTK starts for snixembed, aborts too.
    if [ -d /usr/share/glib-2.0/schemas ]; then glib-compile-schemas /usr/share/glib-2.0/schemas || exit 1; fi; \
    for g in audio video render input bluetooth seat docker; do groupadd -f "$g"; done; \
    mkdir -p /var/lib/docker /var/lib/xkb; \
    # The selected session launcher lives in /usr/bin (NOT /usr/local): Kairos mounts /usr/local
    # from the persistent partition on the installed system, which shadows
    # anything baked into the image there — ly would exec a missing launcher and
    # bounce straight back to the login screen. The installer TUI lives in
    # /usr/bin for the same reason (it is COPYied in after this block, already
    # executable, so the glob below deliberately does not have to reach it).
    chmod +x /usr/bin/hadron-*; \
    if [ -e /usr/bin/start-desktop ]; then chmod +x /usr/bin/start-desktop; fi; \
    if [ -e /usr/lib/systemd/system/ly@.service ]; then \
    # ly runs on tty1 via the ly@tty1 instance of the ly@.service template (ly
    # 1.3.x dropped the plain ly.service and the config `tty` option — the tty is
    # the systemd instance). It authenticates the cloud-config user and launches
    # the selected Sway or i3 session via its desktop entry.
    # Hadron colours for ly, in eight-colour mode. The Linux console has 16
    # colours: it maps ly's 24-bit values to the nearest of them by hue, so
    # the accent came out cyan and the navy background black. Instead ly asks
    # for the console's own colours, which the kernel command line sets to the
    # Hadron palette (vt.default_* in grubmenu.cfg): the default background is
    # colour 0 (base), bright white is 15 (text), bright blue is 12 (accent),
    # red is 1 (error). The values are 0xSSRRGGBB: styling 0x40 is TB_BRIGHT,
    # 0x01 is bold, and the low byte is a termbox colour (2 red, 5 blue,
    # 8 white). No animation: decision 8 dropped the "Black Hole" one, whose
    # licence is not stated.
    sed -i \
      -e 's/^animation = .*/animation = none/' \
      -e 's/^full_color = .*/full_color = false/' \
      -e 's/^bg = .*/bg = 0x00000000/' \
      -e 's/^fg = .*/fg = 0x40000008/' \
      -e 's/^border_fg = .*/border_fg = 0x40000005/' \
      -e 's/^error_fg = .*/error_fg = 0x01000002/' \
      -e 's/^error_bg = .*/error_bg = 0x00000000/' \
      -e 's/^box_title = .*/box_title = Hadron Desktop/' \
      -e 's/^clock = .*/clock = %H:%M/' \
      /etc/ly/config.ini 2>/dev/null || true; \
    # XLibre's libseat integration is initialized before its later automatic
    # controlling-TTY detection. Rootless X sessions therefore must pass
    # -keeptty explicitly; otherwise libseat is disabled and XLibre falls back
    # to opening /dev/ttyN itself, which fails for the authenticated user.
    if [ -x /usr/bin/Xorg ]; then \
      sed -i 's|^x_cmd = .*|x_cmd = /usr/bin/X -keeptty|' /etc/ly/config.ini; \
    fi; \
    # ly's unit ships only `Alias=display-manager.service` (no WantedBy=), so a
    # plain `systemctl enable` never pulls it into a target. And Kairos forces
    # `systemctl set-default multi-user.target` at boot, so graphical.target
    # (which Wants=display-manager.service) is never reached. Net result: ly never
    # starts and boot stops at a login-less multi-user state. Wire ly straight
    # into multi-user.target — it's a VT TUI with no graphical prerequisites.
    systemctl enable ly@tty1.service 2>/dev/null || true; \
    mkdir -p /etc/systemd/system/multi-user.target.wants; \
    ln -sf /usr/lib/systemd/system/ly@.service /etc/systemd/system/multi-user.target.wants/ly@tty1.service; \
    else \
    # COSMIC: greetd runs cosmic-greeter on tty1 instead of ly (rootfs-cosmic/).
    # The greeter runs as its own system user, which needs the seat groups for
    # its compositor.
      useradd -r -M -d /var/lib/cosmic-greeter -s /usr/sbin/nologin -G video,render,input,seat cosmic-greeter; \
      mkdir -p /var/lib/cosmic-greeter && chown cosmic-greeter: /var/lib/cosmic-greeter; \
      # cosmic-osd (the password prompt) execs the polkit helper at Debian's
      # path, compiled in; exec follows the link to the setuid target.
      mkdir -p /usr/libexec && ln -sf /usr/lib/polkit-1/polkit-agent-helper-1 /usr/libexec/polkit-agent-helper-1; \
      # COSMIC's play/pause, next and previous keys run playerctl, which Hadron
      # does not ship; hadron-media-key does the same over MPRIS with busctl.
      # The build fails if upstream renames these actions, so the keys cannot
      # silently stop working.
      f=/usr/share/cosmic/com.system76.CosmicSettings.Shortcuts/v1/system_actions; \
      sed -i -E 's/"playerctl (play-pause|next|previous)"/"hadron-media-key \1"/' "$f" || exit 1; \
      grep -q '"hadron-media-key play-pause"' "$f" && ! grep -q playerctl "$f" || exit 1; \
      # Super+Escape opens the Hadron menu, as in every variant. COSMIC binds
      # it to the screen lock, which moves to Super+Alt+L, the lock key of the
      # other variants (Super+Alt+Right still moves to the output on the right).
      # The build fails if upstream changes either line.
      # Super+; opens the emoji picker, as in every variant. COSMIC binds
      # nothing there; the binding is added on the line after the menu's,
      # and the build fails if Super+; is bound twice.
      f=/usr/share/cosmic/com.system76.CosmicSettings.Shortcuts/v1/defaults; \
      # Super+Ctrl+V opens the clipboard history, as in every variant. COSMIC
      # binds nothing to it; the build fails if upstream starts to.
      ! grep -qF '(modifiers: [Super, Ctrl], key: "v")' "$f" || exit 1; \
      sed -i \
        -e 's/(modifiers: \[Super\], key: "Escape"): System(LockScreen),/(modifiers: [Super], key: "Escape"): Spawn("hadron-menu"),\n    (modifiers: [Super], key: "semicolon"): Spawn("hadron-emoji"),/' \
        -e 's/(modifiers: \[Super, Alt\], key: "l"): SwitchOutput(Right),/(modifiers: [Super, Alt], key: "l"): System(LockScreen),/' \
        -e '/(modifiers: \[Super\], key: "Escape"): Spawn("hadron-menu"),/a\    (modifiers: [Super, Ctrl], key: "v"): Spawn("hadron-clipboard"),' \
        "$f" || exit 1; \
      grep -qF '(modifiers: [Super], key: "Escape"): Spawn("hadron-menu"),' "$f" || exit 1; \
      grep -qF '(modifiers: [Super, Alt], key: "l"): System(LockScreen),' "$f" || exit 1; \
      # Super+Alt+BackSpace turns do not disturb on and off, as in the other
      # variants (hadron-notify-key). COSMIC has no command to close a
      # notification, so the other notification keys are not bound here.
      sed -i 's/\((modifiers: \[Super\], key: "Escape"): Spawn("hadron-menu"),\)/\1\n    (modifiers: [Super, Alt], key: "BackSpace"): Spawn("hadron-notify-key dnd"),/' "$f" || exit 1; \
      grep -qF '(modifiers: [Super, Alt], key: "BackSpace"): Spawn("hadron-notify-key dnd"),' "$f" || exit 1; \
      [ "$(grep -cF '(modifiers: [Super], key: "semicolon")' "$f")" -eq 1 ] || exit 1; \
      grep -qF '(modifiers: [Super], key: "semicolon"): Spawn("hadron-emoji"),' "$f" || exit 1; \
      [ "$(grep -cF '(modifiers: [Super, Ctrl], key: "v"): Spawn("hadron-clipboard"),' "$f")" -eq 1 ] || exit 1; \
      mkdir -p /etc/systemd/system/multi-user.target.wants; \
      for u in cosmic-greeter cosmic-greeter-daemon accounts-daemon; do \
        ln -sf /usr/lib/systemd/system/$u.service /etc/systemd/system/multi-user.target.wants/$u.service; \
      done; \
    fi; \
    # polkit (Sway, i3, COSMIC and niri): polkitd runs the daemon, which D-Bus
    # starts on the first request (polkit.service needs no enable link). The agent helper is
    # what checks a password for the prompt, so it must be setuid root, and the
    # rules directories must be readable by polkitd only.
    if [ -d /usr/lib/polkit-1 ]; then \
      useradd -r -M -d / -s /usr/sbin/nologin polkitd; \
      chmod 4755 /usr/lib/polkit-1/polkit-agent-helper-1; \
      for d in /etc/polkit-1/rules.d /usr/share/polkit-1/rules.d; do \
        mkdir -p "$d" && chown polkitd:root "$d" && chmod 700 "$d"; \
      done; \
    fi; \
    # pam_unix checks a password through this helper when the caller is not
    # root, which is how every screen locker unlocks (Noctalia's, COSMIC's).
    # Without setuid it cannot read /etc/shadow and every unlock fails with
    # "cannot retrieve authentication info".
    if [ -e /usr/sbin/unix_chkpwd ]; then chmod 4755 /usr/sbin/unix_chkpwd; fi; \
    systemctl mask getty@tty1.service 2>/dev/null || true; \
    # M2: NetworkManager is the network manager (systemd-networkd disabled at
    # runtime in favour of NM).
    systemctl enable NetworkManager.service wpa_supplicant.service 2>/dev/null || true; \
    # M3: PipeWire + WirePlumber as per-user services
    systemctl --global enable pipewire.socket pipewire-pulse.socket wireplumber.service 2>/dev/null || true; \
    # The keyring's control socket, so pam_gnome_keyring (ly, greetd) unlocks
    # the login keyring in a daemon the user's systemd owns.
    systemctl --global enable gnome-keyring-daemon.socket; \
    test -L /etc/systemd/user/sockets.target.wants/gnome-keyring-daemon.socket || exit 1; \
    # M4: Bluetooth daemon. `enable` only links it into bluetooth.target.wants,
    # but Kairos forces multi-user.target (bluetooth.target is never reached), so
    # like ly it must be pulled into multi-user.target directly or bluetoothd
    # never starts and bluetoothctl hangs on "waiting for bluetoothd".
    systemctl enable bluetooth.service 2>/dev/null || true; \
    ln -sf /usr/lib/systemd/system/bluetooth.service /etc/systemd/system/multi-user.target.wants/bluetooth.service; \
    # Docker must stay disabled in the baked filesystem: the live installer's
    # /var is itself an overlayfs, so starting dockerd there would make overlay2
    # repeatedly fail its nested-overlay check. The guarded 92_docker.yaml boot
    # stage enables and starts Docker only on the installed system, after Kairos
    # has bind-mounted persistent /var/lib/docker.
    systemctl disable docker.socket docker.service 2>/dev/null || true; \
    rm -f /etc/systemd/system/sockets.target.wants/docker.socket \
      /etc/systemd/system/multi-user.target.wants/docker.service; \
    # Flatpak's fusermount helpers must be setuid-root (revokefs-fuse + the
    # document portal). Docker is rootful, so no newuidmap/newgidmap setuid here.
    for h in /usr/bin/fusermount3 /usr/bin/fusermount; do \
        [ -e "$h" ] && chmod u+s "$h" || true; \
    done; \
    # Per-user setup (Flathub remote) runs late on the booted system via a
    # systemd oneshot. Like ly/bluetooth it must be pulled into
    # multi-user.target directly (Kairos forces that target).
    chmod +x /usr/bin/hadron-user-setup; \
    systemctl enable hadron-user-setup.service 2>/dev/null || true; \
    ln -sf /usr/lib/systemd/system/hadron-user-setup.service /etc/systemd/system/multi-user.target.wants/hadron-user-setup.service

# Installer TUI. /usr/bin, not /usr/local/bin: /usr/local is shadowed by an empty
# overlay at runtime on Kairos, so a binary there is invisible to the installed
# system. (The shell installer it replaces lived in /usr/local/bin and only
# worked because the installer runs in the live environment.)
COPY --from=install-ui-build /out/hadron-install-ui /usr/bin/hadron-install-ui
# Update checker + upgrade CLI. /usr/bin for the same /usr/local reason.
COPY --from=update-build /out/hadron-update /usr/bin/hadron-update
# hadron-ext is the same binary: run under that name it is `hadron-update ext`
# (system extensions, see update/ext.go).
RUN ln -sf hadron-update /usr/bin/hadron-ext


# ===========================================================================
# Kairos init layer — makes the image bootable/installable.
#
# Folds in what used to be a separately-fetched Kairos Dockerfile
# (kairos-io/kairos images/Dockerfile): bind-mount the kairos-init tool and run
# its `install` then `init` stages on top of the desktop image. This is the
# default build target, so a single `docker build .` produces an image
# AuroraBoot can turn straight into an ISO. Build `--target default` for the
# bare desktop image without this layer.
# ===========================================================================
FROM ${KAIROS_INIT_IMAGE} AS kairos-init

FROM default AS kairos
ARG MODEL=generic
ARG TRUSTED_BOOT=false
ARG VERSION=v0.0.0
ARG FIPS=no-fips
# The splash shows the release. kairos-init's dracut module copies every file in
# /etc/kairos/branding/splash into the initramfs, so writing it here, before
# kairos-init builds the initramfs, gives both halves of the boot the same one.
RUN mkdir -p /etc/kairos/branding/splash && printf '%s\n' "${VERSION}" > /etc/kairos/branding/splash/version
# System extensions from the catalogs are unsigned; the normal install merges
# them anyway (rootfs/etc/kairos/90_hadron_extensions.yaml and
# rootfs/system/oem/93_hadron_extensions.yaml). A Trusted Boot image keeps
# signatures on: it gets neither file.
RUN [ "${TRUSTED_BOOT}" = false ] || \
    rm -f /etc/kairos/90_hadron_extensions.yaml /system/oem/93_hadron_extensions.yaml
RUN --mount=type=bind,from=kairos-init,src=/kairos-init,dst=/kairos-init \
    fips_flag=""; [ "$FIPS" = "fips" ] && fips_flag="--fips"; \
    /kairos-init -l debug -s install -m "${MODEL}" -t "${TRUSTED_BOOT}" --version "${VERSION}" $fips_flag && \
    /kairos-init -l debug -s init    -m "${MODEL}" -t "${TRUSTED_BOOT}" --version "${VERSION}" $fips_flag
# No default `kairos` user on an installed system. kairos-init's
# 10_accounting.yaml creates a locked one on every boot, and COSMIC's greeter
# lists it as "Created by entities". Keep it on the live ISO and in UKI install
# mode, where it gets the password "kairos". The script fails the build if
# kairos-init changes that step; test/accounting/check-default-user.sh checks
# the result.
RUN --mount=type=bind,source=scripts/kairos-user-live-only.sh,target=/tmp/kairos-user-live-only.sh \
    sh /tmp/kairos-user-live-only.sh /system/oem/10_accounting.yaml
# Brand the GRUB boot-menu entry. kairos-agent reads GRUB_ENTRY_NAME from
# /etc/kairos-release (which kairos-init just generated) when install.grub-entry-
# name is unset, preferring it over /etc/os-release. The OS identity fields in
# /etc/os-release are intentionally left untouched.
RUN echo 'GRUB_ENTRY_NAME="hadron-desktop"' >> /etc/kairos-release
# Release identity for hadron-update. FLAVOR and GPU name the image line this
# system upgrades along: hadron-update pulls ${IMAGE_REPO}:${FLAVOR}-<tag> for
# GPU=full and ${IMAGE_REPO}:${FLAVOR}-vm-<tag> for GPU=vm, which release.yml
# pushes for every published release. Dockerfile.agent rewrites FLAVOR to
# `agent` for the appliance, which has the one line agent-<tag>. VERSION is the
# release tag, so a dev build (v0.0.0) never sorts as newer than a real
# release.
ARG IMAGE_REPO=ghcr.io/kairos-community/hadron-desktop
ARG HADRON_VERSION
ARG DESKTOP
ARG GPU
ARG FIRMWARE=false
RUN case "${GPU}" in full|vm) ;; *) echo "GPU must be full or vm, not '${GPU}'" >&2; exit 1 ;; esac && \
    printf 'FLAVOR=%s\nGPU=%s\nVERSION=%s\nHADRON_VERSION=%s\nIMAGE_REPO=%s\nGITHUB_REPO=%s\nTRUSTED_BOOT=%s\n' \
      "${DESKTOP}" "${GPU}" "${VERSION}" "${HADRON_VERSION}" "${IMAGE_REPO}" kairos-community/hadron-desktop "${TRUSTED_BOOT}" \
      > /etc/hadron-desktop/release
# The same identity as OCI labels, so hadron-update can tell a Hadron Desktop
# image from any other one without pulling it (the registry config blob carries
# them). Kept in this last stage, after every expensive one, because the value
# changes per release and a LABEL ahead of them would invalidate their cache.
# The keys must match the constants in update/policy.go.
LABEL io.kairos.hadron-desktop.flavor="${DESKTOP}" \
      io.kairos.hadron-desktop.version="${VERSION}" \
      io.kairos.hadron-desktop.hadron-version="${HADRON_VERSION}" \
      io.kairos.hadron-desktop.gpu="${GPU}" \
      io.kairos.hadron-desktop.firmware="${FIRMWARE}"
# Tokyo Night GRUB menu colours — text-mode fallback. The main menu renders from
# /etc/cos/grub.cfg (copied to COS_STATE at install) before grubmenu.cfg switches
# to the gfxterm theme, and these 16-colour accents also apply if gfxterm/the
# theme ever fail to load. Inserted after `loadfont unicode`, before the menu.
# A Trusted Boot image has no GRUB: kairos-init writes no /etc/cos/grub.cfg
# with -t true, and systemd-boot boots the signed UKIs directly.
RUN [ "${TRUSTED_BOOT}" = true ] || sed -i '/^loadfont unicode/a set color_normal=light-gray/black\nset color_highlight=black/cyan\nset menu_color_normal=cyan/black\nset menu_color_highlight=black/cyan' /etc/cos/grub.cfg
# Tokyo Night gfxterm theme with a background image. kairos-init's branding stage
# writes its own /etc/kairos/branding/grubmenu.cfg (bundled.ExtraGrubCfg),
# clobbering the overlay copy, so re-apply ours AFTER kairos-init: it switches the
# installed menu to gfxterm and `set theme=` to themes/hadron/theme.txt. 08_grub
# copies this to COS_STATE/grubmenu at install; 09_hadron_grub_theme.yaml (oem)
# runs /usr/libexec/hadron-grub-theme, which puts the theme dir, the font and the
# EFI grub modules on STATE so gfxmenu/tga/the theme all resolve there.
# (hadron-theme/ isn't written by kairos-init, so its COPY at line ~1982 survives.)
COPY rootfs/etc/kairos/branding/grubmenu.cfg /etc/kairos/branding/grubmenu.cfg
# Console font. kairos-init may regenerate /etc/vconsole.conf, so re-apply ours
# after it — same treatment as /etc/issue and /etc/motd below.
COPY rootfs/etc/vconsole.conf /etc/vconsole.conf
# Per-variant GRUB theme subtitle. theme.txt ships in the COMMON overlay with an
# @VARIANT_SUBTITLE@ placeholder; the variant overlay supplies DESKTOP_NAME and
# DISPLAY_NAME via /etc/hadron-desktop/session. Lowercased to match the theme's
# typographic style ("i3 · xlibre · kairos").
#
# The chain is a single && list ending in `! grep`, so ANY link failing (missing
# session file, empty names, sed error) or the placeholder surviving fails the
# build: a theme with a literal @VARIANT_SUBTITLE@ would render that text on the
# boot menu.
RUN . /etc/hadron-desktop/session && \
    [ -n "$DESKTOP_NAME" ] && [ -n "$DISPLAY_NAME" ] && \
    subtitle="$(printf '%s · %s · kairos' "$DESKTOP_NAME" "$DISPLAY_NAME" | tr '[:upper:]' '[:lower:]')" && \
    sed -i "s|@VARIANT_SUBTITLE@|${subtitle}|" /etc/kairos/branding/hadron-theme/theme.txt && \
    ! grep -q '@VARIANT_SUBTITLE@' /etc/kairos/branding/hadron-theme/theme.txt
# kairos-init regenerates /etc/motd (and may touch /etc/issue); re-apply the
# branded console banners from the selected desktop overlay on top.
COPY --from=desktop-config /etc/issue /etc/motd /etc/
