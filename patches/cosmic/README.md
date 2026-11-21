# COSMIC patches

Isotope's changes to the COSMIC components that `tools/cosmic-build.sh` builds
from source. The Dockerfile copies this directory to `/usr/share/isotope-patches`
(override with `COSMIC_PATCHES`).

Layout:

- `<repo>/*.patch` is applied with `patch -p1` in the unpacked source of that
  repo (`cosmic-comp`, `cosmic-panel`, ...), in name order. For a directory
  passed to `cosmic-build.sh` (the `applets/` workspace), `<repo>` is the
  directory's base name.
- `libcosmic/*.patch` is applied, in name order, to every libcosmic git checkout
  that the repo's `Cargo.lock` names (`git+https://github.com/pop-os/libcosmic`).
  Each checkout gets a stamp `.isotope-patched-<sha256 of these patches>` and is
  patched once; components built later in the same stage see it already patched.

Only what `cosmic-build.sh` builds is patched: greetd and pop-launcher are built
by their own Dockerfile stages, so patches for them are not applied.

A patch that does not apply fails the build. `CARGO_TARGET_DIR` gets a
`/<first 12 hex of the sha256 of all patches>` suffix when there are patches, so
a cached target dir cannot hold code built without them.

Header rule (see `patches/i3lock-indicator.patch`): start every patch with a
comment block that says what it changes, why, and which upstream version it was
made against, then the unified diff with `a/` and `b/` prefixes (`-p1`).

## libcosmic revisions at epoch-1.11.0

The `Cargo.lock` of each repo `cosmic-build.sh` builds (the Dockerfile
`cosmic-shell` and `cosmic-apps` stages), at tag `epoch-1.11.0`:

| libcosmic rev | Repos |
|---|---|
| `430f3d48a97d` | cosmic-settings-daemon, cosmic-panel, cosmic-applets, cosmic-bg, cosmic-osd, cosmic-notifications, cosmic-launcher, cosmic-app-library, cosmic-idle, xdg-desktop-portal-cosmic, cosmic-settings, cosmic-files, cosmic-term, cosmic-edit, cosmic-store, cosmic-greeter |
| `d922ac380134` | cosmic-comp |
| `8cb10bf6e24b` | cosmic-workspaces-epoch |
| none | cosmic-session, cosmic-randr, cosmic-screenshot |

`src/config/mod.rs` and `src/widget/header_bar.rs`, the files
`libcosmic/0001-window-controls-left.patch` changes, are the same at the three
revisions, so one patch serves them all. When an epoch bump makes a patch fail
on one revision, the build stops at that component: list the revisions again
(`gh api` the `Cargo.lock` of each repo at the new tag) and refresh the patch.
`applets/` (Isotope's own applets) must pin one of the revisions above.
