# Signed Flatpak Verification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every Hadron desktop flavor a pinned GPGME OpenPGP engine and a fail-closed, signed Flathub configuration, then use that shared trust path to finish the Cua Chromium fixture gate.

**Architecture:** Source-build the missing GnuPG dependency closure into one size-bounded runtime artifact copied by the common desktop target. Ship a checksummed Flathub descriptor and public key in that artifact, migrate each user's remote through an idempotent shell routine, and exercise the same files from the Cua system-wide fixture image.

**Tech Stack:** Hadron musl toolchain, GnuPG 2.4.9, libgpg-error 1.56, libgcrypt 1.12.2, libksba 1.8.0, npth 1.8, NTBTLS 0.3.2, SQLite 3.49.2, GPGME 1.24.3, Flatpak 1.16.6, POSIX shell integration tests, Docker BuildKit, GTK 3.24.43, Chromium Flatpak.

## Global Constraints

- Both Sway and i3 receive the common verification runtime; neither receives Cua services, accounts, fixtures, tokens, or agent configuration.
- Build every new native component from its pinned official source with the Hadron musl toolchain; do not copy Alpine packages into Hadron.
- Verify every new source archive and `flathub.flatpakrepo` against the exact SHA-256 values below before extraction or installation.
- The final runtime includes `gpg`, `gpgv`, `gpgconf`, `gpg-agent`, `dirmngr`, and `keyboxd`; omit GPGSM, smartcard, WKS, documentation, and localization components.
- Ship no private key, general-user GnuPG trust database, keyserver configuration, enabled GnuPG service, or additional privilege.
- Install `/usr/share/hadron/flathub.flatpakrepo` with SHA-256 `3371dd250e61d9e1633630073fefda153cd4426f72f4afa0c3373ae2e8fea03a` and `/usr/share/hadron/flathub.gpg` with SHA-256 `8bdc20abc4e19c0796460beb5bfe0e7aa4138716999e19c6f2dbdd78cc41aeaa` and primary fingerprint `6E5C05D979C76DAF93C081354184DD4D907A7CAE`.
- Keep the complete new runtime artifact, including SQLite and Flathub bootstrap files, at or below 20 MiB (20,971,520 bytes) uncompressed.
- Never use `--no-gpg-verify` in production setup or the Cua fixture. The only permitted occurrence is the migration integration test that constructs an intentionally insecure legacy remote.
- Repair existing `flathub` remotes in place: disable first, set canonical URL `https://dl.flathub.org/repo/`, import the pinned key, enable GPG verification, assert while disabled, and only then re-enable.
- A failed add or repair must return nonzero; any pre-existing insecure remote must remain disabled, with no unsigned fallback.
- Treat remote discovery as present, absent, or error; never interpret a failed Flatpak query as absence. Replace an existing remote's trusted-key set rather than importing additively, and validate both exact bootstrap-file digests immediately before use.
- Keep Chromium application commit `eba0ee8ff9359eacd8470be0c6c684498e002956dc4c32d378f021f3d33ee14d`; Flatpak 1.16.6 requires stable install followed by `flatpak update --commit` and an exact equality assertion.
- Address all open Task 3 review findings: signed remote trust, precise GTK error reporting, exact GTK drag payload validation, and correct Chromium drag-source semantics.

---

## File Structure

**Created:**

- `test/flatpak/check-runtime.sh` — validates the closed GnuPG runtime, bootstrap descriptor/key, dynamic links, omissions, and size budget in an image.
- `test/flatpak/check-user-setup.sh` — exercises clean, insecure-existing, and malformed-key per-user remote scenarios with real Flatpak installations.
- `test/agent/check-fixtures.sh` — focused regression contract for the signed fixture Dockerfile and the three reviewed UI-fixture defects.

**Modified:**

- `Dockerfile` — builds the pinned dependency closure, assembles the closed artifact, and copies it into the shared `default` image.
- `rootfs/usr/bin/hadron-user-setup` — creates or repairs signed per-user Flathub remotes and fails closed.
- `test/agent/Dockerfile.compat` — consumes the shared signed bootstrap, verifies metadata, installs the exact Chromium commit, and records every resolved Flatpak ref.
- `test/agent/fixtures/gtk3/main.c` — preserves the actual failing errno and validates the exact drag token.
- `test/agent/fixtures/chromium/index.html` — removes unsupported button activation semantics from the drag source.

## Task 1: Build the closed OpenPGP runtime and trust bootstrap

**Files:**

- Create: `test/flatpak/check-runtime.sh`
- Modify: `Dockerfile:1339-1372`
- Modify: `Dockerfile:1475-1479`
- Modify: `Dockerfile:2453-2459`

**Interfaces:**

- Consumes: existing `libassuan` 3.0.2, GPGME 1.24.3, `${COMMON_CONFIGURE_ARGS}`, and the common `default` image target; upgrades the shared libgpg-error stage from 1.51 to the libgcrypt-required minimum 1.56.
- Produces: Docker stage `signed-flatpak-runtime` rooted at `/signed-flatpak-runtime`, runtime size file `/usr/share/hadron/gnupg-runtime-size`, descriptor `/usr/share/hadron/flathub.flatpakrepo`, and key `/usr/share/hadron/flathub.gpg`.

- [ ] **Step 1: Add the image-level runtime contract before changing the image**

Create executable `test/flatpak/check-runtime.sh` with this behavior:

```bash
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
```

- [ ] **Step 2: Run the contract against the pre-change image and verify RED**

Run:

```bash
chmod +x test/flatpak/check-runtime.sh
IMAGE=hadron-agent:compat test/flatpak/check-runtime.sh
```

Expected: exit 1 with `missing required program: gpg`.

- [ ] **Step 3: Add the pinned dependency source stages**

Add these versions, hashes, and source URLs next to the existing GPGME stages in `Dockerfile`:

```dockerfile
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
```

- [ ] **Step 4: Build GnuPG with only the approved component boundary**

Add the following stage after those dependencies. Keep `dirmngr` and `keyboxd`, while explicitly disabling S/MIME, smartcard, WKS, docs, localization, LDAP, libdns, and bzip2:

```dockerfile
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
```

- [ ] **Step 5: Assemble and bound one closed runtime artifact**

Create `signed-flatpak-runtime` from the toolchain. Copy each new dependency into the live stage so GnuPG can run, copy only its shared library into the artifact, copy the complete `/gnupg` install tree, remove excluded documentation/localization, and install the pinned Flathub files:

```dockerfile
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
```

- [ ] **Step 6: Copy the artifact into the shared image and update stale comments**

In `FROM ${BASE_IMAGE} AS default`, add:

```dockerfile
COPY --from=signed-flatpak-runtime /signed-flatpak-runtime /
```

Place it with the existing Flatpak/GPGME copies. Rewrite the comments at the GPGME and Flatpak stages to state that runtime verification is supplied by `signed-flatpak-runtime`; remove every claim that Hadron has no GPG engine.

- [ ] **Step 7: Build the i3 default target and verify GREEN**

Run:

```bash
docker build --target default --build-arg DESKTOP=i3 \
  -t hadron-signed-flatpak:i3 .
IMAGE=hadron-signed-flatpak:i3 test/flatpak/check-runtime.sh
```

Expected: build exits 0; GnuPG reports 2.4.9; all six programs and GPGME OpenPGP discovery pass; all dynamic links resolve; the exact descriptor/key assertions pass; runtime size is at most 20,971,520 bytes.

- [ ] **Step 8: Record size evidence and commit**

Run:

```bash
docker run --rm hadron-signed-flatpak:i3 \
  cat /usr/share/hadron/gnupg-runtime-size
docker image inspect hadron-signed-flatpak:i3 \
  --format '{{.Size}}'
git add Dockerfile test/flatpak/check-runtime.sh
git commit -m "build: add signed Flatpak GPG engine"
```

Record both byte counts in the task report. Expected: artifact count is numeric and no greater than 20,971,520.

## Task 2: Migrate per-user Flathub remotes fail-closed

**Files:**

- Modify: `Dockerfile` (final-image `/tmp` permissions)
- Create: `test/flatpak/check-user-setup.sh`
- Modify: `rootfs/usr/bin/hadron-user-setup:1-17`

**Interfaces:**

- Consumes: `/usr/share/hadron/flathub.flatpakrepo`, `/usr/share/hadron/flathub.gpg`, Flatpak 1.16.6, and the `signed-flatpak-runtime` copied by Task 1.
- Produces: idempotent `hadron-user-setup` behavior with an enabled signed `flathub` remote or a nonzero result with any legacy insecure remote disabled.

- [ ] **Step 1: Add real migration scenarios before changing setup behavior**

Create executable `test/flatpak/check-user-setup.sh`. It must run these four independent containers against `${IMAGE:?}`:

```bash
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
```

- [ ] **Step 2: Run the migration test against Task 1 and verify RED**

Run:

```bash
chmod +x test/flatpak/check-user-setup.sh
IMAGE=hadron-signed-flatpak:i3 test/flatpak/check-user-setup.sh
```

Expected: the precondition or first case exits nonzero. The Task 1 image has `/tmp`
mode `0755`, which prevents libostree from creating its per-user temporary GPG
home, and the current setup script still requests `no-gpg-verify`.

- [ ] **Step 3: Restore the temporary-directory invariant and replace `hadron-user-setup` with explicit state transitions**

In the existing final `default` image setup `RUN`, set `/tmp` to the standard
world-writable sticky mode before any per-user Flatpak operation can run:

```dockerfile
    chmod 1777 /tmp; \
```

This is required because libostree creates `ostree-gpg-*` directories below
`g_get_tmp_dir()` as the unprivileged user while importing a remote key.

Implement these POSIX-shell functions and constants in `rootfs/usr/bin/hadron-user-setup`:

```sh
#!/bin/sh
set -u

FLATHUB_NAME=flathub
FLATHUB_URL=https://dl.flathub.org/repo/
FLATHUB_DESCRIPTOR=/usr/share/hadron/flathub.flatpakrepo
FLATHUB_KEY=/usr/share/hadron/flathub.gpg
FLATHUB_FINGERPRINT=6E5C05D979C76DAF93C081354184DD4D907A7CAE

GNUPGHOME=$(mktemp -d /run/hadron-user-setup-gpg.XXXXXX) || exit 1
chmod 700 "$GNUPGHOME"
export GNUPGHOME
trap 'rm -rf "$GNUPGHOME"' 0 HUP INT TERM

remote_details() {
    su - "$1" -c \
        'flatpak --user remotes --show-disabled --columns=name,url,options'
}

remote_exists() {
    remote_details "$1" | awk -F '\t' '$1 == "flathub" { found=1 }
        END { exit !found }'
}

assert_remote_state() {
    user=$1
    expected=$2
    remote_details "$user" | awk -F '\t' \
        -v expected="$expected" -v url="$FLATHUB_URL" '
        $1 == "flathub" {
            found=1
            if ($2 != url || $3 ~ /no-gpg-verify/) bad=1
            if (expected == "disabled" && $3 !~ /disabled/) bad=1
            if (expected == "enabled" && $3 ~ /disabled/) bad=1
        }
        END { exit (!found || bad) }
    '
}

disable_remote() {
    su - "$1" -c 'flatpak --user remote-modify --disable flathub'
}

bootstrap_key_valid() {
    fingerprint=$(gpg --batch --with-colons --show-keys \
        "$FLATHUB_KEY" 2>/dev/null |
        awk -F: '$1 == "fpr" { print $10; exit }')
    [ "$fingerprint" = "$FLATHUB_FINGERPRINT" ]
}

leave_disabled() {
    if remote_exists "$1" && ! disable_remote "$1"; then
        echo "hadron-user-setup: unable to disable failed flathub remote for $1" >&2
    fi
    return 1
}

configure_remote() {
    user=$1
    if remote_exists "$user"; then
        disable_remote "$user" || return 1
        if ! bootstrap_key_valid || ! su - "$user" -c \
            "flatpak --user remote-modify --url='$FLATHUB_URL' \
             --gpg-import='$FLATHUB_KEY' --gpg-verify '$FLATHUB_NAME'" ||
           ! assert_remote_state "$user" disabled; then
            leave_disabled "$user"
            return 1
        fi
        if ! su - "$user" -c \
            "flatpak --user remote-modify --enable '$FLATHUB_NAME'" ||
           ! assert_remote_state "$user" enabled; then
            leave_disabled "$user"
            return 1
        fi
        return 0
    fi

    if ! bootstrap_key_valid || ! su - "$user" -c \
        "flatpak --user remote-add '$FLATHUB_NAME' '$FLATHUB_DESCRIPTOR'" ||
       ! assert_remote_state "$user" enabled; then
        leave_disabled "$user"
        return 1
    fi
}

status=0
for user in $(awk -F: '$3 >= 1000 && $3 < 60000 { print $1 }' /etc/passwd); do
    home=$(awk -F: -v user="$user" '$1 == user { print $6 }' /etc/passwd)
    if [ -d "$home" ] && ! configure_remote "$user"; then
        echo "hadron-user-setup: signed Flathub setup failed for $user" >&2
        status=1
    fi
done
exit "$status"
```

Keep comments concise and aligned with the signed migration. Do not suppress Flatpak stderr and do not use a final `|| true`.

- [ ] **Step 4: Verify shell syntax and GREEN migration behavior**

Run:

```bash
sh -n rootfs/usr/bin/hadron-user-setup
docker build --target default --build-arg DESKTOP=i3 \
  -t hadron-signed-flatpak:i3 .
IMAGE=hadron-signed-flatpak:i3 test/flatpak/check-runtime.sh
IMAGE=hadron-signed-flatpak:i3 test/flatpak/check-user-setup.sh
```

Expected: all commands exit 0. `/tmp` is `01777`, the clean case is idempotent,
the insecure URL/trust case is repaired and enabled, the malformed-key case
returns nonzero with `flathub` disabled, and the malformed-descriptor case
leaves no enabled `flathub` remote.

- [ ] **Step 5: Prove the shared Sway path**

Run:

```bash
docker build --target default --build-arg DESKTOP=sway \
  -t hadron-signed-flatpak:sway .
IMAGE=hadron-signed-flatpak:sway test/flatpak/check-runtime.sh
IMAGE=hadron-signed-flatpak:sway test/flatpak/check-user-setup.sh
```

Expected: the same runtime and all four migration scenarios pass in the Sway image.

- [ ] **Step 6: Commit the migration**

Run:

```bash
git add Dockerfile rootfs/usr/bin/hadron-user-setup test/flatpak/check-user-setup.sh
git commit -m "fix: require signed Flathub remotes"
```

### Task 2 review corrections (binding before acceptance)

The first independent review found two security gaps not covered by the four
nominal scenarios. Apply these corrections test-first before Task 2 is closed:

- Capture `flatpak remotes` status before parsing. Distinguish an absent remote
  from a query error; on query error, make a best-effort disable attempt and
  return nonzero.
- Route an initial disable failure through the same cleanup path so a transient
  first failure is retried. Assert disabled state whenever discovery is usable.
- Do not allocate the temporary GnuPG home before existing remotes are
  disabled. Check both `mktemp` and `chmod` failures.
- Validate the exact descriptor and decoded-key SHA-256 values above, not only
  the first `fpr` record.
- Replace any existing trusted-key set while the remote is disabled; do not
  add the pinned key to a keyring that may contain other primary keys.
- Add focused fault-injection regressions for a failed remote query and a
  failed first disable, plus valid extra-key regressions for clean and repair
  paths. These supplement, rather than replace, the four real Flatpak cases.
- Add a two-user aggregation case and use a line-preserving passwd loop.

Expected: injected failures return nonzero and the real Flatpak state is
disabled; tampered bootstrap inputs return nonzero; successful repair leaves
only the pinned trust material; one user's failure does not prevent processing
the other user.

## Task 3: Close the Cua fixture review on the shared signed path

**Files:**

- Create: `test/agent/check-fixtures.sh`
- Modify: `test/agent/Dockerfile.compat:22-34`
- Modify: `test/agent/fixtures/gtk3/main.c:70-135,225-250`
- Modify: `test/agent/fixtures/chromium/index.html:68-72`
- Update report: `.superpowers/sdd/task-3-report.md`

**Interfaces:**

- Consumes: `hadron-signed-flatpak:i3`, the shared descriptor/key files, GPGME engine, Task 3 Chromium pin, and the existing GTK/Chromium fixture state contracts.
- Produces: `hadron-agent-compat:test` with a signed system `flathub`, exact Chromium application commit, `/etc/hadron-agent-test/flatpak-refs.tsv`, exact GTK drag-token semantics, accurate persistence diagnostics, and valid Chromium accessibility semantics.

- [ ] **Step 1: Add a focused regression contract before fixing the findings**

Create executable `test/agent/check-fixtures.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

dockerfile=test/agent/Dockerfile.compat
gtk=test/agent/fixtures/gtk3/main.c
chromium=test/agent/fixtures/chromium/index.html

if grep -F -- '--no-gpg-verify' "$dockerfile"; then
    echo 'unsigned remote flag still present' >&2
    exit 1
fi
grep -F '/usr/share/hadron/flathub.flatpakrepo' "$dockerfile" >/dev/null
grep -F 'remotes --system --show-details --columns=name,url,options' \
    "$dockerfile" >/dev/null
grep -F 'flatpak-refs.tsv' "$dockerfile" >/dev/null

if grep -F 'gtk_selection_data_get_length(selection_data) > 0' "$gtk"; then
    echo 'GTK accepts arbitrary nonempty drag payloads' >&2
    exit 1
fi
grep -F 'gtk_selection_data_get_text(selection_data)' "$gtk" >/dev/null
if grep -F 'g_strerror(errno)' "$gtk"; then
    echo 'GTK persistence reports ambient errno' >&2
    exit 1
fi

if grep -E 'id="drag-source"[^>]*role="button"' "$chromium"; then
    echo 'Chromium drag source still promises button activation' >&2
    exit 1
fi
grep -E 'id="drag-source"[^>]*role="group"' "$chromium" >/dev/null
```

- [ ] **Step 2: Run it against the reviewed Task 3 commit and verify RED**

Run:

```bash
chmod +x test/agent/check-fixtures.sh
test/agent/check-fixtures.sh
```

Expected: exit 1 with `unsigned remote flag still present`.

- [ ] **Step 3: Replace the fixture's unsigned system remote**

In `test/agent/Dockerfile.compat`, replace the remote/install `RUN` with a signed sequence that:

```dockerfile
RUN commit="$(cat /src/chromium.commit)" && \
    test "${#commit}" -eq 64 && \
    printf '%s\n' "$commit" | grep -Eq '^[0-9a-f]{64}$' && \
    gpgconf --list-components | grep -E '^gpg:OpenPGP:' >/dev/null && \
    flatpak --system remote-add flathub \
      /usr/share/hadron/flathub.flatpakrepo && \
    details="$(flatpak remotes --system --show-details \
      --columns=name,url,options)" && \
    printf '%s\n' "$details" | awk -F '\t' \
      '$1 == "flathub" && $2 == "https://dl.flathub.org/repo/" && \
       $3 !~ /no-gpg-verify/ { found=1 } END { exit !found }' && \
    flatpak remote-info --system --show-commit \
      flathub org.chromium.Chromium >/dev/null && \
    flatpak install --system --noninteractive -y \
      flathub org.chromium.Chromium//stable && \
    flatpak update --commit="$commit" --system --noninteractive -y \
      org.chromium.Chromium && \
    test "$(flatpak info --system --show-commit org.chromium.Chromium)" = \
      "$commit" && \
    install -Dm644 /src/chromium.commit \
      /etc/hadron-agent-test/chromium-commit && \
    flatpak list --system --all --columns=ref,active:f,origin,runtime \
      > /etc/hadron-agent-test/flatpak-refs.tsv && \
    test -s /etc/hadron-agent-test/flatpak-refs.tsv && \
    awk -F '\t' '$3 != "flathub" { bad=1 } \
      END { exit (NR == 0 || bad) }' \
      /etc/hadron-agent-test/flatpak-refs.tsv
```

Do not add `--if-not-exists`: a same-named ambient remote must make this deterministic test build fail instead of bypassing the descriptor.

- [ ] **Step 4: Preserve the actual GTK persistence error**

In `write_all`, set `errno = EIO` before returning false on a zero-byte write. Replace `persist_state`'s ambient-errno warning with an `error_number` captured at the operation that failed:

```c
static void persist_state(const Fixture *fixture)
{
    gchar *contents = fixture_state_json(fixture);
    gchar *temporary_path = g_strdup_printf("%s.XXXXXX", STATE_PATH);
    gint fd = g_mkstemp_full(temporary_path, O_WRONLY | O_CLOEXEC, 0600);
    gint error_number = 0;

    if (fd < 0) {
        error_number = errno;
    } else {
        if (!write_all(fd, contents, strlen(contents))) {
            error_number = errno;
        } else if (fsync(fd) != 0) {
            error_number = errno;
        }
        if (close(fd) != 0 && error_number == 0) {
            error_number = errno;
        }
        if (error_number == 0 && g_rename(temporary_path, STATE_PATH) != 0) {
            error_number = errno;
        }
    }

    if (error_number != 0) {
        g_warning("unable to atomically write %s: %s",
                  STATE_PATH,
                  g_strerror(error_number));
        g_unlink(temporary_path);
    }

    g_free(temporary_path);
    g_free(contents);
}
```

- [ ] **Step 5: Require the exact GTK drag token**

Replace the length-only acceptance in `on_drag_data_received` with:

```c
guchar *payload = gtk_selection_data_get_text(selection_data);
gboolean accepted = payload != NULL &&
    strcmp((const gchar *)payload, "hadron-cua-fixture") == 0;

g_free(payload);
```

Keep the existing state update, label update, persistence call, and `gtk_drag_finish` after this block.

- [ ] **Step 6: Correct Chromium's drag-source role**

Change only the drag source element to a named group, retaining focusability and HTML drag behavior without promising button activation:

```html
<div id="drag-source" class="drag-box" role="group" tabindex="0"
     draggable="true" aria-label="Drag source">Drag source</div>
```

- [ ] **Step 7: Run focused GREEN checks and build through the shared base**

Run:

```bash
test/agent/check-fixtures.sh
docker build -f Dockerfile.agent --target agent \
  --build-arg BASE_IMAGE=hadron-signed-flatpak:i3 \
  -t hadron-agent:compat .
docker build -f test/agent/Dockerfile.compat \
  --build-arg BASE_IMAGE=hadron-agent:compat \
  -t hadron-agent-compat:test .
```

Expected: the source contract passes; GTK compiles under `-Wall -Wextra` without diagnostics; the signed Flatpak step installs the exact pinned Chromium application commit.

- [ ] **Step 8: Inspect trust, pin, inventory, and fixture runtime**

Run:

```bash
docker run --rm -i hadron-agent-compat:test sh -eu -s <<'CONTAINER'
test -x /usr/local/libexec/hadron-cua-gtk-fixture
gpgconf --list-components | grep -E "^gpg:OpenPGP:" >/dev/null
details=$(flatpak remotes --system --show-details \
  --columns=name,url,options)
printf "%s\n" "$details" | awk -F "\t" '
  $1 == "flathub" && $2 == "https://dl.flathub.org/repo/" &&
  $3 !~ /no-gpg-verify/ { found=1 }
  END { exit !found }
'
test "$(flatpak info --system --show-commit org.chromium.Chromium)" =
  "$(cat /etc/hadron-agent-test/chromium-commit)"
test -s /etc/hadron-agent-test/flatpak-refs.tsv
flatpak info --system org.chromium.Chromium
CONTAINER
```

Expected: exit 0; signed remote assertions pass; Chromium commit equals the baked pin; the dependency inventory is nonempty.

- [ ] **Step 9: Append evidence and commit the review fixes**

Append the RED result, focused GREEN result, image build result, trust/pin output, and resolved Flatpak inventory to `.superpowers/sdd/task-3-report.md`, then run:

```bash
git add test/agent/Dockerfile.compat test/agent/check-fixtures.sh \
  test/agent/fixtures/gtk3/main.c \
  test/agent/fixtures/chromium/index.html
git commit -m "test(agent): verify signed Chromium fixtures"
```

Regenerate the Task 3 review package from base `3b2d5f4` through the new `HEAD` and send it back to the existing Task 3 reviewer with the updated report.

## Final Verification for This Prerequisite Plan

After all three task reviews are clean, run fresh:

```bash
sh -n rootfs/usr/bin/hadron-user-setup
IMAGE=hadron-signed-flatpak:i3 test/flatpak/check-runtime.sh
IMAGE=hadron-signed-flatpak:i3 test/flatpak/check-user-setup.sh
IMAGE=hadron-signed-flatpak:sway test/flatpak/check-runtime.sh
IMAGE=hadron-signed-flatpak:sway test/flatpak/check-user-setup.sh
test/agent/check-fixtures.sh
docker run --rm hadron-agent-compat:test sh -eu -c '
  test -x /usr/local/libexec/hadron-cua-gtk-fixture
  test "$(flatpak info --system --show-commit org.chromium.Chromium)" =
    "$(cat /etc/hadron-agent-test/chromium-commit)"
  test -s /etc/hadron-agent-test/flatpak-refs.tsv
'
git diff --check 96f730b..HEAD
```

Expected: every command exits 0. Then mark the signed Flatpak prerequisite complete, mark Cua Phase 1 Task 3 complete, and resume Phase 1 Task 4 without finishing the feature branch.
