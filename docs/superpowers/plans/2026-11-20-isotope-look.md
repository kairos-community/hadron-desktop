# Isotope's look: COSMIC theme, login screen, icons — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A new Isotope install shows the approved mock's look: an Isotope COSMIC theme (dark and light, translucent, ivory accent), a centred login screen, a thin top bar with the Isotope mark and a quick-settings panel, a floating dock, left window buttons, a wallpaper that follows the mode, and the Conflux icons on all four variants.

**Architecture:**
- **System defaults in the image.** Everything ships as defaults under `/usr/share/cosmic` (cosmic-config's system layer), so the greeter and every user who has not changed a key get the look.
- **Colours.** gen-theme.py maps palette.toml onto COSMIC's theme fields. A small Rust tool built against the pinned cosmic-theme turns that map into the builder and computed themes.
- **COSMIC sources.** COSMIC changes are patches in `patches/cosmic/`, applied by tools/cosmic-build.sh. libcosmic patches go into every libcosmic checkout an app's Cargo.lock names.
- **Our own code.** The two applets are a cargo workspace in `applets/`. The wallpaper helper is an `isotope` subcommand in Go.

**Tech Stack:** Python 3 (tools/gen-theme.py), Rust with libcosmic and cosmic-theme at the COSMIC epoch-1.11.0 revisions, Go (update/, welcome/), Dockerfile stages, and bash image checks.

**Spec:** `docs/superpowers/specs/2026-11-20-isotope-look-design.md` (read it with `assets/isotope-look/mock.html` open in a browser).

## Global Constraints

- COSMIC epoch `1.11.0`. libcosmic as pinned by each component's Cargo.lock; cosmic-settings pins `430f3d48…` (look up the full hash in its Cargo.lock).
- palette.toml is the only colour source and is not changed. Every colour in a new file is written or checked by `tools/gen-theme.py`, and `--check` covers it.
- Theme accent = palette `accent` (night `#e9e7c1`, day `#3e3e32`). Text on the accent = `on_accent`. The brand yellow (`brand`, `brand_glow`) is only for the mark.
- Conflux is pinned at commit `85b0ef6dc59e4d459364337a92a14f604a2baa82` (GPL-3.0), installed at `/usr/share/icons/Conflux`, with `Inherits=Cosmic,hicolor`.
- UI fonts: `Inter Variable` and `JetBrains Mono`.
- `com.system76.CosmicTheme.Mode/v1/is_dark` defaults to `true`.
- Wallpapers: `/usr/share/backgrounds/isotope/isotope-desktop-day.png` and `isotope-desktop-night.png`.
- Every variant keeps the same features (AGENTS.md). The icon theme goes on all four; the rest is COSMIC-only.
- Pins: a `*_COMMIT` ARG for git commits (like `GOOGLE_FONTS_COMMIT`); a `# renovate:` comment only on `*_VERSION` ARGs.
- Every patch file starts with a header that says what it changes and why (AGENTS.md patch rule).
- Commits: `git -c user.name="Ettore Di Giacinto" -c user.email=mudler@kairos.io commit -s`. Messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- One branch, `feat/isotope-look`, with one commit or more per task. Open one PR at the end (Task 12), via ci-forks.

## Review Focus

1. **A user who already changed their COSMIC theme or panel.** Their keys must win, and nothing may crash or half-apply. Task 5 tests that a user panel dir is kept until they choose the Isotope layout. Task 9 tests that a user's own wallpaper is never replaced.
2. **The light theme over the day wallpaper and the dark theme over the night one: text contrast through frosted surfaces.** Task 2 tests the neutral ramp's order and the contrast floors. Task 12 checks screenshots against the anti-goal.
3. **One COSMIC app built with an unpatched libcosmic** (a different revision, or a stale cargo target cache). Task 6 fails the build when a checkout named in a Cargo.lock has no stamp. Task 7's image check finds the marker in every libcosmic binary.
4. **An app ID in the dock or first-run list with no Conflux icon,** which shows a mixed or missing icon where people look most. Task 1's check fails unless the ID resolves or is listed in `theme/conflux-missing.txt`.
5. **A layout id or panel config that references an applet that is not installed.** Task 10 and Task 11 extend the image check: every applet named in the default panel entries has a `.desktop` file in the image.

---

### Task 1: Conflux on every variant

**Files:**
- Modify: `Dockerfile`:
  - a new `icon-themes` stage after `hicolor-icon-theme` (~line 3623);
  - copy it next to every `COPY --from=hicolor-icon-theme` (~lines 4754, 4838, 4932, 5033);
  - add `icon-theme='Conflux'` to the gsettings override (~line 4221) and assert it in that RUN.
- Create: `theme/conflux-links.txt`, `theme/conflux-missing.txt`
- Modify: `rootfs/etc/xdg/fuzzel/fuzzel.ini`, `rootfs-sway/etc/mako/config`, `rootfs-i3/etc/dunst/dunstrc`, and Noctalia's settings, only if it has an icon theme key.
- Create: `test/look/check-look.sh` (image check; later tasks add to it)
- Modify: `.github/workflows/desktop-images.yml` and `.github/workflows/release.yml` (add a "Check the look" step next to "Check Isotope Welcome")

**Interfaces:**
- Produces:
  - `/usr/share/icons/Conflux` and `/usr/share/icons/Cosmic` on every variant;
  - `test/look/check-look.sh IMAGE DESKTOP`, which prints `PASS:`/`FAIL:` lines like `test/boot-ux/check-branding.sh` and exits 1 on any FAIL. Later tasks add `check` lines guarded by `[ "$DESKTOP" = cosmic ]`.

- [ ] **Step 1: Write the image check (fails on today's image).** Its checks:
  - `/usr/share/icons/Conflux/index.theme` exists, and its `Inherits=` line is exactly `Inherits=Cosmic,hicolor`.
  - Every theme named in `Inherits=` has an `index.theme`.
  - `find -L /usr/share/icons/Conflux -xtype l` prints nothing (every link resolves).
  - Every app ID in `/usr/share/cosmic/com.system76.CosmicAppList/v1/favorites` (COSMIC) and in `/usr/share/isotope/first-run-apps.list` (all variants) resolves to `Conflux/*/apps/<id>.svg`, or is listed in `/usr/share/isotope/conflux-missing.txt`.
  - `gsettings get org.gnome.desktop.interface icon-theme` is `'Conflux'`.
  - fuzzel.ini has `icon-theme=Conflux` (all variants).
  - On i3, dunstrc has `icon_theme = Conflux` and `enable_recursive_icon_lookup = true`.
  - On sway, the mako config resolves icons from Conflux.

- [ ] **Step 2: Run it against today's image to see it fail.**
  Run: `make sway-desktop && test/look/check-look.sh sway-desktop:dev sway`. Use an existing local image instead if a full build is too slow.
  Expected: `FAIL:` on the Conflux lines.

- [ ] **Step 3: Add the `icon-themes` stage.**
  - `ARG CONFLUX_COMMIT=85b0ef6dc59e4d459364337a92a14f604a2baa82`.
  - Fetch `https://github.com/<conflux repo>/archive/${CONFLUX_COMMIT}.tar.gz` (take the repo from the spec's source) and install it to `/icon-themes/usr/share/icons/Conflux`.
  - `sed` the `Inherits=` line to `Cosmic,hicolor`.
  - Install pop-os/cosmic-icons at `epoch-${COSMIC_VERSION}` as `Cosmic` (copy what its justfile's `install` copies; the toolchain has no `just`).
  - For each `<app-id> <conflux-name>` line in `theme/conflux-links.txt`, create `apps/scalable/<app-id>.svg` → `<conflux-name>.svg` in each size dir that has the target.
  - Copy `theme/conflux-missing.txt` to `/usr/share/isotope/`.
  - End the RUN with the "no dangling link" test.

- [ ] **Step 4: Write `theme/conflux-links.txt` and `theme/conflux-missing.txt`.**
  - Links cover the five COSMIC favourites, the other COSMIC apps the image installs (screenshot, the COSMIC Settings sub-apps that have desktop files), and the first-run apps (e.g. `org.videolan.VLC`).
  - Each line is `<app-id> <conflux icon name>`; `#` starts a comment.
  - An app with no fitting Conflux icon goes in conflux-missing.txt with a one-line reason.

- [ ] **Step 5: Set the icon theme.**
  - gsettings override: `icon-theme='Conflux'`, asserted in the same RUN with `gsettings --schemadir … get`.
  - fuzzel: `icon-theme=Conflux`.
  - dunst: `icon_theme = Conflux` and `enable_recursive_icon_lookup = true`.
  - mako: the icon setting its man page names (`icon-path=` with Conflux's apps directories).
  - Then run `python3 tools/gen-theme.py --check`. Expected: no drift; these lines hold no colours.

- [ ] **Step 6: Build one tiling variant and COSMIC, then run the check on both.**
  Run: `test/look/check-look.sh sway-desktop:dev sway` and `test/look/check-look.sh cosmic-desktop:dev cosmic`
  Expected: all `PASS:`.

- [ ] **Step 7: Wire the check into both workflows and commit.**
  `git add -A Dockerfile theme/ rootfs* test/look .github/workflows && git commit -s -m "feat(icons): Conflux, the Isotope icon theme on every desktop"`

### Task 2: gen-theme.py maps the palette onto COSMIC's theme

**Files:**
- Modify: `tools/gen-theme.py`
- Create: `theme/cosmic-theme.json` (generated; a build input, not shipped)
- Create: `tools/test_gen_theme.py`, run in ci.yml next to `test_gen_shortcuts.py`

**Interfaces:**
- Produces: `theme/cosmic-theme.json`, sorted keys, 2-space indent:

```json
{"dark": {"palette": {"name": "Isotope", "neutral_0": "#rrggbb", "...": "..."},
          "builder": {"accent": "#rrggbb", "success": "...", "warning": "...", "destructive": "...",
                      "bg_color": "...", "primary_container_bg": "...", "secondary_container_bg": "..."},
          "on_accent": "#rrggbb",
          "look": {"frosted": "Medium", "frosted_windows": true, "frosted_system_interface": true,
                   "frosted_panel": true, "frosted_applets": true, "frosted_maximized_apps": false,
                   "active_hint": 0, "gaps": [0, 12], "corner_radii": "Round"}},
 "light": { ...same keys from [day]... }}
```

  - **Builder fields:**
    - `accent` = `accent`;
    - `success` = `success`;
    - `warning` = `warning`;
    - `destructive` = `error`;
    - `bg_color` = `base`;
    - `primary_container_bg` = `surface`;
    - `secondary_container_bg` = `overlay`.
  - **Palette fields:**
    - `bright_red` and `accent_red` = `error`;
    - `bright_green` and `accent_green` = `success`;
    - `bright_orange` = `warning`;
    - `accent_orange` = `orange`;
    - `accent_purple` = `violet`;
    - `accent_blue` = `blue`;
    - `accent_indigo` = `blue`;
    - `accent_pink` = `violet`;
    - `accent_yellow` = `warning`;
    - `accent_warm_grey` = `muted`.

    Any other palette field stays COSMIC's stock value: the Rust tool (Task 3) leaves it alone.
  - **Neutral ramp:** `neutral_0`..`neutral_10` = `[crust, base, surface, overlay, border, muted, mix(muted,dim), dim, mix(dim,subtext), subtext, text]` for dark. For light, the same list sorted so that neutral_0 is the lightest. `mix` is the per-channel sRGB midpoint, rounded.
  - `look` values are constants in gen-theme.py, the same for both modes. Task 12 retunes them against the mock and keeps them in one dict, `COSMIC_LOOK`.

- [ ] **Step 1: Write the failing tests in `tools/test_gen_theme.py`** (unittest; import gen-theme via `importlib`, as test_gen_shortcuts.py does):
  - `test_cosmic_theme_accent_is_palette_accent`: `cosmic_theme(pal)["dark"]["builder"]["accent"] == pal["night"]["accent"]` and `["light"]["builder"]["accent"] == pal["day"]["accent"]`.
  - `test_cosmic_neutral_ramp_is_monotonic`: the relative luminance of `neutral_0`..`neutral_10` rises strictly in dark and falls strictly in light.
  - `test_cosmic_theme_has_no_foreign_colour`: every `#rrggbb` in the JSON is a palette value or a `mix` of two palette values.
  - `test_cosmic_theme_on_accent`: `["dark"]["on_accent"] == pal["night"]["on_accent"]`.

- [ ] **Step 2: Run them to see them fail.** Run: `python3 tools/test_gen_theme.py`. Expected: `AttributeError` (no `cosmic_theme`).

- [ ] **Step 3: Implement `cosmic_theme(pal: dict) -> dict` and the output `COSMIC_THEME = "theme/cosmic-theme.json"`.** In `main()`, add it to `outputs` the same way `NOCTALIA` is added, so `--check` covers it.

- [ ] **Step 4: Run the tests and generate the file.**
  - Run: `python3 tools/test_gen_theme.py && python3 tools/gen-theme.py && python3 tools/gen-theme.py --check`.
  - Expected: tests OK, `wrote theme/cosmic-theme.json`, then `theme: N files match`.

- [ ] **Step 5: Wire the test into ci.yml and commit.** `feat(theme): map the Isotope palette onto COSMIC's theme`

### Task 3: Build the COSMIC theme at image build time

**Files:**
- Create: `tools/isotope-cosmic-theme/Cargo.toml`, `Cargo.lock`, `src/main.rs`
- Modify: `Dockerfile`: a new stage `cosmic-theme` FROM `cosmic-build`; copy its output into the `cosmic` image next to the `rootfs-cosmic/` COPY (~line 5051).
- Modify: `test/look/check-look.sh`

**Interfaces:**
- Consumes: `theme/cosmic-theme.json` (Task 2).
- Produces:
  - `isotope-cosmic-theme <cosmic-theme.json> <out-dir>`, which writes `<out-dir>/com.system76.CosmicTheme.{Dark,Light}{,.Builder}/v2/*` and `com.system76.CosmicTheme.Mode/v1/is_dark` (`true`). It exits non-zero if a computed accent differs from the JSON accent.
  - The installed files land at `/usr/share/cosmic/...` in the COSMIC image.

- [ ] **Step 1: Write the failing Rust tests in `src/main.rs`:**
  - `dark_accent_matches_json`: build from a JSON fixture (`include_str!("../../../theme/cosmic-theme.json")`). Check `theme.accent.base` (to 8-bit hex) == `json.dark.builder.accent`.
  - `light_is_not_dark`: `!light.is_dark && dark.is_dark`.
  - `on_accent_is_set`: `theme.accent.on` and `theme.accent_button.on` hex == `json.<mode>.on_accent`.
  - `frosted_flags_carried`: `theme.frosted_windows && !theme.frosted_maximized_apps`. Use the 1.11 field names, which the spec lists.

- [ ] **Step 2: Run them to see them fail.**
  - Run `cargo test --locked` (in a `cosmic-build` container: `docker build --target cosmic-build -t cb . && docker run --rm -v $PWD:/src -w /src/tools/isotope-cosmic-theme cb cargo test`).
  - Expected: compile error or FAIL.

- [ ] **Step 3: Implement the tool.**
  - Dependencies: `cosmic-theme` and `cosmic-config` as git deps of `https://github.com/pop-os/libcosmic` at the revision cosmic-settings' epoch-1.11.0 Cargo.lock names; `serde_json`.
  - Procedure:
    1. Start from `ThemeBuilder::dark()` / `::light()`.
    2. Overlay the JSON:
       - each palette key onto the `CosmicPaletteInner` field of that name, through one `match` on the name that returns `&mut Srgba`;
       - the builder colours;
       - the `look` values.
    3. `build()`, then set `accent.on` and `accent_button.on` to `on_accent`.
    4. With `XDG_CONFIG_HOME=<tmp>`, `write_entry` the builder to `ThemeBuilder::{dark,light}_config()` and the theme to `Theme::{dark,light}_config()`.
    5. Write `Mode` with `is_dark = true`.
    6. Copy `<tmp>/cosmic/*` to `<out-dir>`.
  - An unknown JSON key is an error, not a skip.

- [ ] **Step 4: Run the tests to see them pass.** Run: `cargo test --locked`. Expected: 4 passed.

- [ ] **Step 5: Add the Dockerfile stage.**
  - `COPY tools/isotope-cosmic-theme theme/cosmic-theme.json`.
  - `cargo test --locked && cargo run --release --locked -- theme/cosmic-theme.json /cosmic-theme/usr/share/cosmic`.
  - Then `test -s .../com.system76.CosmicTheme.Dark/v2/accent`.

- [ ] **Step 6: Extend the image check (COSMIC).**
  - The four theme dirs and `Mode/v1/is_dark` = `true` exist under `/usr/share/cosmic`.
  - The Dark builder's `accent` file names the palette accent's channels. Compare the floats to `int(c*255+.5)`, using the python3 in the image or a sed. Pass the expected hex in from `theme/palette.toml` on the host.

- [ ] **Step 7: Build the COSMIC image, run the check, and commit.** `feat(cosmic): ship the Isotope theme, dark and light, as COSMIC's default`

### Task 4: Toolkit, compositor and panel defaults, and the Isotope layout

**Files:**
- Create:
  - `rootfs-cosmic/usr/share/cosmic/com.system76.CosmicTk/v1/{icon_theme,interface_font,monospace_font,header_size,interface_density}`;
  - `rootfs-cosmic/usr/share/cosmic/com.system76.CosmicComp/v1/{active_hint,appearance_settings}`.
  Each file holds only the RON value, the way COSMIC writes them.
- Create: `rootfs-cosmic/usr/share/cosmic-layouts/isotope/layout.kdl`, `icon.png`, `com.system76.CosmicPanel/v1/entries`, and `com.system76.CosmicPanel.Panel/v1/*` and `com.system76.CosmicPanel.Dock/v1/*`. Start from cosmic-panel `data/default_schema/` at epoch-1.11.0 and change only:
  - Panel: `size` XS, `expand_to_edges` true, `anchor_gap` false, `border_radius` 0, `opacity` 0.8.
  - Dock: `expand_to_edges` false, `anchor_gap` true, `border_radius` 160 (a pill), `opacity` 0.8, `keep_style_on_maximize` true.
  - Panel `plugins_wings`: workspaces on the left; status area, notifications, network, bluetooth, audio, battery, time, power on the right. Tasks 10 and 11 change these. `plugins_center` stays empty.
- Modify: `Dockerfile`: in `cosmic-config`, after `COPY rootfs-cosmic/ /`, run `cp -a /usr/share/cosmic-layouts/isotope/com.system76.CosmicPanel* /usr/share/cosmic/`. One source, no drift.
- Modify: `test/look/check-look.sh`

**Interfaces:**
- Produces:
  - layout ID `isotope`, whose `layout.kdl` `id` is lower than every stock layout's;
  - CosmicTk `icon_theme` = `"Conflux"`, `interface_font`/`monospace_font` naming `Inter Variable`/`JetBrains Mono`;
  - CosmicComp `active_hint` = `false`; `appearance_settings` with `shadow_tiled_windows: true`.

- [ ] **Step 1: Add the failing image checks (COSMIC):**
  - each CosmicTk and CosmicComp file above exists, holding the value above;
  - `diff -r` of each `/usr/share/cosmic-layouts/isotope/com.system76.CosmicPanel*` dir against `/usr/share/cosmic/<same>` is empty;
  - `isotope` has the lowest `id` of all `/usr/share/cosmic-layouts/*/layout.kdl`.
- [ ] **Step 2: Run the check against the Task 3 image to see it fail.**
- [ ] **Step 3: Write the files and the Dockerfile `cp`.** Read the value formats from cosmic-panel's and libcosmic's default schema at the pinned tags; do not guess the RON shape.
- [ ] **Step 4: Rebuild, run the check (all PASS), and commit.** `feat(cosmic): a thin frosted top bar, a floating dock and the Isotope layout`

### Task 5: Welcome treats the Isotope layout as "the system default"

**Files:**
- Modify: `welcome/cosmic.go:169-230`
- Test: `welcome/cosmic_test.go`

**Interfaces:**
- Consumes: layout ID `isotope` (Task 4).
- Produces:
  - `const SystemLayoutID = "isotope"`;
  - `func (l Layout) IsSystem() bool`;
  - `ApplyLayout` and `CurrentLayout` keep their signatures.

- [ ] **Step 1: Write the failing tests** (use the existing temp-home and fake-Runner helpers in `fake_test.go`):
  - `TestApplySystemLayoutRemovesUserPanelDirs`:
    - **Setup:** `home/.config/cosmic/com.system76.CosmicPanel.Panel/v1/size` exists, and the layout dir names that id.
    - **Expect:** after `ApplyLayout(r, home, isotope)`, that dir does not exist, other `~/.config/cosmic` dirs (e.g. `com.system76.CosmicTk`) are untouched, and the runner saw `pkill -x cosmic-panel`.
  - `TestCurrentLayoutIsSystemWhenUserHasNoPanelDirs`: empty `~/.config/cosmic` → `"isotope"`.
  - `TestCurrentLayoutNotSystemWhenUserHasPanelDirs`: user has the bottom-panel files → `"bottom-panel"`. User has some other panel edit → `""`.
  - `TestApplyOtherLayoutStillCopies`: the existing behaviour for a non-system layout is unchanged (keep or extend the current test).
- [ ] **Step 2: Run them to see them fail.** Run: `cd welcome && go test -run 'Layout' ./...`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - `ApplyLayout`: when `l.IsSystem()`, `os.RemoveAll` each `configDirs()` name under `~/.config/cosmic` instead of copying.
  - `installedIn`: when `l.IsSystem()`, true iff none of those dirs exists under home.
- [ ] **Step 4: Run `go test ./...` in welcome/ (all PASS), then commit.** `feat(welcome): the Isotope layout restores COSMIC's system panel defaults`

### Task 6: Patches in the COSMIC build

**Files:**
- Modify: `tools/cosmic-build.sh`
- Create: `patches/cosmic/README.md` (the layout below, and the header rule)
- Create: `test/cosmic-build/test-cosmic-build-patches.sh`, run in ci.yml
- Modify: `Dockerfile:3480`: `COPY patches/cosmic /usr/share/isotope-patches` next to the `cosmic-build` COPY.

**Interfaces:**
- **Patch layout:**
  - `patches/cosmic/<repo>/*.patch` (applied with `patch -p1` in the unpacked repo, in name order);
  - `patches/cosmic/libcosmic/*.patch` (applied to every libcosmic git checkout the repo's Cargo.lock names).
- **cosmic-build.sh:**
  - New env `COSMIC_PATCHES` (default `/usr/share/isotope-patches`).
  - `cosmic-build.sh <repo-or-dir>`: a first argument that is an existing directory is copied instead of downloaded. Task 10 uses this for `applets/`.
- **Rules:**
  - Each patched checkout gets a stamp `.isotope-patched-<sha256 of the libcosmic patches>`. A checkout that has the stamp is skipped; a failed patch fails the build.
  - `CARGO_TARGET_DIR` gets the suffix `/<first 12 hex of the sha256 of all patches>`, so a stale cache cannot hold unpatched code.

- [ ] **Step 1: Write the failing test.** It stubs `curl`, `cargo` and `just` on `PATH` and runs cosmic-build.sh against a fixture repo with a fake `$CARGO_HOME/git/checkouts/libcosmic-x/abc1234/` (Cargo.lock names `rev=abc1234…`). It asserts:
  - the repo patch applied;
  - the libcosmic patch applied once, and running the build twice does not re-apply it;
  - a broken patch exits non-zero;
  - `CARGO_TARGET_DIR` seen by the stub `cargo` ends in the 12-hex hash.
- [ ] **Step 2: Run it to see it fail.** Run: `test/cosmic-build/test-cosmic-build-patches.sh`. Expected: FAIL.
- [ ] **Step 3: Implement it in cosmic-build.sh.**
  - Order: unpack/copy → repo patches → `cargo fetch --locked` → libcosmic patches.
  - Find each checkout by the short revisions in the Cargo.lock's `git+https://github.com/pop-os/libcosmic…#<rev>` lines.
  - Then build as today.
- [ ] **Step 4: Run the test (PASS) and the whole unit CI locally, then commit.** `build(cosmic): apply Isotope's patches to COSMIC and to every libcosmic it builds with`

### Task 7: Window buttons on the left

**Files:**
- Create: `patches/cosmic/libcosmic/0001-window-controls-left.patch`. If the Cargo.locks name more than one libcosmic revision and one patch does not apply to all, use one directory per revision; Task 6's lookup picks it by rev.
- Create: `rootfs-cosmic/usr/share/cosmic/com.system76.CosmicTk/v1/window_controls_left` (`true`)
- Modify: the `Dockerfile` gsettings override: `[org.gnome.desktop.wm.preferences] button-layout='close,minimize,maximize:'`, asserted in the same RUN.
- Modify: `test/look/check-look.sh`

**Interfaces:**
- **New CosmicTk field:** `window_controls_left: bool` (serde default `false`).
- **Header bar change:** when the field is true, the header bar puts close, minimize, maximize at the start, in that order. They are round 12 px buttons in a neutral colour that show their glyph on hover, as the mock's `.lights`.
- **Marker:** the binary marker is the field name `window_controls_left`.

- [ ] **Step 1: List the libcosmic revisions.** Grep the epoch-1.11.0 Cargo.lock of every repo cosmic-build builds, plus `applets/`. Record them in the patch README.
- [ ] **Step 2: Add the failing image check (COSMIC).**
  - Every ELF under `/usr/bin` and `/usr/libexec` that contains `com.system76.CosmicTk` also contains `window_controls_left` (`grep -qa`).
  - CosmicTk `window_controls_left` is `true`.
  - gsettings `button-layout` is `'close,minimize,maximize:'`.
- [ ] **Step 3: Write the patch** against the libcosmic source at each revision: the `CosmicTk` config struct and the `header_bar` widget. Give it a header naming what it changes and why. Check it with `patch --dry-run -p1` in each checkout.
- [ ] **Step 4: Build the COSMIC shell and apps** (about 5 h on CI), run the check (PASS), and commit. `feat(cosmic): round window buttons on the left, as Isotope's default`

### Task 8: The centred login screen

**Files:**
- Create: `patches/cosmic/cosmic-greeter/0001-isotope-centred-login.patch`
- Create: `rootfs-cosmic/usr/share/isotope/greeter-mark.svg` (from `docs/assets/isotope-mark.svg`; add a `RULES` entry in gen-theme.py so its colours are `brand`/`brand_glow`)
- Modify: `Dockerfile` (cosmic-config): make `isotope-desktop-{day,night}-blur.png` from the two wallpapers with a Gaussian blur, radius 40, in a build stage with PIL (tools/gen-wallpaper.py already uses it), into `/usr/share/backgrounds/isotope/`.
- Modify: `test/look/check-look.sh`

**Interfaces:**
- Consumes: the system theme (Task 3); Task 6's patch layout.
- Produces:
  - the greeter background rule `fn greeter_background(wallpaper: &Path) -> (PathBuf, bool /*scrim*/)`: the `-blur.png` sibling for the two Isotope wallpapers, otherwise the original with a scrim of `base` at 45% alpha;
  - the greeter layout:
    - date and large clock at the top centre;
    - avatar, name and pill password field in the centre, with caps-lock and error lines under the field and the "Other users ›" link under those;
    - glass buttons below for accessibility, sleep, restart and shut down;
    - top-right strip: keyboard layout, session, status icons;
    - top-left: mark plus "Isotope" wordmark.

- [ ] **Step 1: Write the failing Rust unit test in the patch** (`#[cfg(test)]` in the new module): `greeter_background` maps both Isotope wallpapers to `-blur.png` with no scrim, and `/home/x/pic.jpg` to itself with the scrim.
- [ ] **Step 2: Write the patch.**
  - Re-lay out `App::menu` in `src/greeter.rs` as above.
  - Remove the two-column frame and fix the subsurface size and divider widths that assume 866 px.
  - Keep every action and chooser the greeter has today.
  - Run the test with `cargo test` in the patched checkout: PASS.
- [ ] **Step 3: Add the image checks.**
  - Both `-blur.png` files exist.
  - `greeter-mark.svg` exists.
  - `cosmic-greeter` contains the string `greeter-mark.svg`.
  - `python3 tools/gen-theme.py --check` passes.
- [ ] **Step 4: Build cosmic-apps, run the check, boot a VM (`tools/vm.sh`) and screenshot the greeter. Commit.** `feat(cosmic): a centred, branded login screen in the desktop's look`

### Task 9: The wallpaper follows light and dark

**Files:**
- Create: `update/wallpaper.go`, `update/wallpaper_test.go`
- Modify: `update/main.go` (a `case "wallpaper-follow":` next to `case "welcome":`; one help line)
- Create: `rootfs-cosmic/etc/xdg/autostart/isotope-wallpaper.desktop` (`Exec=isotope wallpaper-follow`, `OnlyShowIn=COSMIC;`, `NoDisplay=true`, like isotope-update-tray.desktop)
- Modify: `test/look/check-look.sh`

**Interfaces:**
- **Pure rule:** `func wallpaperFor(current string, dark bool) (string, bool)` returns the new path and whether to change.
- **Effective config readers** (user file, else `/usr/share/cosmic`):
  - `func readIsDark(home, sysDir string) bool`;
  - `func currentWallpaper(home, sysDir string) (ron string, path string, err error)`.
- **Writer:** `func setWallpaper(home, ron, newPath string) error` writes the effective `all` RON, with only the `source: Path("…")` value replaced, to `~/.config/cosmic/com.system76.CosmicBackground/v1/all`.
- **Loop:** `func runWallpaperFollow(ctx context.Context, home, sysDir string) error` runs once at start, then on every change of `~/.config/cosmic/com.system76.CosmicTheme.Mode/v1/` (inotify via `golang.org/x/sys/unix`, already a dependency; watch the directory, since the file may not exist yet).

- [ ] **Step 1: Write the failing tests:**
  - `TestWallpaperForSwitchesIsotopePair`:
    - night path + light mode → day path, true;
    - day path + dark mode → night path, true;
    - night + dark → unchanged, false.
  - `TestWallpaperForLeavesOwnPicture`: `/home/u/cat.jpg` either mode → false.
  - `TestReadIsDarkFallsBackToSystem`: no user file, system `true` → true. User `false` → false.
  - `TestSetWallpaperKeepsOtherFields`: the written RON equals the input with only the path changed (use the repo's `rootfs-cosmic/.../CosmicBackground/v1/all` as the fixture).
  - `TestFollowCatchesUpAtStart`: user `is_dark` false, no user background → after one pass the user file names the day picture.
- [ ] **Step 2: Run them to see them fail.** Run: `cd update && go test -run 'Wallpaper|IsDark|Follow' ./...`. Expected: FAIL.
- [ ] **Step 3: Implement** the signatures above.
- [ ] **Step 4: Run `go test ./...` (PASS). Add the image check for the autostart entry on COSMIC. Commit.** `feat(cosmic): the default wallpaper follows light and dark`

### Task 10: The top-bar mark and the focused app

**Files:**
- Create:
  - `applets/Cargo.toml` (workspace) and `applets/Cargo.lock`;
  - `applets/isotope-mark/{Cargo.toml,src/main.rs,src/name.rs}`;
  - `applets/isotope-mark/data/io.kairos.Isotope.Mark.desktop` (the applet keys the stock applets' desktop files use);
  - `applets/justfile` with an `install` recipe, so cosmic-build.sh can install it.
- Modify: `Dockerfile`: a stage `isotope-applets` FROM `cosmic-build` running `cosmic-build /src/applets` (Task 6's directory mode); copy `/cosmic` into the COSMIC image.
- Modify: the Panel `plugins_wings` left list (Task 4's layout dir): `io.kairos.Isotope.Mark` first, then workspaces.
- Modify: `test/look/check-look.sh`

**Interfaces:**
- Consumes:
  - libcosmic at the revision cosmic-applets epoch-1.11.0 pins, as a git dep, so Task 6 patches it;
  - the toplevel info subscription the stock app-list applet uses (`cosmic::cctk`).
- Produces:
  - binary `isotope-applet-mark`;
  - `fn display_name(app_id: &str, title: &str, entries: &[DesktopEntryName]) -> String`, which returns the desktop entry's localized name for the app ID, else the title, else `""`;
  - clicking runs `isotope-menu`;
  - the mark is `docs/assets/isotope-mark.svg` drawn as a symbolic icon in the panel's text colour, using the same shapes as `update/icons.go`.

- [ ] **Step 1: Write the failing tests in `name.rs`:**
  - `known_app_uses_entry_name`: `com.system76.CosmicFiles` → `"Files"`;
  - `unknown_app_uses_title`;
  - `no_focus_is_empty`.
- [ ] **Step 2: Run `cargo test` in the cosmic-build container. Expected: FAIL.**
- [ ] **Step 3: Implement the applet** (button with the mark and a semibold label; focused-toplevel subscription; `display_name`). Run `cargo test`: PASS.
- [ ] **Step 4: Add image checks (COSMIC).**
  - `isotope-applet-mark` exists and links (`ldd` clean).
  - Every applet ID named in the Panel and Dock `plugins_*` has a `.desktop` file under `/usr/share/applications`.
  - The binary contains `window_controls_left` (patched libcosmic).
- [ ] **Step 5: Build, check, and commit.** `feat(cosmic): the Isotope mark and the focused app at the left of the top bar`

### Task 11: Quick settings

**Files:**
- Create:
  - `applets/isotope-quick/{Cargo.toml,src/main.rs,src/state.rs,src/backend/*.rs}` (one file per backend: network, bluetooth, keyboard, mode, dnd, brightness, audio);
  - `applets/isotope-quick/data/io.kairos.Isotope.QuickSettings.desktop`.
- Modify: the Panel `plugins_wings` right list: replace network, bluetooth, audio and power with `io.kairos.Isotope.QuickSettings`. Keep status area, notifications, battery and time.
- Modify: `test/look/check-look.sh`

**Interfaces:**
- **Pure state:**

```rust
pub struct State { wifi: Toggle<Option<String>>, bt: Toggle<Option<String>>, layout: String,
                   dark: bool, dnd: bool, brightness: Option<f32>, volume: Option<f32>, muted: bool }
pub enum Msg { WifiChanged(bool, Option<String>), BtChanged(bool, Option<String>), LayoutChanged(String),
               DarkToggled, DndToggled, Brightness(f32), Volume(f32), MuteToggled, OpenDetail(Detail) }
pub fn update(s: &mut State, m: Msg) -> Vec<Effect>
```

  `Effect` is what the backends do: `SetWifi(bool)`, `SetBt(bool)`, `SetDark(bool)`, `SetDnd(bool)`, `SetBrightness(f32)`, `SetVolume(f32)`, `SetMute(bool)`, `Spawn(Vec<String>)`.
- **Backends,** reusing the crates the stock applets use at the pinned tag (read cosmic-applets' Cargo.toml; prefer `cosmic-settings-subscriptions`):
  - NetworkManager and BlueZ over D-Bus;
  - the keyboard layout from CosmicComp `xkb_config`;
  - dark via cosmic-config `CosmicTheme.Mode`;
  - DND via CosmicNotifications' `do_not_disturb` config key (check the name at the tag);
  - brightness via the settings daemon's D-Bus interface;
  - audio as the stock audio applet does it.
- **Detail rows:** spawn `cosmic-settings wireless`, `cosmic-settings bluetooth`, `cosmic-settings keyboard`. The bottom row has Settings (`cosmic-settings`) and power (spawn what the stock power applet's buttons do).
- **Panel button:** the status icons (network, volume, battery).

- [ ] **Step 1: Write the failing tests in `state.rs`:**
  - `dark_toggle_flips_and_emits_set_dark`;
  - `dnd_toggle_emits_set_dnd`;
  - `volume_clamps_to_0_1`;
  - `wifi_off_hides_network_name`;
  - `brightness_none_hides_slider`: no backlight → `brightness` stays `None`.
  - `open_detail_wifi_spawns_cosmic_settings_wireless`.
- [ ] **Step 2: Run `cargo test`. Expected: FAIL.**
- [ ] **Step 3: Implement `state.rs`, then the backends, then the popup view** (layout and colours per the mock: on-state tiles use the accent, everything else the neutrals). Run `cargo test`: PASS.
- [ ] **Step 4: Run the image check (Task 10's applet-ID check now covers this applet). Build and commit.** `feat(cosmic): quick settings in one frosted panel`

### Task 12: Whole look on a real session, docs, PR

**Files:**
- Modify: `AGENTS.md`:
  - the theme pipeline (palette → cosmic-theme.json → isotope-cosmic-theme);
  - `patches/cosmic/`;
  - `applets/`;
  - the icon theme.
- Modify: the release notes draft, if the repo keeps one. Otherwise put the text in the PR body: how existing users get the new look (COSMIC Settings → Appearance → Reset to default, or remove `~/.config/cosmic/com.system76.CosmicTheme*`), and that previously saved panel layouts stay until "Isotope" is chosen in Welcome.
- Modify: `COSMIC_LOOK` in gen-theme.py and the panel opacity, only if tuning requires it.

- [ ] **Step 1: Build all four images.** Run `test/look/check-look.sh` on each. Expected: all PASS.
- [ ] **Step 2: Boot COSMIC in a VM (`tools/vm.sh`, the driving-qemu-vms skill) and take screenshots of:**
  - the greeter;
  - the desktop in dark;
  - the desktop in light (flip in Welcome's Appearance page; the wallpaper must change too);
  - a frosted app window with left buttons;
  - the dock;
  - the quick-settings popup.

  Also boot sway and screenshot fuzzel. Compare each with the mock. Text over the frosted top bar and popups must read on both wallpapers; if not, raise `frosted` or lower the opacity in `COSMIC_LOOK`, regenerate, and rebuild.
- [ ] **Step 3: Tiling-mode focus check.** With `active_hint` 0, tile two windows. If the focused one is not obvious, set CosmicComp `active_hint` to a thin neutral hint (the spec's fallback) and say so in the PR.
- [ ] **Step 4: Update the docs.** Commit `docs: the Isotope look, its pipeline and its patches`.
- [ ] **Step 5: Push and open the PR.**
  - `git push ci-forks feat/isotope-look`.
  - Open the PR with `gh api repos/kairos-community/isotope/pulls -f head_repo=ci-forks/isotope -f head=feat/isotope-look -f base=main -f title="feat: Isotope's look — COSMIC theme, login screen, quick settings and Conflux icons" -F body=@<body file> --jq .html_url`.
  - The body includes the screenshots, the list of apps without a Conflux icon, the libcosmic revisions patched, and the end line `🤖 Generated with [Claude Code](https://claude.com/claude-code)`.
