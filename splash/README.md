# hadron-desktop boot splash

A C99 fork of the upstream Kairos hadron splash (by way of
[AIOS](https://github.com/mudler/AIOS)'s `image/aios-splash`), rebranded for
hadron-desktop.

Built by the `hadron-splash` stage in the top-level `Dockerfile` and installed
to `/usr/bin/hadron-splash`, overwriting the base image's stock copy. The same
binary is used twice:

- **initramfs** — baked in by the `50hadron-splash` dracut module
  (`rootfs/usr/lib/dracut/modules.d/50hadron-splash/`), so the animation starts
  before switch-root.
- **booted system** — `rootfs/usr/lib/systemd/system/hadron-splash.service`,
  ordered `Before=ly@tty1.service`.

## Constraints

- **VGA-16 colours only.** The kernel VT is a 16-colour console; truecolor and
  256-colour values round to the wrong hue. See the `TOKYO` ramp in `main.c`.
  `flush_grid` emits only plain `30-37`/`90-97` SGR — never the 256-colour
  `38;5;N` form.
- **`ART_COLS` is display columns, not bytes.** `paint_logo` decodes UTF-8 and
  counts one column per glyph.
- **`MIN_COLS` must be at least `ART_COLS + 6`** — the HUD brackets are drawn at
  `left - 3` and `left + ART_COLS + 2`.
- The box-drawing and block glyphs need a console font that covers them; see
  `rootfs/etc/vconsole.conf`.

## Build locally

    cd splash && make && ./hadron-splash
