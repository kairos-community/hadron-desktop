# Isotope wallpapers

The renderers of the release and build wallpapers. Every picture is a pure
function of the script, its arguments and the pinned libraries: the same
input writes the same bytes.

    pip install -r tools/wallpapers/requirements.txt

## Release wallpapers

Each major release is named after an element and gets one drawn wallpaper:
Isotope 1 is Hydrogen, 2 Helium, 3 Lithium.

    python3 tools/wallpapers/hydrogen.py --mode night --out OUTDIR
    python3 tools/wallpapers/hydrogen.py --mode day --out OUTDIR

writes `isotope-1-hydrogen-{night,day}-3840x2160.png` and its 1920x1080
half (about 2 minutes on 4 cores). `--mode lock` moves the light to the
centre of the screen, for the lock screen idea. `--preview` writes a quick
1280x720 draft into `preview/`. `helium.py` and `lithium.py` are the next
two majors.

The current release's two images are committed in
`rootfs/usr/share/backgrounds/isotope/`, so the image build does not need
these libraries.

## Build wallpapers

Any build that is not a release gets a wallpaper drawn from its commit: forty
pillars of light over a night lake, one per hex digit of the sha, their
heights spelling it, and the sha engraved in the lower right. Maintainers see
at a glance that the build they run is a different one.

    python3 tools/wallpapers/build_wallpaper.py --seed "$(git rev-parse HEAD)" --out OUTDIR

writes `isotope-build-<sha12>-night-3840x2160.png` and its 1920x1080 half
(`--size 1080p` for that alone, about 10 s; `--mode day`; `--no-stamp`
leaves the sha out). A 40-digit sha is used as it is; any other seed is
hashed.

## Which one the desktop shows

Every variant points at `/usr/share/backgrounds/isotope/isotope-desktop-night.png`
(and `-day` for the shells with a light mode), a link to the release
wallpaper. A development build renders its build wallpaper and points the
link at it instead. Nothing else changes.

`tools/gen-wallpaper.py` (stdlib only) still draws the tiles and the plain
image the lock screen uses, as the fallback.

## Files

- `lib.py`: the shared renderer (canvas, light, water, palette).
- `mark-m1-256.png`: the mark's alpha for the engraving, from
  `docs/assets/isotope-mark.svg` (shell at 70%, the lifted side and core at
  100%). Redraw it when the mark changes.
- `fonts/`: JetBrains Mono and Inter for the engraving, with their licences.
