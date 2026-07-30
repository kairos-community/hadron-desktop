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

ARG BASE_IMAGE=ghcr.io/kairos-io/hadron:main
# Select the display server + window manager pair. Supported values: sway, i3.
ARG DESKTOP=sway
# GPU=vm (default): software/virtual GL only — no LLVM is built.
# GPU=full: hardware GL (iris/radeonsi) — builds the LLVM/SPIRV stack below.
ARG GPU=vm
# kairos-init tool version for the bootable layer (final `kairos` stage). Global
# ARG so it is usable in that stage's FROM.
ARG KAIROS_INIT=v0.14.0

FROM ghcr.io/kairos-io/hadron-toolchain:main AS toolchain

# ---------------------------------------------------------------------------
# Boot splash. A vendored C99 fork of the upstream hadron splash, rebranded for
# hadron-desktop (Tokyo Night VGA-16 ramp, ANSI-Shadow HADRON wordmark). The
# same binary serves BOTH the initramfs copy (baked in by the 50hadron-splash
# dracut module) and the booted system's hadron-splash.service, so there is one
# source of truth for the animation.
#
# The build-time smoke test exercises the non-tty fallback path in main(), which
# links and runs the whole program — a segfault or a missing symbol fails the
# build here rather than at boot, where it would be invisible behind `quiet`.
# ---------------------------------------------------------------------------
FROM toolchain AS hadron-splash
COPY splash/ /build/splash/
RUN cd /build/splash && make clean && make && ./hadron-splash | grep -q HADRON

# ===========================================================================
# Wayland display stack (shared with the doom example)
# ===========================================================================

FROM toolchain AS wayland
ARG WAYLAND_VERSION=1.24.0
ARG WAYLAND_PROTOCOLS_VERSION=1.46
RUN mkdir -p /wayland
WORKDIR /build
RUN curl -L https://gitlab.freedesktop.org/wayland/wayland/-/archive/${WAYLAND_VERSION}/wayland-${WAYLAND_VERSION}.tar -o wayland.tar && tar -xf wayland.tar && rm wayland.tar && mv wayland-* wayland-src
RUN curl -L https://gitlab.freedesktop.org/wayland/wayland-protocols/-/archive/${WAYLAND_PROTOCOLS_VERSION}/wayland-protocols-${WAYLAND_PROTOCOLS_VERSION}.tar -o wayland-protocols.tar && tar -xf wayland-protocols.tar && rm wayland-protocols.tar && mv wayland-protocols-* wayland-protocols-src
RUN pip3 install meson ninja
WORKDIR /build/wayland-src
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=false -Ddocumentation=false -Dscanner=true -Dlibraries=true
RUN DESTDIR=/wayland ninja -C buildDir install
RUN ninja -C buildDir install
WORKDIR /build/wayland-protocols-src
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=false
RUN DESTDIR=/wayland ninja -C buildDir install


FROM toolchain AS libdrm
ARG LIBDRM_VERSION=2.4.129
RUN mkdir -p /libdrm
WORKDIR /build
RUN curl -L https://dri.freedesktop.org/libdrm/libdrm-${LIBDRM_VERSION}.tar.xz -o libdrm.tar.xz && tar -xf libdrm.tar.xz && rm libdrm.tar.xz && mv libdrm-* libdrm-src
WORKDIR /build/libdrm-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=false
RUN DESTDIR=/libdrm ninja -C buildDir install


# systemd/libudev in the Hadron musl toolchain is linked against libucontext.
# Build it explicitly so every stage using those toolchain libraries can link
# reliably, and copy it into both desktop runtime roots below.
FROM toolchain AS libucontext
ARG LIBUCONTEXT_VERSION=1.5.1
RUN mkdir -p /libucontext
WORKDIR /build
RUN curl -fL --retry 5 https://github.com/kaniini/libucontext/archive/refs/tags/libucontext-${LIBUCONTEXT_VERSION}.tar.gz -o libucontext.tar.gz && \
    tar -xf libucontext.tar.gz && mv libucontext-* src
WORKDIR /build/src
RUN make -j"$(nproc)" ARCH=x86_64
RUN make ARCH=x86_64 DESTDIR=/libucontext install


# ===========================================================================
# LLVM / SPIRV stack for hardware GL (Mesa iris + radeonsi).
#
# Kept ENTIRELY inside this example so the main Hadron toolchain Dockerfile is
# never touched: these stages build LLVM/clang/libclc + SPIRV on top of the
# published hadron-toolchain (same musl ABI — no Alpine cross-mix). They are
# compiled ONLY for GPU=full; the `llvm-stack` indirection at the bottom makes
# the default GPU=vm build prune all of them (BuildKit only builds referenced
# stages), so a normal build never pays for LLVM.
#
# Validated on hadron-toolchain (musl / GCC 15): LLVM 20.1.8 (LLVM 18 fails on
# GCC 15), amdgcn libclc, SPIRV-Tools, and llvm-spirv all compile; Mesa 25.3
# links iris/radeonsi against them.
# ===========================================================================

FROM toolchain AS spirv-tools
ARG SPIRV_HEADERS_VERSION=vulkan-sdk-1.4.309.0
ARG SPIRV_TOOLS_VERSION=vulkan-sdk-1.4.309.0
RUN pip3 install ninja
WORKDIR /b
RUN curl -fL https://github.com/KhronosGroup/SPIRV-Headers/archive/refs/tags/${SPIRV_HEADERS_VERSION}.tar.gz -o sh.tgz && \
    curl -fL https://github.com/KhronosGroup/SPIRV-Tools/archive/refs/tags/${SPIRV_TOOLS_VERSION}.tar.gz -o st.tgz && \
    tar -xf sh.tgz && mv SPIRV-Headers-* spirv-headers && \
    tar -xf st.tgz && mv SPIRV-Tools-* spirv-tools && \
    cp -a spirv-headers spirv-tools/external/spirv-headers
RUN cd spirv-headers && cmake -G Ninja -B build -DCMAKE_INSTALL_PREFIX=/usr && \
    DESTDIR=/spirv-tools ninja -C build install
RUN cd spirv-tools && cmake -G Ninja -B build -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr \
      -DSPIRV_SKIP_TESTS=ON -DBUILD_SHARED_LIBS=ON \
      -DSPIRV-Headers_SOURCE_DIR=/b/spirv-tools/external/spirv-headers && \
    ninja -C build -j$(nproc) && DESTDIR=/spirv-tools ninja -C build install

# LLVM + clang + amdgcn libclc. LTO off (brutal for LLVM); shared libLLVM dylib;
# in-tree SPIRV backend kept so clang can emit SPIR-V. The spirv libclc targets
# need llvm-spirv at configure time (circular dep on LLVM), so libclc builds the
# amdgcn targets only — enough for radeonsi + Mesa's libclc dependency.
FROM toolchain AS llvm
ARG LLVM_VERSION=20.1.8
RUN pip3 install ninja
WORKDIR /build
RUN curl -fL https://github.com/llvm/llvm-project/releases/download/llvmorg-${LLVM_VERSION}/llvm-project-${LLVM_VERSION}.src.tar.xz -o llvm.tar.xz && \
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

# SPIRV-LLVM-Translator (llvm-spirv). Its cmake otherwise git-clones SPIRV-Headers
# at a pinned commit; with no git/network in the build, feed it a local checkout
# at exactly that commit (its sources use newer SPIR-V caps than the spirv-tools
# headers tag).
FROM toolchain AS spirv-llvm-translator
ARG SPIRV_LLVM_TRANSLATOR_VERSION=20.1.3
ARG SPIRV_HEADERS_TRANSLATOR_COMMIT=0e710677989b4326ac974fd80c5308191ed80965
RUN pip3 install ninja
COPY --from=llvm /llvm /llvm
RUN rsync -aHAX --keep-dirlinks /llvm/. /
WORKDIR /b
RUN curl -fL https://github.com/KhronosGroup/SPIRV-Headers/archive/${SPIRV_HEADERS_TRANSLATOR_COMMIT}.tar.gz -o sh.tgz && \
    tar -xf sh.tgz && mv SPIRV-Headers-* spirv-headers && \
    curl -fL https://github.com/KhronosGroup/SPIRV-LLVM-Translator/archive/refs/tags/v${SPIRV_LLVM_TRANSLATOR_VERSION}.tar.gz -o tr.tgz && \
    tar -xf tr.tgz && mv SPIRV-LLVM-Translator-* tr
RUN cd tr && cmake -G Ninja -B build -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr \
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

# Empty stand-in (GPU=vm path) — selecting this prunes every LLVM stage above.
FROM toolchain AS llvm-stack-vm
RUN mkdir -p /llvm-out

# Pick the stack by GPU flag. For vm this resolves to the empty stage, so the
# heavy LLVM stages are never referenced and BuildKit skips them.
FROM llvm-stack-${GPU} AS llvm-stack


FROM toolchain AS mesa
COPY --from=libdrm /libdrm /
# LLVM stack: a populated /usr tree for GPU=full, empty for GPU=vm.
COPY --from=llvm-stack /llvm-out /llvm-out
RUN rsync -aHAX --keep-dirlinks /llvm-out/. / && rm -rf /llvm-out
COPY --from=wayland /wayland /
ARG MESA_VERSION=25.3.0
RUN mkdir -p /mesa
WORKDIR /build
RUN curl -L https://archive.mesa3d.org/mesa-${MESA_VERSION}.tar.xz -o mesa.tar.xz && tar -xf mesa.tar.xz && rm mesa.tar.xz && mv mesa-* mesa-src
WORKDIR /build/mesa-src
RUN pip3 install meson ninja setuptools mako pyyaml
# GPU=vm (default): software/virtual GL only (virgl/softpipe/svga), no LLVM.
# GPU=full: real hardware GL — iris (Intel) + radeonsi (AMD), linked against the
#   LLVM/libclc/SPIRV stack built by the stages above (rsync'd into /usr). Pair
#   with --build-arg FIRMWARE=true so the i915/amdgpu blobs are present.
ARG GPU=vm
# The example's libLLVM is built without RTTI, so Mesa must disable it too
# (cpp_rtti=false) when linking LLVM; harmless for the software-only vm build.
RUN if [ "$GPU" = "full" ]; then \
      DRV="iris,radeonsi,virgl,softpipe,svga"; LLVMOPT=true; RTTI=false; \
    else \
      DRV="virgl,softpipe,svga"; LLVMOPT=false; RTTI=true; \
    fi; \
    meson setup buildDir ${COMMON_MESON_FLAGS} -Dplatforms=wayland \
    -Dgallium-drivers="$DRV" \
    -Dglx=disabled \
    -Dopengl=true \
    -Dgles1=enabled \
    -Dgles2=enabled \
    -Degl=enabled \
    -Dvulkan-drivers= \
    -Dllvm="$LLVMOPT" \
    -Dcpp_rtti="$RTTI" \
    -Dbuild-tests=false
RUN DESTDIR=/mesa ninja -C buildDir install
# The radeonsi/iris megadriver links libLLVM (+ libelf) at runtime; these come
# from the toolchain and aren't otherwise in the OS image, so bundle them for
# the GPU=full build (~125MB — the cost of hardware GL).
RUN if [ "$GPU" = "full" ]; then \
      mkdir -p /mesa/usr/lib && \
      cp -a /usr/lib/libLLVM.so* /mesa/usr/lib/ && \
      cp -a /usr/lib/libelf.so* /mesa/usr/lib/ ; \
    fi


FROM toolchain AS xkeyboard-config
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
ARG LIBXKBCOMMON_VERSION=1.13.0
RUN mkdir -p /libxkb
WORKDIR /build
RUN curl -L https://github.com/xkbcommon/libxkbcommon/archive/refs/tags/xkbcommon-${LIBXKBCOMMON_VERSION}.tar.gz -o libxkbcommon.tar.gz && tar -xzf libxkbcommon.tar.gz && rm libxkbcommon.tar.gz && mv libxkbcommon-* libxkbcommon-src
WORKDIR /build/libxkbcommon-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Denable-x11=false -Denable-xkbregistry=true -Denable-bash-completion=false
RUN DESTDIR=/libxkb ninja -C buildDir install


FROM toolchain AS pixman
ARG PIXMAN_VERSION=0.46.0
RUN mkdir -p /pixman
WORKDIR /build
RUN curl -L https://www.cairographics.org/releases/pixman-${PIXMAN_VERSION}.tar.gz -o pixman.tar.gz && tar -xzf pixman.tar.gz && rm pixman.tar.gz && mv pixman-* pixman-src
WORKDIR /build/pixman-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS}
RUN DESTDIR=/pixman ninja -C buildDir install


FROM toolchain AS libevdev
ARG LIBEVDEV_VERSION=1.13.5
RUN mkdir -p /libevdev
WORKDIR /build
RUN curl -L https://gitlab.freedesktop.org/libevdev/libevdev/-/archive/libevdev-${LIBEVDEV_VERSION}/libevdev-libevdev-${LIBEVDEV_VERSION}.tar.gz -o libevdev.tar.gz && tar -xzf libevdev.tar.gz && rm libevdev.tar.gz && mv libevdev-* libevdev-src
WORKDIR /build/libevdev-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=disabled -Ddocumentation=disabled -Dtools=disabled
RUN DESTDIR=/libevdev ninja -C buildDir install


FROM toolchain AS libinput
COPY --from=libucontext /libucontext /
COPY --from=libevdev /libevdev /
ARG LIBINPUT_VERSION=1.30.0
RUN mkdir -p /libinput
WORKDIR /build
RUN curl -L https://gitlab.freedesktop.org/libinput/libinput/-/archive/${LIBINPUT_VERSION}/libinput-${LIBINPUT_VERSION}.tar.gz -o libinput.tar.gz && tar -xf libinput.tar.gz && rm libinput.tar.gz && mv libinput-* libinput-src
WORKDIR /build/libinput-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dmtdev=false -Dlibwacom=false -Dtests=false -Ddebug-gui=false
RUN DESTDIR=/libinput ninja -C buildDir install


FROM toolchain AS hwdata
ARG HWDATA_VERSION=0.401
RUN mkdir -p /hwdata
WORKDIR /build
RUN curl -L https://github.com/vcrhonek/hwdata/archive/v${HWDATA_VERSION}/hwdata-${HWDATA_VERSION}.tar.gz -o hwdata.tar.xz && tar -xf hwdata.tar.xz && rm hwdata.tar.xz && mv hwdata-* hwdata-src
WORKDIR /build/hwdata-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking
RUN make -j$(nproc) && make install DESTDIR=/hwdata


FROM toolchain AS libdisplay-info
COPY --from=hwdata /hwdata /
ARG LIBDISPLAY_INFO_VERSION=0.3.0
RUN mkdir -p /libdisplay-info
WORKDIR /build
RUN curl -L https://gitlab.freedesktop.org/emersion/libdisplay-info/-/releases/${LIBDISPLAY_INFO_VERSION}/downloads/libdisplay-info-${LIBDISPLAY_INFO_VERSION}.tar.xz -o libdisplay-info.tar.xz && tar -xf libdisplay-info.tar.xz && rm libdisplay-info.tar.xz && mv libdisplay-info-* libdisplay-info-src
WORKDIR /build/libdisplay-info-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS}
RUN DESTDIR=/libdisplay-info ninja -C buildDir install


# ===========================================================================
# Shared desktop libraries
# ===========================================================================

# PCRE2 — required by glib
FROM toolchain AS pcre2
ARG PCRE2_VERSION=10.44
RUN mkdir -p /pcre2
WORKDIR /build
RUN curl -L https://github.com/PCRE2Project/pcre2/releases/download/pcre2-${PCRE2_VERSION}/pcre2-${PCRE2_VERSION}.tar.gz -o pcre2.tar.gz && tar -xzf pcre2.tar.gz && rm pcre2.tar.gz && mv pcre2-* pcre2-src
WORKDIR /build/pcre2-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking --enable-pcre2-16 --enable-pcre2-32
RUN make -j$(nproc) && make install DESTDIR=/pcre2


# libpng — needed by grim (PNG screenshots)
FROM toolchain AS libpng
ARG LIBPNG_VERSION=1.6.44
RUN mkdir -p /libpng
WORKDIR /build
RUN curl -L https://download.sourceforge.net/libpng/libpng-${LIBPNG_VERSION}.tar.xz -o libpng.tar.xz && tar -xf libpng.tar.xz && rm libpng.tar.xz && mv libpng-* libpng-src
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
ARG GLIB_VERSION=2.82.5
RUN mkdir -p /glib2
WORKDIR /build
RUN GLIB_MAJOR="${GLIB_VERSION%.*}" && curl -L https://download.gnome.org/sources/glib/${GLIB_MAJOR}/glib-${GLIB_VERSION}.tar.xz -o glib.tar.xz && tar -xf glib.tar.xz && rm glib.tar.xz && mv glib-* glib-src
WORKDIR /build/glib-src
RUN pip3 install meson ninja packaging
RUN meson setup buildDir ${COMMON_MESON_FLAGS} \
    -Dnls=disabled -Dtests=false -Dlibmount=disabled -Dselinux=disabled \
    -Dman-pages=disabled -Dintrospection=disabled -Dglib_debug=disabled \
    -Ddtrace=disabled -Dsysprof=disabled
RUN DESTDIR=/glib2 ninja -C buildDir install


# FreeType — built without harfbuzz (broken circular dep), png/brotli optional
FROM toolchain AS freetype
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
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking --with-harfbuzz=no --with-png=no --with-brotli=no --with-bzip2=no --with-zlib=yes
RUN make -j$(nproc) && make install DESTDIR=/freetype


# HarfBuzz — needs freetype + glib
FROM toolchain AS harfbuzz
COPY --from=freetype /freetype /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
ARG HARFBUZZ_VERSION=10.1.0
RUN mkdir -p /harfbuzz
WORKDIR /build
RUN curl -L https://github.com/harfbuzz/harfbuzz/releases/download/${HARFBUZZ_VERSION}/harfbuzz-${HARFBUZZ_VERSION}.tar.xz -o harfbuzz.tar.xz && tar -xf harfbuzz.tar.xz && rm harfbuzz.tar.xz && mv harfbuzz-* harfbuzz-src
WORKDIR /build/harfbuzz-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=disabled -Ddocs=disabled -Dintrospection=disabled -Dglib=enabled -Dfreetype=enabled -Dcairo=disabled -Dchafa=disabled
RUN DESTDIR=/harfbuzz ninja -C buildDir install


# FriBidi — bidirectional text, needed by pango
FROM toolchain AS fribidi
ARG FRIBIDI_VERSION=1.0.16
RUN mkdir -p /fribidi
WORKDIR /build
RUN curl -L https://github.com/fribidi/fribidi/releases/download/v${FRIBIDI_VERSION}/fribidi-${FRIBIDI_VERSION}.tar.xz -o fribidi.tar.xz && tar -xf fribidi.tar.xz && rm fribidi.tar.xz && mv fribidi-* fribidi-src
WORKDIR /build/fribidi-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=false -Ddocs=false -Dbin=false
RUN DESTDIR=/fribidi ninja -C buildDir install


# gperf — build-time tool required by fontconfig
FROM toolchain AS gperf
ARG GPERF_VERSION=3.1
RUN mkdir -p /gperf
WORKDIR /build
RUN curl -L https://ftp.gnu.org/pub/gnu/gperf/gperf-${GPERF_VERSION}.tar.gz -o gperf.tar.gz && tar -xzf gperf.tar.gz && rm gperf.tar.gz && mv gperf-* gperf-src
WORKDIR /build/gperf-src
# gperf 3.1 ships an ancient K&R getopt.c; the toolchain GCC defaults to C23
# where `()` means `(void)`, so force the older language semantics.
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking
RUN make -j$(nproc) CFLAGS="-std=gnu17 -O2 -g" && make install DESTDIR=/gperf


# Fontconfig (+ DejaVu fonts) — expat from toolchain, freetype built above
FROM toolchain AS fontconfig
COPY --from=freetype /freetype /
COPY --from=gperf /gperf /
ARG FONTCONFIG_VERSION=2.15.0
RUN mkdir -p /fontconfig
WORKDIR /build
RUN curl -L https://www.freedesktop.org/software/fontconfig/release/fontconfig-${FONTCONFIG_VERSION}.tar.xz -o fontconfig.tar.xz && tar -xf fontconfig.tar.xz && rm fontconfig.tar.xz && mv fontconfig-* fontconfig-src
WORKDIR /build/fontconfig-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=disabled -Dtools=disabled -Ddoc=disabled -Dnls=disabled -Dcache-build=disabled
RUN DESTDIR=/fontconfig ninja -C buildDir install
# Bundle DejaVu fonts
ARG DEJAVU_VERSION=2.37
RUN mkdir -p /fontconfig/usr/share/fonts/dejavu
RUN curl -L https://github.com/dejavu-fonts/dejavu-fonts/releases/download/version_2_37/dejavu-fonts-ttf-${DEJAVU_VERSION}.tar.bz2 -o dejavu.tar.bz2 && \
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
ARG CAIRO_VERSION=1.18.4
RUN mkdir -p /cairo
WORKDIR /build
RUN curl -L https://cairographics.org/releases/cairo-${CAIRO_VERSION}.tar.xz -o cairo.tar.xz && tar -xf cairo.tar.xz && rm cairo.tar.xz && mv cairo-* cairo-src
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
ARG PANGO_VERSION=1.54.0
RUN mkdir -p /pango
WORKDIR /build
RUN PANGO_MAJOR="${PANGO_VERSION%.*}" && curl -L https://download.gnome.org/sources/pango/${PANGO_MAJOR}/pango-${PANGO_VERSION}.tar.xz -o pango.tar.xz && tar -xf pango.tar.xz && rm pango.tar.xz && mv pango-* pango-src
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
ARG LIBEPOXY_VERSION=1.5.10
RUN mkdir -p /libepoxy
WORKDIR /build
RUN curl -fL https://github.com/anholt/libepoxy/archive/refs/tags/${LIBEPOXY_VERSION}.tar.gz -o epoxy.tar.gz && tar -xf epoxy.tar.gz && rm epoxy.tar.gz && mv libepoxy-* epoxy-src
WORKDIR /build/epoxy-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dglx=no -Dx11=false -Degl=yes -Dtests=false
RUN DESTDIR=/libepoxy ninja -C buildDir install

# gdk-pixbuf — image loading (PNG only is plenty for the bar/applets).
FROM toolchain AS gdk-pixbuf
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=libpng /libpng /
ARG GDKPIXBUF_VERSION=2.42.12
RUN mkdir -p /gdk-pixbuf
WORKDIR /build
RUN GP_MAJOR="${GDKPIXBUF_VERSION%.*}" && curl -fL https://download.gnome.org/sources/gdk-pixbuf/${GP_MAJOR}/gdk-pixbuf-${GDKPIXBUF_VERSION}.tar.xz -o gp.tar.xz && tar -xf gp.tar.xz && rm gp.tar.xz && mv gdk-pixbuf-* gp-src
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
RUN ATK_MAJOR="${ATK_VERSION%.*}" && curl -fL https://download.gnome.org/sources/atk/${ATK_MAJOR}/atk-${ATK_VERSION}.tar.xz -o atk.tar.xz && tar -xf atk.tar.xz && rm atk.tar.xz && mv atk-* atk-src
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
ARG GTK3_VERSION=3.24.43
RUN mkdir -p /gtk3
WORKDIR /build
RUN GTK_MAJOR="${GTK3_VERSION%.*}" && curl -fL https://download.gnome.org/sources/gtk+/${GTK_MAJOR}/gtk+-${GTK3_VERSION}.tar.xz -o gtk.tar.xz && tar -xf gtk.tar.xz && rm gtk.tar.xz && mv gtk+-* gtk-src
WORKDIR /build/gtk-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} \
    -Dx11_backend=false -Dwayland_backend=true -Dbroadway_backend=false \
    -Dintrospection=false -Dgtk_doc=false -Dman=false -Ddemos=false -Dexamples=false -Dtests=false \
    -Dprint_backends=file -Dcolord=no
RUN DESTDIR=/gtk3 ninja -C buildDir install

# --- gtkmm3 C++ wrappers (waybar links these) ------------------------------
FROM toolchain AS libsigcpp
ARG LIBSIGCPP_VERSION=2.12.1
RUN mkdir -p /libsigcpp
WORKDIR /build
RUN curl -fL https://download.gnome.org/sources/libsigc++/2.12/libsigc++-${LIBSIGCPP_VERSION}.tar.xz -o s.tar.xz && tar -xf s.tar.xz && rm s.tar.xz && mv libsigc++-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dbuild-documentation=false -Dbuild-examples=false
RUN DESTDIR=/libsigcpp ninja -C buildDir install

FROM toolchain AS glibmm
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=libsigcpp /libsigcpp /
ARG GLIBMM_VERSION=2.66.7
RUN mkdir -p /glibmm
WORKDIR /build
RUN curl -fL https://download.gnome.org/sources/glibmm/2.66/glibmm-${GLIBMM_VERSION}.tar.xz -o g.tar.xz && tar -xf g.tar.xz && rm g.tar.xz && mv glibmm-* src
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
ARG PANGOMM_VERSION=2.46.4
RUN mkdir -p /pangomm
WORKDIR /build
RUN curl -fL https://download.gnome.org/sources/pangomm/2.46/pangomm-${PANGOMM_VERSION}.tar.xz -o p.tar.xz && tar -xf p.tar.xz && rm p.tar.xz && mv pangomm-* src
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
ARG ATKMM_VERSION=2.28.4
RUN mkdir -p /atkmm
WORKDIR /build
RUN curl -fL https://download.gnome.org/sources/atkmm/2.28/atkmm-${ATKMM_VERSION}.tar.xz -o a.tar.xz && tar -xf a.tar.xz && rm a.tar.xz && mv atkmm-* src
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
ARG GTKMM_VERSION=3.24.9
RUN mkdir -p /gtkmm3
WORKDIR /build
RUN curl -fL https://download.gnome.org/sources/gtkmm/3.24/gtkmm-${GTKMM_VERSION}.tar.xz -o gm.tar.xz && tar -xf gm.tar.xz && rm gm.tar.xz && mv gtkmm-* src
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
ARG GTKLAYERSHELL_VERSION=0.8.2
RUN mkdir -p /gtk-layer-shell
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/wmww/gtk-layer-shell/archive/refs/tags/v${GTKLAYERSHELL_VERSION}.tar.gz -o g.tar.gz && tar -xf g.tar.gz && rm g.tar.gz && mv gtk-layer-shell-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dintrospection=false -Dvapi=false -Dexamples=false -Ddocs=false -Dtests=false
RUN DESTDIR=/gtk-layer-shell ninja -C buildDir install

FROM toolchain AS jsoncpp
ARG JSONCPP_VERSION=1.9.6
RUN mkdir -p /jsoncpp
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/open-source-parsers/jsoncpp/archive/refs/tags/${JSONCPP_VERSION}.tar.gz -o j.tar.gz && tar -xf j.tar.gz && rm j.tar.gz && mv jsoncpp-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dtests=false
RUN DESTDIR=/jsoncpp ninja -C buildDir install

FROM toolchain AS libfmt
ARG FMT_VERSION=10.2.1
RUN mkdir -p /libfmt
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/fmtlib/fmt/archive/refs/tags/${FMT_VERSION}.tar.gz -o f.tar.gz && tar -xf f.tar.gz && rm f.tar.gz && mv fmt-* src
WORKDIR /build/src
RUN cmake -B buildDir -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_BUILD_TYPE=MinSizeRel -DBUILD_SHARED_LIBS=ON -DFMT_TEST=OFF -DFMT_DOC=OFF
RUN cmake --build buildDir -j"$(nproc)" && DESTDIR=/libfmt cmake --install buildDir

FROM toolchain AS spdlog
COPY --from=libfmt /libfmt /
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
FROM toolchain AS libdbusmenu
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
ARG LIBDBUSMENU_VERSION=16.04.0
RUN mkdir -p /libdbusmenu
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
ARG SEATD_VERSION=0.9.1
RUN mkdir -p /libseat
WORKDIR /build
RUN curl -L https://git.sr.ht/~kennylevinsen/seatd/archive/${SEATD_VERSION}.tar.gz -o seatd.tar.gz && tar -xf seatd.tar.gz && rm seatd.tar.gz && mv seatd-* seatd-src
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
ARG WLROOTS_VERSION=0.18.2
WORKDIR /build
RUN curl -L https://gitlab.freedesktop.org/wlroots/wlroots/-/archive/${WLROOTS_VERSION}/wlroots-${WLROOTS_VERSION}.tar -o wlroots.tar && tar -xf wlroots.tar && rm wlroots.tar && mv wlroots-* wlroots-src
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
ARG SWAY_VERSION=1.10.1
RUN mkdir -p /sway
WORKDIR /build
RUN curl -L https://github.com/swaywm/sway/releases/download/${SWAY_VERSION}/sway-${SWAY_VERSION}.tar.gz -o sway.tar.gz && tar -xzf sway.tar.gz && rm sway.tar.gz && mv sway-* sway-src
WORKDIR /build/sway-src
RUN pip3 install meson ninja
# swaybar/swaynag image loading uses cairo PNG (libpng now available).
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dman-pages=disabled -Dgdk-pixbuf=disabled -Dtray=disabled -Dswaybar=true -Dswaynag=true -Dwerror=false
RUN DESTDIR=/sway ninja -C buildDir install


# tllist — foot dependency (header-only-ish)
FROM toolchain AS tllist
ARG TLLIST_VERSION=1.1.0
RUN mkdir -p /tllist
WORKDIR /build
RUN curl -L https://codeberg.org/dnkl/tllist/archive/${TLLIST_VERSION}.tar.gz -o tllist.tar.gz && tar -xzf tllist.tar.gz && rm tllist.tar.gz && mv tllist* tllist-src
WORKDIR /build/tllist-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS}
RUN DESTDIR=/tllist ninja -C buildDir install


# fcft — font loading/glyph rasterization for foot (shaping disabled)
FROM toolchain AS fcft
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY --from=pixman /pixman /
COPY --from=harfbuzz /harfbuzz /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
COPY --from=tllist /tllist /
ARG FCFT_VERSION=3.3.1
RUN mkdir -p /fcft
WORKDIR /build
RUN curl -L https://codeberg.org/dnkl/fcft/archive/${FCFT_VERSION}.tar.gz -o fcft.tar.gz && tar -xzf fcft.tar.gz && rm fcft.tar.gz && mv fcft* fcft-src
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
COPY --from=fontconfig /fontconfig /
COPY --from=fcft /fcft /
COPY --from=tllist /tllist /
COPY --from=harfbuzz /harfbuzz /
COPY --from=glib2 /glib2 /
COPY --from=pcre2 /pcre2 /
ARG FOOT_VERSION=1.20.2
RUN mkdir -p /foot
WORKDIR /build
RUN curl -L https://codeberg.org/dnkl/foot/archive/${FOOT_VERSION}.tar.gz -o foot.tar.gz && tar -xzf foot.tar.gz && rm foot.tar.gz && mv foot* foot-src
WORKDIR /build/foot-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Ddocs=disabled -Dgrapheme-clustering=disabled -Dthemes=false -Dterminfo=disabled -Dwerror=false
RUN DESTDIR=/foot ninja -C buildDir install


# grim — Wayland screenshot tool (used by the test harness and desktop)
FROM toolchain AS grim
COPY --from=wayland /wayland /
COPY --from=pixman /pixman /
COPY --from=libpng /libpng /
ARG GRIM_VERSION=1.4.1
RUN mkdir -p /grim
WORKDIR /build
RUN curl -L https://gitlab.freedesktop.org/emersion/grim/-/archive/v${GRIM_VERSION}/grim-v${GRIM_VERSION}.tar.gz -o grim.tar.gz && tar -xzf grim.tar.gz && rm grim.tar.gz && mv grim-* grim-src
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
RUN curl -L https://github.com/thom311/libnl/releases/download/libnl3_11_0/libnl-${LIBNL_VERSION}.tar.gz -o libnl.tar.gz && tar -xzf libnl.tar.gz && rm libnl.tar.gz && mv libnl-* libnl-src
WORKDIR /build/libnl-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking
RUN make -j$(nproc) && make install DESTDIR=/libnl


# wpa_supplicant — wifi backend for NetworkManager (D-Bus control interface)
FROM toolchain AS wpa-supplicant
COPY --from=libnl /libnl /
ARG WPA_VERSION=2.11
RUN mkdir -p /wpa
WORKDIR /build
RUN curl -L https://w1.fi/releases/wpa_supplicant-${WPA_VERSION}.tar.gz -o wpa.tar.gz && tar -xzf wpa.tar.gz && rm wpa.tar.gz && mv wpa_supplicant-* wpa-src
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
RUN curl -L https://w1.fi/releases/hostapd-${HOSTAPD_VERSION}.tar.gz -o hostapd.tar.gz && tar -xzf hostapd.tar.gz && rm hostapd.tar.gz && mv hostapd-* hostapd-src
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
ARG LIBNDP_VERSION=1.9
RUN mkdir -p /libndp
WORKDIR /build
RUN curl -L https://github.com/jpirko/libndp/archive/refs/tags/v${LIBNDP_VERSION}.tar.gz -o libndp.tar.gz && tar -xzf libndp.tar.gz && rm libndp.tar.gz && mv libndp-* libndp-src
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
RUN curl -L https://www.kernel.org/pub/software/network/wireless-regdb/wireless-regdb-${WIRELESS_REGDB_VERSION}.tar.xz -o regdb.tar.xz && tar -xf regdb.tar.xz && rm regdb.tar.xz && mv wireless-regdb-* regdb-src
RUN cp regdb-src/regulatory.db regdb-src/regulatory.db.p7s /wireless-regdb/usr/local/lib/firmware/


# ncurses — provides the termcap library (libtinfo) that the toolchain's
# libreadline needs; without it nmcli fails to link (undefined tgetent, tputs…).
FROM toolchain AS ncurses
ARG NCURSES_VERSION=6.5
RUN mkdir -p /ncurses
WORKDIR /build
RUN curl -L https://ftp.gnu.org/gnu/ncurses/ncurses-${NCURSES_VERSION}.tar.gz -o ncurses.tar.gz && tar -xzf ncurses.tar.gz && rm ncurses.tar.gz && mv ncurses-* ncurses-src
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
ARG LIBXSLT_VERSION=1.1.42
RUN mkdir -p /libxslt
WORKDIR /build
RUN curl -L https://download.gnome.org/sources/libxslt/1.1/libxslt-${LIBXSLT_VERSION}.tar.xz -o libxslt.tar.xz && tar -xf libxslt.tar.xz && rm libxslt.tar.xz && mv libxslt-* libxslt-src
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
ARG NM_VERSION=1.50.0
RUN mkdir -p /networkmanager
WORKDIR /build
RUN curl -L https://download.gnome.org/sources/NetworkManager/1.50/NetworkManager-${NM_VERSION}.tar.xz -o nm.tar.xz && tar -xf nm.tar.xz && rm nm.tar.xz && mv NetworkManager-* nm-src
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
ARG ALSA_VERSION=1.2.12
RUN mkdir -p /alsa-lib
WORKDIR /build
RUN curl -L https://www.alsa-project.org/files/pub/lib/alsa-lib-${ALSA_VERSION}.tar.bz2 -o alsa-lib.tar.bz2 && tar -xjf alsa-lib.tar.bz2 && rm alsa-lib.tar.bz2 && mv alsa-lib-* alsa-lib-src
WORKDIR /build/alsa-lib-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --disable-dependency-tracking --disable-python
RUN make -j$(nproc) && make install DESTDIR=/alsa-lib


# ALSA UCM configuration (data files describing sound cards)
FROM toolchain AS alsa-ucm-conf
ARG ALSA_UCM_VERSION=1.2.12
RUN mkdir -p /alsa-ucm-conf
WORKDIR /build
RUN curl -L https://www.alsa-project.org/files/pub/lib/alsa-ucm-conf-${ALSA_UCM_VERSION}.tar.bz2 -o ucm.tar.bz2 && tar -xjf ucm.tar.bz2 && rm ucm.tar.bz2 && mv alsa-ucm-conf-* ucm-src
RUN mkdir -p /alsa-ucm-conf/usr/share/alsa && cp -a ucm-src/ucm2 /alsa-ucm-conf/usr/share/alsa/


# sbc — Bluetooth A2DP audio codec (used by PipeWire's bluez5 plugin)
FROM toolchain AS sbc
ARG SBC_VERSION=2.0
RUN mkdir -p /sbc
WORKDIR /build
RUN curl -L https://www.kernel.org/pub/linux/bluetooth/sbc-${SBC_VERSION}.tar.xz -o sbc.tar.xz && tar -xf sbc.tar.xz && rm sbc.tar.xz && mv sbc-* sbc-src
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
ARG BLUEZ_VERSION=5.79
RUN mkdir -p /bluez
WORKDIR /build
RUN curl -L https://www.kernel.org/pub/linux/bluetooth/bluez-${BLUEZ_VERSION}.tar.xz -o bluez.tar.xz && tar -xf bluez.tar.xz && rm bluez.tar.xz && mv bluez-* bluez-src
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
ARG PIPEWIRE_VERSION=1.2.7
RUN mkdir -p /pipewire
WORKDIR /build
RUN curl -L https://gitlab.freedesktop.org/pipewire/pipewire/-/archive/${PIPEWIRE_VERSION}/pipewire-${PIPEWIRE_VERSION}.tar.gz -o pipewire.tar.gz && tar -xzf pipewire.tar.gz && rm pipewire.tar.gz && mv pipewire-* pipewire-src
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
ARG WIREPLUMBER_VERSION=0.5.7
RUN mkdir -p /wireplumber
WORKDIR /build
RUN curl -L https://gitlab.freedesktop.org/pipewire/wireplumber/-/archive/${WIREPLUMBER_VERSION}/wireplumber-${WIREPLUMBER_VERSION}.tar.gz -o wireplumber.tar.gz && tar -xzf wireplumber.tar.gz && rm wireplumber.tar.gz && mv wireplumber-* wireplumber-src
WORKDIR /build/wireplumber-src
RUN pip3 install meson ninja
# Pre-seed meson's wrap cache with lua (the wrap fetches it from www.lua.org,
# which is frequently unreachable). MacPorts mirrors the identical tarball
# (sha 164c7849…), and meson verifies the hash so the patch still applies.
RUN mkdir -p subprojects/packagecache && \
    curl -fL https://distfiles.macports.org/lua/lua-5.4.4.tar.gz -o subprojects/packagecache/lua-5.4.4.tar.gz
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
ARG NERDFONT_VERSION=3.1.1
RUN mkdir -p /nerdfont/usr/share/fonts/nerd-fonts
WORKDIR /tmp
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://github.com/ryanoasis/nerd-fonts/releases/download/v${NERDFONT_VERSION}/NerdFontsSymbolsOnly.tar.xz -o n.tar.xz \
    && tar -xf n.tar.xz -C /nerdfont/usr/share/fonts/nerd-fonts SymbolsNerdFont-Regular.ttf SymbolsNerdFontMono-Regular.ttf \
    && rm n.tar.xz

# --- docker (static bundle) — rootful OCI containers ------------------------
# Docker's daemon + CLI + containerd + runc ship as fully-static Go binaries in
# the official static bundle (a musl-friendly, no-libc-dependency pattern).
# dockerd spawns its own containerd child when --containerd is not given, so a
# single docker.service is enough. The base image already provides iptables-nft
# and the kernel ships nf_conntrack/bridge/br_netfilter, so docker's bridge+NAT
# networking works without vendoring a separate network stack.
FROM toolchain AS docker
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
ARG JSONGLIB_VERSION=1.10.8
RUN mkdir -p /json-glib
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://download.gnome.org/sources/json-glib/1.10/json-glib-${JSONGLIB_VERSION}.tar.xz -o j.tar.xz && tar -xf j.tar.xz && rm j.tar.xz && mv json-glib-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dintrospection=disabled -Ddocumentation=disabled -Dgtk_doc=disabled -Dman=false -Dtests=false
RUN DESTDIR=/json-glib ninja -C buildDir install

FROM toolchain AS bubblewrap
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
ARG SWAYBG_VERSION=1.2.1
RUN mkdir -p /swaybg
WORKDIR /build
RUN curl -L https://github.com/swaywm/swaybg/releases/download/v${SWAYBG_VERSION}/swaybg-${SWAYBG_VERSION}.tar.gz -o swaybg.tar.gz && tar -xzf swaybg.tar.gz && rm swaybg.tar.gz && mv swaybg-* swaybg-src
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
ARG MAKO_VERSION=1.9.0
RUN mkdir -p /mako
WORKDIR /build
RUN curl -L https://github.com/emersion/mako/releases/download/v${MAKO_VERSION}/mako-${MAKO_VERSION}.tar.gz -o mako.tar.gz && tar -xzf mako.tar.gz && rm mako.tar.gz && mv mako-* mako-src
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
ARG FUZZEL_VERSION=1.11.0
RUN mkdir -p /fuzzel
WORKDIR /build
RUN curl -L https://codeberg.org/dnkl/fuzzel/archive/${FUZZEL_VERSION}.tar.gz -o fuzzel.tar.gz && tar -xzf fuzzel.tar.gz && rm fuzzel.tar.gz && mv fuzzel* fuzzel-src
WORKDIR /build/fuzzel-src
RUN pip3 install meson ninja
# Drop the man-page subdir (needs scdoc, which we don't build).
RUN sed -i "/subdir('doc')/d" meson.build
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Denable-cairo=disabled -Dpng-backend=libpng -Dsvg-backend=none -Dwerror=false
RUN DESTDIR=/fuzzel ninja -C buildDir install


# wl-clipboard — clipboard utilities
FROM toolchain AS wl-clipboard
COPY --from=wayland /wayland /
ARG WLCLIP_VERSION=2.2.1
RUN mkdir -p /wl-clipboard
WORKDIR /build
RUN curl -L https://github.com/bugaevc/wl-clipboard/archive/refs/tags/v${WLCLIP_VERSION}.tar.gz -o wlclip.tar.gz && tar -xzf wlclip.tar.gz && rm wlclip.tar.gz && mv wl-clipboard-* wlclip-src
WORKDIR /build/wlclip-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS}
RUN DESTDIR=/wl-clipboard ninja -C buildDir install


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
ARG SLURP_VERSION=1.5.0
RUN mkdir -p /slurp
WORKDIR /build
RUN curl -L https://github.com/emersion/slurp/releases/download/v${SLURP_VERSION}/slurp-${SLURP_VERSION}.tar.gz -o slurp.tar.gz && tar -xzf slurp.tar.gz && rm slurp.tar.gz && mv slurp-* slurp-src
WORKDIR /build/slurp-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dman-pages=disabled
RUN DESTDIR=/slurp ninja -C buildDir install


# swayidle — idle management
FROM toolchain AS swayidle
COPY --from=libucontext /libucontext /
COPY --from=wayland /wayland /
ARG SWAYIDLE_VERSION=1.8.0
RUN mkdir -p /swayidle
WORKDIR /build
RUN curl -L https://github.com/swaywm/swayidle/releases/download/${SWAYIDLE_VERSION}/swayidle-${SWAYIDLE_VERSION}.tar.gz -o swayidle.tar.gz && tar -xzf swayidle.tar.gz && rm swayidle.tar.gz && mv swayidle-* swayidle-src
WORKDIR /build/swayidle-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dman-pages=disabled -Dlogind=enabled
RUN DESTDIR=/swayidle ninja -C buildDir install


# X protocol libs — ly links libxcb (X11 session support) even in a Wayland setup
FROM toolchain AS xorgproto
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
ARG LIBXAU_VERSION=1.0.12
RUN mkdir -p /libxau
WORKDIR /build
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors https://gitlab.freedesktop.org/xorg/lib/libxau/-/archive/libXau-${LIBXAU_VERSION}/libxau-libXau-${LIBXAU_VERSION}.tar.gz -o libxau.tar.gz && tar -xf libxau.tar.gz && rm libxau.tar.gz && mv libxau-libXau-* libxau-src
WORKDIR /build/libxau-src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS}
RUN DESTDIR=/libxau ninja -C buildDir install

FROM toolchain AS xcbproto
ARG XCB_VERSION=1.17.0
RUN mkdir -p /xcbproto
WORKDIR /build
RUN curl -L https://xcb.freedesktop.org/dist/xcb-proto-${XCB_VERSION}.tar.gz -o xcb-proto.tar.gz && tar -xzf xcb-proto.tar.gz && rm xcb-proto.tar.gz && mv xcb-proto-* xcb-src
WORKDIR /build/xcb-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --build=${BUILD} --disable-dependency-tracking
RUN make -j$(nproc) && make install DESTDIR=/xcbproto

FROM toolchain AS libxcb
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
ARG LIBXCB_VERSION=1.17.0
RUN mkdir -p /libxcb
WORKDIR /build
RUN curl -L https://xcb.freedesktop.org/dist/libxcb-${LIBXCB_VERSION}.tar.gz -o libxcb.tar.gz && tar -xzf libxcb.tar.gz && rm libxcb.tar.gz && mv libxcb-* libxcb-src
WORKDIR /build/libxcb-src
RUN ./configure ${COMMON_CONFIGURE_ARGS} --build=${BUILD} --disable-dependency-tracking
RUN make -j$(nproc) && make install DESTDIR=/libxcb


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
COPY --from=fontconfig /fontconfig /
RUN mkdir -p /x11-libs
WORKDIR /build

ARG XTRANS_VERSION=1.6.0
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/xtrans-${XTRANS_VERSION}.tar.xz -o xtrans.tar.xz && \
    tar -xf xtrans.tar.xz && cd xtrans-${XTRANS_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-docs && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBX11_VERSION=1.8.12
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libX11-${LIBX11_VERSION}.tar.xz -o libX11.tar.xz && \
    tar -xf libX11.tar.xz && cd libX11-${LIBX11_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-specs --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBXEXT_VERSION=1.3.6
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libXext-${LIBXEXT_VERSION}.tar.xz -o libXext.tar.xz && \
    tar -xf libXext.tar.xz && cd libXext-${LIBXEXT_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-specs --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBXRENDER_VERSION=0.9.12
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libXrender-${LIBXRENDER_VERSION}.tar.xz -o libXrender.tar.xz && \
    tar -xf libXrender.tar.xz && cd libXrender-${LIBXRENDER_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBXFIXES_VERSION=6.0.1
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libXfixes-${LIBXFIXES_VERSION}.tar.xz -o libXfixes.tar.xz && \
    tar -xf libXfixes.tar.xz && cd libXfixes-${LIBXFIXES_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBXDAMAGE_VERSION=1.1.6
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libXdamage-${LIBXDAMAGE_VERSION}.tar.xz -o libXdamage.tar.xz && \
    tar -xf libXdamage.tar.xz && cd libXdamage-${LIBXDAMAGE_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBXCOMPOSITE_VERSION=0.4.6
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libXcomposite-${LIBXCOMPOSITE_VERSION}.tar.xz -o libXcomposite.tar.xz && \
    tar -xf libXcomposite.tar.xz && cd libXcomposite-${LIBXCOMPOSITE_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBXRANDR_VERSION=1.5.4
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libXrandr-${LIBXRANDR_VERSION}.tar.xz -o libXrandr.tar.xz && \
    tar -xf libXrandr.tar.xz && cd libXrandr-${LIBXRANDR_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBXI_VERSION=1.8.2
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libXi-${LIBXI_VERSION}.tar.xz -o libXi.tar.xz && \
    tar -xf libXi.tar.xz && cd libXi-${LIBXI_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBXINERAMA_VERSION=1.1.5
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libXinerama-${LIBXINERAMA_VERSION}.tar.xz -o libXinerama.tar.xz && \
    tar -xf libXinerama.tar.xz && cd libXinerama-${LIBXINERAMA_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBXCURSOR_VERSION=1.2.3
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libXcursor-${LIBXCURSOR_VERSION}.tar.xz -o libXcursor.tar.xz && \
    tar -xf libXcursor.tar.xz && cd libXcursor-${LIBXCURSOR_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBXFT_VERSION=2.3.9
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libXft-${LIBXFT_VERSION}.tar.xz -o libXft.tar.xz && \
    tar -xf libXft.tar.xz && cd libXft-${LIBXFT_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBXSS_VERSION=1.2.4
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libXScrnSaver-${LIBXSS_VERSION}.tar.xz -o libXScrnSaver.tar.xz && \
    tar -xf libXScrnSaver.tar.xz && cd libXScrnSaver-${LIBXSS_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# XLibre's mandatory font/XKB support libraries and the runtime xkb compiler.
ARG LIBFONTENC_VERSION=1.1.8
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libfontenc-${LIBFONTENC_VERSION}.tar.xz -o libfontenc.tar.xz && \
    tar -xf libfontenc.tar.xz && cd libfontenc-${LIBFONTENC_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBXFONT2_VERSION=2.0.7
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libXfont2-${LIBXFONT2_VERSION}.tar.xz -o libXfont2.tar.xz && \
    tar -xf libXfont2.tar.xz && cd libXfont2-${LIBXFONT2_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-devel-docs --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBXKBFILE_VERSION=1.1.3
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libxkbfile-${LIBXKBFILE_VERSION}.tar.xz -o libxkbfile.tar.xz && \
    tar -xf libxkbfile.tar.xz && cd libxkbfile-${LIBXKBFILE_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG XKBCOMP_VERSION=1.4.7
RUN curl -fL --retry 5 https://www.x.org/releases/individual/app/xkbcomp-${XKBCOMP_VERSION}.tar.xz -o xkbcomp.tar.xz && \
    tar -xf xkbcomp.tar.xz && cd xkbcomp-${XKBCOMP_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# DRM/modesetting support used by the XLibre server.
RUN pip3 install meson ninja
ARG LIBXCVT_VERSION=0.1.3
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libxcvt-${LIBXCVT_VERSION}.tar.xz -o libxcvt.tar.xz && \
    tar -xf libxcvt.tar.xz && cd libxcvt-${LIBXCVT_VERSION} && \
    meson setup buildDir ${COMMON_MESON_FLAGS} && \
    ninja -C buildDir install && DESTDIR=/x11-libs ninja -C buildDir install

ARG LIBPCIACCESS_VERSION=0.18.1
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libpciaccess-${LIBPCIACCESS_VERSION}.tar.xz -o libpciaccess.tar.xz && \
    tar -xf libpciaccess.tar.xz && cd libpciaccess-${LIBPCIACCESS_VERSION} && \
    meson setup buildDir ${COMMON_MESON_FLAGS} -Dzlib=disabled && \
    ninja -C buildDir install && DESTDIR=/x11-libs ninja -C buildDir install

ARG LIBXSHMFENCE_VERSION=1.3.3
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libxshmfence-${LIBXSHMFENCE_VERSION}.tar.xz -o libxshmfence.tar.xz && \
    tar -xf libxshmfence.tar.xz && cd libxshmfence-${LIBXSHMFENCE_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# i3's XCB utility dependencies.
ARG XCB_UTIL_VERSION=0.4.1
RUN curl -fL --retry 5 https://xcb.freedesktop.org/dist/xcb-util-${XCB_UTIL_VERSION}.tar.xz -o xcb-util.tar.xz && \
    tar -xf xcb-util.tar.xz && cd xcb-util-${XCB_UTIL_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG XCB_UTIL_IMAGE_VERSION=0.4.1
RUN curl -fL --retry 5 https://xcb.freedesktop.org/dist/xcb-util-image-${XCB_UTIL_IMAGE_VERSION}.tar.xz -o xcb-image.tar.xz && \
    tar -xf xcb-image.tar.xz && cd xcb-util-image-${XCB_UTIL_IMAGE_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG XCB_UTIL_RENDERUTIL_VERSION=0.3.10
RUN curl -fL --retry 5 https://xcb.freedesktop.org/dist/xcb-util-renderutil-${XCB_UTIL_RENDERUTIL_VERSION}.tar.xz -o xcb-renderutil.tar.xz && \
    tar -xf xcb-renderutil.tar.xz && cd xcb-util-renderutil-${XCB_UTIL_RENDERUTIL_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG XCB_UTIL_CURSOR_VERSION=0.1.5
RUN curl -fL --retry 5 https://xcb.freedesktop.org/dist/xcb-util-cursor-${XCB_UTIL_CURSOR_VERSION}.tar.xz -o xcb-cursor.tar.xz && \
    tar -xf xcb-cursor.tar.xz && cd xcb-util-cursor-${XCB_UTIL_CURSOR_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG XCB_UTIL_KEYSYMS_VERSION=0.4.1
RUN curl -fL --retry 5 https://xcb.freedesktop.org/dist/xcb-util-keysyms-${XCB_UTIL_KEYSYMS_VERSION}.tar.xz -o xcb-keysyms.tar.xz && \
    tar -xf xcb-keysyms.tar.xz && cd xcb-util-keysyms-${XCB_UTIL_KEYSYMS_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG XCB_UTIL_WM_VERSION=0.4.2
RUN curl -fL --retry 5 https://xcb.freedesktop.org/dist/xcb-util-wm-${XCB_UTIL_WM_VERSION}.tar.xz -o xcb-wm.tar.xz && \
    tar -xf xcb-wm.tar.xz && cd xcb-util-wm-${XCB_UTIL_WM_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

# xcb-util-xrm uses the BSD queue macros. musl does not ship sys/queue.h, so
# install the header-only compatibility pair from libbsd for this build graph.
ARG LIBBSD_HEADERS_VERSION=0.12.2
RUN curl -fL --retry 5 https://libbsd.freedesktop.org/releases/libbsd-${LIBBSD_HEADERS_VERSION}.tar.xz -o libbsd.tar.xz && \
    tar -xf libbsd.tar.xz && \
    mkdir -p /usr/include/sys /usr/include/bsd/sys /x11-libs/usr/include/sys /x11-libs/usr/include/bsd/sys && \
    cp libbsd-${LIBBSD_HEADERS_VERSION}/include/bsd/sys/queue.h /usr/include/sys/queue.h && \
    cp libbsd-${LIBBSD_HEADERS_VERSION}/include/bsd/sys/cdefs.h /usr/include/bsd/sys/cdefs.h && \
    cp /usr/include/sys/queue.h /x11-libs/usr/include/sys/queue.h && \
    cp /usr/include/bsd/sys/cdefs.h /x11-libs/usr/include/bsd/sys/cdefs.h

ARG XCB_UTIL_XRM_VERSION=1.3
RUN curl -fL --retry 5 https://github.com/Airblader/xcb-util-xrm/releases/download/v${XCB_UTIL_XRM_VERSION}/xcb-util-xrm-${XCB_UTIL_XRM_VERSION}.tar.gz -o xcb-xrm.tar.gz && \
    tar -xf xcb-xrm.tar.gz && cd xcb-util-xrm-${XCB_UTIL_XRM_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG STARTUP_NOTIFICATION_VERSION=0.12
RUN curl -fL --retry 5 https://www.freedesktop.org/software/startup-notification/releases/startup-notification-${STARTUP_NOTIFICATION_VERSION}.tar.gz -o startup-notification.tar.gz && \
    tar -xf startup-notification.tar.gz && cd startup-notification-${STARTUP_NOTIFICATION_VERSION} && \
    ./configure --quiet --prefix=/usr --enable-shared --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG LIBEV_VERSION=4.33
RUN curl -fL --retry 5 https://dist.schmorp.de/libev/libev-${LIBEV_VERSION}.tar.gz -o libev.tar.gz && \
    tar -xf libev.tar.gz && cd libev-${LIBEV_VERSION} && \
    ./configure ${COMMON_CONFIGURE_ARGS} --disable-static && \
    make -j"$(nproc)" && make install && make install DESTDIR=/x11-libs

ARG YAJL_VERSION=2.1.0
RUN curl -fL --retry 5 https://github.com/lloyd/yajl/archive/refs/tags/${YAJL_VERSION}.tar.gz -o yajl.tar.gz && \
    tar -xf yajl.tar.gz && cd yajl-${YAJL_VERSION} && \
    sed -i '/ADD_SUBDIRECTORY(test)/,$d' CMakeLists.txt && \
    cmake -G Ninja -B buildDir -DCMAKE_POLICY_VERSION_MINIMUM=3.5 -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DBUILD_SHARED_LIBS=ON && \
    ninja -C buildDir && ninja -C buildDir install && DESTDIR=/x11-libs ninja -C buildDir install

ARG LIBXXF86VM_VERSION=1.1.7
RUN curl -fL --retry 5 https://www.x.org/releases/individual/lib/libXxf86vm-${LIBXXF86VM_VERSION}.tar.xz -o libXxf86vm.tar.xz && \
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
ARG LIBXKBCOMMON_VERSION=1.13.0
RUN mkdir -p /libxkb-x11
WORKDIR /build
RUN curl -fL https://github.com/xkbcommon/libxkbcommon/archive/refs/tags/xkbcommon-${LIBXKBCOMMON_VERSION}.tar.gz -o libxkbcommon.tar.gz && \
    tar -xf libxkbcommon.tar.gz && mv libxkbcommon-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Denable-x11=true -Denable-wayland=false -Denable-xkbregistry=true -Denable-bash-completion=false
RUN DESTDIR=/libxkb-x11 ninja -C buildDir install


# Mesa rebuilt for the X11 platform and DRI GLX. GPU retains the same vm/full
# semantics as the Sway Mesa stage.
FROM toolchain AS mesa-x11
COPY --from=libdrm /libdrm /
COPY --from=llvm-stack /llvm-out /llvm-out
RUN rsync -aHAX --keep-dirlinks /llvm-out/. / && rm -rf /llvm-out
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
ARG MESA_VERSION=25.3.0
ARG GPU=vm
RUN mkdir -p /mesa-x11
WORKDIR /build
RUN curl -fL https://archive.mesa3d.org/mesa-${MESA_VERSION}.tar.xz -o mesa.tar.xz && \
    tar -xf mesa.tar.xz && mv mesa-* src
WORKDIR /build/src
RUN pip3 install meson ninja setuptools mako pyyaml
RUN if [ "$GPU" = "full" ]; then \
      DRV="iris,radeonsi,virgl,softpipe,svga"; LLVMOPT=true; RTTI=false; \
    else \
      DRV="virgl,softpipe,svga"; LLVMOPT=false; RTTI=true; \
    fi; \
    meson setup buildDir ${COMMON_MESON_FLAGS} -Dplatforms=x11 \
      -Dgallium-drivers="$DRV" -Dglx=dri -Dopengl=true -Dgles1=enabled -Dgles2=enabled \
      -Degl=enabled -Dvulkan-drivers= -Dllvm="$LLVMOPT" -Dcpp_rtti="$RTTI" -Dbuild-tests=false
RUN DESTDIR=/mesa-x11 ninja -C buildDir install
RUN if [ "$GPU" = "full" ]; then \
      mkdir -p /mesa-x11/usr/lib && cp -a /usr/lib/libLLVM.so* /mesa-x11/usr/lib/ && cp -a /usr/lib/libelf.so* /mesa-x11/usr/lib/; \
    fi


FROM toolchain AS libepoxy-x11
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=mesa-x11 /mesa-x11 /
ARG LIBEPOXY_VERSION=1.5.10
RUN mkdir -p /libepoxy-x11
WORKDIR /build
RUN curl -fL https://github.com/anholt/libepoxy/archive/refs/tags/${LIBEPOXY_VERSION}.tar.gz -o epoxy.tar.gz && \
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
ARG CAIRO_VERSION=1.18.4
RUN mkdir -p /cairo-x11
WORKDIR /build
RUN curl -fL https://cairographics.org/releases/cairo-${CAIRO_VERSION}.tar.xz -o cairo.tar.xz && \
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
ARG PANGO_VERSION=1.54.0
RUN mkdir -p /pango-x11
WORKDIR /build
RUN PANGO_MAJOR="${PANGO_VERSION%.*}" && curl -fL https://download.gnome.org/sources/pango/${PANGO_MAJOR}/pango-${PANGO_VERSION}.tar.xz -o pango.tar.xz && \
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
COPY --from=libucontext /libucontext /
COPY --from=x11-libs /x11-libs /
COPY --from=xkeyboard-config /xkeyboard-config /
COPY --from=pixman /pixman /
COPY --from=libdrm /libdrm /
COPY --from=mesa-x11 /mesa-x11 /
COPY --from=libepoxy-x11 /libepoxy-x11 /
COPY --from=libseat /libseat /
ARG XLIBRE_VERSION=25.2.0
RUN mkdir -p /xlibre
WORKDIR /build
RUN curl -fL --retry 5 https://github.com/X11Libre/xserver/archive/refs/tags/xlibre-xserver-${XLIBRE_VERSION}.tar.gz -o xlibre.tar.gz && \
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
COPY --from=x11-libs /x11-libs /
COPY --from=xlibre /xlibre /
ARG XLIBRE_INPUT_LIBINPUT_VERSION=25.0.1
RUN mkdir -p /xlibre-input-libinput
WORKDIR /build
RUN curl -fL --retry 5 https://github.com/X11Libre/xf86-input-libinput/archive/refs/tags/xlibre-xf86-input-libinput-${XLIBRE_INPUT_LIBINPUT_VERSION}.tar.gz -o driver.tar.gz && \
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
ARG I3_VERSION=4.25.1
RUN mkdir -p /i3
WORKDIR /build
RUN curl -fL --retry 5 https://i3wm.org/downloads/i3-${I3_VERSION}.tar.xz -o i3.tar.xz && \
    tar -xf i3.tar.xz && mv i3-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Ddocs=false -Dmans=false
RUN DESTDIR=/i3 ninja -C buildDir install


# Native X11 desktop tools: xauth for Ly's X session cookie, st terminal, dmenu
# launcher, xsel clipboard, and a tiny root-window color helper. These replace
# foot/fuzzel/wl-clipboard/swaybg.
FROM toolchain AS x11-desktop-tools
COPY --from=xorgproto /xorgproto /
COPY --from=xcbproto /xcbproto /
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=x11-libs /x11-libs /
COPY --from=freetype /freetype /
COPY --from=fontconfig /fontconfig /
COPY tools/hadron-xroot.c /build/hadron-xroot.c
RUN mkdir -p /x11-desktop-tools/usr/bin
WORKDIR /build
ARG XAUTH_VERSION=1.1.5
# xauth only uses XmuGetHostname from libXmuu; that function is a thin
# gethostname(2) wrapper. Use libc directly instead of pulling libXt and the
# rest of libXmu's legacy widget dependency chain into this small image.
RUN curl -fL --retry 5 https://www.x.org/releases/individual/app/xauth-${XAUTH_VERSION}.tar.xz -o xauth.tar.xz && \
    tar -xf xauth.tar.xz && cd xauth-${XAUTH_VERSION} && \
    sed -i -e 's|#include <X11/Xmu/SysUtil.h>|#include <unistd.h>|' \
           -e 's/XmuGetHostname/gethostname/' \
           -e '/(void) gethostname (buf, maxlen);/a\    buf[maxlen - 1] = '\''\\0'\'';' parsedpy.c && \
    XAUTH_CFLAGS="-I/usr/include $(pkg-config --cflags x11 xau xext)" \
    XAUTH_LIBS="$(pkg-config --libs x11 xau xext)" \
    ./configure ${COMMON_CONFIGURE_ARGS} && \
    make -j"$(nproc)" && make install DESTDIR=/x11-desktop-tools

ARG ST_VERSION=0.9.3
RUN curl -fL --retry 5 https://dl.suckless.org/st/st-${ST_VERSION}.tar.gz -o st.tar.gz && \
    tar -xf st.tar.gz && cd st-${ST_VERSION} && \
    sed -i -e 's|^PREFIX = .*|PREFIX = /usr|' -e 's|^X11INC = .*|X11INC = /usr/include|' -e 's|^X11LIB = .*|X11LIB = /usr/lib|' config.mk && \
    sed -i -e 's|Liberation Mono:pixelsize=12|DejaVu Sans Mono:pixelsize=15|' \
           -e 's|static int borderpx = 2;|static int borderpx = 10;|' \
           -e 's|char \*termname = "st-256color";|char *termname = "xterm-256color";|' \
           -e 's|"gray90", /\* default foreground colour \*/|"#c0caf5", /* default foreground colour */|' \
           -e 's|"black", /\* default background colour \*/|"#1a1b26", /* default background colour */|' config.def.h && \
    make -j"$(nproc)" CC=cc && install -m0755 st /x11-desktop-tools/usr/bin/st

ARG DMENU_VERSION=5.3
RUN curl -fL --retry 5 https://dl.suckless.org/tools/dmenu-${DMENU_VERSION}.tar.gz -o dmenu.tar.gz && \
    tar -xf dmenu.tar.gz && cd dmenu-${DMENU_VERSION} && \
    sed -i -e 's|^PREFIX = .*|PREFIX = /usr|' -e 's|^X11INC = .*|X11INC = /usr/include|' -e 's|^X11LIB = .*|X11LIB = /usr/lib|' config.mk && \
    make -j"$(nproc)" CC=cc && make install CC=cc DESTDIR=/x11-desktop-tools

ARG XSEL_VERSION=1.2.1
RUN curl -fL --retry 5 https://github.com/kfish/xsel/archive/refs/tags/${XSEL_VERSION}.tar.gz -o xsel.tar.gz && \
    tar -xf xsel.tar.gz && cd xsel-${XSEL_VERSION} && \
    autoreconf -fi && ./configure ${COMMON_CONFIGURE_ARGS} && \
    make -j"$(nproc)" && make install DESTDIR=/x11-desktop-tools

RUN cc -O2 -Wall -Wextra /build/hadron-xroot.c $(pkg-config --cflags --libs x11) -o /x11-desktop-tools/usr/bin/hadron-xroot


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
ARG DUNST_VERSION=1.13.0
RUN mkdir -p /dunst
WORKDIR /build
RUN curl -fL --retry 5 https://github.com/dunst-project/dunst/archive/refs/tags/v${DUNST_VERSION}.tar.gz -o dunst.tar.gz && \
    tar -xf dunst.tar.gz && mv dunst-* src
WORKDIR /build/src
RUN pip3 install meson ninja
RUN meson setup buildDir ${COMMON_MESON_FLAGS} -Dx11=enabled -Dwayland=disabled -Ddunstify=disabled -Dsystemd=enabled -Ddocs=disabled
RUN DESTDIR=/dunst ninja -C buildDir install


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
# ly 1.4.x has the dur_file animation (durdraw .dur playback) used for the login
# background AND dur_offset_alignment for centering a movie wider than the
# console (1.3.x's offset is unsigned, so it can't shift left to centre). Needs
# Zig 0.16.x.
ARG LY_VERSION=1.4.1
RUN mkdir -p /ly
WORKDIR /build
RUN curl -L https://ziglang.org/download/${ZIG_VERSION}/zig-x86_64-linux-${ZIG_VERSION}.tar.xz -o zig.tar.xz && tar -xf zig.tar.xz && rm zig.tar.xz && mv zig-x86_64-linux-* /opt/zig
ENV PATH=/opt/zig:$PATH
RUN curl -L https://github.com/fairyglade/ly/archive/refs/tags/v${LY_VERSION}.tar.gz -o ly.tar.gz && tar -xzf ly.tar.gz && rm ly.tar.gz && mv ly-* ly-src
WORKDIR /build/ly-src
RUN zig build
RUN zig build installexe -Dinit_system=systemd -Ddest_directory=/ly
# The "Black Hole" durdraw animation shown in ly's own README (256-colour .dur),
# used as the login background via `animation = dur_file`.
RUN curl -fL --retry 5 --retry-delay 3 --retry-all-errors "https://codeberg.org/attachments/f336d6ac-8331-4323-91fc-0e4619803401" -o /ly/etc/ly/blackhole-smooth.dur


# ===========================================================================
# M6: real-hardware firmware (optional)
#
# Real wifi / bluetooth / GPU hardware needs firmware blobs in /lib/firmware.
# This is OFF by default (the QEMU test path and slim images need no blobs);
# build with --build-arg FIRMWARE=true to bundle a curated linux-firmware
# subset for common laptop hardware. Validated on real hardware, not in CI.
# ===========================================================================
FROM alpine:3 AS firmware
ARG FIRMWARE=false
RUN mkdir -p /firmware/usr/local/lib/firmware
RUN if [ "$FIRMWARE" = "true" ]; then \
      apk add --no-cache git && \
      git clone --depth 1 https://git.kernel.org/pub/scm/linux/kernel/git/firmware/linux-firmware.git /lf && \
      cd /lf && for p in \
        iwlwifi-*.ucode iwlwifi-*.pnvm ath10k ath11k ath12k ath9k_htc \
        rtw88 rtw89 rtlwifi rtl_bt rtl_nic brcm mrvl mediatek \
        intel/ibt-* qca i915 amdgpu nvidia regulatory.db regulatory.db.p7s ; do \
          cp -a --parents $p /firmware/usr/local/lib/firmware/ 2>/dev/null || true ; \
      done ; \
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
COPY --from=slurp /slurp /
COPY --from=swayidle /swayidle /
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
# Login manager (ly) + its X protocol libs
COPY --from=libxau /libxau /
COPY --from=libxcb /libxcb /
COPY --from=ly /ly /


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
COPY --from=dunst /dunst /
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
# because the kairos stage's `dracut -f` bakes the splash into the initramfs
# behind BuildKit's back. Here the cache key is honest: `COPY install-ui/ ./` is
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
# Overwrite the base image's stock splash with our rebranded build. Must land
# before the kairos-init stage runs `dracut -f`, so the 50hadron-splash dracut
# module (Task 2) bakes THIS binary into the initramfs rather than the stock one.
COPY --from=hadron-splash /build/splash/hadron-splash /usr/bin/hadron-splash

# System setup. NOTE: no user is created here — the desktop user is defined at
# install time via a Kairos cloud-config (see cloud-config.yaml) and lives on
# the persistent /home. We only ensure the groups it will join exist, enable
# the system services, and configure the ly display manager on tty1.
RUN chmod 1777 /tmp; \
    ldconfig 2>/dev/null || true; \
    # Register the bundled fonts for the selected bar, terminal, and launcher.
    fc-cache -f 2>/dev/null || true; \
    for g in audio video render input bluetooth seat docker; do groupadd -f "$g"; done; \
    mkdir -p /var/lib/docker /var/lib/xkb; \
    # The selected session launcher lives in /usr/bin (NOT /usr/local): Kairos mounts /usr/local
    # from the persistent partition on the installed system, which shadows
    # anything baked into the image there — ly would exec a missing launcher and
    # bounce straight back to the login screen. The installer TUI lives in
    # /usr/bin for the same reason (it is COPYied in after this block, already
    # executable, so the glob below deliberately does not have to reach it).
    chmod +x /usr/bin/start-desktop /usr/bin/hadron-*; \
    # ly runs on tty1 via the ly@tty1 instance of the ly@.service template (ly
    # 1.3.x dropped the plain ly.service and the config `tty` option — the tty is
    # the systemd instance). It authenticates the cloud-config user and launches
    # the selected Sway or i3 session via its desktop entry.
    # Tokyo Night theme for ly + the "Black Hole" .dur animation from ly's README
    # as the login background (full_color must stay on for the 256-colour file).
    sed -i \
      -e 's/^animation = .*/animation = dur_file/' \
      -e 's|^dur_file_path = .*|dur_file_path = /etc/ly/blackhole-smooth.dur|' \
      -e 's/^dur_offset_alignment = .*/dur_offset_alignment = center/' \
      -e 's/^full_color = .*/full_color = true/' \
      -e 's/^bg = .*/bg = 0x001a1b26/' \
      -e 's/^fg = .*/fg = 0x00c0caf5/' \
      -e 's/^border_fg = .*/border_fg = 0x007aa2f7/' \
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
    systemctl mask getty@tty1.service 2>/dev/null || true; \
    # M2: NetworkManager is the network manager (systemd-networkd disabled at
    # runtime in favour of NM).
    systemctl enable NetworkManager.service wpa_supplicant.service 2>/dev/null || true; \
    # M3: PipeWire + WirePlumber as per-user services
    systemctl --global enable pipewire.socket pipewire-pulse.socket wireplumber.service 2>/dev/null || true; \
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
    # Boot splash: the base image ships /usr/bin/hadron-splash (animated "HADRON"
    # ASCII, self-limits to ~5s). Pull it into multi-user.target like ly; the unit
    # is ordered Before=ly@tty1.service so it renders on tty1 before the login.
    systemctl enable hadron-splash.service 2>/dev/null || true; \
    ln -sf /usr/lib/systemd/system/hadron-splash.service /etc/systemd/system/multi-user.target.wants/hadron-splash.service; \
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
FROM quay.io/kairos/kairos-init:${KAIROS_INIT} AS kairos-init

FROM default AS kairos
ARG MODEL=generic
ARG TRUSTED_BOOT=false
ARG VERSION=v0.0.0
ARG FIPS=no-fips
RUN --mount=type=bind,from=kairos-init,src=/kairos-init,dst=/kairos-init \
    fips_flag=""; [ "$FIPS" = "fips" ] && fips_flag="--fips"; \
    /kairos-init -l debug -s install -m "${MODEL}" -t "${TRUSTED_BOOT}" --version "${VERSION}" $fips_flag && \
    /kairos-init -l debug -s init    -m "${MODEL}" -t "${TRUSTED_BOOT}" --version "${VERSION}" $fips_flag
# Brand the GRUB boot-menu entry. kairos-agent reads GRUB_ENTRY_NAME from
# /etc/kairos-release (which kairos-init just generated) when install.grub-entry-
# name is unset, preferring it over /etc/os-release. The OS identity fields in
# /etc/os-release are intentionally left untouched.
RUN echo 'GRUB_ENTRY_NAME="hadron-desktop"' >> /etc/kairos-release
# Tokyo Night GRUB menu colours — text-mode fallback. The main menu renders from
# /etc/cos/grub.cfg (copied to COS_STATE at install) before grubmenu.cfg switches
# to the gfxterm theme, and these 16-colour accents also apply if gfxterm/the
# theme ever fail to load. Inserted after `loadfont unicode`, before the menu.
RUN sed -i '/^loadfont unicode/a set color_normal=light-gray/black\nset color_highlight=black/cyan\nset menu_color_normal=cyan/black\nset menu_color_highlight=black/cyan' /etc/cos/grub.cfg
# Tokyo Night gfxterm theme with a background image. kairos-init's branding stage
# writes its own /etc/kairos/branding/grubmenu.cfg (bundled.ExtraGrubCfg),
# clobbering the overlay copy, so re-apply ours AFTER kairos-init: it switches the
# installed menu to gfxterm and `set theme=` to themes/hadron/theme.txt. 08_grub
# copies this to COS_STATE/grubmenu at install; 09_hadron_grub_theme.yaml (oem)
# puts the theme dir (theme.txt + background.tga), the vendored unicode.pf2 font,
# and the grub module tree on STATE so gfxmenu/tga/the theme all resolve there.
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
