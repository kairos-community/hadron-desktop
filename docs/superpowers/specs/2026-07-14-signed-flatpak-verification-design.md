# Signed Flatpak verification in Hadron

**Date:** 2026-07-14

**Status:** Approved

**Scope:** Add a pinned OpenPGP engine to the shared Hadron desktop image,
replace Hadron's unsigned Flathub setup with signed verification, and make the
Cua compatibility fixture consume that same trust path. Both Sway and i3 gain
the verification runtime; neither gains agent services or agent files.

## Context

Hadron already builds Flatpak 1.16.6, OSTree with GPGME support, and the
libgpg-error, libassuan, and GPGME libraries. It deliberately omits a `gpg`
engine, so `hadron-user-setup` and the current Cua Chromium fixture add Flathub
with `--no-gpg-verify`.

That omission is no longer acceptable. An immutable Chromium application
commit does not authenticate the application, runtime, or extensions that are
downloaded. It also leaves an existing remote insecure forever because
`--if-not-exists` accepts any remote already named `flathub`.

The change is platform hardening rather than an agent-only workaround. Normal
Hadron users and the Cua release gate will exercise one signed Flatpak path.

## Decision

Hadron will source-build and pin the OpenPGP verification engine needed by its
existing GPGME stack. The common desktop target will contain that runtime, a
pinned copy of Flathub's official repository descriptor, and the public key
extracted from that descriptor.

The initial compatibility baseline is:

| Component | Version | SHA-256 for new source archive |
|---|---:|---|
| libgpg-error | 1.51 | existing, retained |
| libassuan | 3.0.2 | existing, retained |
| GPGME | 1.24.3 | existing, retained |
| libgcrypt | 1.12.2 | `7ce33c2492221a0436f96a8500215e9f3e3dcb5fd26a757cd415e7a843babd5e` |
| libksba | 1.8.0 | `296b9db9095749f2aa104202d7ab7fd09ad10710e00780a709c9754b1a1d9292` |
| npth | 1.8 | `8bd24b4f23a3065d6e5b26e98aba9ce783ea4fd781069c1b35d149694e90ca3e` |
| SQLite | 3.49.2 | `5c6d8697e8a32a1512a9be5ad2b2e7a891241c334f56f8b0fb4fc6051e1652e8` |
| GnuPG | 2.4.9 | `dd17ab2e9a04fd79d39d853f599cbc852062ddb9ab52a4ddeb4176fd8b302964` |

Every new source archive and the Flathub descriptor must come from its official
HTTPS distribution URL and pass a repository-pinned SHA-256 check before it is
unpacked or installed. Version and checksum changes are reviewed together.

GnuPG 2.4.9 is chosen as the mature 2.4-series baseline that matches the
working Alpine 3.22 package family used for the size experiment. Hadron does
not copy Alpine packages into its musl image; all new components use the same
Hadron toolchain and source-build pattern as GPGME and Flatpak.

## Runtime boundary

The final image contains the GnuPG programs and shared libraries needed for a
reliable GPGME OpenPGP engine: `gpg`, `gpgv`, `gpgconf`, `gpg-agent`,
`dirmngr`, and `keyboxd`. It omits S/MIME, smartcard, WKS, documentation, and
localization components unless a build-time or runtime gate proves one is a
hard dependency. The build copies a closed runtime artifact into the common
`default` image and rejects unresolved dynamic links.

`keyboxd` requires SQLite, which the Hadron toolchain does not ship. The closed
artifact therefore includes a pinned SQLite 3.49.2 shared library but not the
SQLite command-line program. SQLite is counted inside the same 20 MiB budget.

The supported Hadron use is repository-signature verification. Hadron ships no
private key, general-user GnuPG trust database, keyserver configuration, or
enabled GnuPG system service. The general-purpose `gpg` CLI may still be used
explicitly by a human; it is not exposed as an agent service and it receives no
additional privilege.

The final `/usr` file delta introduced by the new engine and dependencies must
not exceed 20 MiB uncompressed. The expected practical delta is about
13-16 MiB based on the Alpine 3.22 package closure; the implementation records
the actual Hadron artifact size and Docker image delta.

## Trust bootstrap

The image build downloads Flathub's official
`https://dl.flathub.org/repo/flathub.flatpakrepo`, verifies its pinned SHA-256,
`3371dd250e61d9e1633630073fefda153cd4426f72f4afa0c3373ae2e8fea03a`,
and installs the exact descriptor as
`/usr/share/hadron/flathub.flatpakrepo`. It also decodes the descriptor's single
embedded `GPGKey` into `/usr/share/hadron/flathub.gpg` for idempotent repair of
existing remotes. The build asserts primary-key fingerprint
`6E5C05D979C76DAF93C081354184DD4D907A7CAE` as well as the descriptor checksum.

This makes the reviewed image contents, rather than a network response during
each boot, the trust bootstrap. Flatpak still uses the canonical repository URL
from the descriptor for summaries and objects. Updating Flathub's bootstrap
key or descriptor is an explicit source change.

This protects Flatpak objects and summaries against unsigned or differently
signed content after the bootstrap is accepted. It does not protect against a
compromised Flathub signing key, a compromised Hadron build, or malicious code
that Flathub intentionally signs.

## Per-user remote migration

`hadron-user-setup` remains the idempotent late-boot owner of each human user's
Flathub remote, but its behavior changes:

1. If `flathub` is absent, add it from the pinned local `.flatpakrepo` file.
2. If `flathub` already exists, disable it before repair. In one modification,
   set its URL to the descriptor's canonical URL, import the pinned public key,
   and explicitly enable GPG verification.
3. Re-read the disabled remote and require the canonical URL with no
   `no-gpg-verify` option, then enable it and assert the same state again.

The script never preserves a same-named remote merely because it exists. It
also never falls back to `--no-gpg-verify`. A failed repair leaves the old
remote disabled. Any failed add, repair, or assertion leaves the setup service
failed and emits a useful diagnostic; desktop boot may continue, but Hadron
must not report Flathub as configured successfully.

The migration repairs existing installations in place so installed Flatpak
deployments keep their `flathub` origin. It does not delete user applications
or silently replace the remote with a different provider.

## Cua compatibility fixture

The Task 3 test image adds its system-wide `flathub` remote from the same local
descriptor. Before installing Chromium it asserts:

- the remote URL is canonical;
- the remote options do not contain `no-gpg-verify`;
- GPGME discovers a working OpenPGP engine;
- the configured remote can verify signed Flathub metadata.

Chromium remains pinned to the reviewed application commit. Every downloaded
application, runtime, and extension object must be accepted through the signed
remote. The fixture records the resolved application and runtime commits in
its build evidence so later failures can distinguish an application-pin change
from a runtime change.

The fixture must not carry a second GnuPG installation, a test-only signing
key, or an unsigned fallback. A failure here is evidence that the shared
Hadron trust path is broken.

## Verification gates

The implementation is accepted only when all of these pass:

- A pre-change test demonstrates that signed `.flatpakrepo` setup fails in the
  current image because GPGME cannot find an OpenPGP engine.
- Both `DESKTOP=sway` and `DESKTOP=i3` default images build with the shared
  runtime and contain no unresolved GnuPG or GPGME links.
- `gpg --version` and GPGME engine discovery succeed in the final image.
- A clean user installation adds Flathub from the pinned descriptor with GPG
  verification enabled.
- An existing `flathub` remote created with `--no-gpg-verify` is repaired to the
  canonical URL and signed mode by a second setup run.
- A deliberately malformed descriptor or key fails closed and never leaves an
  insecure remote enabled.
- The Cua compatibility image installs the exact Chromium application commit
  through the signed system remote and records its signed dependency inventory.
- The added runtime files stay within the 20 MiB uncompressed budget.

## Delivery sequence

1. Add the pinned source-build stages, closed runtime artifact, descriptor, and
   verification checks to the shared Hadron image.
2. Change and test `hadron-user-setup` migration behavior.
3. Rebuild both normal desktop flavors and verify their common trust path.
4. Resume Cua Phase 1 Task 3, remove its unsigned remote, address the remaining
   fixture review findings, and rerun the Task 3 review.

The GPG work lands as a separately reviewed platform-hardening commit before
the Cua fixture review is declared complete.
