# Notes for agents working on hadron-desktop

## Every desktop variant has the same feature set

A variant is one value of `DESKTOP` (today sway, i3, cosmic, niri). The agent
image is an overlay on i3, not a variant, and is exempt where noted.

A user who switches variants must find the same Hadron features, reached the
same way. When you add a variant, or a feature that one variant gets, add it to
every variant in the same change, or say in the PR which variants are missing
it and why. A variant whose shell already provides a feature (COSMIC's panel,
Noctalia) uses that instead of a Hadron script, but the feature must exist.

The baseline:

| Feature | How the variants provide it today |
|---|---|
| Boot splash and branded login | `kairos-splash`; ly, or cosmic-greeter for COSMIC. `make check-boot-ux` asserts the subtitle per variant. In the initramfs the splash is also the password agent that asks for the disk passphrase (`rootfs/usr/lib/dracut/modules.d/51hadron-askpass/`, `splash/README.md`) |
| Session identity | `/etc/hadron-desktop/session`, `/etc/issue`, `/etc/motd` in `rootfs-<variant>/`; `/etc/hadron-desktop/release` written in the Dockerfile `kairos` stage |
| Notifications server | mako (sway), dunst (i3), cosmic-notifications, Noctalia |
| Update tray icon (hadron-update) | `hadron-update tray` at session start, talking to the root `hadron-updated` daemon over D-Bus; see `docs/superpowers/specs/2026-11-05-tray-updater-design.md`. Not on agent, which keeps `hadron-update upgrade` through sudo |
| Upgrades only by `admin`, through polkit | polkit with the `admin` rule (`50-hadron-update.rules`) on all four variants. Agent removes the update service and its polkit files |
| System tray that hosts StatusNotifierItem icons | waybar `tray`, i3bar + snixembed (started from `config.d/30-update-tray.conf`), COSMIC status area, Noctalia `tray` |
| First-login welcome | `hadron-welcome`, a notification that opens the shortcut sheet when clicked; COSMIC uses cosmic-initial-setup |
| First-run app picker (a browser and essentials from Flathub) | `hadron-first-run-apps --login` at session start: `exec` in the sway and i3 configs, `spawn-at-startup` on niri, `/etc/xdg/autostart/hadron-first-run-apps.desktop` on COSMIC (it waits for cosmic-initial-setup). Once per user (stamp in `~/.local/state/hadron/`), after `nm-online`; "Install apps" in `hadron-menu` reopens it. `test/first-run/check-first-run-apps.sh`. Not on agent, which removes the script and its i3 line |
| Keyboard habits (tiling variants) | Super+Enter terminal, Super+D launcher (`hadron-apps` on i3, which lists .desktop and Flatpak apps; Super+Shift+D keeps `dmenu_run`), Super+Shift+Q close, Super+Shift+E power menu, Super+F1 shortcut sheet (`hadron-help`) |
| Notification keys | Super+BackSpace closes the newest notification, Super+Shift+BackSpace closes all, Super+Ctrl+BackSpace shows the last closed (the history panel on niri), Super+Alt+BackSpace turns do not disturb on and off. `hadron-notify-key` sends them to mako, dunst or Noctalia. COSMIC binds only do not disturb, because its notification server has no command interface; its notifications close with a click |
| Network, audio, power menus | `hadron-wifi-menu`, `hadron-audio-menu`, `hadron-power-menu`, or the shell's own panels |
| Audio | PipeWire and WirePlumber started with the session, media keys work |
| Screenshots and screen lock | a key binding for each. Lockers: swaylock (sway, niri), i3lock (i3), COSMIC's own; sway (swayidle) and i3 (xss-lock) also lock on idle and before suspend. i3 takes screenshots with `hadron-screenshot`. The agent overlay starts no locker |
| Flatpak apps with working file dialogs and dark mode | flatpak, xdg-desktop-portal and a backend reporting `color-scheme`: the GTK backend with its Settings interface (plus xdg-desktop-portal-wlr on sway) on sway, i3 and niri, reading the `prefer-dark` override in gsettings-desktop-schemas; COSMIC's own portal |
| Hardware keys | volume, mute, brightness, play/pause/next, with an on-screen level. `hadron-media-key` on sway and i3 (wpctl, logind `SetBrightness`, MPRIS, and a mako or dunst progress pop-up); Noctalia and COSMIC (cosmic-settings-daemon, cosmic-osd) handle the keys themselves, except that COSMIC's playback keys call `hadron-media-key` because Hadron has no playerctl |
| Hadron menu on Super+Escape | one menu for apps, Wi-Fi, Bluetooth, audio, capture, clipboard history, update, extensions, help and power: `hadron-menu` through `hadron-pick` (fuzzel on sway, niri and COSMIC; dmenu on i3). Each entry opens what the variant uses: the `hadron-*` menus, Noctalia's panels, COSMIC Settings and cosmic-osd. COSMIC's own Super+Escape lock moves to Super+Alt+L, niri's shortcut-inhibit toggle to Super+Shift+Escape |
| Clipboard history on Super+Ctrl+V | `hadron-clipboard`, also in the Hadron menu: the last 200 copies, kept in memory for the session, without the copies a password manager marks `x-kde-passwordManagerHint`. cliphist fed by `wl-paste --watch` on sway and COSMIC (cosmic-comp offers data-control to unsandboxed clients, no setting), clipnotify + xsel on i3 (text only), Noctalia's clipboard panel on niri. Super+V stays split (sway, i3) and float (niri). Not on agent: an AI-driven appliance must not keep the secrets it copies; Dockerfile.agent removes it |
| Bluetooth menu | pair, connect, disconnect, forget, power. `hadron-bluetooth-menu` (bluetoothctl through `hadron-pick`) on sway and i3, Super+Shift+B and the waybar module; the Noctalia control center and COSMIC's Bluetooth applet |
| Keyboard layout and timezone from the installer | the installer's cloud-config writes `/etc/hadron-desktop/keyboard`, `/etc/X11/xorg.conf.d/00-keyboard.conf`, the `KEYMAP` in `/etc/vconsole.conf` and the `/etc/localtime` link in an initramfs stage, so every boot gets them again. sway (`start-desktop`), niri (`niri-session`) and COSMIC (`start-cosmic`, greetd) read the layout as `XKB_DEFAULT_LAYOUT`/`XKB_DEFAULT_VARIANT` from `/etc/profile.d/hadron-keyboard.sh`; i3's XLibre reads the xorg.conf.d file. Hadron has no localed, so `localectl` does not work |
| Saved-password store | gnome-keyring (Secret Service and its portal) on all four variants, so Electron and Chromium Flatpaks do not store secrets in plain text. `pam_gnome_keyring` in ly's and greetd's PAM services unlocks the login keyring with the login password; `gnome-keyring-daemon.socket` is enabled for every user. The agent removes it (autologin, nothing can unlock it) |
| Night light | a warmer screen (4500 K) from 20:00 to 07:00, at fixed times because Hadron has no location. Super+Alt+N and the Hadron menu's "Night light" run `hadron-night-light toggle`, which keeps the setting in `$XDG_STATE_HOME/hadron-desktop/night-light` and restores it at session start: wlsunset on sway, xsct in a one-minute loop on i3, Noctalia's night light on niri (`noctalia msg nightlight-enable`). Not on COSMIC: cosmic-comp has no gamma control protocol and COSMIC Settings has its night light page disabled (epoch 1.11.0). `test/night-light/check-night-light.sh` checks the script |
| Emoji | Noto Color Emoji (the Dockerfile `emoji` stage; FreeType is built with libpng to load its PNG glyphs) as the last fallback in `60-hadron.conf`. `hadron-emoji` on Super+; and in the Hadron menu: the list `/usr/share/hadron/emoji.txt`, made from Unicode's `emoji-test.txt`, through `hadron-pick`, then wl-copy (sway, niri, COSMIC) or xsel (i3) and a notification. COSMIC's Super+; is added in the Dockerfile. `test/emoji/check-emoji.sh` |
| System extensions | `hadron-ext` (a link to `hadron-update`, also `hadron-update ext`): browse the catalogs in `/etc/hadron-desktop/extensions.conf` (default hadron-layers), install, enable, disable, remove. The root `hadron-updated` runs `kairos-agent sysext` (`--common`, enabled `--now`) behind polkit's `io.kairos.hadron-update.extensions` (admin only, no Flatpak callers), shares the update job slot, and refuses an extension whose files differ from the running `/usr` (systemd-dissect `--mtree`) unless `--force`. The "Extensions" entry of `hadron-menu` (hadron-pick, ✓ on the installed ones) and the tray's "Extensions…" open the same list on every variant. `extensions.ignore_signatures` is set in `/etc/kairos/90_hadron_extensions.yaml` and `/system/oem/93_hadron_extensions.yaml`, both removed on a Trusted Boot image. See `docs/superpowers/specs/2026-11-07-extensions-design.md`; `test/extensions/check-extensions-menu.sh`. Not on agent: Dockerfile.agent removes `hadron-ext`, its config and the signature setting |
| Disk encryption from the installer (opt-in) | the installer's "Encrypt your files?" step, on every machine and off by default: `install.encrypted_partitions: [COS_PERSISTENT]` with kcrypt `method: passphrase` (LUKS2, argon2id, label `COS_PERSISTENT_LUKS`, subsystem `kcrypt:passphrase`); the passphrase reaches kcrypt only through a 0600 file in `/run` that the installer deletes, never through `/oem`. Asked for at every boot by `hadron-unlock` in the initramfs (#60). Marker `/etc/hadron-desktop/encryption` (`mode=passphrase`). The same installer for every variant; see `docs/superpowers/specs/2026-11-07-encrypt-home-design.md`. Not on agent, whose installer runs `hadron-install-ui --progress-only` without the wizard |

## The shortcut sheet is generated

`hadron-help` shows `rootfs-<variant>/usr/share/hadron/shortcuts.txt`, which
`tools/gen-shortcuts.py` writes from the key bindings in the sway, i3 and niri
configs. A binding appears on the sheet only when a `# help: ...` comment
(`// help:` in niri) is on the line above it; see the script's docstring for
`help+:`, `help-section:` and `help-row:`. After you change a binding or its
comment, run `python3 tools/gen-shortcuts.py` and commit the result; CI fails
on a stale sheet. COSMIC's list is hand-kept in `tools/shortcuts-cosmic.txt`.

## Adding a variant

Besides the features above, a variant is wired into the build and release in
these places. Missing one ships a variant that builds locally but is never
released, never upgraded or never checked:

- `Makefile`: `VALID_DESKTOPS`, the help header, `check-boot-ux`.
- `Dockerfile`: its stages, the `DESKTOP` switch, the `kairos` stage.
- `rootfs-<variant>/` for its configuration.
- `.github/workflows/release.yml`: the build matrix (every variant is built
  in both GPU lines, full and vm), the allowed flavors and the expected asset
  counts (two ISOs per variant), the manifest's flavor/line pairs, and the
  public-image check (`<variant>-<tag>` and `<variant>-vm-<tag>`).
- `scripts/release-manifest.sh` (the flavor/line pairs) and `update/release.go`
  (the flavor list).
- The tray updater's "Switch desktop" list.
- `README.md`: the desktop list and the download table;
  `docs/DEVELOPMENT.md`: the variant list and the releases section.
- A design note in `docs/superpowers/specs/`.
