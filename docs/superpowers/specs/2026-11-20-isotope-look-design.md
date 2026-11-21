# Isotope's look: the COSMIC theme, the login screen and the icons

Date: 2026-11-20. Status: design, approved approach (below).

## Why

COSMIC on Isotope still looks like stock COSMIC. #124 gave Isotope its own
colours (theme/palette.toml: the warm "Carbon" neutrals, an ivory accent, the
Autunite brand yellow), but only the tiling desktops, the installer, the
splash and GRUB use them. The COSMIC desktop and its login screen do not, and
no variant has an icon theme: COSMIC uses its own set, and sway, i3 and niri
have only hicolor, so most launchers show few or no app icons.

The owner wants Isotope to feel like a polished product, "like macOS so to
say": calm translucent materials, depth instead of outlines, a thin top bar,
a floating dock, a centred lock-screen style login, and one consistent,
good-looking icon set. These must be the defaults: what a new install shows
without the user changing anything.

## Goals

- A COSMIC theme built from palette.toml, in dark and light, as the default.
  It looks like the approved mock, `assets/isotope-look/mock.html` (open it in
  a browser; the switches show dark/light and solid/translucent; the approved
  state is ivory accent, translucent, Conflux icons).
- The COSMIC login screen (cosmic-greeter) in the same look, laid out as in
  the mock: date and a large clock at the top centre, the user's avatar, name
  and a pill password field in the centre, the power and accessibility
  actions as glass buttons below.
- The Conflux icon theme, installed on all four variants and the default
  icon theme on each.
- The parts of the mock COSMIC 1.11 does not have, which the owner wants in
  this project too: a grouped quick-settings panel (control centre), the
  Isotope mark and the focused app's name at the left of the top bar, and
  round window buttons on the left of the title bar.
- The wallpaper follows light and dark: the day picture in light mode, the
  night picture in dark mode, as in the mock.

## Non-goals

- New colours. palette.toml is not changed: the theme accent stays ivory
  (`accent`), the brand yellow stays for the mark, splash and GRUB. The owner
  tried Autunite as the accent and chose ivory, to keep #124's split.
- A second icon theme (Tela) or an icon/theme picker. Tela can be added later
  as an option.
- Restyling sway, i3 or niri beyond the icon theme. They already use the
  palette; translucency and the dock are COSMIC's.
- Restyling ly (the text login of sway, i3 and niri).
- Left window buttons in apps that do not use the patched libcosmic or GTK's
  `button-layout` (Flathub builds of COSMIC apps, Qt and Electron apps).
- Left window buttons and the Conflux icon theme in Flatpak GTK apps on COSMIC.
  Known limitation: they read those keys through the Settings portal, and
  xdg-desktop-portal-cosmic serves only `org.freedesktop.appearance` and
  text-scaling-factor, not `button-layout` or `icon-theme`. The gsettings
  override stays as the default for host GTK apps. Installing
  xdg-desktop-portal-gtk as a second Settings backend would fix it, and is
  left for later.
- Migrating users who have changed their COSMIC theme: their changes win.

## Anti-goals (a failure even if it works)

- A login screen that looks different from the desktop it opens.
- A theme that changes colours the palette does not define, or hard-codes a
  hex value outside tools/gen-theme.py's reach.
- Breaking Isotope Welcome's Appearance (light/dark) or Layout pages.
- Mixed icons in the places people look most: the dock, the launcher and the
  file manager. Gaps in rarely seen apps are acceptable.
- Low contrast text on translucent surfaces (text over the wallpaper through
  a frosted panel must stay readable on both bundled wallpapers).

## Constraints

- COSMIC epoch 1.11.0, built from source in the Dockerfile (`cosmic-build`).
  Upstream facts this design relies on (read from source at epoch-1.11.0;
  libcosmic at the commit cosmic-settings pins, 430f3d48):
  - cosmic-config reads `~/.config/cosmic/<id>/v<N>/<key>` first, then the
    system default from the first XDG data dir that has
    `cosmic/<id>/v<N>/` (`/usr/share/cosmic/...` on Isotope). The match is per
    version directory, so an id's keys must all ship in one place. Nothing
    copies defaults into ~/.config; the settings daemon writes only keys that
    change.
  - Apps, cosmic-comp, cosmic-panel and the greeter read the computed theme
    (`com.system76.CosmicTheme.Dark` / `.Light`, v2), not the builder. Only
    cosmic-settings and the settings daemon read the builder
    (`...Dark.Builder` / `...Light.Builder`, v2) to rebuild the theme. A
    missing computed key falls back to stock COSMIC. So both the builder and
    the computed theme must ship, for dark and light.
  - `ThemeBuilder::build()` turns a builder into a theme;
    `cosmic-settings appearance import <file.ron>` runs it and writes both
    into the user config (whether it runs with no display or session bus is
    unconfirmed).
  - Blur: builder/theme keys `frosted` (strength) and the booleans
    `frosted_windows`, `frosted_system_interface`, `frosted_panel`,
    `frosted_applets`, `frosted_maximized_apps`. cosmic-comp blurs windows;
    cosmic-panel requests blur through `ext_background_effect`.
  - `com.system76.CosmicTk/v1`: `icon_theme`, `interface_font`,
    `monospace_font`, `header_size`, `interface_density`. The settings daemon
    mirrors the icon theme and fonts to gsettings.
  - Panels: `com.system76.CosmicPanel/v1/entries` and
    `com.system76.CosmicPanel.Panel` / `.Dock` (v1). cosmic-panel installs
    its stock defaults from `data/default_schema/`; the stock dock already
    floats (`expand_to_edges: false`, `anchor_gap: true`).
  - `com.system76.CosmicComp/v1`: `active_hint`, `appearance_settings`
    (`clip_floating_windows`, `clip_tiled_windows`, `shadow_tiled_windows`).
    Floating windows get a compositor shadow; focused ones at full alpha.
  - cosmic-greeter's login card is built in `src/greeter.rs`, `App::menu`:
    one 866 px card with the clock on the left and the user on the right,
    centred horizontally in the upper fifth. Its daemon reads each user's
    theme (falling back to `/usr/share/cosmic`); before a user is selected,
    the greeter uses the `cosmic-greeter` user's config, so also
    `/usr/share/cosmic`.
- palette.toml is the only colour source. Every colour in the new COSMIC
  files is written by tools/gen-theme.py, and CI's `--check` covers them.
- Every variant has the same features (AGENTS.md). The icon theme is a
  feature: all four variants. The COSMIC theme and greeter are COSMIC's look
  of features the others already have (a palette-themed desktop and login).
- Isotope Welcome (welcome/) writes `CosmicTheme.Mode/v1/is_dark` for light
  or dark, and its Layout page installs a layout from
  `/usr/share/cosmic-layouts` into ~/.config/cosmic and tells which one is
  current by comparing files.
- The UI fonts are Inter Variable and JetBrains Mono (the gsettings override
  in the Dockerfile `gsettings-desktop-schemas` stage).
- Conflux is GPL-3.0 and has no release tags: pin a commit (today
  85b0ef6dc59e4d459364337a92a14f604a2baa82, 11 MB unpacked). Its
  `index.theme` says `Inherits=Adwaita,breeze,hicolor`; Isotope has neither
  Adwaita icons nor breeze.

## Approaches

1. **Ship the look as system defaults in the image** (chosen). gen-theme.py
   writes the COSMIC theme builders from palette.toml; the image build turns
   them into computed themes and installs both, with the panel, toolkit and
   compositor defaults, under `/usr/share/cosmic`. A cosmic-greeter patch
   gives the centred login. Conflux is installed on every variant and set as
   the icon theme. The greeter, new users and anyone who has not changed a
   key get the look; nothing runs at login.
2. Apply the look from Isotope Welcome at first login (import the theme,
   write the keys). Simpler to build, but the greeter and people who skip
   Welcome never see it, and it writes everything into the user config, so
   later image updates to the look never reach them.
3. Patch COSMIC's built-in defaults (libcosmic's default theme, the panel's
   default schema). Same result as 1, but the patches must be carried and
   rebased for every COSMIC release, across several repositories.

The owner chose 1. Conflux only, no Tela (see Non-goals). The owner also
chose to include the mock's quick settings, top-bar mark and left window
buttons now, knowing that this adds two applets of our own and a libcosmic
patch (besides the greeter patch) to carry for every COSMIC release.

## The design

### Colours: palette.toml to a COSMIC theme

gen-theme.py gains two generated files, the dark and light theme builders,
as RON (`ThemeBuilder` serialization, the format `cosmic-settings appearance
import/export` uses): night roles for dark, day roles for light.

- The builder's `palette` (a `Dark(...)` / `Light(...)` palette): the
  neutrals `neutral_0`..`neutral_10` run from `crust` to `text` through the
  palette's grey ramp (crust, base, surface, overlay, border, muted, dim,
  subtext, text; interpolate where COSMIC has more steps). `accent_*` and the
  bright/ext colours come from the matching roles (accent, success, warning,
  error, violet, orange, blue, cyan).
- `accent` = `accent` (ivory at night, #3e3e32 by day), `success`,
  `warning`, `destructive` = `error`. `bg_color` = `base`,
  `primary_container_bg` = `surface`, `secondary_container_bg` = `overlay`.
  Text on the accent must be `on_accent`; if COSMIC derives it and gets it
  wrong, set it.
- The palette's `name` is "Isotope".

The image build turns each builder into the computed theme and installs
`com.system76.CosmicTheme.{Dark,Light}{,.Builder}/v2/*` and
`com.system76.CosmicTheme.Mode/v1/is_dark` (true: Isotope is dark by
default) under `/usr/share/cosmic`. A build that produces no computed theme,
or one whose accent is not the palette's, fails.

### Materials, shape and depth (the mock's look)

In both builders:

- Translucency: `frosted_windows`, `frosted_system_interface`,
  `frosted_panel` and `frosted_applets` on; `frosted_maximized_apps` off (a
  maximized window is opaque); `frosted` strength chosen so text stays
  readable on both bundled wallpapers (start at the default, Medium).
- Corners: windows and cards round like the mock (about 12 px on windows,
  `radius_s`); buttons and fields per the mock. Exact radii are the builder's
  call, matched against the mock.
- Focus by depth, not outline: `active_hint` 0 (no coloured border on the
  focused window) and the compositor's shadows on (CosmicComp
  `appearance_settings` with `shadow_tiled_windows` true as well, so tiled
  windows also get depth). If removing the hint makes the focused window
  hard to find in tiling mode, keep a thin hint in a neutral colour instead
  of the accent; say so in the PR.
  (Taken: the first VM session showed the focused tiled window was hard to
  find, so CosmicComp `active_hint` is true with a 2 px hint in the
  palette's `dim`, the ThemeBuilder's `window_hint`, set by
  tools/gen-theme.py.)
- Gaps: the mock's spacing between tiled windows (stock is 8).

CosmicTk (v1): `icon_theme` "Conflux", `interface_font` Inter Variable,
`monospace_font` JetBrains Mono, `header_size` and `interface_density`
Standard.

### Top bar and dock

Ship Isotope's own `com.system76.CosmicPanel*` defaults (start from
cosmic-panel's `data/default_schema/` at the pinned version, change what
differs):

- Top bar: thin (XS), full width, translucent (frosted, `opacity` below 1),
  no radius. Left: the workspaces; centre: nothing or the app title (as the
  mock); right: the status applets and the clock with the date. The
  isotope-update tray icon sits in the status area as today.
- Dock: floating at the bottom, not spanning the edge, translucent and
  frosted, pill-shaped, the app list with running indicators; it keeps its
  style when a window is maximized only if that looks right in testing.
- Add an "Isotope" layout to `/usr/share/cosmic-layouts` that is exactly
  these panel defaults, listed first (lowest `id`); the stock layouts stay
  as alternatives. (My call:) Welcome's Layout page changes for it: the
  Isotope layout is current when the user has no panel keys of their own,
  and choosing it removes the user's panel keys (the ids the layout names)
  instead of copying files, so the system defaults apply again and later
  image updates to the look reach that user. Other layouts are applied as
  today. This is a change to welcome/cosmic.go with tests, not a break of
  the Layout page.

The dock favourites stay as today
(`rootfs-cosmic/usr/share/cosmic/com.system76.CosmicAppList/v1/favorites`).

### Top bar: the Isotope mark and the focused app

A new COSMIC panel applet, in this repository (Rust, libcosmic's applet
API, built in the COSMIC stages like the upstream applets), at the left of
the top bar:

- The Isotope mark, monochrome in the panel's text colour (like the tray
  icon, #127). Clicking it opens the Isotope menu (`isotope-menu`), as the
  Apple menu does on macOS; Super+Escape still opens it too.
- Next to it, in semibold, the name of the focused app (from the toplevel
  info protocol the app list applet already uses), empty on the desktop.

The workspaces move to its right, as in the mock.

### Quick settings (control centre)

A second new applet in this repository: one top-bar button (the status
icons for network, sound and battery, as in the mock) that opens one
frosted popup laid out like the mock:

- A block with Wi-Fi (on/off, network name), Bluetooth (on/off, state) and
  Keyboard (current layout); each row's chevron or long press opens the
  matching detail: the stock COSMIC network and Bluetooth applet popups or
  COSMIC Settings pages, and the keyboard layout list.
- Dark style and Do not disturb as two toggle tiles (CosmicTheme.Mode
  `is_dark`; cosmic-notifications' do-not-disturb setting).
- Display brightness and Sound volume sliders (the settings daemon's
  brightness interface; the PipeWire default sink, as the stock audio
  applet does).
- A row at the bottom for Settings and the power menu.

It replaces the stock network, Bluetooth, audio and power applets in the
default top bar (they stay installed and can be added back). The battery
applet and the status area (tray icons, including isotope-update) stay. The
"on" state uses the accent; everything else the neutrals, per the mock.

### Window buttons on the left

A libcosmic patch, applied to the libcosmic every COSMIC app and cosmic-comp
in the image is built with (cosmic-comp draws the server-side title bars
with it): the header bar puts close, minimize and maximize at the left
start, as round neutral buttons that show their glyph on hover, as in the
mock (`.lights`). It is controlled by a new CosmicTk key (for example
`window_controls_left`, true in Isotope's defaults), so a user can put them
back on the right; COSMIC Settings does not show the key, which is
acceptable. The Isotope gsettings override sets
`button-layout='close,minimize,maximize:'` as the default for host GTK apps
(see the known limitation in Non-goals). Because each COSMIC app pins its own libcosmic revision, the
build must patch the revision each app's Cargo.lock names (a cargo
`[patch]` to a patched checkout of that revision, or patching the cargo git
checkout before the build); a build where an app is not patched fails the
image check.

### Wallpaper follows light and dark

cosmic-bg 1.11 does not switch pictures with the theme (its
`filter_by_theme` is not used). A small session helper, started on COSMIC
(an `isotope` subcommand with an autostart entry, per AGENTS.md), watches
`CosmicTheme.Mode/v1/is_dark` and sets `com.system76.CosmicBackground` to
`isotope-desktop-day.png` or `isotope-desktop-night.png`
(`/usr/share/backgrounds/isotope/`), only while the current wallpaper is one
of those two, so a user's own wallpaper is never replaced. It also runs
once at start, so a mode changed while logged out is caught up. The greeter
uses the same rule for its background. Contrast is tuned for the two
pairings that occur: dark over night, light over day.

### Login screen (cosmic-greeter)

A patch to cosmic-greeter, kept in the repository with a header that says
what it changes and why, applied by the COSMIC build (tools/cosmic-build.sh
has no patch step today; add one). The patch re-lays out `App::menu`:

- The date and a large clock, centred at the top.
- In the centre: the avatar, the user's name, a pill-shaped password field;
  the caps-lock and error lines under it.
- Below: the existing actions (accessibility, sleep, restart, shut down)
  as round glass buttons with labels; "Other users ›" as a text link under
  the password field, as in the mock.
- (My call:) nothing the greeter does today is dropped: the keyboard
  layout and session choosers move to a small status strip at the top
  right (with the existing status icons), as in the mock.
- (My call:) the Isotope mark and wordmark at the top left, as in the
  mock: this is the "branded login" AGENTS.md asks of every variant. The
  mark is docs/assets/isotope-mark.svg in the brand colours (brand,
  brand_glow), generated or checked by gen-theme.py like every other
  colour.
- The card loses its two-column frame; the subsurface size and the divider
  widths that assume two columns are adjusted.

The background is the user's wallpaper, softened (blurred or darkened) so the
text reads; the mock blurs it. If the greeter cannot blur cheaply, a
pre-blurred copy of the default wallpaper made at image build time, or a dark
scrim, is acceptable: the builder picks and says which in the PR.

The greeter's colours come from the system default theme above, so the
login and the desktop match.

### Icons: Conflux on every variant

- A Dockerfile stage downloads Conflux at the pinned commit (with
  renovate-style pinning like the other downloads), installs it to
  `/usr/share/icons/Conflux`, and rewrites `Inherits` to a theme that is in
  the image: COSMIC's own icon set (`Cosmic`, already built from
  pop-os/cosmic-icons) then `hicolor`. cosmic-icons is data only; install it
  on every variant so the fallback symbolic icons (toolbar, status) are the
  same everywhere.
- Conflux names few COSMIC and Flathub app IDs. Add links in the installed
  theme from the IDs Isotope shows to the Conflux icon that fits, at least:
  the COSMIC apps (Files, Terminal, Settings, Store, Text Editor, and the
  others the image installs, where Conflux has a fitting icon) and the
  first-run apps in `first-run-apps.list` (Firefox already matches; VLC
  needs `org.videolan.VLC`). An app with no fitting Conflux icon keeps its
  own; list those in the PR.
- Default everywhere: CosmicTk `icon_theme` (COSMIC; its settings daemon
  mirrors it to gsettings), `icon-theme='Conflux'` in the Isotope gsettings
  override (host GTK apps; Flatpak apps on COSMIC do not get it, see Non-goals), and the icon settings
  of the tiling variants' own programs that draw icons (fuzzel, mako, dunst,
  waybar, Noctalia, as applicable).
- License: GPL-3.0 icons, shipped as data; record it where the image lists
  third-party components, if it does.

### Light and dark

Both themes ship; `is_dark` defaults to true. Isotope Welcome's Appearance
page keeps working unchanged: it flips `is_dark`, and COSMIC switches to the
light theme built from the day palette. Since #126 removed
cosmic-initial-setup, nothing else installs COSMIC theme presets.

### Existing installs

System defaults apply to every key a user has not changed. A user whose
~/.config/cosmic already holds computed theme keys (for example from
changing the accent in Settings) keeps those keys. The release notes say how
to get the new look: remove `~/.config/cosmic/com.system76.CosmicTheme.Dark*`
and `~/.config/cosmic/com.system76.CosmicTheme.Light*` (the theme and builder
directories), then log out and in. COSMIC Settings, Appearance, Reset to
default does not do this: it writes libcosmic's stock theme into the user's
config, so it replaces the Isotope look with stock COSMIC.

## How it is checked

- `gen-theme.py --check` (CI) covers the two builder files.
- An image check script (under `test/`, like the other check scripts) on the
  built COSMIC image: the computed and builder themes exist for dark and
  light under `/usr/share/cosmic`, their accent equals the palette's; the
  panel, CosmicTk and CosmicComp defaults are there; the Isotope layout is in
  `/usr/share/cosmic-layouts`; on every variant `Conflux` is installed, its
  `Inherits` names only themes that exist, every link resolves, and the
  default icon theme settings name it.
- The applets build in CI with their own tests where logic allows (layout
  of the popup state, the wallpaper helper's rule as Go tests).
- The image check also finds the left-buttons patch in every COSMIC app
  binary that links libcosmic, and the two applets in the default panel.
- A real session, on hardware or a VM, before merge: screenshots of the
  greeter, the desktop in dark and light, an app with a frosted window, the
  dock, and the launcher on one tiling variant, compared with the mock.

## Left to the builder

- How the computed theme is produced at build time: `cosmic-settings
  appearance import` with a temporary `XDG_CONFIG_HOME` (if it runs headless)
  or a small Rust program on `cosmic-theme` calling `ThemeBuilder::build()`.
- The exact neutral ramp interpolation, radii, gaps, blur strength and panel
  opacity, matched against the mock and the contrast anti-goal.
- The top bar's centre content, and the dock's `keep_style_on_maximize`.
- The greeter background treatment (blur, pre-blurred image, or scrim).
- The full app-ID link list, and which tiling-variant programs need an icon
  theme setting.
- Splitting the work into PRs (for example: icons; COSMIC theme and panels;
  greeter; the two applets; the libcosmic patch; the wallpaper helper), as
  long as each PR leaves every variant working.
- The applets' crate layout and names, and how a row's detail opens.
- Whether the patch-every-revision step lives in tools/cosmic-build.sh.
